package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"kiro-go/config"

	"github.com/google/uuid"
)

// This file translates a REQUEST body from one wire protocol to the other so an
// external provider can serve a client that speaks the opposite protocol.
//
// Why translate at all, when passthrough is lossless? Because the endpoint a
// client uses is not a preference, it is fixed by the tool: Claude Code only ever
// calls /v1/messages, and most cheap OpenAI-compatible providers only implement
// /v1/chat/completions. Without a bridge those two can never meet, no matter how
// the operator prices or prioritises the provider.
//
// The translation is deliberately field-by-field rather than a generic passthrough
// of unknown keys: sending an OpenAI-only field to an Anthropic endpoint (or the
// reverse) is how you get a 400 from a strict upstream. Anything not mapped here
// is dropped on purpose. Where a concept genuinely has no counterpart, the loss is
// documented at the point it happens rather than hidden.

// bridgeDefaultMaxTokens is used when translating OpenAI -> Anthropic and the
// client sent no token cap. Anthropic requires max_tokens; OpenAI treats it as
// optional, so the field is frequently absent and a request without it would be
// rejected outright. 32000 is high enough not to truncate real answers while
// staying inside the output ceiling of current Claude-class models.
const bridgeDefaultMaxTokens = 32000

// ---------------------------------------------------------------------------
// Shared wire shapes
// ---------------------------------------------------------------------------

// bridgeOpenAIRequest is the subset of a Chat Completions request the bridge
// understands. json.RawMessage is used where a field is polymorphic on the wire
// so the decision can be made after inspecting it.
type bridgeOpenAIRequest struct {
	Model    string             `json:"model"`
	Messages []bridgeOpenAIMsg  `json:"messages"`
	Tools    []bridgeOpenAITool `json:"tools"`

	// Both spellings exist in the wild; max_completion_tokens is the newer one.
	MaxTokens           *int `json:"max_tokens"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`

	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	Stream      bool     `json:"stream"`

	Stop            json.RawMessage `json:"stop"`        // string or []string
	ToolChoice      json.RawMessage `json:"tool_choice"` // string or object
	ReasoningEffort string          `json:"reasoning_effort"`
}

type bridgeOpenAIMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string, []part, or null
	ToolCalls  []ToolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	// ReasoningContent is what an OpenAI-compatible provider echoes back for a
	// previous thinking turn. Carried across so multi-turn reasoning survives.
	ReasoningContent string `json:"reasoning_content"`
}

type bridgeOpenAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// bridgeAnthropicRequest is the subset of a /v1/messages request the bridge
// understands.
type bridgeAnthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []bridgeAnthropicMsg `json:"messages"`
	MaxTokens     int                  `json:"max_tokens"`
	System        json.RawMessage      `json:"system"` // string or []block
	Temperature   *float64             `json:"temperature"`
	TopP          *float64             `json:"top_p"`
	Stream        bool                 `json:"stream"`
	StopSequences []string             `json:"stop_sequences"`
	Tools         []bridgeAnthropicTool
	ToolChoice    json.RawMessage       `json:"tool_choice"`
	Thinking      *ClaudeThinkingConfig `json:"thinking"`
}

