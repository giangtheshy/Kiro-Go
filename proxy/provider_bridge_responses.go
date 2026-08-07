package proxy

// Bridging for clients that speak the OpenAI Responses API (/v1/responses)
// against providers that only implement /v1/chat/completions or /v1/messages.
//
// Why this exists: the Responses API is now the default surface for the OpenAI
// SDK and for Codex-style clients, while almost every cheap OpenAI-compatible
// upstream (DeepSeek, Moonshot, z.ai, most OpenRouter models) implements only
// Chat Completions. Without a bridge those providers can never serve Responses
// traffic no matter how the operator prices or prioritises them — the same
// problem AllowProtocolBridge already solves for Claude Code on /v1/messages.
//
// The implementation deliberately CHAINS through the Chat Completions layer
// rather than adding a direct translator per protocol pair:
//
//	responses -> chat        : new request translator, new response translator
//	responses -> anthropic   : new request translator + existing openai->anthropic
//	                           request translator; existing anthropic->openai
//	                           response translator + new response translator
//
// Two protocol pairs are therefore served by ONE new translator in each
// direction, and the Anthropic path reuses code that already has test coverage.
// The cost is one extra in-memory re-shaping per event, which is irrelevant next
// to the network round-trip it rides on.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kiro-go/config"
)

// ---------------------------------------------------------------------------
// Request: Responses -> Chat Completions
// ---------------------------------------------------------------------------

// bridgeResponsesRequestToOpenAIChat rewrites a /v1/responses body as a
// /v1/chat/completions body targeting upstreamModel.
//
// The heavy lifting — flattening the Responses typed-item input array into role
// messages — is already done by parseResponsesInput, which the native Kiro path
// uses for exactly this purpose. Reusing it keeps the two paths from drifting:
// a client whose input shape works against a Kiro account also works against a
// bridged provider.
//
// Deliberately dropped, because Chat Completions has no counterpart:
//   - previous_response_id / store: server-side conversation state. The caller's
//     history is already flattened into messages by the handler before it gets
//     here, so nothing is lost for a well-behaved client.
//   - include, truncation, text.format, metadata, service_tier.
//
// Reasoning items in the input are flattened to assistant text by
// parseResponsesInput rather than replayed as signed reasoning, which no
// OpenAI-compatible upstream would accept from a different vendor anyway.
func bridgeResponsesRequestToOpenAIChat(raw []byte, upstreamModel string, stream bool) ([]byte, error) {
	var in ResponsesRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}

	messages, err := parseResponsesInput(in.Input)
	if err != nil {
		return nil, fmt.Errorf("parse responses input: %w", err)
	}

	// instructions is the Responses equivalent of a system prompt and outranks
	// anything in the input array, so it goes first.
	out := make([]map[string]any, 0, len(messages)+1)
	if s := strings.TrimSpace(in.Instructions); s != "" {
		out = append(out, map[string]any{"role": "system", "content": s})
	}
	for i := range messages {
		out = append(out, bridgeOpenAIMessageToWire(&messages[i]))
	}
	// An upstream rejects an empty messages array outright; a lone system prompt
	// with no turn is a real shape (a client priming a conversation), so it gets an
	// empty user turn rather than a 400.
	if len(out) == 0 || (len(out) == 1 && out[0]["role"] == "system") {
		out = append(out, map[string]any{"role": "user", "content": ""})
	}

	body := map[string]any{
		"model":    upstreamModel,
		"messages": out,
		"stream":   stream,
	}
	if in.MaxOutputTokens != nil && *in.MaxOutputTokens > 0 {
		body["max_tokens"] = *in.MaxOutputTokens
	}
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	if effort := bridgeResponsesReasoningEffort(raw); effort != "" {
		body["reasoning_effort"] = effort
	}
	if tools := bridgeResponsesToolsToOpenAI(in.Tools); len(tools) > 0 {
		body["tools"] = tools
		if tc, ok := bridgeResponsesToolChoiceToOpenAI(in.ToolChoice); ok {
			body["tool_choice"] = tc
		}
	}
	if stream {
		// Without this the upstream reports no usage at all and the request would be
		// billed as free.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(body)
}

