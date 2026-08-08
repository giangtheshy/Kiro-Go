package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// End-to-end wire tests for the metadataEvent.stopReason fix. The helper-level
// tests prove the mapping; these prove the value actually survives the handler
// and lands in the bytes a client reads. They are the ones that would catch a
// callback that was never wired up.
//
// The scenario under test is the production symptom: the upstream cuts a turn at
// its OWN output ceiling and says so, while the output sits far below the
// client's max_tokens. Inference alone calls that a clean end_turn, so the client
// believes the answer finished and quietly stops mid-task.

// metadataStopReasonServer streams text, then a metadataEvent carrying reason,
// then meteringEvent so the turn counts as properly completed (not truncated).
func metadataStopReasonServer(t *testing.T, text, reason string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": text,
		}))
		if reason != "" {
			_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
				"stopReason": reason,
			}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
}

func TestClaudeStreamReportsUpstreamMaxTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "cut short by the server", "MAX_TOKENS")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	// max_tokens is deliberately huge relative to the output: this is exactly the
	// case where inference used to report a clean end_turn.
	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	delta := sseFrameContaining(got, "message_delta")
	if !strings.Contains(delta, `"stop_reason":"max_tokens"`) {
		t.Fatalf("upstream said MAX_TOKENS; message_delta must report max_tokens:\n%s", delta)
	}
	if strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("a server-cut turn must not report end_turn:\n%s", delta)
	}
	// The turn WAS billed, so it is complete-as-far-as-the-protocol-goes: the
	// client still needs the closing frame to act on the stop_reason.
	if !strings.Contains(got, "event: message_stop") {
		t.Fatalf("expected message_stop on a metered turn:\n%s", got)
	}
}

// The opposite direction: a genuine END_TURN must stay end_turn. This is the
// regression guard that the fix does not start labelling healthy turns truncated.
func TestClaudeStreamHonoursUpstreamEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "a complete answer", "END_TURN")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	delta := sseFrameContaining(got, "message_delta")
	if !strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("upstream said END_TURN; must report end_turn:\n%s", delta)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("a clean turn must not emit an error event:\n%s", got)
	}
}

// No metadataEvent at all — the pre-fix behaviour must be preserved exactly, so
// that upstreams (or endpoints) which never send the frame are unaffected.
func TestClaudeStreamWithoutMetadataFallsBackToInference(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "answer", "")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	delta := sseFrameContaining(rec.Body.String(), "message_delta")
	if !strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("no metadataEvent: inference should still say end_turn:\n%s", delta)
	}
}

func TestClaudeNonStreamReportsUpstreamMaxTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "cut short", "MAX_TOKENS")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	raw, _ := io.ReadAll(rec.Body)
	var resp struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	if resp.StopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens\nbody: %s", resp.StopReason, raw)
	}
}

func TestOpenAIStreamReportsUpstreamLength(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "cut short", "MAX_TOKENS")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIChat(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, `"finish_reason":"length"`) {
		t.Fatalf("upstream said MAX_TOKENS; expected finish_reason length:\n%s", got)
	}
	if strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("a server-cut turn must not report stop:\n%s", got)
	}
}

func TestOpenAIStreamHonoursUpstreamEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "complete", "END_TURN")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIChat(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("upstream said END_TURN; expected finish_reason stop:\n%s", got)
	}
	if strings.Contains(got, `"finish_reason":"length"`) {
		t.Fatalf("a clean turn must not report length:\n%s", got)
	}
}

func TestOpenAINonStreamReportsUpstreamLength(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "cut short", "MAX_TOKENS")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIChat(rec, req)

	raw, _ := io.ReadAll(rec.Body)
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response: %s", raw)
	}
	if resp.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length\nbody: %s", resp.Choices[0].FinishReason, raw)
	}
}

func TestOpenAINonStreamHonoursUpstreamEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "complete", "END_TURN")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIChat(rec, req)

	raw, _ := io.ReadAll(rec.Body)
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response: %s", raw)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop\nbody: %s", resp.Choices[0].FinishReason, raw)
	}
}

