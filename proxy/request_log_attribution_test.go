package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// The admin API Log view reads ClientIP straight off the request log entry, so a
// handler that forgets to thread it through leaves the column permanently blank
// no matter how the proxy is deployed. These tests assert the value survives the
// whole path — resolve, thread, log — rather than just that the field exists.

// logEntryAfter runs fn and returns the newest request-log entry it produced.
func logEntryAfter(t *testing.T, fn func()) RequestLogEntry {
	t.Helper()
	requestLog.reset()
	fn()
	entries := requestLog.snapshot()
	if len(entries) == 0 {
		t.Fatal("expected a request log entry to be recorded")
	}
	return entries[0]
}

func TestClaudeStreamRecordsClientIPAndCacheTokens(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "hello",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	entry := logEntryAfter(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "203.0.113.9:51000"
		h.handleClaudeMessages(httptest.NewRecorder(), req)
	})

	if entry.ClientIP != "203.0.113.9" {
		t.Fatalf("client IP must reach the request log, got %q", entry.ClientIP)
	}
	if entry.Status != "ok" {
		t.Fatalf("expected a success entry, got %q (%s)", entry.Status, entry.Error)
	}
	// In/Out are shown as separate columns now, so both have to be populated
	// independently — a single Total would hide an empty half.
	if entry.InputTokens <= 0 || entry.OutputTokens <= 0 {
		t.Fatalf("expected both token sides populated, got in=%d out=%d", entry.InputTokens, entry.OutputTokens)
	}
	if entry.TotalTokens != entry.InputTokens+entry.OutputTokens {
		t.Fatalf("total must stay consistent with the split: %d != %d+%d",
			entry.TotalTokens, entry.InputTokens, entry.OutputTokens)
	}
}

// A failing turn is the case operators most need the IP for, so it must be
// recorded on the error path too — not only on success.
func TestClaudeStreamRecordsClientIPOnFailure(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	entry := logEntryAfter(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "198.51.100.4:40000"
		h.handleClaudeMessages(httptest.NewRecorder(), req)
	})

	if entry.Status != "error" {
		t.Fatalf("expected a failure entry, got %q", entry.Status)
	}
	if entry.ClientIP != "198.51.100.4" {
		t.Fatalf("client IP must be recorded on failures too, got %q", entry.ClientIP)
	}
}

// KIRO_TRUST_PROXY deployments read the IP out of X-Forwarded-For. The guard
// already resolves it; this pins that the resolved value (not RemoteAddr) is
// what lands in the log.
func TestRequestLogUsesForwardedIPWhenProxyTrusted(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()
	h.guard = testGuard(dosGuardConfig{TrustProxy: true})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	entry := logEntryAfter(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.77")
		h.handleClaudeMessages(httptest.NewRecorder(), req)
	})

	if entry.ClientIP != "203.0.113.77" {
		t.Fatalf("expected the forwarded IP, got %q", entry.ClientIP)
	}
}

func TestOpenAIStreamRecordsClientIP(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}))
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	entry := logEntryAfter(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.RemoteAddr = "203.0.113.21:9000"
		h.handleOpenAIChat(httptest.NewRecorder(), req)
	})

	if entry.ClientIP != "203.0.113.21" {
		t.Fatalf("openai path must record the client IP, got %q", entry.ClientIP)
	}
}

// Cache columns are informational: adding them must not change what the customer
// is billed, so Input stays the full input side and Total stays In+Out.
func TestCacheTokensAreReportedWithoutChangingTotals(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	entry := logEntryAfter(t, func() {
		h.recordSuccessForApiKey("", requestUsage{
			Input:      1000,
			Output:     200,
			CacheRead:  700,
			CacheWrite: 100,
			Credits:    0.5,
			ClientIP:   "203.0.113.5",
		}, "claude-test", nil, "claude", time.Time{})
	})

	if entry.CacheReadTokens != 700 || entry.CacheWriteTokens != 100 {
		t.Fatalf("cache tokens must be recorded, got read=%d write=%d",
			entry.CacheReadTokens, entry.CacheWriteTokens)
	}
	if entry.InputTokens != 1000 {
		t.Fatalf("input must stay the full input side, got %d", entry.InputTokens)
	}
	if entry.TotalTokens != 1200 {
		t.Fatalf("total must remain input+output regardless of cache, got %d", entry.TotalTokens)
	}
}