// bridgeOpenAIMessageToWire renders one parsed message in Chat Completions shape.
// It is written by hand rather than by marshalling OpenAIMessage because a tool
// message must carry tool_call_id and an assistant tool call must not emit a
// null content, both of which strict upstreams reject.
func bridgeOpenAIMessageToWire(msg *OpenAIMessage) map[string]any {
	out := map[string]any{"role": msg.Role}
	switch {
	case len(msg.ToolCalls) > 0:
		calls := make([]map[string]any, 0, len(msg.ToolCalls))
		for i := range msg.ToolCalls {
			tc := &msg.ToolCalls[i]
			args := tc.Function.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Function.Name,
					"arguments": args,
				},
			})
		}
		out["tool_calls"] = calls
		if text := extractOpenAIMessageText(msg.Content); text != "" {
			out["content"] = text
		} else {
			out["content"] = nil
		}
	default:
		out["content"] = msg.Content
	}
	if msg.ToolCallID != "" {
		out["tool_call_id"] = msg.ToolCallID
	}
	return out
}

// bridgeResponsesReasoningEffort reads reasoning.effort off the raw body.
// ResponsesRequest does not model the field, and adding it there would change a
// struct the native path also decodes, so it is read directly.
func bridgeResponsesReasoningEffort(raw []byte) string {
	var probe struct {
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Reasoning == nil {
		return ""
	}
	return strings.TrimSpace(probe.Reasoning.Effort)
}

// bridgeResponsesToolsToOpenAI renders Responses tools in Chat Completions shape.
// OpenAITool.UnmarshalJSON already accepts both the flat Responses form and the
// nested Chat form, so only the output shape has to be normalised here.
//
// Built-in tools (web_search, file_search, code_interpreter, computer_use,
// image_generation) are skipped: they are server-side capabilities of OpenAI's
// own stack with no equivalent an arbitrary provider could honour, and passing
// them through unchanged is how you get a 400 from a strict upstream.
func bridgeResponsesToolsToOpenAI(tools []OpenAITool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for i := range tools {
		t := &tools[i]
		if t.Type != "" && t.Type != "function" && t.Type != "custom" {
			continue
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		params := t.Function.Parameters
		if params == nil {
			// An absent schema is legal in the Responses API but not in Chat
			// Completions, where "parameters" must at least be an object.
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

// bridgeResponsesToolChoiceToOpenAI normalises tool_choice. The Responses API
// names the tool at the top level ({"type":"function","name":"x"}) while Chat
// Completions nests it ({"type":"function","function":{"name":"x"}}); forwarding
// the Responses form verbatim silently loses the constraint.
func bridgeResponsesToolChoiceToOpenAI(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto", "none", "required":
			return asString, true
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
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
}

// ---------------------------------------------------------------------------
// Response: OpenAI chunks -> Responses events (stream)
// ---------------------------------------------------------------------------

// bridgeResponsesToolCall is one function call being assembled from OpenAI
// tool_call deltas.
type bridgeResponsesToolCall struct {
	itemID      string
	callID      string
	name        string
	args        strings.Builder
	outputIndex int
	opened      bool
}

// bridgeOpenAIToResponsesStream turns an OpenAI chunk stream into a Responses
// event stream.
//
// The invariant this type protects: the Responses wire format is a sequence of
// explicitly framed items. Every item must be opened with output_item.added and
// closed with output_item.done before the next one opens, output_index must
// increase monotonically with no gaps, and the document must end with a terminal
// response.* event. An SDK building a Response object from the stream rejects a
// part that was never opened or never closed.
//
// OpenAI's format carries none of that framing — it is a flat run of
// interchangeable chunks — so the open/close boundaries have to be INFERRED from
// what changes between chunks. Concretely: reasoning closes when text starts,
// text closes when a tool call starts, and everything still open closes in
// finish(), which is why finish() must run even when the upstream dies mid-turn.
type bridgeOpenAIToResponsesStream struct {
	model     string
	respID    string
	createdAt int64

	seq             int
	nextOutputIndex int

	createdSent bool

	// message (assistant text) state
	msgOpen        bool
	msgItemID      string
	msgOutputIndex int
	msgText        strings.Builder
	msgCount       int

	// reasoning state
	reasoningOpen        bool
	reasoningItemID      string
	reasoningOutputIndex int
	reasoningText        strings.Builder

	// completed items, in output order
	items []json.RawMessage

	// tool calls keyed by the upstream chunk's tool_call index
	toolCalls map[int]*bridgeResponsesToolCall
	toolOrder []int

	usage        openAIStyleUsage
	finishReason string
	failed       bool
	done         bool
}

func newBridgeOpenAIToResponsesStream(model string, fallbackInput int) *bridgeOpenAIToResponsesStream {
	b := &bridgeOpenAIToResponsesStream{
		model:          model,
		respID:         generateOutputItemID("resp"),
		createdAt:      time.Now().Unix(),
		msgOutputIndex: -1,
		toolCalls:      make(map[int]*bridgeResponsesToolCall),
	}
	// A provider that reports no usage would otherwise bill as free; seed the
	// caller's own estimate so the floor is never zero.
	if fallbackInput > 0 {
		b.usage.prompt = fallbackInput
	}
	return b
}

func (b *bridgeOpenAIToResponsesStream) nextSeq() int {
	b.seq++
	return b.seq
}

func (b *bridgeOpenAIToResponsesStream) allocOutputIndex() int {
	idx := b.nextOutputIndex
	b.nextOutputIndex++
	return idx
}

// event builds one Responses SSE frame. Every event carries type and
// sequence_number; SDKs treat both as required.
func (b *bridgeOpenAIToResponsesStream) event(name string, fields map[string]any) []byte {
	fields["type"] = name
	fields["sequence_number"] = b.nextSeq()
	return sseFrame(name, fields)
}

// responseEnvelope is the response object echoed on lifecycle events. output is
// always present, even empty: a strict SDK rejects a Response without it.
func (b *bridgeOpenAIToResponsesStream) responseEnvelope(status string) map[string]any {
	out := map[string]any{
		"id":         b.respID,
		"object":     "response",
		"created_at": b.createdAt,
		"status":     status,
		"model":      b.model,
		"output":     []json.RawMessage{},
		"error":      nil,
	}
	return out
}

func (b *bridgeOpenAIToResponsesStream) ensureCreated() [][]byte {
	if b.createdSent {
		return nil
	}
	b.createdSent = true
	created := b.responseEnvelope("in_progress")
	created["background"] = false
	return [][]byte{
		b.event("response.created", map[string]any{"response": created}),
		b.event("response.in_progress", map[string]any{"response": b.responseEnvelope("in_progress")}),
	}
}

// --- reasoning ---

func (b *bridgeOpenAIToResponsesStream) openReasoning() [][]byte {
	if b.reasoningOpen {
		return nil
	}
	b.reasoningOpen = true
	b.reasoningOutputIndex = b.allocOutputIndex()
	b.reasoningItemID = generateOutputItemID("rs")
	b.reasoningText.Reset()

	return [][]byte{
		b.event("response.output_item.added", map[string]any{
			"output_index": b.reasoningOutputIndex,
			"item": map[string]any{
				"id":                b.reasoningItemID,
				"type":              "reasoning",
				"status":            "in_progress",
				"encrypted_content": "",
				"summary":           []any{},
			},
		}),
		b.event("response.reasoning_summary_part.added", map[string]any{
			"item_id":       b.reasoningItemID,
			"output_index":  b.reasoningOutputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		}),
	}
}

func (b *bridgeOpenAIToResponsesStream) closeReasoning() [][]byte {
	if !b.reasoningOpen {
		return nil
	}
	text := b.reasoningText.String()
	summary := []any{map[string]any{"type": "summary_text", "text": text}}
	item := map[string]any{
		"id":                b.reasoningItemID,
		"type":              "reasoning",
		"status":            "completed",
		"encrypted_content": "",
		"summary":           summary,
	}
	out := [][]byte{
		b.event("response.reasoning_summary_text.done", map[string]any{
			"item_id":       b.reasoningItemID,
			"output_index":  b.reasoningOutputIndex,
			"summary_index": 0,
			"text":          text,
		}),
		b.event("response.reasoning_summary_part.done", map[string]any{
			"item_id":       b.reasoningItemID,
			"output_index":  b.reasoningOutputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": text},
		}),
		b.event("response.output_item.done", map[string]any{
			"output_index": b.reasoningOutputIndex,
			"item":         item,
		}),
	}
	b.recordItem(b.reasoningOutputIndex, item)

	b.reasoningOpen = false
	b.reasoningItemID = ""
	b.reasoningOutputIndex = -1
	b.reasoningText.Reset()
	return out
}

// --- assistant message ---

func (b *bridgeOpenAIToResponsesStream) openMessage() [][]byte {
	if b.msgOpen {
		return nil
	}
	// Reasoning always precedes the visible answer, and an item cannot stay open
	// across another item's lifetime.
	out := b.closeReasoning()

	b.msgOpen = true
	b.msgOutputIndex = b.allocOutputIndex()
	b.msgItemID = generateOutputItemID("msg")
	b.msgCount++
	b.msgText.Reset()

	out = append(out,
		b.event("response.output_item.added", map[string]any{
			"output_index": b.msgOutputIndex,
			"item": map[string]any{
				"id":      b.msgItemID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		}),
		b.event("response.content_part.added", map[string]any{
			"item_id":       b.msgItemID,
			"output_index":  b.msgOutputIndex,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
				"logprobs":    []any{},
			},
		}),
	)
	return out
}

func (b *bridgeOpenAIToResponsesStream) closeMessage() [][]byte {
	if !b.msgOpen {
		return nil
	}
	text := b.msgText.String()
	content := []any{map[string]any{
		"type":        "output_text",
		"text":        text,
		"annotations": []any{},
		"logprobs":    []any{},
	}}
	item := map[string]any{
		"id":      b.msgItemID,
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": content,
	}
	out := [][]byte{
		b.event("response.output_text.done", map[string]any{
			"item_id":       b.msgItemID,
			"output_index":  b.msgOutputIndex,
			"content_index": 0,
			"text":          text,
			"logprobs":      []any{},
		}),
		b.event("response.content_part.done", map[string]any{
			"item_id":       b.msgItemID,
			"output_index":  b.msgOutputIndex,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
				"logprobs":    []any{},
			},
		}),
		b.event("response.output_item.done", map[string]any{
			"output_index": b.msgOutputIndex,
			"item":         item,
		}),
	}
	b.recordItem(b.msgOutputIndex, item)

	b.msgOpen = false
	b.msgItemID = ""
	b.msgOutputIndex = -1
	b.msgText.Reset()
	return out
}

// --- tool calls ---

// openToolCall registers a tool call and announces it. id and name are emitted up
// front, while the arguments are still streaming, so a client can show "calling
// tool X" instead of waiting for the whole JSON.
func (b *bridgeOpenAIToResponsesStream) openToolCall(idx int, callID, name string) ([][]byte, *bridgeResponsesToolCall) {
	if tc, ok := b.toolCalls[idx]; ok {
		// A later delta may be the first to carry the id or the name.
		if tc.callID == "" && callID != "" {
			tc.callID = callID
		}
		if tc.name == "" && name != "" {
			tc.name = name
		}
		return nil, tc
	}

	// Both text and reasoning must be closed before a function_call item opens.
	out := b.closeMessage()
	out = append(out, b.closeReasoning()...)

	if callID == "" {
		callID = generateOutputItemID("call")
	}
	tc := &bridgeResponsesToolCall{
		itemID:      "fc_" + callID,
		callID:      callID,
		name:        name,
		outputIndex: b.allocOutputIndex(),
		opened:      true,
	}
	b.toolCalls[idx] = tc
	b.toolOrder = append(b.toolOrder, idx)

	out = append(out, b.event("response.output_item.added", map[string]any{
		"output_index": tc.outputIndex,
		"item": map[string]any{
			"id":        tc.itemID,
			"type":      "function_call",
			"status":    "in_progress",
			"call_id":   tc.callID,
			"name":      tc.name,
			"arguments": "",
		},
	}))
	return out, tc
}

func (b *bridgeOpenAIToResponsesStream) closeToolCalls() [][]byte {
	var out [][]byte
	for _, idx := range b.toolOrder {
		tc := b.toolCalls[idx]
		if tc == nil || !tc.opened {
			continue
		}
		tc.opened = false
		args := tc.args.String()
		if strings.TrimSpace(args) == "" {
			// arguments is a string on the wire and must be valid JSON for a client
			// that parses it; an empty string is not.
			args = "{}"
		}
		item := map[string]any{
			"id":        tc.itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   tc.callID,
			"name":      tc.name,
			"arguments": args,
		}
		out = append(out,
			b.event("response.function_call_arguments.done", map[string]any{
				"item_id":      tc.itemID,
				"output_index": tc.outputIndex,
				"arguments":    args,
			}),
			b.event("response.output_item.done", map[string]any{
				"output_index": tc.outputIndex,
				"item":         item,
			}),
		)
		b.recordItem(tc.outputIndex, item)
	}
	return out
}

// recordItem stores a finished item at its output index so response.completed can
// replay them in order. Indexes are allocated contiguously, so the slice is grown
// to fit rather than indexed sparsely — a nil hole would marshal as null and be
// rejected.
func (b *bridgeOpenAIToResponsesStream) recordItem(index int, item map[string]any) {
	raw, err := json.Marshal(item)
	if err != nil {
		return
	}
	for len(b.items) <= index {
		b.items = append(b.items, nil)
	}
	b.items[index] = raw
}

func (b *bridgeOpenAIToResponsesStream) translate(line []byte) [][]byte {
	if b.done {
		return nil
	}
	payload := sseDataPayload(line)
	if payload == nil {
		return nil
	}
	var chunk bridgeOpenAIChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil
	}

	// A mid-stream error: the client already has a 200 and some bytes, so the only
	// honest ending is a terminal response.failed inside the stream.
	if chunk.Error != nil {
		msg := "upstream error"
		if chunk.Error.Message != "" {
			msg = chunk.Error.Message
		}
		out := b.ensureCreated()
		out = append(out, b.closeMessage()...)
		out = append(out, b.closeReasoning()...)
		out = append(out, b.closeToolCalls()...)
		b.failed = true
		b.done = true
		out = append(out, b.event("response.failed", map[string]any{
			"response": map[string]any{
				"id":     b.respID,
				"object": "response",
				"status": "failed",
				"error":  map[string]any{"type": "server_error", "message": msg},
			},
		}))
		return append(out, []byte("data: [DONE]\n\n"))
	}

	chunk.mergeUsageInto(&b.usage)

	out := b.ensureCreated()
	for i := range chunk.Choices {
		choice := &chunk.Choices[i]
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			b.finishReason = *choice.FinishReason
		}
		if choice.Delta == nil {
			continue
		}

		// Thinking arrives under either spelling depending on the upstream.
		if r := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning); r != "" {
			out = append(out, b.openReasoning()...)
			b.reasoningText.WriteString(r)
			out = append(out, b.event("response.reasoning_summary_text.delta", map[string]any{
				"item_id":       b.reasoningItemID,
				"output_index":  b.reasoningOutputIndex,
				"summary_index": 0,
				"delta":         r,
			}))
		}

		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			out = append(out, b.openMessage()...)
			b.msgText.WriteString(*choice.Delta.Content)
			out = append(out, b.event("response.output_text.delta", map[string]any{
				"item_id":       b.msgItemID,
				"output_index":  b.msgOutputIndex,
				"content_index": 0,
				"delta":         *choice.Delta.Content,
				"logprobs":      []any{},
			}))
		}

		for j := range choice.Delta.ToolCalls {
			call := &choice.Delta.ToolCalls[j]
			// index is how an OpenAI stream correlates fragments of the same call. It
			// is optional in practice, so a missing one falls back to declaration
			// order — which is correct for the single-call case and the only guess
			// available for the rest.
			slot := len(b.toolCalls)
			if call.Index != nil {
				slot = *call.Index
			}
			name := ""
			args := ""
			if call.Function != nil {
				name = call.Function.Name
				args = call.Function.Arguments
			}
			frames, tc := b.openToolCall(slot, call.ID, name)
			out = append(out, frames...)
			if args != "" && tc != nil {
				tc.args.WriteString(args)
				out = append(out, b.event("response.function_call_arguments.delta", map[string]any{
					"item_id":      tc.itemID,
					"output_index": tc.outputIndex,
					"delta":        args,
				}))
			}
		}
	}
	return out
}

