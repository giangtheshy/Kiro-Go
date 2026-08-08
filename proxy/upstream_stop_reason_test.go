package proxy

import (
	"bytes"
	"testing"
)

// The upstream states why a turn ended via metadataEvent.stopReason. Kiro-Go
// used to drop that frame entirely and infer the reason from the output length,
// which silently mislabels a server-side cut as a clean finish — the model looks
// like it just decided to stop, so agentic clients end the task.

func TestMetadataEventStopReasonReachesOutcome(t *testing.T) {
	var body []byte
	body = append(body, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial answer"})...)
	body = append(body, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "MAX_TOKENS"})...)
	body = append(body, awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0})...)
	stream := bytes.NewReader(body)

	var seen string
	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnStopReason: func(reason string) { seen = reason },
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.StopReason != "MAX_TOKENS" {
		t.Fatalf("outcome.StopReason = %q, want MAX_TOKENS", outcome.StopReason)
	}
	if seen != "MAX_TOKENS" {
		t.Fatalf("OnStopReason got %q, want MAX_TOKENS", seen)
	}
}

func TestMetadataEventWithoutStopReasonIsIgnored(t *testing.T) {
	var body []byte
	body = append(body, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"})...)
	body = append(body, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"conversationId": "abc"})...)
	body = append(body, awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0})...)
	stream := bytes.NewReader(body)

	fired := false
	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnStopReason: func(string) { fired = true },
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.StopReason != "" {
		t.Fatalf("outcome.StopReason = %q, want empty", outcome.StopReason)
	}
	if fired {
		t.Fatal("OnStopReason fired for a metadataEvent carrying no stopReason")
	}
}

// A nil OnStopReason must not panic: most callers (websearch loop, probes) do
// not set it.
func TestMetadataEventWithNilCallbackDoesNotPanic(t *testing.T) {
	var body []byte
	body = append(body, awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"})...)
	body = append(body, awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0})...)
	stream := bytes.NewReader(body)
	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.StopReason != "END_TURN" {
		t.Fatalf("outcome.StopReason = %q, want END_TURN", outcome.StopReason)
	}
}

func TestUpstreamStopReasonToClaude(t *testing.T) {
	cases := map[string]string{
		"END_TURN":       "end_turn",
		"end_turn":       "end_turn",
		"  MAX_TOKENS  ": "max_tokens",
		"TOOL_USE":       "tool_use",
		"STOP_SEQUENCE":  "stop_sequence",
		"":               "",
		"WEIRD_NEW_CODE": "",
	}
	for in, want := range cases {
		if got := upstreamStopReasonToClaude(in); got != want {
			t.Errorf("upstreamStopReasonToClaude(%q) = %q, want %q", in, got, want)
		}
	}
}

// The whole point of the fix: MAX_TOKENS from upstream must win even when the
// output is nowhere near the client's own max_tokens. Kiro enforces its own
// ceiling, so inference alone reports a clean end_turn here.
func TestUpstreamMaxTokensBeatsInferredEndTurn(t *testing.T) {
	got := claudeStopReasonWithUpstream("MAX_TOKENS", nil, 120, 32000)
	if got != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens (upstream said MAX_TOKENS)", got)
	}
	// Without the upstream signal this is exactly the case that used to be
	// mislabelled, which is what made the stop look voluntary.
	if got := claudeStopReason(nil, 120, 32000); got != "end_turn" {
		t.Fatalf("inference baseline = %q, want end_turn", got)
	}
}

// REGRESSION: an upstream TOOL_USE must never be echoed when no tool_use block
// reaches the client. This is reachable in production — the proxy executes
// web_search itself and strips it from the forwarded content — and reporting
// "tool_use" over a message with no tool leaves the client blocked forever
// waiting to run a tool it never received. That is strictly worse than the
// premature-stop bug this whole change set exists to fix, so it is pinned here.
func TestUpstreamToolUseWithoutToolBlocksDoesNotClaimToolUse(t *testing.T) {
	if got := claudeStopReasonWithUpstream("TOOL_USE", nil, 50, 32000); got != "end_turn" {
		t.Fatalf("claude stop_reason = %q, want end_turn: no tool block reached the client", got)
	}
	// Same shape, but the output also hit the cap: inference must still be the
	// one deciding, and it says the answer was cut.
	if got := claudeStopReasonWithUpstream("TOOL_USE", nil, 4096, 4096); got != "max_tokens" {
		t.Fatalf("claude stop_reason = %q, want max_tokens", got)
	}
	if got := openaiFinishReasonWithUpstream("TOOL_USE", false, 50, 32000); got != "stop" {
		t.Fatalf("openai finish_reason = %q, want stop", got)
	}
}

