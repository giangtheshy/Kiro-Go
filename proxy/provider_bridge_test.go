package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"kiro-go/config"
)

// --- helpers ---------------------------------------------------------------

// parseSSE splits a translated frame list into (event, payload) pairs. Frames are
// what a client actually receives, so asserting on these rather than on internal
// state is what makes these tests meaningful.
type sseEvent struct {
	Event   string
	Payload map[string]any
	Raw     string
}

func parseFrames(t *testing.T, frames [][]byte) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, frame := range frames {
		text := string(frame)
		if strings.TrimSpace(text) == "" {
			continue
		}
		ev := sseEvent{Raw: text}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "event:"):
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				body := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if body == "[DONE]" {
					ev.Event = "[DONE]"
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(body), &payload); err != nil {
					t.Fatalf("frame is not valid JSON: %s (%v)", body, err)
				}
				ev.Payload = payload
			}
		}
		out = append(out, ev)
	}
	return out
}

// runStream feeds lines through a translator and returns every frame produced,
// including the ones finish() adds.
func runStream(t *testing.T, tr bridgeStreamTranslator, lines []string) []sseEvent {
	t.Helper()
	var frames [][]byte
	for _, line := range lines {
		frames = append(frames, tr.translate([]byte(line))...)
	}
	frames = append(frames, tr.finish()...)
	return parseFrames(t, frames)
}

func eventTypes(events []sseEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		if e.Event != "" {
			out = append(out, e.Event)
			continue
		}
		if e.Payload != nil {
			if typ, _ := e.Payload["type"].(string); typ != "" {
				out = append(out, typ)
				continue
			}
		}
		out = append(out, "?")
	}
	return out
}

// --- request: OpenAI -> Anthropic ------------------------------------------

func TestBridgeOpenAIRequestToAnthropicBasics(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4.5",
		"max_tokens": 512,
		"temperature": 0.4,
		"top_p": 0.9,
		"stop": ["END"],
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": "hi"}
		]
	}`)

	out, err := bridgeOpenAIRequestToAnthropic(raw, "glm-4.6", true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}

	if got["model"] != "glm-4.6" {
		t.Fatalf("model = %v, want the upstream name", got["model"])
	}
	if got["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v", got["stream"])
	}
	if got["temperature"] != 0.4 || got["top_p"] != 0.9 {
		t.Fatalf("sampling params lost: %v", got)
	}
	stop, _ := got["stop_sequences"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop_sequences = %v", got["stop_sequences"])
	}

	// A system message must be hoisted to the top-level system field rather than
	// staying in messages. It is emitted as a block array, not a bare string, so
	// that per-block cache_control stays expressible.
	sysBlocks, _ := got["system"].([]any)
	if len(sysBlocks) != 1 {
		t.Fatalf("system = %v, want one hoisted block", got["system"])
	}
	if block, _ := sysBlocks[0].(map[string]any); block["type"] != "text" || block["text"] != "be brief" {
		t.Fatalf("system block = %v", sysBlocks[0])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected only the user message, got %d", len(msgs))
	}
}

// max_tokens is required by the Anthropic API but optional for OpenAI clients, so
// a request that omits it must still be accepted upstream.
func TestBridgeOpenAIRequestSuppliesMaxTokens(t *testing.T) {
	out, err := bridgeOpenAIRequestToAnthropic(
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "glm-4.6", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if mt, _ := got["max_tokens"].(float64); mt <= 0 {
		t.Fatalf("max_tokens = %v, want a positive default", got["max_tokens"])
	}
}

// The "developer" role is OpenAI's newer name for a system message. Dropping it
// would silently discard operator instructions.
func TestBridgeOpenAIRequestKeepsDeveloperRole(t *testing.T) {
	out, err := bridgeOpenAIRequestToAnthropic([]byte(`{
		"model":"m",
		"messages":[
			{"role":"developer","content":"rule one"},
			{"role":"user","content":"hi"}
		]}`), "glm-4.6", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(string(out), "rule one") {
		t.Fatalf("developer instructions were dropped: %s", out)
	}
}

// Anthropic rejects two consecutive messages with the same role; OpenAI allows
// them. Merging is what keeps a bridged request valid.
func TestBridgeOpenAIRequestMergesConsecutiveRoles(t *testing.T) {
	out, err := bridgeOpenAIRequestToAnthropic([]byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"first"},
			{"role":"user","content":"second"},
			{"role":"assistant","content":"reply"}
		]}`), "glm-4.6", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("expected the two user turns merged into one, got %d messages", len(got.Messages))
	}
	if got.Messages[0].Role != "user" || len(got.Messages[0].Content) != 2 {
		t.Fatalf("first message = %+v, want one user turn with two blocks", got.Messages[0])
	}
	if got.Messages[0].Content[0].Text != "first" || got.Messages[0].Content[1].Text != "second" {
		t.Fatalf("merged content out of order: %+v", got.Messages[0].Content)
	}
}

