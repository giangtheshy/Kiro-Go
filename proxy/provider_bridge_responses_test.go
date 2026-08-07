package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"kiro-go/config"
)

// Tests for the /v1/responses bridge. The helpers (parseFrames, runStream,
// eventTypes) live in provider_bridge_test.go.

// --- request: Responses -> Chat Completions --------------------------------

func TestBridgeResponsesRequestToChatBasics(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4.5",
		"instructions": "Be terse.",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		],
		"max_output_tokens": 256,
		"temperature": 0.3,
		"stream": true
	}`)

	out, err := bridgeResponsesRequestToOpenAIChat(raw, "deepseek-chat", true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}

	if got["model"] != "deepseek-chat" {
		t.Fatalf("model = %v, want the upstream name", got["model"])
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true", got["stream"])
	}
	if got["max_tokens"] != float64(256) {
		t.Fatalf("max_tokens = %v, want max_output_tokens carried over", got["max_tokens"])
	}
	if got["temperature"] != 0.3 {
		t.Fatalf("temperature = %v, want 0.3", got["temperature"])
	}

	// Streaming must opt into usage reporting or the request bills as free.
	opts, _ := got["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage true", got["stream_options"])
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected system + user message, got %d: %s", len(msgs), out)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "Be terse." {
		t.Fatalf("instructions did not become the leading system message: %v", first)
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "user" {
		t.Fatalf("second message role = %v, want user", second["role"])
	}
}

func TestBridgeResponsesRequestAcceptsStringInput(t *testing.T) {
	// The plain-string form of "input" is the shortest legal Responses request, and
	// dropping it would silently lose the entire prompt.
	raw := []byte(`{"model":"m","input":"hello there"}`)

	out, err := bridgeResponsesRequestToOpenAIChat(raw, "up", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %s", len(got.Messages), out)
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != "hello there" {
		t.Fatalf("string input lost: %+v", got.Messages[0])
	}
}

func TestBridgeResponsesRequestNeverSendsEmptyMessages(t *testing.T) {
	// An upstream rejects an empty messages array, so a request carrying only
	// instructions must still produce a turn.
	raw := []byte(`{"model":"m","instructions":"system only"}`)

	out, err := bridgeResponsesRequestToOpenAIChat(raw, "up", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	json.Unmarshal(out, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("expected system + placeholder user, got %d: %s", len(got.Messages), out)
	}
	if got.Messages[1].Role != "user" {
		t.Fatalf("expected a user turn to be appended, got %q", got.Messages[1].Role)
	}
}

func TestBridgeResponsesRequestConvertsToolsAndCalls(t *testing.T) {
	// Tools are declared in the flat Responses shape and must come out nested;
	// function_call / function_call_output items must round-trip as an assistant
	// tool call plus a tool result, or the upstream sees a broken conversation.
	raw := []byte(`{
		"model": "m",
		"input": [
			{"type":"message","role":"user","content":"list files"},
			{"type":"function_call","call_id":"call_1","name":"ls","arguments":"{\"path\":\".\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"}
		],
		"tools": [
			{"type":"function","name":"ls","description":"list","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},
			{"type":"web_search"}
		],
		"tool_choice": {"type":"function","name":"ls"}
	}`)

	out, err := bridgeResponsesRequestToOpenAIChat(raw, "up", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	var got struct {
		Messages []struct {
			Role       string     `json:"role"`
			Content    any        `json:"content"`
			ToolCalls  []ToolCall `json:"tool_calls"`
			ToolCallID string     `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string `json:"name"`
				Parameters any    `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice map[string]any `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}

	// The built-in web_search has no counterpart an arbitrary provider could honour.
	if len(got.Tools) != 1 {
		t.Fatalf("expected only the function tool to survive, got %d: %s", len(got.Tools), out)
	}
	if got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "ls" {
		t.Fatalf("tool not nested into chat shape: %+v", got.Tools[0])
	}
	if got.Tools[0].Function.Parameters == nil {
		t.Fatal("tool parameters were dropped")
	}

	fn, _ := got.ToolChoice["function"].(map[string]any)
	if got.ToolChoice["type"] != "function" || fn == nil || fn["name"] != "ls" {
		t.Fatalf("tool_choice not nested: %v", got.ToolChoice)
	}

	if len(got.Messages) != 3 {
		t.Fatalf("expected user + assistant call + tool result, got %d: %s", len(got.Messages), out)
	}
	assistant := got.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("function_call did not become an assistant tool call: %+v", assistant)
	}
	if assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Function.Name != "ls" {
		t.Fatalf("tool call identity lost: %+v", assistant.ToolCalls[0])
	}
	if assistant.Content != nil {
		t.Fatalf("a pure tool-call turn must send a null content, got %#v", assistant.Content)
	}
	result := got.Messages[2]
	if result.Role != "tool" || result.ToolCallID != "call_1" || result.Content != "a.txt" {
		t.Fatalf("function_call_output did not become a tool message: %+v", result)
	}
}

