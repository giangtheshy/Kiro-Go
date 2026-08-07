package proxy

// Response translation for bridged providers.
//
// Two directions, each with a streaming and a non-streaming form:
//
//	BridgeOpenAIToAnthropic  provider replies Anthropic -> client wants OpenAI
//	BridgeAnthropicToOpenAI  provider replies OpenAI    -> client wants Anthropic
//
// The streaming translators are state machines fed one upstream SSE line at a
// time, because that is the only way to relay a stream without buffering the
// whole answer: a client that asked for a stream must see tokens as they arrive,
// not a single burst at the end.
//
// A note on why the Anthropic side is the fiddly one. The OpenAI wire format is
// a flat sequence of interchangeable chunks, so producing it needs almost no
// state. The Anthropic format is a nested, explicitly framed document:
// message_start, then content blocks that must each be opened and closed with a
// monotonically increasing index, then message_delta, then message_stop. A
// client parsing it will reject a block that was never opened or never closed,
// so emitting it correctly means tracking exactly which blocks are open and
// closing them even when the upstream stream dies halfway through — which is
// what finish() is for.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kiro-go/config"

	"github.com/google/uuid"
)

// bridgeStreamTranslator converts upstream SSE lines into client SSE frames.
//
// translate is called once per upstream line, in order, and returns zero or more
// complete SSE frames to write. finish is called exactly once after the upstream
// stream ends (cleanly or not) and returns whatever is still needed to leave the
// client with a well-formed document.
type bridgeStreamTranslator interface {
	translate(line []byte) [][]byte
	finish() [][]byte
}

// sseFrame builds one SSE frame. An empty event name omits the event line, which
// is what OpenAI-style streams look like; Anthropic always names its events.
func sseFrame(event string, payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	if event == "" {
		return []byte("data: " + string(data) + "\n\n")
	}
	return []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
}

// sseDataPayload extracts the JSON object from a `data:` line, or nil when the
// line is not one (comments, `event:` lines, blank separators, `[DONE]`).
func sseDataPayload(line []byte) []byte {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return nil
	}
	payload := strings.TrimSpace(trimmed[len("data:"):])
	if payload == "" || !strings.HasPrefix(payload, "{") {
		return nil
	}
	return []byte(payload)
}

// --- shared shapes read off the wire --------------------------------------

