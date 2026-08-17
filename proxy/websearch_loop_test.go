package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// mixedToolsBody is a request carrying native web_search alongside a client tool,
// which is what routes to the agentic loop.
const mixedToolsBody = `{
  "model":"claude-sonnet-4.5",
  "max_tokens":64,
  "stream":true,
  "tools":[
    {"name":"web_search","type":"web_search_20250305"},
    {"name":"bash","description":"run a command","input_schema":{"type":"object"}}
  ],
  "messages":[{"role":"user","content":"look this up then run it"}]
}`

// A round cut off mid-turn carries a PARTIAL tool_use set. If the loop judged it,
// a stream that died before the web_search tool_use arrived would look like "the
// model decided not to search" and the half answer would flush as a clean turn.
func TestWebSearchLoopTruncatedRoundDoesNotReportCleanTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		// Content, then a clean close with no meteringEvent: cut mid-turn.
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "let me look that up",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(mixedToolsBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, "let me look that up") {
		t.Fatalf("text produced before the cut must still reach the client:\n%s", got)
	}
	if delta := sseFrameContaining(got, "message_delta"); strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("a truncated round must not report end_turn:\n%s", delta)
	}
	if !strings.Contains(got, "event: error") {
		t.Fatalf("expected an error event announcing the truncation:\n%s", got)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Fatalf("message_stop marks a finished turn and must be withheld:\n%s", got)
	}
}

// The truncated round must not be folded into history either: later rounds would
// otherwise build on a half-written assistant turn. Since the loop bails out
// immediately, the upstream is called exactly once.
func TestWebSearchLoopTruncatedRoundStopsTheLoop(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(mixedToolsBody))
	h.handleClaudeMessages(httptest.NewRecorder(), req)

	// One round only. A second call would mean the truncated round was treated
	// as a legitimate search turn and fed back into the conversation.
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected the loop to stop after the truncated round, upstream called %d times", got)
	}
}

// A complete round with no tool calls is an ordinary answer and must still flush
// as a finished turn — guards against the truncation check over-firing.
func TestWebSearchLoopCompleteRoundReportsEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "no search needed, here is the answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(mixedToolsBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, "here is the answer") {
		t.Fatalf("expected the answer to reach the client:\n%s", got)
	}
	if delta := sseFrameContaining(got, "message_delta"); !strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("a complete round must report end_turn, got:\n%s", delta)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Fatalf("expected message_stop on a complete turn:\n%s", got)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("a complete turn must not emit an error event:\n%s", got)
	}
}

// A client tool call has to reach the client as a normal tool_use, with
// stop_reason tool_use so the client knows to run it and call back.
func TestWebSearchLoopForwardsClientToolCall(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_01BashAAAAAAAAAAAAAAAAAA",
			"name":      "bash",
			"input":     `{"cmd":"ls"}`,
			"stop":      true,
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(mixedToolsBody))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, "toolu_01BashAAAAAAAAAAAAAAAAAA") {
		t.Fatalf("the client's tool call must reach the client:\n%s", got)
	}
	if delta := sseFrameContaining(got, "message_delta"); !strings.Contains(delta, `"stop_reason":"tool_use"`) {
		t.Fatalf("a client tool call requires stop_reason tool_use, got:\n%s", delta)
	}
}