func TestBridgeOpenAIRequestConvertsToolCallsAndResults(t *testing.T) {
	out, err := bridgeOpenAIRequestToAnthropic([]byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Hanoi\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"28C"}
		],
		"tools":[
			{"type":"function","function":{"name":"get_weather","description":"look up","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}
		],
		"tool_choice":"required"
	}`), "glm-4.6", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string         `json:"type"`
				ID        string         `json:"id"`
				Name      string         `json:"name"`
				Input     map[string]any `json:"input"`
				ToolUseID string         `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		ToolChoice map[string]any `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// assistant tool_calls -> tool_use block with parsed input
	var toolUse *struct {
		Type      string         `json:"type"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	var toolResult *struct {
		Type      string         `json:"type"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	for i := range got.Messages {
		for j := range got.Messages[i].Content {
			b := &got.Messages[i].Content[j]
			switch b.Type {
			case "tool_use":
				toolUse = b
			case "tool_result":
				toolResult = b
			}
		}
	}

	if toolUse == nil {
		t.Fatal("no tool_use block produced from tool_calls")
	}
	if toolUse.Name != "get_weather" || toolUse.ID != "call_1" {
		t.Fatalf("tool_use = %+v", toolUse)
	}
	// arguments arrive as a JSON *string* on the OpenAI side and must become a real
	// object on the Anthropic side.
	if toolUse.Input["city"] != "Hanoi" {
		t.Fatalf("tool arguments not parsed into an object: %v", toolUse.Input)
	}

	if toolResult == nil {
		t.Fatal("no tool_result block produced from the tool message")
	}
	if toolResult.ToolUseID != "call_1" {
		t.Fatalf("tool_result.tool_use_id = %q, want it to match the call", toolResult.ToolUseID)
	}

	if len(got.Tools) != 1 || got.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.Tools[0].InputSchema == nil {
		t.Fatal("tool parameters must become input_schema")
	}
	// "required" has no direct Anthropic equivalent; {"type":"any"} is the match.
	if got.ToolChoice["type"] != "any" {
		t.Fatalf("tool_choice = %v, want type=any", got.ToolChoice)
	}
}

func TestBridgeOpenAIRequestConvertsImages(t *testing.T) {
	out, err := bridgeOpenAIRequestToAnthropic([]byte(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}]}`), "glm-4.6", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Messages []struct {
			Content []struct {
				Type   string `json:"type"`
				Source *struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 {
		t.Fatalf("expected text + image, got %+v", got.Messages)
	}
	img := got.Messages[0].Content[1]
	if img.Type != "image" || img.Source == nil {
		t.Fatalf("image block = %+v", img)
	}
	if img.Source.MediaType != "image/png" || img.Source.Data != "AAAA" {
		t.Fatalf("data URL not split into media_type/data: %+v", img.Source)
	}
}

// --- request: Anthropic -> OpenAI ------------------------------------------

func TestBridgeAnthropicRequestToOpenAIBasics(t *testing.T) {
	out, err := bridgeAnthropicRequestToOpenAI([]byte(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":256,
		"system":"be brief",
		"stop_sequences":["END"],
		"messages":[{"role":"user","content":"hi"}]
	}`), "deepseek-chat", true, true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if got["model"] != "deepseek-chat" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v", got["stream"])
	}
	// Without include_usage the upstream reports no tokens and the request bills as free.
	opts, _ := got["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not set: %v", got["stream_options"])
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected system + user, got %d: %v", len(msgs), msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Fatalf("system message = %v", first)
	}
}

// An Anthropic system field can be a block array rather than a string.
func TestBridgeAnthropicRequestFlattensSystemBlocks(t *testing.T) {
	out, err := bridgeAnthropicRequestToOpenAI([]byte(`{
		"model":"m","max_tokens":10,
		"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],
		"messages":[{"role":"user","content":"hi"}]}`), "deepseek-chat", false, false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(out, &got)
	if len(got.Messages) == 0 || got.Messages[0].Role != "system" {
		t.Fatalf("no system message: %+v", got.Messages)
	}
	if !strings.Contains(got.Messages[0].Content, "one") || !strings.Contains(got.Messages[0].Content, "two") {
		t.Fatalf("system blocks not flattened: %q", got.Messages[0].Content)
	}
}

// A tool_result must leave the user turn and become its own tool-role message, or
// the model never sees the answer it asked for.
func TestBridgeAnthropicRequestSplitsToolResults(t *testing.T) {
	out, err := bridgeAnthropicRequestToOpenAI([]byte(`{
		"model":"m","max_tokens":10,
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Hanoi"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"28C"}
			]}
		]}`), "deepseek-chat", false, false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var assistant, tool *int
	for i := range got.Messages {
		switch got.Messages[i].Role {
		case "assistant":
			idx := i
			assistant = &idx
		case "tool":
			idx := i
			tool = &idx
		}
	}
	if assistant == nil {
		t.Fatalf("no assistant message: %+v", got.Messages)
	}
	a := got.Messages[*assistant]
	if len(a.ToolCalls) != 1 || a.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_use did not become tool_calls: %+v", a)
	}
	// Anthropic input is an object; OpenAI arguments must be a JSON *string*.
	if !strings.Contains(a.ToolCalls[0].Function.Arguments, "Hanoi") {
		t.Fatalf("arguments not serialised: %q", a.ToolCalls[0].Function.Arguments)
	}

	if tool == nil {
		t.Fatalf("tool_result did not become a tool message: %+v", got.Messages)
	}
	if got.Messages[*tool].ToolCallID != "toolu_1" {
		t.Fatalf("tool_call_id = %q", got.Messages[*tool].ToolCallID)
	}
}

func TestBridgeAnthropicRequestMapsThinkingToReasoningEffort(t *testing.T) {
	out, err := bridgeAnthropicRequestToOpenAI([]byte(`{
		"model":"m","max_tokens":4096,
		"thinking":{"type":"enabled","budget_tokens":16000},
		"messages":[{"role":"user","content":"hi"}]}`), "deepseek-reasoner", false, false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["reasoning_effort"] == nil {
		t.Fatalf("thinking config produced no reasoning_effort: %v", got)
	}
}

// --- response: Anthropic upstream -> OpenAI client (stream) ----------------

func TestBridgeAnthropicStreamToOpenAIText(t *testing.T) {
	events := runStream(t, newBridgeAnthropicToOpenAIStream("claude-sonnet-4.5"), []string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":100,"cache_read_input_tokens":20}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		`data: {"type":"message_stop"}`,
	})

	if len(events) < 2 {
		t.Fatalf("too few frames: %d", len(events))
	}
	if events[len(events)-1].Event != "[DONE]" {
		t.Fatalf("stream must terminate with [DONE], got %q", events[len(events)-1].Raw)
	}

	// First chunk announces the role.
	firstDelta := deltaOf(t, events[0])
	if firstDelta["role"] != "assistant" {
		t.Fatalf("first chunk must carry role=assistant, got %v", firstDelta)
	}

	var text strings.Builder
	var finish string
	var usage map[string]any
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if u, ok := e.Payload["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := e.Payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			text.WriteString(c)
		}
	}

	if text.String() != "Hello" {
		t.Fatalf("reassembled text = %q, want %q", text.String(), "Hello")
	}
	if finish != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finish)
	}
	if usage == nil {
		t.Fatal("no usage reported to the client")
	}
	// Anthropic input_tokens excludes cache; OpenAI prompt_tokens includes it.
	if usage["prompt_tokens"] != float64(120) {
		t.Fatalf("prompt_tokens = %v, want 120 (100 input + 20 cache read)", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != float64(7) {
		t.Fatalf("completion_tokens = %v", usage["completion_tokens"])
	}
}

func TestBridgeAnthropicStreamToOpenAIToolCall(t *testing.T) {
	events := runStream(t, newBridgeAnthropicToOpenAIStream("claude-sonnet-4.5"), []string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Hanoi\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`,
		`data: {"type":"message_stop"}`,
	})

	var args strings.Builder
	var sawID, sawName bool
	var finish string
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		choices, _ := e.Payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			if call["id"] == "toolu_9" {
				sawID = true
			}
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if fn["name"] == "get_weather" {
				sawName = true
			}
			if a, ok := fn["arguments"].(string); ok {
				args.WriteString(a)
			}
		}
	}

	if !sawID || !sawName {
		t.Fatalf("tool call id/name never reached the client (id=%t name=%t)", sawID, sawName)
	}
	// The streamed fragments must reassemble into the exact original JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("reassembled arguments are not valid JSON: %q (%v)", args.String(), err)
	}
	if parsed["city"] != "Hanoi" {
		t.Fatalf("arguments = %v", parsed)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish)
	}
}