func (b *bridgeOpenAIToResponsesStream) finish() [][]byte {
	if b.done {
		return nil
	}
	b.done = true

	out := b.ensureCreated()
	out = append(out, b.closeMessage()...)
	out = append(out, b.closeReasoning()...)
	out = append(out, b.closeToolCalls()...)

	// A stream that never opened a single item still has to produce a well-formed
	// document, so the output array is emitted empty rather than omitted.
	items := make([]json.RawMessage, 0, len(b.items))
	for _, item := range b.items {
		if item != nil {
			items = append(items, item)
		}
	}

	resp := b.responseEnvelope("completed")
	resp["output"] = items
	if b.usage.seen || b.usage.prompt > 0 {
		input := b.usage.prompt - b.usage.cached
		if input < 0 {
			input = 0
		}
		resp["usage"] = map[string]any{
			"input_tokens":          b.usage.prompt,
			"input_tokens_details":  map[string]any{"cached_tokens": b.usage.cached},
			"output_tokens":         b.usage.completion,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          b.usage.prompt + b.usage.completion,
		}
	}
	// A truncated turn is reported as incomplete rather than completed so an
	// agentic client does not treat a cut-off answer as a finished one.
	event := "response.completed"
	if b.finishReason == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		event = "response.incomplete"
	}

	out = append(out, b.event(event, map[string]any{"response": resp}))
	return append(out, []byte("data: [DONE]\n\n"))
}