// bridgeAnthropicEvent is the subset of an Anthropic SSE event this file reads.
type bridgeAnthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		ID    string                `json:"id"`
		Model string                `json:"model"`
		Usage *anthropicUsageFields `json:"usage"`
	} `json:"message"`

	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Usage *anthropicUsageFields `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// bridgeOpenAIChunk is the subset of an OpenAI streaming chunk this file reads.
type bridgeOpenAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta *struct {
			Content   *string `json:"content"`
			Reasoning *string `json:"reasoning"`
			// DeepSeek and several other OpenAI-compatible upstreams put chain of
			// thought here; it is not part of the OpenAI spec but is common enough
			// that dropping it would lose the whole thinking stream.
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsageFields `json:"usage"`

	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- stop reason mapping ---------------------------------------------------

// bridgeAnthropicStopToOpenAI maps an Anthropic stop_reason to an OpenAI
// finish_reason. Unknown values fall back to "stop": a finish_reason is a closed
// enum for OpenAI clients and inventing a value breaks strict parsers.
func bridgeAnthropicStopToOpenAI(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		// end_turn, stop_sequence, pause_turn, and anything new.
		return "stop"
	}
}

// bridgeOpenAIFinishToAnthropic maps an OpenAI finish_reason to an Anthropic
// stop_reason.
func bridgeOpenAIFinishToAnthropic(reason string) string {
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// ===========================================================================
// Anthropic upstream -> OpenAI client (stream)
// ===========================================================================

// bridgeAnthropicToOpenAIStream turns an Anthropic event stream into OpenAI
// chat.completion.chunk frames.
type bridgeAnthropicToOpenAIStream struct {
	model   string
	id      string
	created int64

	// toolCalls maps an Anthropic block index to the tool call being built there.
	// Anthropic streams tool arguments as partial JSON, exactly like OpenAI, so the
	// fragments are forwarded as they arrive instead of being buffered.
	toolCalls map[int]*bridgeStreamToolCall

	usage        claudeStyleUsage
	finishReason string
	done         bool
}

type bridgeStreamToolCall struct {
	// slot is the index reported to the OpenAI client. Anthropic block indices count
	// text blocks too, so they cannot be reused directly: a tool call in Anthropic
	// block 1 must still be tool_calls[0] for the client.
	slot     int
	started  bool
	id       string
	name     string
	finished bool
}

// claudeStyleUsage accumulates Anthropic usage fields across events, since
// message_start carries the input side and message_delta the output side.
type claudeStyleUsage struct {
	input      int
	output     int
	cacheWrite int
	cacheRead  int
	seen       bool
}

func (u *claudeStyleUsage) merge(f *anthropicUsageFields) {
	if f == nil {
		return
	}
	if f.InputTokens > 0 {
		u.input = f.InputTokens
		u.seen = true
	}
	if f.OutputTokens > 0 {
		u.output = f.OutputTokens
		u.seen = true
	}
	if f.CacheCreationInputTokens > 0 {
		u.cacheWrite = f.CacheCreationInputTokens
		u.seen = true
	}
	if f.CacheReadInputTokens > 0 {
		u.cacheRead = f.CacheReadInputTokens
		u.seen = true
	}
}

// openAIUsage renders the accumulated counts in OpenAI shape. Anthropic reports
// input_tokens with cache traffic excluded while OpenAI's prompt_tokens is
// inclusive, so the cache counts are added back in — otherwise a client adding
// up the fields would see a total that does not match its own accounting.
func (u claudeStyleUsage) openAIUsage() map[string]any {
	prompt := u.input + u.cacheRead + u.cacheWrite
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": u.output,
		"total_tokens":      prompt + u.output,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.cacheRead,
		},
	}
}

func newBridgeAnthropicToOpenAIStream(model string) *bridgeAnthropicToOpenAIStream {
	return &bridgeAnthropicToOpenAIStream{
		model:     model,
		id:        "chatcmpl-" + uuid.New().String(),
		created:   time.Now().Unix(),
		toolCalls: make(map[int]*bridgeStreamToolCall),
	}
}

// chunk builds a chat.completion.chunk carrying one delta.
func (b *bridgeAnthropicToOpenAIStream) chunk(delta map[string]any, finishReason any) []byte {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": finishReason,
	}
	return sseFrame("", map[string]any{
		"id":      b.id,
		"object":  "chat.completion.chunk",
		"created": b.created,
		"model":   b.model,
		"choices": []map[string]any{choice},
	})
}

func (b *bridgeAnthropicToOpenAIStream) translate(line []byte) [][]byte {
	payload := sseDataPayload(line)
	if payload == nil {
		return nil
	}
	var ev bridgeAnthropicEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			b.usage.merge(ev.Message.Usage)
		}
		// OpenAI clients expect the role exactly once, in the first chunk.
		return [][]byte{b.chunk(map[string]any{"role": "assistant"}, nil)}

	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil
		}
		switch ev.ContentBlock.Type {
		case "tool_use":
			tc := &bridgeStreamToolCall{
				slot: len(b.toolCalls),
				id:   ev.ContentBlock.ID,
				name: ev.ContentBlock.Name,
			}
			b.toolCalls[ev.Index] = tc
			// Announce the call now, with empty arguments. Emitting id and name up
			// front (rather than waiting for the block to close) is what lets a client
			// show "calling tool X" while the arguments are still streaming.
			tc.started = true
			return [][]byte{b.chunk(map[string]any{
				"tool_calls": []map[string]any{{
					"index": tc.slot,
					"id":    tc.id,
					"type":  "function",
					"function": map[string]any{
						"name":      tc.name,
						"arguments": "",
					},
				}},
			}, nil)}
		case "text":
			// A non-empty text block start carries its first characters inline.
			if ev.ContentBlock.Text != "" {
				return [][]byte{b.chunk(map[string]any{"content": ev.ContentBlock.Text}, nil)}
			}
		}
		return nil

	case "content_block_delta":
		if ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return nil
			}
			return [][]byte{b.chunk(map[string]any{"content": ev.Delta.Text}, nil)}
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				return nil
			}
			// reasoning_content is the de-facto field for this; it is what DeepSeek
			// emits and what OpenAI-compatible clients that show thinking look for.
			return [][]byte{b.chunk(map[string]any{"reasoning_content": ev.Delta.Thinking}, nil)}
		case "signature_delta":
			// Thinking-block signatures are an Anthropic-only integrity token with no
			// OpenAI counterpart. Dropped deliberately.
			return nil
		case "input_json_delta":
			tc := b.toolCalls[ev.Index]
			if tc == nil || ev.Delta.PartialJSON == "" {
				return nil
			}
			return [][]byte{b.chunk(map[string]any{
				"tool_calls": []map[string]any{{
					"index": tc.slot,
					"function": map[string]any{
						"arguments": ev.Delta.PartialJSON,
					},
				}},
			}, nil)}
		}
		return nil

	case "content_block_stop":
		if tc := b.toolCalls[ev.Index]; tc != nil {
			tc.finished = true
		}
		return nil

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			b.finishReason = bridgeAnthropicStopToOpenAI(ev.Delta.StopReason)
		}
		b.usage.merge(ev.Usage)
		return nil

	case "message_stop":
		return b.finish()

	case "error":
		// Mid-stream upstream error. The client has already received a 200 and some
		// bytes, so the only honest thing left is to forward the error inside the
		// stream and terminate it.
		msg, typ := "upstream error", "api_error"
		if ev.Error != nil {
			if ev.Error.Message != "" {
				msg = ev.Error.Message
			}
			if ev.Error.Type != "" {
				typ = ev.Error.Type
			}
		}
		b.done = true
		return [][]byte{
			sseFrame("", map[string]any{"error": map[string]any{"message": msg, "type": typ}}),
			[]byte("data: [DONE]\n\n"),
		}

	case "ping":
		return nil
	}
	return nil
}

func (b *bridgeAnthropicToOpenAIStream) finish() [][]byte {
	if b.done {
		return nil
	}
	b.done = true

	if b.finishReason == "" {
		// The upstream never sent a stop_reason — a truncated stream. Report the
		// truncation rather than a clean "stop", so an agentic client knows the turn
		// did not really finish.
		b.finishReason = "stop"
		if len(b.toolCalls) > 0 {
			b.finishReason = "tool_calls"
		}
	}

	final := map[string]any{
		"id":      b.id,
		"object":  "chat.completion.chunk",
		"created": b.created,
		"model":   b.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": b.finishReason,
		}},
	}
	if b.usage.seen {
		final["usage"] = b.usage.openAIUsage()
	}
	return [][]byte{sseFrame("", final), []byte("data: [DONE]\n\n")}
}

// ===========================================================================
// OpenAI upstream -> Anthropic client (stream)
// ===========================================================================

// bridgeOpenAIToAnthropicStream turns an OpenAI chunk stream into a well-formed
// Anthropic event stream.
//
// The invariant this type exists to protect: every content block that is opened
// must be closed, exactly once, with the index it was opened under, and
// message_start must precede everything while message_stop terminates it. The
// upstream can stop at any point (including mid-tool-call), so the closing
// events are emitted from finish() rather than in response to any upstream event.
type bridgeOpenAIToAnthropicStream struct {
	model string
	id    string

	// fallbackInput seeds message_start.usage.input_tokens. OpenAI reports usage
	// only in the final chunk, but Anthropic clients read the input count from
	// message_start, so without an estimate here they would see 0 for the whole
	// stream and only get a real number at the end.
	fallbackInput int

	startEmitted bool
	nextIndex    int

	// One block per content kind, opened lazily on first content of that kind.
	thinkingIndex int
	thinkingOpen  bool
	textIndex     int
	textOpen      bool

	// toolBlocks maps the OpenAI tool_call index to the Anthropic block index.
	toolBlocks map[int]int
	toolOrder  []int

	usage      openAIStyleUsage
	stopReason string
	done       bool
}

// openAIStyleUsage accumulates OpenAI usage fields.
type openAIStyleUsage struct {
	prompt     int
	completion int
	cached     int
	seen       bool
}

func (u *openAIStyleUsage) merge(f *openAIUsageFields) {
	if f == nil {
		return
	}
	prompt, completion, cached := f.PromptTokens, f.CompletionTokens, 0
	if f.PromptTokensDetails != nil {
		cached = f.PromptTokensDetails.CachedTokens
	}
	if prompt == 0 && f.InputTokens > 0 {
		prompt = f.InputTokens
	}
	if completion == 0 && f.OutputTokens > 0 {
		completion = f.OutputTokens
	}
	if cached == 0 && f.InputTokensDetail != nil {
		cached = f.InputTokensDetail.CachedTokens
	}
	if prompt > 0 {
		u.prompt = prompt
		u.seen = true
	}
	if completion > 0 {
		u.completion = completion
		u.seen = true
	}
	if cached > 0 {
		u.cached = cached
		u.seen = true
	}
}

// anthropicUsage renders the counts in Anthropic shape. OpenAI's prompt_tokens
// includes cached tokens whereas Anthropic's input_tokens excludes them, so the
// cached part is split out instead of being counted twice.
func (u openAIStyleUsage) anthropicUsage() map[string]any {
	input := u.prompt - u.cached
	if input < 0 {
		input = 0
	}
	out := map[string]any{
		"input_tokens":  input,
		"output_tokens": u.completion,
	}
	if u.cached > 0 {
		out["cache_read_input_tokens"] = u.cached
	}
	return out
}

func newBridgeOpenAIToAnthropicStream(model string, fallbackInput int) *bridgeOpenAIToAnthropicStream {
	return &bridgeOpenAIToAnthropicStream{
		model:         model,
		id:            "msg_" + uuid.New().String(),
		fallbackInput: fallbackInput,
		toolBlocks:    make(map[int]int),
	}
}

// ensureStart emits message_start once, before any other event.
func (b *bridgeOpenAIToAnthropicStream) ensureStart() [][]byte {
	if b.startEmitted {
		return nil
	}
	b.startEmitted = true
	input := b.fallbackInput
	if input < 0 {
		input = 0
	}
	return [][]byte{sseFrame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            b.id,
			"type":          "message",
			"role":          "assistant",
			"model":         b.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  input,
				"output_tokens": 0,
			},
		},
	})}
}

// openBlock emits content_block_start for a new block and returns its index.
func (b *bridgeOpenAIToAnthropicStream) openBlock(block map[string]any) ([][]byte, int) {
	idx := b.nextIndex
	b.nextIndex++
	return [][]byte{sseFrame("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	})}, idx
}

func (b *bridgeOpenAIToAnthropicStream) closeBlock(idx int) []byte {
	return sseFrame("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (b *bridgeOpenAIToAnthropicStream) translate(line []byte) [][]byte {
	payload := sseDataPayload(line)
	if payload == nil {
		return nil
	}
	var chunk bridgeOpenAIChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil
	}

	var out [][]byte

	// A chunk carrying only usage (the include_usage tail chunk) has no choices.
	chunk.mergeUsageInto(&b.usage)

	if chunk.Error != nil {
		msg, typ := "upstream error", "api_error"
		if chunk.Error.Message != "" {
			msg = chunk.Error.Message
		}
		if chunk.Error.Type != "" {
			typ = chunk.Error.Type
		}
		out = append(out, b.ensureStart()...)
		out = append(out, sseFrame("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": typ, "message": msg},
		}))
		out = append(out, b.closeOpenBlocks()...)
		out = append(out, sseFrame("message_stop", map[string]any{"type": "message_stop"}))
		b.done = true
		return out
	}

	if len(chunk.Choices) == 0 {
		return out
	}
	choice := chunk.Choices[0]

	if choice.Delta != nil {
		out = append(out, b.ensureStart()...)

		// Reasoning first: Anthropic requires thinking blocks to precede text.
		if reasoning := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning); reasoning != "" {
			if !b.thinkingOpen {
				// Anthropic's own thinking blocks carry a signature. There is nothing to
				// derive one from here, so the field is left empty: clients that only
				// display thinking are fine, and the block is not replayable upstream.
				frames, idx := b.openBlock(map[string]any{
					"type":      "thinking",
					"thinking":  "",
					"signature": "",
				})
				out = append(out, frames...)
				b.thinkingIndex = idx
				b.thinkingOpen = true
			}
			out = append(out, sseFrame("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": b.thinkingIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
			}))
		}

		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			// Text ends the thinking phase.
			if b.thinkingOpen {
				out = append(out, b.closeBlock(b.thinkingIndex))
				b.thinkingOpen = false
			}
			if !b.textOpen {
				frames, idx := b.openBlock(map[string]any{"type": "text", "text": ""})
				out = append(out, frames...)
				b.textIndex = idx
				b.textOpen = true
			}
			out = append(out, sseFrame("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": b.textIndex,
				"delta": map[string]any{"type": "text_delta", "text": *choice.Delta.Content},
			}))
		}

		for i := range choice.Delta.ToolCalls {
			tc := &choice.Delta.ToolCalls[i]
			slot := 0
			if tc.Index != nil {
				slot = *tc.Index
			}

			blockIdx, known := b.toolBlocks[slot]
			if !known {
				// A tool call closes text and thinking: Anthropic tool_use blocks come last.
				if b.textOpen {
					out = append(out, b.closeBlock(b.textIndex))
					b.textOpen = false
				}
				if b.thinkingOpen {
					out = append(out, b.closeBlock(b.thinkingIndex))
					b.thinkingOpen = false
				}
				name := ""
				if tc.Function != nil {
					name = tc.Function.Name
				}
				id := tc.ID
				if id == "" {
					// Anthropic requires an id on tool_use, and the client will echo it back
					// as tool_use_id. A provider that omits it would otherwise produce a
					// block the client cannot answer.
					id = "toolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
				}
				frames, idx := b.openBlock(map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": map[string]any{},
				})
				out = append(out, frames...)
				b.toolBlocks[slot] = idx
				b.toolOrder = append(b.toolOrder, slot)
				blockIdx = idx
			}

			if tc.Function != nil && tc.Function.Arguments != "" {
				out = append(out, sseFrame("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": blockIdx,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": tc.Function.Arguments,
					},
				}))
			}
		}
	}

	if choice.FinishReason != nil && *choice.FinishReason != "" {
		b.stopReason = bridgeOpenAIFinishToAnthropic(*choice.FinishReason)
	}
	return out
}

// closeOpenBlocks closes every block still open, in reverse index order.
func (b *bridgeOpenAIToAnthropicStream) closeOpenBlocks() [][]byte {
	var out [][]byte
	for i := len(b.toolOrder) - 1; i >= 0; i-- {
		if idx, ok := b.toolBlocks[b.toolOrder[i]]; ok {
			out = append(out, b.closeBlock(idx))
		}
	}
	b.toolBlocks = make(map[int]int)
	b.toolOrder = nil
	if b.textOpen {
		out = append(out, b.closeBlock(b.textIndex))
		b.textOpen = false
	}
	if b.thinkingOpen {
		out = append(out, b.closeBlock(b.thinkingIndex))
		b.thinkingOpen = false
	}
	return out
}

func (b *bridgeOpenAIToAnthropicStream) finish() [][]byte {
	if b.done {
		return nil
	}
	b.done = true

	var out [][]byte
	// An upstream that returned nothing at all still needs a valid document, or the
	// client sees a stream that opened and never became a message.
	out = append(out, b.ensureStart()...)
	out = append(out, b.closeOpenBlocks()...)

	if b.stopReason == "" {
		b.stopReason = "end_turn"
	}
	delta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": b.stopReason, "stop_sequence": nil},
	}
	if b.usage.seen {
		delta["usage"] = b.usage.anthropicUsage()
	} else {
		// Anthropic clients read the final output count from here; omitting usage
		// entirely makes a turn look like it produced nothing.
		delta["usage"] = map[string]any{"output_tokens": 0}
	}
	out = append(out, sseFrame("message_delta", delta))
	out = append(out, sseFrame("message_stop", map[string]any{"type": "message_stop"}))
	return out
}

// mergeUsageInto folds a chunk's usage into an accumulator, checking both the
// chat/completions and the responses spelling.
func (c *bridgeOpenAIChunk) mergeUsageInto(u *openAIStyleUsage) {
	u.merge(c.Usage)
}

func firstNonEmpty(values ...*string) string {
	for _, v := range values {
		if v != nil && *v != "" {
			return *v
		}
	}
	return ""
}

// ===========================================================================
// Non-streaming translation
// ===========================================================================

// bridgeAnthropicJSONToOpenAI converts a complete Anthropic message response
// into an OpenAI chat completion.
func bridgeAnthropicJSONToOpenAI(payload []byte, model string) ([]byte, error) {
	var src struct {
		ID      string `json:"id"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string                `json:"stop_reason"`
		Usage      *anthropicUsageFields `json:"usage"`
	}
	if err := json.Unmarshal(payload, &src); err != nil {
		return nil, fmt.Errorf("provider returned a body that is not an Anthropic message: %w", err)
	}

	var text, reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	for _, block := range src.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": args,
				},
			})
		}
	}

	message := map[string]any{"role": "assistant"}
	// Anthropic commonly returns prose AND tool_use in the same turn ("Let me look
	// that up" followed by the call). OpenAI permits content alongside tool_calls,
	// so the text is kept; only a genuinely empty text becomes null, which is the
	// shape OpenAI clients expect for a pure tool-call turn.
	if s := text.String(); s != "" {
		message["content"] = s
	} else if len(toolCalls) > 0 {
		message["content"] = nil
	} else {
		message["content"] = ""
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}

	finish := bridgeAnthropicStopToOpenAI(src.StopReason)
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	var usage claudeStyleUsage
	usage.merge(src.Usage)

	out := map[string]any{
		"id":      "chatcmpl-" + strings.TrimPrefix(src.ID, "msg_"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage.openAIUsage(),
	}
	return json.Marshal(out)
}