// The Responses API has no finish_reason: an early stop is status "incomplete"
// plus incomplete_details.reason, and the terminal SSE event must agree.
func TestResponsesStreamReportsIncompleteOnUpstreamMaxTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "cut short", "MAX_TOKENS")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"go","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, "event: response.incomplete") {
		t.Fatalf("expected response.incomplete on a server-cut turn:\n%s", got)
	}
	if strings.Contains(got, "event: response.completed") {
		t.Fatalf("a server-cut turn must not emit response.completed:\n%s", got)
	}
	if !strings.Contains(got, `"reason":"max_output_tokens"`) {
		t.Fatalf("expected incomplete_details.reason max_output_tokens:\n%s", got)
	}
	// A cut turn is NOT a transport failure, so it must not masquerade as one.
	if strings.Contains(got, "event: response.failed") {
		t.Fatalf("an orderly early stop is not a failure:\n%s", got)
	}
}

func TestResponsesStreamHonoursUpstreamEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "complete answer", "END_TURN")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"go","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, "event: response.completed") {
		t.Fatalf("a clean turn must report response.completed:\n%s", got)
	}
	if strings.Contains(got, "event: response.incomplete") {
		t.Fatalf("a clean turn must not report incomplete:\n%s", got)
	}
	if strings.Contains(got, "incomplete_details") {
		t.Fatalf("no incomplete_details on a clean turn:\n%s", got)
	}
}

func TestResponsesNonStreamReportsIncompleteOnUpstreamMaxTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "cut short", "MAX_TOKENS")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"go","store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	raw, _ := io.ReadAll(rec.Body)
	var resp struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	if resp.Status != "incomplete" {
		t.Fatalf("status = %q, want incomplete\nbody: %s", resp.Status, raw)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details = %+v, want reason max_output_tokens\nbody: %s", resp.IncompleteDetails, raw)
	}
}

func TestResponsesNonStreamHonoursUpstreamEndTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := metadataStopReasonServer(t, "complete answer", "END_TURN")
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"go","store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	raw, _ := io.ReadAll(rec.Body)
	var resp struct {
		Status            string      `json:"status"`
		IncompleteDetails interface{} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, raw)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed\nbody: %s", resp.Status, raw)
	}
	if resp.IncompleteDetails != nil {
		t.Fatalf("incomplete_details must be absent on a clean turn: %v", resp.IncompleteDetails)
	}
}

// A tool call is a complete turn: the model stopped to let the client run
// something, not because it ran out of room.
func TestResponsesToolCallIsNotIncomplete(t *testing.T) {
	if got := responsesIncompleteReason("MAX_TOKENS", true, 10, 32000); got != "" {
		t.Fatalf("reason = %q, want empty for a tool-call turn", got)
	}
	if got := responsesIncompleteReason("END_TURN", false, 10, 32000); got != "" {
		t.Fatalf("reason = %q, want empty for a clean turn", got)
	}
	if got := responsesIncompleteReason("MAX_TOKENS", false, 10, 32000); got != "max_output_tokens" {
		t.Fatalf("reason = %q, want max_output_tokens", got)
	}
	// Upstream silent, output at the cap: inference still catches it.
	if got := responsesIncompleteReason("", false, 4096, 4096); got != "max_output_tokens" {
		t.Fatalf("reason = %q, want max_output_tokens from inference", got)
	}
}

// Truncation (no meteringEvent) must keep winning over any stopReason the
// upstream managed to send first. A cut connection is a different, worse failure
// than a stated stop: the client must not receive a "finished" signal at all.
func TestTruncationStillOutranksUpstreamStopReason(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	// END_TURN arrives, then the connection dies WITHOUT metering.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "half an answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "END_TURN",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, "event: error") {
		t.Fatalf("a cut stream must still surface an error event:\n%s", got)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Fatalf("message_stop must stay withheld on a cut stream:\n%s", got)
	}
	if delta := sseFrameContaining(got, "message_delta"); strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("a cut stream must not report end_turn even though upstream sent it:\n%s", delta)
	}
}
