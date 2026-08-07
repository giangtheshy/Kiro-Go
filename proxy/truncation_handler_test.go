package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A turn cut off mid-generation must never be reported to the client as a clean
// finish. These tests pin the wire-level contract per protocol, because that
// signal is the only way an agentic caller can tell a partial answer from a
// complete one.

func TestClaudeStreamTruncatedWithholdsStopReason(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	// Content, then a clean close with no meteringEvent: the upstream dropped
	// the connection mid-turn.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	out, _ := io.ReadAll(rec.Body)
	got := string(out)

	if !strings.Contains(got, "partial answer") {
		t.Fatalf("text delivered before the cut must reach the client:\n%s", got)
	}
	// message_start always carries "stop_reason":null, so assert on the
	// message_delta frame specifically rather than substring-matching the body.
	if delta := sseFrameContaining(got, "message_delta"); strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("truncated turn must not report end_turn:\n%s", delta)
	}
	if !strings.Contains(got, "event: error") {
		t.Fatalf("expected an error event announcing the truncation:\n%s", got)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Fatalf("message_stop marks a finished turn and must be withheld:\n%s", got)
	}
}

// Guards the opposite direction: a properly metered turn keeps reporting a clean
// finish, so the truncation check cannot regress normal responses.
func TestClaudeStreamMeteredTurnStillReportsEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "complete answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	if delta := sseFrameContaining(got, "message_delta"); !strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("a metered turn must still report end_turn, got:\n%s", delta)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Fatalf("expected message_stop on a complete turn:\n%s", got)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("a complete turn must not emit an error event:\n%s", got)
	}
}

func TestResponsesStreamTruncatedEmitsFailedNotCompleted(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "half",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"go","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	got := rec.Body.String()
	if strings.Contains(got, "event: response.completed") {
		t.Fatalf("truncated turn must not emit response.completed:\n%s", got)
	}
	if !strings.Contains(got, "event: response.failed") {
		t.Fatalf("expected response.failed on a truncated turn:\n%s", got)
	}
}

// An empty stream (no output, no metering) means the turn never started, so the
// client has seen nothing and the SAME endpoint is retried. This matters most
// for API-key accounts, which resolve to exactly one endpoint — advancing to
// "the next" one would end the loop after a single attempt.
func TestEmptyStreamRetriesSameEndpoint(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// First attempt: nothing at all.
			return
		}
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second attempt worked",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	if got := atomic.LoadInt64(&calls); got < 2 {
		t.Fatalf("expected the empty stream to be retried, upstream called %d time(s)", got)
	}
	if !strings.Contains(rec.Body.String(), "second attempt worked") {
		t.Fatalf("expected the retry's content to reach the client:\n%s", rec.Body.String())
	}
}

// A permanently empty upstream must stop after the retry budget instead of
// spinning forever.
func TestEmptyStreamRetriesAreBounded(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	// maxEmptyStreamRetries retries on top of the initial attempt, per account
	// attempt. The exact total depends on account rotation; what must hold is
	// that it terminates and stays bounded.
	got := atomic.LoadInt64(&calls)
	if got < 2 {
		t.Fatalf("expected retries to happen, got %d call(s)", got)
	}
	if max := int64((maxEmptyStreamRetries + 1) * maxAccountRetryAttempts); got > max {
		t.Fatalf("retries not bounded: %d calls, expected at most %d", got, max)
	}
}

// sseFrameContaining returns the first SSE frame whose payload mentions marker,
// so assertions can target one event instead of the whole body.
func sseFrameContaining(body, marker string) string {
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.Contains(frame, marker) {
			return frame
		}
	}
	return ""
}