func TestBridgeAnthropicStreamToOpenAIThinking(t *testing.T) {
	events := runStream(t, newBridgeAnthropicToOpenAIStream("m"), []string{
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	})

	found := false
	for _, e := range events {
		d := deltaOfSoft(e)
		if d == nil {
			continue
		}
		if r, ok := d["reasoning_content"].(string); ok && r == "pondering" {
			found = true
		}
	}
	if !found {
		t.Fatal("thinking_delta did not become reasoning_content")
	}
}

// A stream that dies before message_delta must still terminate properly, or the
// client waits forever on a response it will never get.
func TestBridgeAnthropicStreamTruncatedStillTerminates(t *testing.T) {
	events := runStream(t, newBridgeAnthropicToOpenAIStream("m"), []string{
		`data: {"type":"message_start","message":{"id":"m1","usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
	})

	last := events[len(events)-1]
	if last.Event != "[DONE]" {
		t.Fatalf("truncated stream must still end with [DONE], got %q", last.Raw)
	}
	var sawFinish bool
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		choices, _ := e.Payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Fatal("truncated stream produced no finish_reason")
	}
}

// --- response: OpenAI upstream -> Anthropic client (stream) ----------------

// The framing invariant: every block opened is closed, message_start comes first,
// message_stop last. A Claude client rejects a document that breaks this.
func TestBridgeOpenAIStreamToAnthropicFraming(t *testing.T) {
	events := runStream(t, newBridgeOpenAIToAnthropicStream("claude-sonnet-4.5", 42), []string{
		`data: {"id":"c1","choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"c1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":10}}}`,
		`data: [DONE]`,
	})

	types := eventTypes(events)
	if types[0] != "message_start" {
		t.Fatalf("stream must open with message_start, got %v", types)
	}
	if types[len(types)-1] != "message_stop" {
		t.Fatalf("stream must close with message_stop, got %v", types)
	}

	// Every opened index must be closed exactly once.
	open := map[float64]int{}
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		idx, hasIdx := e.Payload["index"].(float64)
		if !hasIdx {
			continue
		}
		switch e.Payload["type"] {
		case "content_block_start":
			open[idx]++
		case "content_block_stop":
			open[idx]--
		}
	}
	for idx, n := range open {
		if n != 0 {
			t.Fatalf("block %v left unbalanced (%d)", idx, n)
		}
	}

	var text strings.Builder
	var stopReason string
	var usage map[string]any
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if e.Payload["type"] == "content_block_delta" {
			d, _ := e.Payload["delta"].(map[string]any)
			if t2, ok := d["text"].(string); ok {
				text.WriteString(t2)
			}
		}
		if e.Payload["type"] == "message_delta" {
			d, _ := e.Payload["delta"].(map[string]any)
			if sr, ok := d["stop_reason"].(string); ok {
				stopReason = sr
			}
			if u, ok := e.Payload["usage"].(map[string]any); ok {
				usage = u
			}
		}
	}

	if text.String() != "Hello" {
		t.Fatalf("text = %q", text.String())
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", stopReason)
	}
	if usage == nil {
		t.Fatal("no usage in message_delta")
	}
	// prompt_tokens 50 includes 10 cached, so Anthropic input_tokens must be 40.
	if usage["input_tokens"] != float64(40) {
		t.Fatalf("input_tokens = %v, want 40 (50 prompt - 10 cached)", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(10) {
		t.Fatalf("cache_read_input_tokens = %v", usage["cache_read_input_tokens"])
	}
}

func TestBridgeOpenAIStreamToAnthropicToolCall(t *testing.T) {
	events := runStream(t, newBridgeOpenAIToAnthropicStream("m", 0), []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_7","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Hanoi\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	})

	var blockIdx float64 = -1
	var name, id string
	var args strings.Builder
	var stopReason string
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		switch e.Payload["type"] {
		case "content_block_start":
			cb, _ := e.Payload["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				blockIdx, _ = e.Payload["index"].(float64)
				name, _ = cb["name"].(string)
				id, _ = cb["id"].(string)
			}
		case "content_block_delta":
			d, _ := e.Payload["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				if pj, ok := d["partial_json"].(string); ok {
					args.WriteString(pj)
				}
			}
		case "message_delta":
			d, _ := e.Payload["delta"].(map[string]any)
			stopReason, _ = d["stop_reason"].(string)
		}
	}

	if blockIdx < 0 {
		t.Fatal("no tool_use block was opened")
	}
	if name != "get_weather" || id != "call_7" {
		t.Fatalf("tool_use name=%q id=%q", name, id)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("reassembled partial_json invalid: %q (%v)", args.String(), err)
	}
	if parsed["city"] != "Hanoi" {
		t.Fatalf("args = %v", parsed)
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stopReason)
	}
}