// The admin table needs a breakdown that adds up. UncachedInputTokens is the
// fresh part of the prompt, so the four token columns are disjoint and sum to
// Total — without disturbing InputTokens, which the quota and /check both read.
func TestUncachedInputTokensCompleteTheBreakdown(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	entry := logEntryAfter(t, func() {
		h.recordSuccessForApiKey("", requestUsage{
			Input:      1000,
			Output:     200,
			CacheRead:  700,
			CacheWrite: 100,
		}, "claude-test", nil, "claude", time.Time{})
	})

	if entry.UncachedInputTokens != 200 {
		t.Fatalf("uncached input must be input-cacheRead-cacheWrite, got %d", entry.UncachedInputTokens)
	}
	// The whole point of the field: these four columns must reconcile.
	sum := entry.UncachedInputTokens + entry.CacheReadTokens + entry.CacheWriteTokens + entry.OutputTokens
	if sum != entry.TotalTokens {
		t.Fatalf("breakdown must sum to total: %d != %d", sum, entry.TotalTokens)
	}
	// InputTokens stays the raw prompt size so quota accounting is untouched.
	if entry.InputTokens != 1000 {
		t.Fatalf("input must stay the full prompt, got %d", entry.InputTokens)
	}
}

// Cache counts are estimated on some paths, so they could exceed the recorded
// prompt size. That must clamp to zero rather than render a negative column.
func TestUncachedInputTokensNeverGoNegative(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	entry := logEntryAfter(t, func() {
		h.recordSuccessForApiKey("", requestUsage{
			Input:      100,
			Output:     10,
			CacheRead:  900,
			CacheWrite: 50,
		}, "claude-test", nil, "claude", time.Time{})
	})

	if entry.UncachedInputTokens != 0 {
		t.Fatalf("uncached input must clamp at zero, got %d", entry.UncachedInputTokens)
	}
}

// A request with no cache activity must report the whole prompt as uncached, so
// the In column is unchanged for the OpenAI/Responses paths that never track cache.
func TestUncachedInputEqualsInputWithoutCache(t *testing.T) {
	mustInitConfig(t)
	h := &Handler{}

	entry := logEntryAfter(t, func() {
		h.recordSuccessForApiKey("", requestUsage{Input: 500, Output: 40},
			"claude-test", nil, "claude", time.Time{})
	})

	if entry.UncachedInputTokens != 500 {
		t.Fatalf("with no cache traffic uncached input must equal input, got %d", entry.UncachedInputTokens)
	}
}

// ClientIP is admin-only. The customer-facing self-service log must not carry it
// even though it sits on the same underlying entry.
func TestSelfServiceLogsOmitClientIP(t *testing.T) {
	mustInitConfig(t)
	created, err := config.AddApiKey(config.ApiKeyEntry{Name: "cust", Key: "sk-cust", Enabled: true})
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}

	h := &Handler{}
	requestLog.reset()
	h.recordSuccessForApiKey(created.ID, requestUsage{
		Input:    10,
		Output:   5,
		ClientIP: "203.0.113.200",
	}, "claude-test", nil, "claude", time.Time{})

	req := httptest.NewRequest(http.MethodGet, "/v1/key/logs", nil)
	req.Header.Set("X-Api-Key", "sk-cust")
	rec := httptest.NewRecorder()
	h.apiKeySelfLogs(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "claude-test") {
		t.Fatalf("expected the entry to be returned to its owner:\n%s", body)
	}
	if strings.Contains(body, "203.0.113.200") || strings.Contains(body, "clientIp") {
		t.Fatalf("client IP must not leak to the key owner:\n%s", body)
	}
}

// The admin breakdown was added by deriving a new field, deliberately leaving
// InputTokens alone, because the customer-facing /check view reports it directly.
// This pins that: the self-service row keeps showing the full prompt size, and the
// admin-only uncached field does not leak into it.
func TestSelfServiceInputTokensStayTheFullPrompt(t *testing.T) {
	mustInitConfig(t)
	created, err := config.AddApiKey(config.ApiKeyEntry{Name: "cust2", Key: "sk-cust2", Enabled: true})
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}

	h := &Handler{}
	requestLog.reset()
	h.recordSuccessForApiKey(created.ID, requestUsage{
		Input:      1000,
		Output:     200,
		CacheRead:  700,
		CacheWrite: 100,
	}, "claude-test", nil, "claude", time.Time{})

	views := h.usageLogsForKey(created.ID, 10)
	if len(views) != 1 {
		t.Fatalf("expected exactly one row for the key, got %d", len(views))
	}
	if views[0].InputTokens != 1000 {
		t.Fatalf("/check must keep reporting the full prompt size, got %d", views[0].InputTokens)
	}
	if views[0].OutputTokens != 200 {
		t.Fatalf("/check output tokens changed, got %d", views[0].OutputTokens)
	}

	raw, err := json.Marshal(views[0])
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	if strings.Contains(string(raw), "uncachedInputTokens") {
		t.Fatalf("the admin-only uncached field must not reach /check:\n%s", raw)
	}
}