func TestBridgeResponsesRequestCarriesReasoningEffort(t *testing.T) {
	raw := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"high"}}`)
	out, err := bridgeResponsesRequestToOpenAIChat(raw, "up", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
}

func TestBridgeResponsesRequestToAnthropicChains(t *testing.T) {
	// The Anthropic direction is built by chaining through the chat form, so the
	// end result must be a valid /v1/messages body: system hoisted out of the array
	// and max_tokens always present.
	raw := []byte(`{
		"model":"m",
		"instructions":"Be terse.",
		"input":[{"type":"message","role":"user","content":"hi"}]
	}`)

	out, err := bridgeResponsesRequestToAnthropic(raw, "claude-x", true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
		System    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if got.Model != "claude-x" || !got.Stream {
		t.Fatalf("model/stream wrong: %+v", got)
	}
	// Anthropic rejects a request without max_tokens.
	if got.MaxTokens <= 0 {
		t.Fatalf("max_tokens = %d, want a positive default", got.MaxTokens)
	}
	if len(got.System) != 1 || got.System[0].Text != "Be terse." {
		t.Fatalf("instructions did not reach the top-level system field: %+v", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", got.Messages)
	}
}

// --- stream: OpenAI chunks -> Responses events -----------------------------

func TestBridgeResponsesStreamTextFraming(t *testing.T) {
	tr := newBridgeOpenAIToResponsesStream("claude-sonnet-4.5", 0)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		`data: [DONE]`,
	})

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
		"[DONE]",
	}
	if got := eventTypes(events); !equalStrings(got, want) {
		t.Fatalf("event sequence\n got: %v\nwant: %v", got, want)
	}

	// sequence_number must start at 1 and increase by exactly one per event.
	seq := 0
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		n, ok := e.Payload["sequence_number"].(float64)
		if !ok {
			t.Fatalf("event %q has no sequence_number: %s", e.Event, e.Raw)
		}
		seq++
		if int(n) != seq {
			t.Fatalf("event %q sequence_number = %d, want %d", e.Event, int(n), seq)
		}
	}

	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("final status = %v, want completed", resp["status"])
	}
	if resp["model"] != "claude-sonnet-4.5" {
		t.Fatalf("model = %v, want the alias the client asked for", resp["model"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 output item, got %d: %s", len(output), final.Raw)
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "Hello" {
		t.Fatalf("aggregated text = %v, want Hello", part["text"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage not carried: %v", usage)
	}
}

func TestBridgeResponsesStreamToolCall(t *testing.T) {
	tr := newBridgeOpenAIToResponsesStream("m", 0)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"content":"Looking"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"ls","arguments":"{\"p\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\".\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	})

	types := eventTypes(events)
	// The open text item must be closed before the function_call item opens.
	msgDone := indexOfString(types, "response.output_item.done")
	fnAdded := lastIndexOfString(types, "response.output_item.added")
	if msgDone < 0 || fnAdded < 0 || msgDone > fnAdded {
		t.Fatalf("message was not closed before the tool item opened: %v", types)
	}
	if !containsString(types, "response.function_call_arguments.delta") {
		t.Fatalf("arguments were not streamed: %v", types)
	}
	if !containsString(types, "response.function_call_arguments.done") {
		t.Fatalf("arguments were never closed: %v", types)
	}

	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected message + function_call, got %d: %s", len(output), final.Raw)
	}
	fn, _ := output[1].(map[string]any)
	if fn["type"] != "function_call" || fn["call_id"] != "call_9" || fn["name"] != "ls" {
		t.Fatalf("function_call item wrong: %v", fn)
	}
	// Fragments must be reassembled into the complete argument string.
	if fn["arguments"] != `{"p":"."}` {
		t.Fatalf("arguments = %v, want the reassembled JSON", fn["arguments"])
	}

	// output_index must be contiguous, since a gap marshals as a null the SDK rejects.
	for i, raw := range output {
		item, _ := raw.(map[string]any)
		if item == nil {
			t.Fatalf("output[%d] is null", i)
		}
	}
}

func TestBridgeResponsesStreamReasoningOrdering(t *testing.T) {
	tr := newBridgeOpenAIToResponsesStream("m", 0)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	})

	types := eventTypes(events)
	if !containsString(types, "response.reasoning_summary_text.delta") {
		t.Fatalf("reasoning was not streamed: %v", types)
	}
	// Reasoning must be a closed item before the visible answer opens.
	rsDone := indexOfString(types, "response.reasoning_summary_part.done")
	textAdded := indexOfString(types, "response.content_part.added")
	if rsDone < 0 || textAdded < 0 || rsDone > textAdded {
		t.Fatalf("reasoning item was not closed before the message opened: %v", types)
	}

	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected reasoning + message, got %d: %s", len(output), final.Raw)
	}
	rs, _ := output[0].(map[string]any)
	if rs["type"] != "reasoning" {
		t.Fatalf("first item = %v, want reasoning", rs["type"])
	}
	summary, _ := rs["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary must always hold exactly one part, got %d", len(summary))
	}
	sp, _ := summary[0].(map[string]any)
	if sp["text"] != "thinking..." {
		t.Fatalf("summary text = %v", sp["text"])
	}
}

func TestBridgeResponsesStreamTruncatedStillTerminates(t *testing.T) {
	// The upstream dies mid-text. The client must still receive a closed document,
	// otherwise its SDK waits forever on an item that never ends.
	tr := newBridgeOpenAIToResponsesStream("m", 0)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
	})

	types := eventTypes(events)
	for _, required := range []string{
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
		"[DONE]",
	} {
		if !containsString(types, required) {
			t.Fatalf("truncated stream is missing %s: %v", required, types)
		}
	}
}

func TestBridgeResponsesStreamEmptyUpstreamIsWellFormed(t *testing.T) {
	// A provider that returns nothing at all still has to produce a document with
	// an output array present, or a strict SDK rejects the response.
	tr := newBridgeOpenAIToResponsesStream("m", 0)
	events := runStream(t, tr, nil)

	if got := eventTypes(events); !equalStrings(got, []string{
		"response.created", "response.in_progress", "response.completed", "[DONE]",
	}) {
		t.Fatalf("unexpected sequence: %v", got)
	}
	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	if _, ok := resp["output"]; !ok {
		t.Fatalf("response.output must be present even when empty: %s", final.Raw)
	}
}

func TestBridgeResponsesStreamLengthBecomesIncomplete(t *testing.T) {
	// A truncated turn reported as "completed" would let an agentic client treat a
	// cut-off answer as finished.
	tr := newBridgeOpenAIToResponsesStream("m", 0)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"content":"cut"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
	})

	final := events[len(events)-2]
	if final.Event != "response.incomplete" {
		t.Fatalf("terminal event = %q, want response.incomplete", final.Event)
	}
	resp, _ := final.Payload["response"].(map[string]any)
	if resp["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", resp["status"])
	}
	details, _ := resp["incomplete_details"].(map[string]any)
	if details == nil || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v", resp["incomplete_details"])
	}
}

func TestBridgeResponsesStreamMidStreamError(t *testing.T) {
	// The client already has a 200 and some bytes, so the only honest ending is a
	// terminal failure inside the stream.
	tr := newBridgeOpenAIToResponsesStream("m", 0)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})

	types := eventTypes(events)
	if !containsString(types, "response.failed") {
		t.Fatalf("no terminal failure emitted: %v", types)
	}
	// The open message item must be closed before the failure.
	if !containsString(types, "response.output_item.done") {
		t.Fatalf("open item was abandoned: %v", types)
	}
	failed := events[len(events)-2]
	resp, _ := failed.Payload["response"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || !strings.Contains(errObj["message"].(string), "slow down") {
		t.Fatalf("upstream error message lost: %s", failed.Raw)
	}
}

func TestBridgeResponsesStreamFallbackInputTokens(t *testing.T) {
	// A provider that reports no usage must not bill as free.
	tr := newBridgeOpenAIToResponsesStream("m", 1234)
	events := runStream(t, tr, []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	})

	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil || usage["input_tokens"] != float64(1234) {
		t.Fatalf("fallback input tokens not used: %v", usage)
	}
}

// --- stream: Anthropic upstream -> Responses client (chained) --------------

func TestBridgeResponsesChainedFromAnthropic(t *testing.T) {
	tr := bridgeResponsesStreamTranslator(config.BridgeResponsesToAnthropic, "claude-x", 0)
	if tr == nil {
		t.Fatal("no translator for the anthropic->responses direction")
	}
	events := runStream(t, tr, []string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`data: {"type":"message_stop"}`,
	})

	types := eventTypes(events)
	for _, required := range []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.completed",
	} {
		if !containsString(types, required) {
			t.Fatalf("chained stream is missing %s: %v", required, types)
		}
	}

	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 message item, got %d: %s", len(output), final.Raw)
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "Hi" {
		t.Fatalf("text lost through the chain: %v", part["text"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["output_tokens"] != float64(3) {
		t.Fatalf("usage lost through the chain: %v", usage)
	}
}

func TestBridgeResponsesChainedFromAnthropicToolCall(t *testing.T) {
	tr := bridgeResponsesStreamTranslator(config.BridgeResponsesToAnthropic, "claude-x", 0)
	events := runStream(t, tr, []string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"ls"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"p\":\".\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	})

	final := events[len(events)-2]
	resp, _ := final.Payload["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 function_call item, got %d: %s", len(output), final.Raw)
	}
	fn, _ := output[0].(map[string]any)
	if fn["type"] != "function_call" || fn["call_id"] != "toolu_1" || fn["name"] != "ls" {
		t.Fatalf("tool identity lost through the chain: %v", fn)
	}
	if fn["arguments"] != `{"p":"."}` {
		t.Fatalf("arguments = %v", fn["arguments"])
	}
}

func TestBridgeResponsesChainedTruncatedTerminates(t *testing.T) {
	// Upstream dies before message_stop: both stages must still unwind.
	tr := bridgeResponsesStreamTranslator(config.BridgeResponsesToAnthropic, "claude-x", 0)
	events := runStream(t, tr, []string{
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
	})

	types := eventTypes(events)
	for _, required := range []string{"response.output_item.done", "response.completed", "[DONE]"} {
		if !containsString(types, required) {
			t.Fatalf("truncated chain is missing %s: %v", required, types)
		}
	}
}

// --- non-streaming ---------------------------------------------------------

func TestBridgeOpenAIChatJSONToResponses(t *testing.T) {
	payload := []byte(`{
		"id": "chatcmpl-1",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "done",
				"reasoning_content": "thought",
				"tool_calls": [{"id":"call_2","type":"function","function":{"name":"ls","arguments":"{}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens":20,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}
	}`)

	out, err := bridgeOpenAIChatJSONToResponses(payload, "claude-x", 0)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}

	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("envelope wrong: %v", got)
	}
	if got["model"] != "claude-x" {
		t.Fatalf("model = %v, want the client's alias", got["model"])
	}

	output, _ := got["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("expected reasoning + message + function_call, got %d: %s", len(output), out)
	}
	if rs, _ := output[0].(map[string]any); rs["type"] != "reasoning" {
		t.Fatalf("output[0] = %v, want reasoning first", rs["type"])
	}
	msg, _ := output[1].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("output[1] = %v, want message", msg["type"])
	}
	fn, _ := output[2].(map[string]any)
	if fn["type"] != "function_call" || fn["call_id"] != "call_2" {
		t.Fatalf("output[2] = %v", fn)
	}

	usage, _ := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(20) || usage["output_tokens"] != float64(4) {
		t.Fatalf("usage wrong: %v", usage)
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(5) {
		t.Fatalf("cached tokens lost: %v", details)
	}
}

func TestBridgeOpenAIChatJSONToResponsesLengthIsIncomplete(t *testing.T) {
	payload := []byte(`{"choices":[{"message":{"content":"cut"},"finish_reason":"length"}]}`)
	out, err := bridgeOpenAIChatJSONToResponses(payload, "m", 0)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", got["status"])
	}
	details, _ := got["incomplete_details"].(map[string]any)
	if details == nil || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %v", got["incomplete_details"])
	}
}

func TestBridgeOpenAIChatJSONToResponsesFallbackTokens(t *testing.T) {
	payload := []byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	out, err := bridgeOpenAIChatJSONToResponses(payload, "m", 99)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	usage, _ := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(99) {
		t.Fatalf("fallback tokens not applied: %v", usage)
	}
}

func TestBridgeOpenAIChatJSONToResponsesRejectsGarbage(t *testing.T) {
	// A body that is not a chat completion must fail before anything is written, so
	// the router can fall through to the next upstream.
	if _, err := bridgeOpenAIChatJSONToResponses([]byte(`not json`), "m", 0); err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
}

func TestBridgeAnthropicJSONToResponsesChains(t *testing.T) {
	payload := []byte(`{
		"id": "msg_1",
		"content": [
			{"type":"thinking","thinking":"hmm"},
			{"type":"text","text":"answer"},
			{"type":"tool_use","id":"toolu_7","name":"ls","input":{"p":"."}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens":11,"output_tokens":2}
	}`)

	out, err := bridgeAnthropicJSONToResponses(payload, "claude-x", 0)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)

	output, _ := got["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("expected reasoning + message + function_call, got %d: %s", len(output), out)
	}
	fn, _ := output[2].(map[string]any)
	if fn["call_id"] != "toolu_7" {
		t.Fatalf("tool id lost through the chain: %v", fn)
	}
	usage, _ := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) {
		t.Fatalf("usage lost through the chain: %v", usage)
	}
}

// --- dispatch wiring -------------------------------------------------------

func TestBridgeRequestBodyDispatchesResponses(t *testing.T) {
	raw := []byte(`{"model":"alias","input":"hi"}`)

	cases := []struct {
		bridge   string
		endpoint string
		wantKey  string // a field only the target protocol has
	}{
		{config.BridgeResponsesToOpenAI, config.ProviderEndpointChatCompletions, "messages"},
		{config.BridgeResponsesToAnthropic, config.ProviderEndpointMessages, "max_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.bridge, func(t *testing.T) {
			step := &upstreamStep{
				UpstreamModel:    "real-model",
				Bridge:           tc.bridge,
				UpstreamEndpoint: tc.endpoint,
			}
			body, err := bridgeRequestBody(step, &passthroughCtx{
				Raw:      raw,
				Stream:   false,
				Endpoint: config.ProviderEndpointResponses,
			})
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if got["model"] != "real-model" {
				t.Fatalf("model = %v, want the upstream name", got["model"])
			}
			if _, ok := got[tc.wantKey]; !ok {
				t.Fatalf("translated body has no %q: %s", tc.wantKey, body)
			}
			// The Responses-only field must not leak to either upstream.
			if _, ok := got["input"]; ok {
				t.Fatalf("responses-only field leaked upstream: %s", body)
			}
		})
	}
}

func TestBridgeJSONResponseDispatchesResponses(t *testing.T) {
	chat := []byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	out, err := bridgeJSONResponse(config.BridgeResponsesToOpenAI, chat, "m", 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["object"] != "response" {
		t.Fatalf("not translated to a Responses object: %s", out)
	}

	anth := []byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`)
	out, err = bridgeJSONResponse(config.BridgeResponsesToAnthropic, anth, "m", 0)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	json.Unmarshal(out, &got)
	if got["object"] != "response" {
		t.Fatalf("not translated to a Responses object: %s", out)
	}
}

func TestNewBridgeStreamTranslatorCoversResponses(t *testing.T) {
	for _, bridge := range []string{config.BridgeResponsesToOpenAI, config.BridgeResponsesToAnthropic} {
		if newBridgeStreamTranslator(bridge, "m", 0) == nil {
			t.Fatalf("no stream translator registered for %q", bridge)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	return indexOfString(list, want) >= 0
}

func indexOfString(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

func lastIndexOfString(list []string, want string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == want {
			return i
		}
	}
	return -1
}