// A provider that streams reasoning_content must produce a thinking block, and it
// has to be closed before the text block opens.
func TestBridgeOpenAIStreamToAnthropicReasoningOrdering(t *testing.T) {
	events := runStream(t, newBridgeOpenAIToAnthropicStream("m", 0), []string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	})

	var order []string
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if e.Payload["type"] == "content_block_start" {
			cb, _ := e.Payload["content_block"].(map[string]any)
			order = append(order, "start:"+cb["type"].(string))
		}
		if e.Payload["type"] == "content_block_stop" {
			order = append(order, "stop")
		}
	}

	if len(order) < 4 {
		t.Fatalf("expected thinking and text blocks, got %v", order)
	}
	if order[0] != "start:thinking" {
		t.Fatalf("thinking must open first, got %v", order)
	}
	if order[1] != "stop" || order[2] != "start:text" {
		t.Fatalf("thinking must close before text opens, got %v", order)
	}
}

// An upstream that returns literally nothing must still yield a valid, empty
// Anthropic message rather than a stream the client cannot parse.
func TestBridgeOpenAIStreamToAnthropicEmptyUpstream(t *testing.T) {
	events := runStream(t, newBridgeOpenAIToAnthropicStream("m", 15), nil)
	types := eventTypes(events)
	if len(types) < 3 {
		t.Fatalf("empty upstream produced %v", types)
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" {
		t.Fatalf("framing broken on empty upstream: %v", types)
	}
	// The estimate seeds input_tokens so the turn is not billed as free.
	msg, _ := events[0].Payload["message"].(map[string]any)
	u, _ := msg["usage"].(map[string]any)
	if u["input_tokens"] != float64(15) {
		t.Fatalf("message_start input_tokens = %v, want the fallback estimate", u["input_tokens"])
	}
}