// bridgeOpenAIJSONToAnthropic converts a complete OpenAI chat completion into an
// Anthropic message response.
func bridgeOpenAIJSONToAnthropic(payload []byte, model string, fallbackInput int) ([]byte, error) {
	var src struct {
		ID      string `json:"id"`
		Choices []struct {
			Message *struct {
				Content          *string `json:"content"`
				Reasoning        *string `json:"reasoning"`
				ReasoningContent *string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function *struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openAIUsageFields `json:"usage"`
	}
	if err := json.Unmarshal(payload, &src); err != nil {
		return nil, fmt.Errorf("provider returned a body that is not an OpenAI completion: %w", err)
	}

	blocks := make([]map[string]any, 0, 2)
	stopReason := "end_turn"

	if len(src.Choices) > 0 {
		choice := src.Choices[0]
		if choice.FinishReason != "" {
			stopReason = bridgeOpenAIFinishToAnthropic(choice.FinishReason)
		}
		if m := choice.Message; m != nil {
			if reasoning := firstNonEmpty(m.ReasoningContent, m.Reasoning); reasoning != "" {
				blocks = append(blocks, map[string]any{
					"type":      "thinking",
					"thinking":  reasoning,
					"signature": "",
				})
			}
			if m.Content != nil && *m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": *m.Content})
			}
			for i := range m.ToolCalls {
				tc := &m.ToolCalls[i]
				name := ""
				args := json.RawMessage("{}")
				if tc.Function != nil {
					name = tc.Function.Name
					if trimmed := strings.TrimSpace(tc.Function.Arguments); trimmed != "" && json.Valid([]byte(trimmed)) {
						args = json.RawMessage(trimmed)
					}
				}
				id := tc.ID
				if id == "" {
					id = "toolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": args,
				})
				stopReason = "tool_use"
			}
		}
	}

	var usage openAIStyleUsage
	usage.merge(src.Usage)
	usageOut := usage.anthropicUsage()
	if !usage.seen && fallbackInput > 0 {
		usageOut["input_tokens"] = fallbackInput
	}

	id := src.ID
	if id == "" {
		id = uuid.New().String()
	}
	out := map[string]any{
		"id":            "msg_" + strings.TrimPrefix(id, "chatcmpl-"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usageOut,
	}
	return json.Marshal(out)
}

// newBridgeStreamTranslator returns the translator for a bridge direction, or
// nil for passthrough.
//
// clientModel is the alias the caller asked for, not the provider's real model
// name: a client that sent "claude-sonnet-4.5" must see that string echoed back,
// otherwise it looks like it silently got a different model.
func newBridgeStreamTranslator(bridge, clientModel string, fallbackInput int) bridgeStreamTranslator {
	switch bridge {
	case config.BridgeOpenAIToAnthropic:
		// Provider replies Anthropic, client wants OpenAI.
		return newBridgeAnthropicToOpenAIStream(clientModel)
	case config.BridgeAnthropicToOpenAI:
		// Provider replies OpenAI, client wants Anthropic.
		return newBridgeOpenAIToAnthropicStream(clientModel, fallbackInput)
	case config.BridgeResponsesToOpenAI, config.BridgeResponsesToAnthropic:
		// Client wants Responses events; see provider_bridge_responses.go.
		return bridgeResponsesStreamTranslator(bridge, clientModel, fallbackInput)
	}
	return nil
}

// bridgeJSONResponse translates a non-streaming provider body for the client.
func bridgeJSONResponse(bridge string, payload []byte, clientModel string, fallbackInput int) ([]byte, error) {
	switch bridge {
	case config.BridgeOpenAIToAnthropic:
		return bridgeAnthropicJSONToOpenAI(payload, clientModel)
	case config.BridgeAnthropicToOpenAI:
		return bridgeOpenAIJSONToAnthropic(payload, clientModel, fallbackInput)
	case config.BridgeResponsesToOpenAI:
		return bridgeOpenAIChatJSONToResponses(payload, clientModel, fallbackInput)
	case config.BridgeResponsesToAnthropic:
		return bridgeAnthropicJSONToResponses(payload, clientModel, fallbackInput)
	}
	return payload, nil
}