// UnmarshalJSON is hand-written only to tolerate Anthropic's server-side tool
// entries (web_search, computer_use, ...), which carry no input_schema and would
// otherwise abort decoding of the whole request. Unknown tool types are skipped.
func (r *bridgeAnthropicRequest) UnmarshalJSON(data []byte) error {
	type alias bridgeAnthropicRequest
	var raw struct {
		alias
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = bridgeAnthropicRequest(raw.alias)
	for _, entry := range raw.Tools {
		var tool bridgeAnthropicTool
		if err := json.Unmarshal(entry, &tool); err != nil {
			continue
		}
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		r.Tools = append(r.Tools, tool)
	}
	return nil
}

type bridgeAnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

type bridgeAnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// bridgeBlock is one Anthropic content block. It doubles as the output shape when
// building blocks, so omitempty matters: an absent field must not serialize.
type bridgeBlock struct {
	Type string `json:"type"`

	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// Signature is required by Anthropic on a replayed thinking block. A bridged
	// block has none (the text came from a different vendor), so it stays empty and
	// is only emitted when non-empty — an unsigned thinking block is rejected.
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// image / document
	Source json.RawMessage `json:"source,omitempty"`
}

// bridgeOpenAIPart is one Chat Completions content part.
type bridgeOpenAIPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI request -> Anthropic request
// ---------------------------------------------------------------------------

// bridgeOpenAIRequestToAnthropic rewrites a Chat Completions body as a
// /v1/messages body targeting upstreamModel.
//
// Structural differences handled here:
//   - system/developer messages move out of the array into the top-level "system"
//   - tool results move out of their own role into a user tool_result block
//   - assistant tool_calls become tool_use blocks
//   - consecutive same-role messages are merged, because Anthropic requires strict
//     user/assistant alternation while OpenAI permits repeats
func bridgeOpenAIRequestToAnthropic(raw []byte, upstreamModel string, stream bool) ([]byte, error) {
	var in bridgeOpenAIRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}

	out := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}

	maxTokens := bridgeDefaultMaxTokens
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		maxTokens = *in.MaxTokens
	} else if in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0 {
		maxTokens = *in.MaxCompletionTokens
	}
	out["max_tokens"] = maxTokens

	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if stops := bridgeParseStop(in.Stop); len(stops) > 0 {
		out["stop_sequences"] = stops
	}
	// reasoning_effort has no portable Anthropic equivalent: "enabled" thinking
	// needs an explicit budget_tokens, and picking one silently changes both cost
	// and latency. Mapping only the unambiguous end (none = off) avoids inventing a
	// budget the caller never asked for.
	if strings.EqualFold(strings.TrimSpace(in.ReasoningEffort), "none") {
		out["thinking"] = map[string]any{"type": "disabled"}
	}

	var systemBlocks []bridgeBlock
	acc := newBridgeMessageAccumulator()

	for i := range in.Messages {
		msg := &in.Messages[i]
		switch msg.Role {
		case "system", "developer":
			// Developer ranks with system in OpenAI's instruction hierarchy, so both
			// become Anthropic system blocks. Dropping developer would silently discard
			// operator instructions.
			systemBlocks = append(systemBlocks, bridgeTextBlocksFromOpenAIContent(msg.Content)...)

		case "user", "assistant":
			blocks := make([]bridgeBlock, 0, 4)
			// A previous thinking turn is replayed as text, not as a thinking block:
			// Anthropic requires a valid signature on thinking blocks and rejects
			// unsigned ones, and this content was produced by a different vendor.
			if msg.Role == "assistant" && strings.TrimSpace(msg.ReasoningContent) != "" {
				blocks = append(blocks, bridgeBlock{Type: "text", Text: msg.ReasoningContent})
			}
			blocks = append(blocks, bridgeBlocksFromOpenAIContent(msg.Content)...)
			if msg.Role == "assistant" {
				for j := range msg.ToolCalls {
					if block, ok := bridgeToolUseFromToolCall(&msg.ToolCalls[j]); ok {
						blocks = append(blocks, block)
					}
				}
			}
			acc.append(msg.Role, blocks)

		case "tool":
			// Anthropic carries a tool result as a user-role tool_result block.
			block := bridgeBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
			}
			if content, ok := bridgeToolResultContentFromOpenAI(msg.Content); ok {
				block.Content = content
			}
			acc.append("user", []bridgeBlock{block})
		}
	}

	messages := acc.messages()

	// Anthropic rejects an empty messages array. A system-only request is legal on
	// the OpenAI side, so synthesise the minimal turn that keeps it valid.
	if len(messages) == 0 && len(systemBlocks) > 0 {
		messages = []map[string]any{{
			"role":    "user",
			"content": []bridgeBlock{{Type: "text", Text: "."}},
		}}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("request has no messages to translate")
	}
	out["messages"] = messages

	if len(systemBlocks) > 0 {
		out["system"] = systemBlocks
	}

	if tools := bridgeToolsToAnthropic(in.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, ok := bridgeToolChoiceToAnthropic(in.ToolChoice); ok {
		out["tool_choice"] = choice
	}

	return json.Marshal(out)
}

// bridgeMessageAccumulator merges consecutive same-role messages into one.
//
// Anthropic requires messages to alternate user/assistant and rejects two
// user-role entries in a row; OpenAI has no such rule, and a tool-calling
// conversation reliably produces runs of tool results that all land on user.
type bridgeMessageAccumulator struct {
	out     []map[string]any
	role    string
	content []bridgeBlock
}

func newBridgeMessageAccumulator() *bridgeMessageAccumulator {
	return &bridgeMessageAccumulator{}
}