// A real tool call must outrank the upstream's reason: the client has to run it
// for the agentic loop to advance.
func TestToolUseOutranksUpstreamStopReason(t *testing.T) {
	toolUses := []KiroToolUse{{ToolUseID: "t1", Name: "bash"}}
	if got := claudeStopReasonWithUpstream("MAX_TOKENS", toolUses, 10, 32000); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
	if got := openaiFinishReasonWithUpstream("MAX_TOKENS", true, 10, 32000); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
}

// An unrecognised or absent reason must fall back to inference rather than
// inventing a verdict from a value the proxy does not understand.
func TestUnknownUpstreamStopReasonFallsBackToInference(t *testing.T) {
	if got := claudeStopReasonWithUpstream("SOMETHING_NEW", nil, 4096, 4096); got != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens from inference", got)
	}
	if got := claudeStopReasonWithUpstream("", nil, 100, 4096); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
}

func TestOpenAIFinishReasonWithUpstream(t *testing.T) {
	if got := openaiFinishReasonWithUpstream("MAX_TOKENS", false, 50, 32000); got != "length" {
		t.Fatalf("finish_reason = %q, want length", got)
	}
	if got := openaiFinishReasonWithUpstream("END_TURN", false, 50, 32000); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
	// Upstream silent, output at the client's cap: inference still catches it.
	if got := openaiFinishReasonWithUpstream("", false, 4096, 4096); got != "length" {
		t.Fatalf("finish_reason = %q, want length from inference", got)
	}
	if got := openaiFinishReasonWithUpstream("", false, 10, 4096); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
}

// END_TURN reported over an output that happens to sit exactly at the client's
// max_tokens must be honoured as a clean finish: the upstream knows whether it
// truncated, the length coincidence does not.
func TestUpstreamEndTurnBeatsMaxTokensCoincidence(t *testing.T) {
	if got := claudeStopReasonWithUpstream("END_TURN", nil, 4096, 4096); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
}

// $schema and friends are JSON-Schema meta keys the native Kiro client never
// sends; Claude Code emits $schema on every tool.
func TestToolSchemaDropsJSONSchemaMetaKeys(t *testing.T) {
	tools := []ClaudeTool{{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: map[string]interface{}{
			"$schema":  "http://json-schema.org/draft-07/schema#",
			"$id":      "https://example.com/read_file.json",
			"$comment": "internal note",
			"type":     "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"path"},
		},
	}}

	converted, _ := convertClaudeTools(tools)
	if len(converted) != 1 {
		t.Fatalf("converted %d tools, want 1", len(converted))
	}
	schema, ok := converted[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok {
		t.Fatalf("schema is %T, want map", converted[0].ToolSpecification.InputSchema.JSON)
	}
	for _, key := range []string{"$schema", "$id", "$comment"} {
		if _, present := schema[key]; present {
			t.Errorf("%s survived into the wire schema", key)
		}
	}
	// The parts that actually constrain arguments must remain untouched.
	if schema["type"] != "object" {
		t.Errorf("type = %v, want object", schema["type"])
	}
	if _, ok := schema["properties"].(map[string]interface{})["path"]; !ok {
		t.Error("properties.path was dropped")
	}
	if req, ok := schema["required"].([]interface{}); !ok || len(req) != 1 {
		t.Errorf("required = %v, want [path]", schema["required"])
	}
}

// Meta keys nested inside a property schema must go too — clients emit them on
// sub-objects as well.
func TestNestedJSONSchemaMetaKeysAreDropped(t *testing.T) {
	tools := []ClaudeTool{{
		Name: "edit",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"target": map[string]interface{}{
					"$schema": "http://json-schema.org/draft-07/schema#",
					"type":    "object",
					"properties": map[string]interface{}{
						"line": map[string]interface{}{"type": "integer"},
					},
				},
			},
		},
	}}

	converted, _ := convertClaudeTools(tools)
	schema := converted[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	target := schema["properties"].(map[string]interface{})["target"].(map[string]interface{})
	if _, present := target["$schema"]; present {
		t.Error("$schema survived on a nested property schema")
	}
	if target["type"] != "object" {
		t.Errorf("nested type = %v, want object", target["type"])
	}
}

func TestOpenAIToolSchemaDropsMetaKeys(t *testing.T) {
	tool := OpenAITool{Type: "function"}
	tool.Function.Name = "run_query"
	tool.Function.Description = "Run a query"
	tool.Function.Parameters = map[string]interface{}{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
		"properties": map[string]interface{}{
			"sql": map[string]interface{}{"type": "string"},
		},
	}

	converted := convertOpenAITools([]OpenAITool{tool})
	if len(converted) != 1 {
		t.Fatalf("converted %d tools, want 1", len(converted))
	}
	schema := converted[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if _, present := schema["$schema"]; present {
		t.Error("$schema survived on the OpenAI path")
	}
	if _, ok := schema["properties"].(map[string]interface{})["sql"]; !ok {
		t.Error("properties.sql was dropped")
	}
}