// ---------------------------------------------------------------------------
// Response: OpenAI chat completion JSON -> Responses JSON
// ---------------------------------------------------------------------------

// bridgeOpenAIChatJSONToResponses converts a complete Chat Completions body into
// a Responses object.
func bridgeOpenAIChatJSONToResponses(payload []byte, model string, fallbackInput int) ([]byte, error) {
	var src struct {
		Choices []struct {
			Message *struct {
				Content          *string    `json:"content"`
				Reasoning        *string    `json:"reasoning"`
				ReasoningContent *string    `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsageFields `json:"usage"`
	}
	if err := json.Unmarshal(payload, &src); err != nil {
		return nil, fmt.Errorf("provider returned a body that is not a chat completion: %w", err)
	}

	var usage openAIStyleUsage
	usage.merge(src.Usage)
	if !usage.seen && fallbackInput > 0 {
		usage.prompt = fallbackInput
	}

	respID := generateOutputItemID("resp")
	output := make([]any, 0, 2)
	finish := ""

	if len(src.Choices) > 0 {
		choice := &src.Choices[0]
		finish = choice.FinishReason
		if msg := choice.Message; msg != nil {
			// Reasoning comes first, mirroring the order the streaming form produces.
			if r := firstNonEmpty(msg.ReasoningContent, msg.Reasoning); r != "" {
				output = append(output, map[string]any{
					"id":                generateOutputItemID("rs"),
					"type":              "reasoning",
					"status":            "completed",
					"encrypted_content": "",
					"summary":           []any{map[string]any{"type": "summary_text", "text": r}},
				})
			}
			if msg.Content != nil && *msg.Content != "" {
				output = append(output, map[string]any{
					"id":     generateOutputItemID("msg"),
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []any{map[string]any{
						"type":        "output_text",
						"text":        *msg.Content,
						"annotations": []any{},
						"logprobs":    []any{},
					}},
				})
			}
			for i := range msg.ToolCalls {
				tc := &msg.ToolCalls[i]
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				callID := tc.ID
				if callID == "" {
					callID = generateOutputItemID("call")
				}
				output = append(output, map[string]any{
					"id":        "fc_" + callID,
					"type":      "function_call",
					"status":    "completed",
					"call_id":   callID,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}
		}
	}

	status := "completed"
	out := map[string]any{
		"id":                 respID,
		"object":             "response",
		"created_at":         time.Now().Unix(),
		"model":              model,
		"output":             output,
		"error":              nil,
		"incomplete_details": nil,
		"usage": map[string]any{
			"input_tokens":          usage.prompt,
			"input_tokens_details":  map[string]any{"cached_tokens": usage.cached},
			"output_tokens":         usage.completion,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          usage.prompt + usage.completion,
		},
	}
	if finish == "length" {
		status = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	out["status"] = status
	return json.Marshal(out)
}

// ---------------------------------------------------------------------------
// Chaining
// ---------------------------------------------------------------------------

// bridgeChainStream composes two stream translators so an Anthropic upstream can
// serve a Responses client: inner re-shapes the upstream's events into OpenAI
// chunks, outer turns those chunks into Responses events.
//
// The frames inner produces are complete `data: {...}` SSE frames, which is
// exactly what outer.translate expects to read, so no re-framing is needed
// between the two stages.
type bridgeChainStream struct {
	inner bridgeStreamTranslator
	outer bridgeStreamTranslator
}

func (c *bridgeChainStream) translate(line []byte) [][]byte {
	var out [][]byte
	for _, frame := range c.inner.translate(line) {
		out = append(out, c.outer.translate(frame)...)
	}
	return out
}

func (c *bridgeChainStream) finish() [][]byte {
	var out [][]byte
	// Drain the inner translator first: its finish() is what emits the final chunk
	// carrying finish_reason and usage, which the outer stage still needs.
	for _, frame := range c.inner.finish() {
		out = append(out, c.outer.translate(frame)...)
	}
	return append(out, c.outer.finish()...)
}

// bridgeAnthropicJSONToResponses chains the two non-streaming translators for the
// same reason bridgeChainStream chains the streaming ones.
func bridgeAnthropicJSONToResponses(payload []byte, model string, fallbackInput int) ([]byte, error) {
	chat, err := bridgeAnthropicJSONToOpenAI(payload, model)
	if err != nil {
		return nil, err
	}
	return bridgeOpenAIChatJSONToResponses(chat, model, fallbackInput)
}

// bridgeResponsesRequestToAnthropic chains the two request translators.
func bridgeResponsesRequestToAnthropic(raw []byte, upstreamModel string, stream bool) ([]byte, error) {
	chat, err := bridgeResponsesRequestToOpenAIChat(raw, upstreamModel, stream)
	if err != nil {
		return nil, err
	}
	return bridgeOpenAIRequestToAnthropic(chat, upstreamModel, stream)
}

// bridgeResponsesStreamTranslator builds the translator for a Responses client.
func bridgeResponsesStreamTranslator(bridge, clientModel string, fallbackInput int) bridgeStreamTranslator {
	switch bridge {
	case config.BridgeResponsesToOpenAI:
		return newBridgeOpenAIToResponsesStream(clientModel, fallbackInput)
	case config.BridgeResponsesToAnthropic:
		return &bridgeChainStream{
			inner: newBridgeAnthropicToOpenAIStream(clientModel),
			outer: newBridgeOpenAIToResponsesStream(clientModel, fallbackInput),
		}
	}
	return nil
}