func (a *bridgeMessageAccumulator) append(role string, blocks []bridgeBlock) {
	if len(blocks) == 0 {
		return
	}
	if a.role != "" && a.role != role {
		a.flush()
	}
	a.role = role
	a.content = append(a.content, blocks...)
}

func (a *bridgeMessageAccumulator) flush() {
	if a.role == "" || len(a.content) == 0 {
		a.role = ""
		a.content = nil
		return
	}
	a.out = append(a.out, map[string]any{
		"role":    a.role,
		"content": a.content,
	})
	a.role = ""
	a.content = nil
}

func (a *bridgeMessageAccumulator) messages() []map[string]any {
	a.flush()
	return a.out
}

// bridgeTextBlocksFromOpenAIContent keeps only text, for the system slot where
// Anthropic accepts nothing else.
func bridgeTextBlocksFromOpenAIContent(raw json.RawMessage) []bridgeBlock {
	out := make([]bridgeBlock, 0, 2)
	for _, block := range bridgeBlocksFromOpenAIContent(raw) {
		if block.Type == "text" {
			out = append(out, block)
		}
	}
	return out
}

// bridgeBlocksFromOpenAIContent converts an OpenAI content field (string or part
// array) into Anthropic content blocks.
func bridgeBlocksFromOpenAIContent(raw json.RawMessage) []bridgeBlock {
	if len(raw) == 0 {
		return nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return nil
		}
		return []bridgeBlock{{Type: "text", Text: asString}}
	}

	var parts []bridgeOpenAIPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]bridgeBlock, 0, len(parts))
	for i := range parts {
		switch parts[i].Type {
		case "text":
			if parts[i].Text != "" {
				out = append(out, bridgeBlock{Type: "text", Text: parts[i].Text})
			}
		case "image_url":
			if parts[i].ImageURL == nil {
				continue
			}
			if source, ok := bridgeImageSourceFromURL(parts[i].ImageURL.URL); ok {
				out = append(out, bridgeBlock{Type: "image", Source: source})
			}
		}
	}
	return out
}

// bridgeImageSourceFromURL builds an Anthropic image source from an OpenAI image
// URL, handling both the inline data-URL form and a plain remote URL.
func bridgeImageSourceFromURL(url string) (json.RawMessage, bool) {
	if url == "" {
		return nil, false
	}
	if strings.HasPrefix(url, "data:") {
		comma := strings.Index(url, ",")
		if comma < 0 {
			return nil, false
		}
		meta := url[len("data:"):comma]
		mediaType := meta
		if semi := strings.Index(meta, ";"); semi >= 0 {
			mediaType = meta[:semi]
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		src, err := json.Marshal(map[string]string{
			"type":       "base64",
			"media_type": mediaType,
			"data":       url[comma+1:],
		})
		if err != nil {
			return nil, false
		}
		return src, true
	}
	src, err := json.Marshal(map[string]string{"type": "url", "url": url})
	if err != nil {
		return nil, false
	}
	return src, true
}

// bridgeToolUseFromToolCall converts an OpenAI tool_call into an Anthropic
// tool_use block. Arguments arrive as a JSON *string* on the OpenAI side and must
// become a real object on the Anthropic side.
func bridgeToolUseFromToolCall(tc *ToolCall) (bridgeBlock, bool) {
	if tc.Function.Name == "" {
		return bridgeBlock{}, false
	}
	id := tc.ID
	if id == "" {
		id = "toolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	}
	input := json.RawMessage("{}")
	if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(args), &probe); err == nil {
			input = json.RawMessage(args)
		}
	}
	return bridgeBlock{
		Type:  "tool_use",
		ID:    id,
		Name:  tc.Function.Name,
		Input: input,
	}, true
}

