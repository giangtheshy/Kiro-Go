package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A turn cut off mid-generation is resumed rather than reported broken, so the
// client receives one continuous answer. These tests pin that the resume actually
// happens, that it does not duplicate text at the seam, and that it still fails
// honestly when resuming cannot recover the turn.

// longEnoughToResume exceeds minContinuationTextBytes, which is what makes a cut
// eligible for a resume at all. Shorter text is deliberately not resumed.
const longEnoughToResume = "Here is the first half of a real answer that got cut off"

func TestClaudeStreamResumesTruncatedTurn(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// Content, then a clean close with no meteringEvent: cut mid-turn.
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content": longEnoughToResume,
			}))
			return
		}
		// The resume delivers the rest and bills the turn.
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": " and this is the second half.",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", body))

	got := rec.Body.String()

	if n := atomic.LoadInt64(&calls); n < 2 {
		t.Fatalf("expected a resume request, upstream called %d time(s)", n)
	}
	if !strings.Contains(got, "second half") {
		t.Fatalf("the resumed text must reach the client:\n%s", got)
	}
	// The whole point: a resumed turn is indistinguishable from one that never
	// broke, so it reports a clean finish.
	if delta := sseFrameContaining(got, "message_delta"); !strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("a successfully resumed turn must report end_turn, got:\n%s", delta)
	}
	if !strings.Contains(got, "event: message_stop") {
		t.Fatalf("expected message_stop after a successful resume:\n%s", got)
	}
	if strings.Contains(got, "event: error") {
		t.Fatalf("a recovered turn must not surface an error to the client:\n%s", got)
	}
}

// A model handed its own partial reply often restates the last few words before
// carrying on. That overlap must be trimmed, or the client sees stuttered text.
func TestClaudeStreamResumeTrimsRepeatedText(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content": longEnoughToResume,
			}))
			return
		}
		// Repeats "got cut off" before continuing.
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "got cut off, then continued cleanly.",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", body))

	got := rec.Body.String()
	if strings.Count(got, "got cut off") != 1 {
		t.Fatalf("the repeated seam text must appear exactly once:\n%s", got)
	}
	if !strings.Contains(got, "then continued cleanly") {
		t.Fatalf("text past the overlap must survive:\n%s", got)
	}
}

// When the resume is ALSO cut short, the turn is unrecoverable and must be
// reported as truncated — the resume path must not mask a broken turn.
func TestClaudeStreamResumeExhaustedStillReportsTruncation(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		// Every attempt: content, never metered.
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": longEnoughToResume,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", body))

	got := rec.Body.String()
	if delta := sseFrameContaining(got, "message_delta"); strings.Contains(delta, `"stop_reason"`) {
		t.Fatalf("an unrecoverable turn must withhold stop_reason, got:\n%s", delta)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Fatalf("message_stop marks a finished turn and must stay withheld:\n%s", got)
	}
	if !strings.Contains(got, "event: error") {
		t.Fatalf("expected an error event once resuming is exhausted:\n%s", got)
	}
	// Bounded: the initial attempt plus at most maxContinuationAttempts resumes,
	// per account attempt. What must hold is that it terminates.
	if n := atomic.LoadInt64(&calls); n > int64((1+maxContinuationAttempts)*maxAccountRetryAttempts) {
		t.Fatalf("resume attempts are not bounded: %d upstream calls", n)
	}
}

// Below minContinuationTextBytes there is too little anchor for "continue where
// you stopped" to work, so the cut is reported instead of resumed.
func TestShortTruncationIsNotResumed(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "hi", // well under the threshold
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", body))

	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("a too-short cut must be reported, not resumed:\n%s", rec.Body.String())
	}
}

func TestTrimResumeOverlap(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		want       string
	}{
		{"no overlap", "abc", "def", "def"},
		{"full repeat of the tail", "hello world", "world, and more", ", and more"},
		{"entire chunk is a repeat", "hello world", "world", ""},
		{"empty prev", "", "abc", "abc"},
		{"empty next", "abc", "", ""},
		// The reason the scan is rune-aware: cutting mid-character would emit
		// mojibake for any non-ASCII language.
		{"vietnamese diacritics", "câu trả lời", "lời tiếp tục", " tiếp tục"},
		{"cjk", "你好世界", "世界和平", "和平"},
		// Longest overlap wins, otherwise a shorter accidental match would leave
		// duplicated text behind.
		{"prefers the longest overlap", "aaa", "aaab", "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimResumeOverlap(tc.prev, tc.next); got != tc.want {
				t.Fatalf("trimResumeOverlap(%q, %q) = %q, want %q", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// overlapAnchor must never split a multi-byte character, or the comparison
// window itself would start with an invalid rune.
func TestOverlapAnchorStaysRuneAligned(t *testing.T) {
	long := strings.Repeat("việt", 500) // multi-byte, longer than the anchor window
	anchor := overlapAnchor(long)

	if len(anchor) > maxOverlapAnchorBytes {
		t.Fatalf("anchor exceeds the window: %d bytes", len(anchor))
	}
	if !strings.HasSuffix(long, anchor) {
		t.Fatal("anchor must be a suffix of the delivered text")
	}
	for i, r := range anchor {
		if r == '�' {
			t.Fatalf("anchor starts mid-character at byte %d: %q", i, anchor)
		}
		break
	}
}

// The retry loop reuses the original payload across accounts, so building a
// resume must not mutate it — otherwise every later attempt would carry a
// resume turn appended to it.
func TestBuildContinuationPayloadDoesNotMutateOriginal(t *testing.T) {
	original := &KiroPayload{}
	original.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "original question",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
		UserInputMessageContext: &UserInputMessageContext{
			Tools:       []KiroToolWrapper{{}},
			ToolResults: []KiroToolResult{{ToolUseID: "tool_1"}},
		},
	}
	historyBefore := len(original.ConversationState.History)

	resume := buildContinuationPayload(original, "the delivered half")
	if resume == nil {
		t.Fatal("expected a resume payload")
	}

	if len(original.ConversationState.History) != historyBefore {
		t.Fatalf("original history was mutated: %d -> %d",
			historyBefore, len(original.ConversationState.History))
	}
	if original.ConversationState.CurrentMessage.UserInputMessage.Content != "original question" {
		t.Fatal("original current message was overwritten")
	}

	// The interrupted exchange moves into history as user turn + partial reply.
	hist := resume.ConversationState.History
	if len(hist) != historyBefore+2 {
		t.Fatalf("expected two appended history entries, got %d", len(hist))
	}
	last := hist[len(hist)-1]
	if last.AssistantResponseMessage == nil || last.AssistantResponseMessage.Content != "the delivered half" {
		t.Fatalf("delivered text must be replayed as the assistant's partial reply, got %+v", last)
	}

	cur := resume.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "cut off mid-stream") {
		t.Fatalf("resume turn must carry the continuation instruction, got %q", cur.Content)
	}
	// Tool specs stay so the model can still issue the call it was cut off
	// before; toolResults must not, or the upstream sees two active tool turns.
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) != 1 {
		t.Fatalf("tool specs must be preserved on the resume turn, got %+v", cur.UserInputMessageContext)
	}
	if len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Fatal("toolResults belong to the turn now in history and must be dropped")
	}
}