func TestBridgeOpenAIStreamToAnthropicMidStreamError(t *testing.T) {
	events := runStream(t, newBridgeOpenAIToAnthropicStream("m", 0), []string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		`data: {"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})

	var sawError bool
	for _, e := range events {
		if e.Event == "error" || (e.Payload != nil && e.Payload["type"] == "error") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("mid-stream upstream error was swallowed")
	}
	types := eventTypes(events)
	if types[len(types)-1] != "message_stop" {
		t.Fatalf("error path must still close the message, got %v", types)
	}
}

// --- response: non-streaming both ways ------------------------------------

func TestBridgeAnthropicJSONToOpenAI(t *testing.T) {
	out, err := bridgeAnthropicJSONToOpenAI([]byte(`{
		"id":"msg_1","model":"glm-4.6","stop_reason":"tool_use",
		"content":[
			{"type":"thinking","thinking":"hmm"},
			{"type":"text","text":"here you go"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Hanoi"}}
		],
		"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5}
	}`), "claude-sonnet-4.5")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Object != "chat.completion" {
		t.Fatalf("object = %q", got.Object)
	}
	// The client asked for the alias and must see the alias echoed back.
	if got.Model != "claude-sonnet-4.5" {
		t.Fatalf("model = %q, want the client-visible alias", got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	c := got.Choices[0]
	if c.Message.Content != "here you go" {
		t.Fatalf("content = %q", c.Message.Content)
	}
	if c.Message.ReasoningContent != "hmm" {
		t.Fatalf("reasoning_content = %q", c.Message.ReasoningContent)
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls = %+v", c.Message.ToolCalls)
	}
	if !strings.Contains(c.Message.ToolCalls[0].Function.Arguments, "Hanoi") {
		t.Fatalf("arguments = %q", c.Message.ToolCalls[0].Function.Arguments)
	}
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", c.FinishReason)
	}
	if got.Usage.PromptTokens != 105 {
		t.Fatalf("prompt_tokens = %d, want 105 (100 + 5 cache read)", got.Usage.PromptTokens)
	}
	if got.Usage.TotalTokens != 125 {
		t.Fatalf("total_tokens = %d", got.Usage.TotalTokens)
	}
}

func TestBridgeOpenAIJSONToAnthropic(t *testing.T) {
	out, err := bridgeOpenAIJSONToAnthropic([]byte(`{
		"id":"chatcmpl-1","model":"deepseek-chat",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{
			"role":"assistant",
			"reasoning_content":"hmm",
			"content":"here you go",
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Hanoi\"}"}}]
		}}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}
	}`), "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string         `json:"type"`
			Text     string         `json:"text"`
			Thinking string         `json:"thinking"`
			ID       string         `json:"id"`
			Name     string         `json:"name"`
			Input    map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			CacheReadInputToks int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Type != "message" || got.Role != "assistant" {
		t.Fatalf("envelope = type:%q role:%q", got.Type, got.Role)
	}
	if got.Model != "claude-sonnet-4.5" {
		t.Fatalf("model = %q, want the client alias", got.Model)
	}
	if got.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", got.StopReason)
	}

	// Block order matters to Anthropic clients: thinking, then text, then tool_use.
	if len(got.Content) != 3 {
		t.Fatalf("expected thinking+text+tool_use, got %d: %+v", len(got.Content), got.Content)
	}
	if got.Content[0].Type != "thinking" || got.Content[0].Thinking != "hmm" {
		t.Fatalf("block 0 = %+v", got.Content[0])
	}
	if got.Content[1].Type != "text" || got.Content[1].Text != "here you go" {
		t.Fatalf("block 1 = %+v", got.Content[1])
	}
	if got.Content[2].Type != "tool_use" || got.Content[2].Name != "get_weather" {
		t.Fatalf("block 2 = %+v", got.Content[2])
	}
	// OpenAI arguments is a string; Anthropic input must be a real object.
	if got.Content[2].Input["city"] != "Hanoi" {
		t.Fatalf("tool input not parsed: %v", got.Content[2].Input)
	}

	// 100 prompt includes 30 cached, so input_tokens is 70 and cache read is 30.
	if got.Usage.InputTokens != 70 {
		t.Fatalf("input_tokens = %d, want 70", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputToks != 30 {
		t.Fatalf("cache_read_input_tokens = %d, want 30", got.Usage.CacheReadInputToks)
	}
	if got.Usage.OutputTokens != 20 {
		t.Fatalf("output_tokens = %d", got.Usage.OutputTokens)
	}
}

// --- routing --------------------------------------------------------------

func TestResolveEndpointBridgeDecisions(t *testing.T) {
	cases := []struct {
		name           string
		protocol       string
		allowBridge    bool
		supportsResp   bool
		clientEndpoint string
		wantOK         bool
		wantUpstream   string
		wantBridge     string
	}{
		{
			name:     "anthropic provider serves messages natively",
			protocol: config.ProviderProtocolAnthropic, clientEndpoint: config.ProviderEndpointMessages,
			wantOK: true, wantUpstream: config.ProviderEndpointMessages, wantBridge: config.BridgeNone,
		},
		{
			name:     "openai provider skips messages without the bridge",
			protocol: config.ProviderProtocolOpenAI, clientEndpoint: config.ProviderEndpointMessages,
			wantOK: false,
		},
		{
			name:     "openai provider bridges messages when allowed",
			protocol: config.ProviderProtocolOpenAI, allowBridge: true, clientEndpoint: config.ProviderEndpointMessages,
			wantOK: true, wantUpstream: config.ProviderEndpointChatCompletions, wantBridge: config.BridgeAnthropicToOpenAI,
		},
		{
			name:     "anthropic provider bridges chat/completions when allowed",
			protocol: config.ProviderProtocolAnthropic, allowBridge: true, clientEndpoint: config.ProviderEndpointChatCompletions,
			wantOK: true, wantUpstream: config.ProviderEndpointMessages, wantBridge: config.BridgeOpenAIToAnthropic,
		},
		{
			name:     "responses is skipped without either opt-in",
			protocol: config.ProviderProtocolOpenAI, clientEndpoint: config.ProviderEndpointResponses,
			wantOK: false,
		},
		{
			name:     "responses is skipped for an anthropic provider without the bridge",
			protocol: config.ProviderProtocolAnthropic, clientEndpoint: config.ProviderEndpointResponses,
			wantOK: false,
		},
		{
			name:     "responses works when opted in",
			protocol: config.ProviderProtocolOpenAI, supportsResp: true, clientEndpoint: config.ProviderEndpointResponses,
			wantOK: true, wantUpstream: config.ProviderEndpointResponses, wantBridge: config.BridgeNone,
		},
		{
			// Passthrough outranks the bridge when the provider really implements
			// /v1/responses: it carries the parts a bridge cannot.
			name:     "responses prefers passthrough over the bridge",
			protocol: config.ProviderProtocolOpenAI, supportsResp: true, allowBridge: true,
			clientEndpoint: config.ProviderEndpointResponses,
			wantOK:         true, wantUpstream: config.ProviderEndpointResponses, wantBridge: config.BridgeNone,
		},
		{
			name:     "responses bridges to chat/completions for an openai provider",
			protocol: config.ProviderProtocolOpenAI, allowBridge: true, clientEndpoint: config.ProviderEndpointResponses,
			wantOK: true, wantUpstream: config.ProviderEndpointChatCompletions, wantBridge: config.BridgeResponsesToOpenAI,
		},
		{
			name:     "responses bridges to messages for an anthropic provider",
			protocol: config.ProviderProtocolAnthropic, allowBridge: true, clientEndpoint: config.ProviderEndpointResponses,
			wantOK: true, wantUpstream: config.ProviderEndpointMessages, wantBridge: config.BridgeResponsesToAnthropic,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &config.Provider{
				Protocol:            tc.protocol,
				AllowProtocolBridge: tc.allowBridge,
				SupportsResponses:   tc.supportsResp,
			}
			upstream, bridge, ok := p.ResolveEndpoint(tc.clientEndpoint)
			if ok != tc.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if upstream != tc.wantUpstream {
				t.Fatalf("upstream endpoint = %q, want %q", upstream, tc.wantUpstream)
			}
			if bridge != tc.wantBridge {
				t.Fatalf("bridge = %q, want %q", bridge, tc.wantBridge)
			}
		})
	}
}

// --- small helpers --------------------------------------------------------

func deltaOf(t *testing.T, e sseEvent) map[string]any {
	t.Helper()
	d := deltaOfSoft(e)
	if d == nil {
		t.Fatalf("frame carries no delta: %s", e.Raw)
	}
	return d
}

func deltaOfSoft(e sseEvent) map[string]any {
	if e.Payload == nil {
		return nil
	}
	choices, _ := e.Payload["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	d, _ := choice["delta"].(map[string]any)
	return d
}