// bridgeToolResultContentFromOpenAI shapes a tool message's content for an
// Anthropic tool_result block, which accepts either a plain string or a block
// array.
func bridgeToolResultContentFromOpenAI(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		encoded, err := json.Marshal(asString)
		if err != nil {
			return nil, false
		}
		return encoded, true
	}
	blocks := bridgeBlocksFromOpenAIContent(raw)
	if len(blocks) == 0 {
		// Preserve the original rather than silently emptying the result: a tool
		// result the model cannot see is worse than an oddly-shaped one.
		return raw, true
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func bridgeToolsToAnthropic(tools []bridgeOpenAITool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for i := range tools {
		fn := tools[i].Function
		if fn.Name == "" {
			continue
		}
		tool := map[string]any{"name": fn.Name}
		if fn.Description != "" {
			tool["description"] = fn.Description
		}
		// input_schema is mandatory on the Anthropic side.
		if len(fn.Parameters) > 0 {
			tool["input_schema"] = fn.Parameters
		} else {
			tool["input_schema"] = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, tool)
	}
	return out
}

func bridgeToolChoiceToAnthropic(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto":
			return map[string]any{"type": "auto"}, true
		case "required", "any":
			return map[string]any{"type": "any"}, true
		case "none":
			// Anthropic has no "never use tools" choice. Omitting tool_choice leaves
			// the model free to call one, so the closest honest translation is to say
			// nothing and let the caller's tools stand unused.
			return nil, false
		}
		return nil, false
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	name := obj.Name
	if name == "" && obj.Function != nil {
		name = obj.Function.Name
	}
	if name == "" {
		return nil, false
	}
	return map[string]any{"type": "tool", "name": name}, true
}

func bridgeParseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		out := make([]string, 0, len(many))
		for _, s := range many {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// Anthropic request -> OpenAI request
// ---------------------------------------------------------------------------

// bridgeAnthropicRequestToOpenAI rewrites a /v1/messages body as a Chat
// Completions body targeting upstreamModel.
//
// This is the direction that unblocks Claude Code against an OpenAI-only
// provider. Structural differences handled here:
//   - top-level "system" becomes a leading system message
//   - tool_result blocks are lifted out into their own tool-role messages, which
//     must precede any remaining user content from the same Anthropic message
//   - tool_use blocks become assistant tool_calls with stringified arguments
//   - thinking blocks are dropped, because OpenAI has no inbound reasoning field
//
// includeUsage opts into stream_options.include_usage so a streamed response
// still reports tokens; without it there is nothing to bill against.
func bridgeAnthropicRequestToOpenAI(raw []byte, upstreamModel string, stream, includeUsage bool) ([]byte, error) {
	var in bridgeAnthropicRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}

	out := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	if stream && includeUsage {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	// Anthropic's explicit thinking budget has no OpenAI counterpart beyond the
	// coarse reasoning_effort knob. Providers that do not know the field ignore it,
	// so setting it is safe and preserves the caller's intent approximately.
	if effort, ok := bridgeThinkingToReasoningEffort(in.Thinking); ok {
		out["reasoning_effort"] = effort
	}

	messages := make([]map[string]any, 0, len(in.Messages)+1)
	if system := bridgeSystemTextFromAnthropic(in.System); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	for i := range in.Messages {
		messages = append(messages, bridgeAnthropicMessageToOpenAI(in.Messages[i])...)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("request has no messages to translate")
	}
	out["messages"] = messages

	if tools := bridgeToolsToOpenAI(in.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, ok := bridgeToolChoiceToOpenAI(in.ToolChoice); ok {
		out["tool_choice"] = choice
	}

	return json.Marshal(out)
}

// bridgeAnthropicMessageToOpenAI expands one Anthropic message into the one or
// more OpenAI messages it corresponds to.
//
// The expansion exists because Anthropic packs tool results into a user message's
// content array, while OpenAI requires each result to be its own tool-role
// message. Those must be emitted first so they stay adjacent to the assistant
// turn that requested them.
func bridgeAnthropicMessageToOpenAI(msg bridgeAnthropicMsg) []map[string]any {
	if len(msg.Content) == 0 {
		return nil
	}

	// Plain string content is the simple case.
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil
		}
		return []map[string]any{{"role": msg.Role, "content": asString}}
	}

	var blocks []bridgeBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}

	out := make([]map[string]any, 0, 2)
	var parts []map[string]any
	var toolCalls []map[string]any
	var textOnly strings.Builder
	hasNonText := false

	for i := range blocks {
		block := &blocks[i]
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			textOnly.WriteString(block.Text)
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})

		case "thinking", "redacted_thinking":
			// Dropped by design: Chat Completions has no inbound reasoning field, and
			// replaying thinking as visible text would corrupt the transcript the model
			// sees. The reasoning is lost, the answer it produced is not.
			continue

		case "image":
			if url, ok := bridgeImageURLFromAnthropicSource(block.Source); ok {
				hasNonText = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]string{"url": url},
				})
			}

		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]string{
					"name":      block.Name,
					"arguments": args,
				},
			})

		case "tool_result":
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": block.ToolUseID,
				"content":      bridgeToolResultTextFromAnthropic(block.Content),
			})
		}
	}

	// Assemble what remains of this message, if anything.
	msgOut := map[string]any{"role": msg.Role}
	switch {
	case hasNonText && len(parts) > 0:
		msgOut["content"] = parts
	case textOnly.Len() > 0:
		// Collapse a pure-text block array back to a plain string: some
		// OpenAI-compatible providers only accept the string form.
		msgOut["content"] = textOnly.String()
	case len(toolCalls) > 0:
		// An assistant turn that is only tool calls carries null content.
		msgOut["content"] = nil
	default:
		return out
	}
	if len(toolCalls) > 0 {
		msgOut["tool_calls"] = toolCalls
	}
	return append(out, msgOut)
}

func bridgeSystemTextFromAnthropic(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []bridgeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for i := range blocks {
		if blocks[i].Type != "text" || blocks[i].Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(blocks[i].Text)
	}
	return sb.String()
}

func bridgeImageURLFromAnthropicSource(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return "", false
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return "", false
		}
		mediaType := src.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		return "data:" + mediaType + ";base64," + src.Data, true
	case "url":
		if src.URL == "" {
			return "", false
		}
		return src.URL, true
	}
	return "", false
}

// bridgeToolResultTextFromAnthropic flattens a tool_result payload to the string
// OpenAI expects. Non-text blocks (an image returned by a tool) cannot be carried
// in a tool-role message, so they are noted rather than dropped without trace.
func bridgeToolResultTextFromAnthropic(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []bridgeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var sb strings.Builder
	for i := range blocks {
		switch blocks[i].Type {
		case "text":
			if blocks[i].Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(blocks[i].Text)
		case "image":
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("[image omitted: tool results cannot carry images over this protocol bridge]")
		}
	}
	return sb.String()
}

func bridgeToolsToOpenAI(tools []bridgeAnthropicTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for i := range tools {
		if tools[i].Name == "" {
			continue
		}
		fn := map[string]any{"name": tools[i].Name}
		if tools[i].Description != "" {
			fn["description"] = tools[i].Description
		}
		if len(tools[i].InputSchema) > 0 {
			fn["parameters"] = tools[i].InputSchema
		} else {
			fn["parameters"] = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func bridgeToolChoiceToOpenAI(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	switch obj.Type {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		if obj.Name == "" {
			return nil, false
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": obj.Name},
		}, true
	}
	return nil, false
}

// bridgeThinkingToReasoningEffort maps an Anthropic thinking budget onto the
// coarse OpenAI reasoning_effort scale. The thresholds are judgement calls, not
// spec: the two knobs are not equivalent, and this only preserves rough intent.
func bridgeThinkingToReasoningEffort(t *ClaudeThinkingConfig) (string, bool) {
	if t == nil {
		return "", false
	}
	switch t.Type {
	case "disabled":
		return "none", true
	case "enabled", "adaptive":
		switch {
		case t.BudgetTokens <= 0:
			return "medium", true
		case t.BudgetTokens <= 4096:
			return "low", true
		case t.BudgetTokens <= 16384:
			return "medium", true
		default:
			return "high", true
		}
	}
	return "", false
}

// bridgeRequestBody dispatches to the right request translator for a step.
// A passthrough step only needs the model field rewritten.
func bridgeRequestBody(step *upstreamStep, pc *passthroughCtx) ([]byte, error) {
	switch step.Bridge {
	case config.BridgeOpenAIToAnthropic:
		return bridgeOpenAIRequestToAnthropic(pc.Raw, step.UpstreamModel, pc.Stream)
	case config.BridgeAnthropicToOpenAI:
		return bridgeAnthropicRequestToOpenAI(pc.Raw, step.UpstreamModel, pc.Stream, true)
	case config.BridgeResponsesToOpenAI:
		return bridgeResponsesRequestToOpenAIChat(pc.Raw, step.UpstreamModel, pc.Stream)
	case config.BridgeResponsesToAnthropic:
		return bridgeResponsesRequestToAnthropic(pc.Raw, step.UpstreamModel, pc.Stream)
	default:
		return rewriteModelField(pc.Raw, step.UpstreamModel,
			pc.Stream && step.UpstreamEndpoint == config.ProviderEndpointChatCompletions)
	}
}
