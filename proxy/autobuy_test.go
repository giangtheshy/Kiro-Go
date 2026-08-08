package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

// newAutoBuyHandler wires just enough of a Handler for the auto-buy paths: a live
// pool (the healthy-account trigger reads it) and a stop channel (purchaseWithRetry
// selects on it).
func newAutoBuyHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	return &Handler{
		pool:        pool.GetPool(),
		stopAutoBuy: make(chan struct{}),
	}
}

func autoBuyTestConfig() *config.AutoBuyConfig {
	return &config.AutoBuyConfig{
		Enabled:       true,
		MarketApiKey:  "usr-test",
		WebhookSecret: "s3cret",
		Zones: map[string]*config.AutoBuyZoneRule{
			config.AutoBuyZoneUS: {Enabled: true, BuyCount: 5, MaxUnitPrice: 25},
			config.AutoBuyZoneEU: {Enabled: true, BuyCount: 3, MaxUnitPrice: 10},
		},
	}
}

// testCtx returns a context cancelled when the test finishes.
//
// Hand-rolled rather than testCtx(t): that helper landed in Go 1.24 and this
// module targets 1.21, so using it breaks the build for anyone on the declared
// toolchain.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// signMarketBody produces the header value the market would send.
func signMarketBody(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// --- signature verification ---

func TestVerifyMarketSignatureAcceptsAGenuineSignature(t *testing.T) {
	body := []byte(`{"event":"new_keys_available","zone":"us"}`)
	ts := "1786215036"
	sig := signMarketBody("s3cret", ts, body)

	if !verifyMarketSignature("s3cret", ts, sig, body) {
		t.Fatal("a correctly signed body should verify")
	}
}

func TestVerifyMarketSignatureRejectsTampering(t *testing.T) {
	body := []byte(`{"event":"new_keys_available","zone":"us","new_keys":20}`)
	ts := "1786215036"
	sig := signMarketBody("s3cret", ts, body)

	cases := []struct {
		name      string
		secret    string
		timestamp string
		signature string
		body      []byte
	}{
		{"wrong secret", "other", ts, sig, body},
		{"empty secret", "", ts, sig, body},
		{"empty signature", "s3cret", ts, "", body},
		{"body modified after signing", "s3cret", ts, sig, []byte(`{"event":"new_keys_available","zone":"eu","new_keys":200}`)},
		{"timestamp swapped", "s3cret", "1786215999", sig, body},
		{"signature without the sha256 prefix", "s3cret", ts, strings.TrimPrefix(sig, "sha256="), body},
		{"garbage signature", "s3cret", ts, "sha256=deadbeef", body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if verifyMarketSignature(tc.secret, tc.timestamp, tc.signature, tc.body) {
				t.Fatal("expected verification to fail")
			}
		})
	}
}

// The signature covers the raw bytes. Re-serialising JSON reorders keys and
// rewrites whitespace, so a handler that parsed first would fail every delivery.
func TestVerifyMarketSignatureIsByteExact(t *testing.T) {
	original := []byte(`{"event":"webhook_test","zone":"us"}`)
	ts := "1786215036"
	sig := signMarketBody("s3cret", ts, original)

	var parsed map[string]any
	if err := json.Unmarshal(original, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reserialised, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !verifyMarketSignature("s3cret", ts, sig, original) {
		t.Fatal("raw bytes should verify")
	}
	if string(reserialised) != string(original) && verifyMarketSignature("s3cret", ts, sig, reserialised) {
		t.Fatal("re-serialised bytes must not verify; the digest covers the original")
	}
}

func TestTimestampWithinSkew(t *testing.T) {
	now := time.Unix(1786215036, 0)
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"exactly now", "1786215036", true},
		{"four minutes old", strconv.FormatInt(now.Add(-4*time.Minute).Unix(), 10), true},
		{"six minutes old is stale", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), false},
		{"six minutes in the future", strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10), false},
		{"not a number", "yesterday", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timestampWithinSkew(tc.raw, now, autoBuyWebhookSkew); got != tc.want {
				t.Fatalf("timestampWithinSkew(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// --- webhook endpoint ---

// postWebhook issues a signed request against the receiver.
func postWebhook(t *testing.T, h *Handler, secret string, body []byte, tweak func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, autoBuyWebhookPath, strings.NewReader(string(body)))
	req.Header.Set("X-KM-Timestamp", ts)
	req.Header.Set("X-KM-Signature", signMarketBody(secret, ts, body))
	if tweak != nil {
		tweak(req)
	}
	rec := httptest.NewRecorder()
	h.handleAutoBuyWebhook(rec, req)
	return rec
}

// An unconfigured secret means the receiver cannot authenticate anyone, so it must
// refuse rather than let whoever found the URL spend the operator's credits.
func TestWebhookRefusesWhenNoSecretIsConfigured(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Enabled = false
	cfg.WebhookSecret = ""
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	rec := postWebhook(t, h, "anything", []byte(`{"event":"webhook_test"}`), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when no secret is configured, got %d", rec.Code)
	}
}

func TestWebhookRejectsBadSignatureAndStaleTimestamp(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	body := []byte(`{"event":"webhook_test","event_id":"e1"}`)

	t.Run("wrong secret", func(t *testing.T) {
		rec := postWebhook(t, h, "not-the-secret", body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		stale := strconv.FormatInt(time.Now().Add(-30*time.Minute).Unix(), 10)
		rec := postWebhook(t, h, "s3cret", body, func(r *http.Request) {
			r.Header.Set("X-KM-Timestamp", stale)
			r.Header.Set("X-KM-Signature", signMarketBody("s3cret", stale, body))
		})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 for a correctly signed but stale request, got %d", rec.Code)
		}
	})
}

func TestWebhookRejectsNonPost(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, autoBuyWebhookPath, nil)
	rec := httptest.NewRecorder()
	h.handleAutoBuyWebhook(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestWebhookAcceptsVerifiedTestEvent(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	rec := postWebhook(t, h, "s3cret", []byte(`{"event":"webhook_test"}`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "ok" {
		t.Fatalf("want status ok, got %q", out["status"])
	}
}

// The market retries a delivery up to three times with the same event_id. Only
// the first may act.
func TestWebhookDeduplicatesByEventID(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	// Disable both zones so the purchase path short-circuits and the test only
	// exercises dedupe, without reaching for the network.
	cfg.Zones[config.AutoBuyZoneUS].Enabled = false
	cfg.Zones[config.AutoBuyZoneEU].Enabled = false
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	body := []byte(`{"event":"new_keys_available","event_id":"evt-42","zone":"us","new_keys":20}`)

	first := postWebhook(t, h, "s3cret", body, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery: want 200, got %d", first.Code)
	}
	var out map[string]string
	json.Unmarshal(first.Body.Bytes(), &out)
	if out["status"] != "accepted" {
		t.Fatalf("first delivery should be accepted, got %q", out["status"])
	}

	for attempt := 2; attempt <= 3; attempt++ {
		rec := postWebhook(t, h, "s3cret", body, nil)
		// Still 200: a non-2xx would ask the market to retry an event that was
		// understood and deliberately ignored.
		if rec.Code != http.StatusOK {
			t.Fatalf("redelivery %d: want 200, got %d", attempt, rec.Code)
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		if out["status"] != "duplicate" {
			t.Fatalf("redelivery %d should report duplicate, got %q", attempt, out["status"])
		}
	}
}

// A test event is exempt from dedupe: an operator pressing Test twice expects two
// confirmations, not silence.
func TestWebhookTestEventIsNotDeduplicated(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	body := []byte(`{"event":"webhook_test","event_id":"same-id"}`)

	for i := 0; i < 2; i++ {
		rec := postWebhook(t, h, "s3cret", body, nil)
		var out map[string]string
		json.Unmarshal(rec.Body.Bytes(), &out)
		if out["status"] != "ok" {
			t.Fatalf("attempt %d: want ok, got %q", i+1, out["status"])
		}
	}
}

// A signed body that does not parse is accepted, not retried: redelivering it
// would reproduce the same parse failure three times.
func TestWebhookAcceptsSignedButUnparseableBody(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	rec := postWebhook(t, h, "s3cret", []byte(`{not json`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 so the market stops retrying, got %d", rec.Code)
	}
}

func TestWebhookFallsBackToHeadersForEventAndID(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	// Body carries no event/event_id; the headers do.
	body := []byte(`{"zone":"us"}`)
	rec := postWebhook(t, h, "s3cret", body, func(r *http.Request) {
		r.Header.Set("X-KM-Event", "webhook_test")
		r.Header.Set("X-KM-Event-Id", "hdr-1")
	})
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "ok" {
		t.Fatalf("header-sourced event should be handled, got %q", out["status"])
	}
}

// all_keys_dead and warranty_refund are informational. They must be acknowledged,
// not retried, and must not trigger a purchase.
func TestWebhookAcknowledgesInformationalEvents(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	cases := []struct {
		event string
		want  string
	}{
		{"all_keys_dead", "noted"},
		{"warranty_refund", "noted"},
		{"something_new", "ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			body := []byte(`{"event":"` + tc.event + `","event_id":"id-` + tc.event + `","round_id":"r1"}`)
			rec := postWebhook(t, h, "s3cret", body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d", rec.Code)
			}
			var out map[string]string
			json.Unmarshal(rec.Body.Bytes(), &out)
			if out["status"] != tc.want {
				t.Fatalf("want status %q, got %q", tc.want, out["status"])
			}
		})
	}
}

// --- idempotency keys ---

func TestClientOrderIDFormat(t *testing.T) {
	id, err := newClientOrderID()
	if err != nil {
		t.Fatalf("newClientOrderID: %v", err)
	}
	if len(id) != 32 {
		t.Fatalf("want 32 characters, got %d (%q)", len(id), id)
	}
	if !isValidClientOrderID(id) {
		t.Fatalf("generated id should validate: %q", id)
	}

	// Two calls must differ, or a second order would replay the first's result.
	other, err := newClientOrderID()
	if err != nil {
		t.Fatalf("newClientOrderID: %v", err)
	}
	if id == other {
		t.Fatal("generated ids must be unique")
	}
}

func TestIsValidClientOrderIDRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"too-short",
		"0a1b2c3d4e5f60718293a4b5c6d7e8f",   // 31 chars
		"0a1b2c3d4e5f60718293a4b5c6d7e8f99", // 33 chars
		"0a1b2c3d4e5f60718293a4b5c6d7e8fZZ", // non-hex
		"ghijklmnopqrstuvwxyz012345678901",  // non-hex letters
	} {
		if isValidClientOrderID(bad) {
			t.Fatalf("%q should be rejected before the round trip", bad)
		}
	}
}

// --- error classification ---

// The market docs say plainly that some failures are not worth retrying. Getting
// this wrong turns one bad night into thousands of requests against a
// rate-limited API.
func TestMarketErrorRetryClassification(t *testing.T) {
	cases := []struct {
		code      string
		status    int
		retryable bool
		terminal  bool
	}{
		{marketCodeRetrySameOrder, 409, true, false},
		{marketCodeNoStock, 409, true, false},
		{marketCodeRateLimited, 429, true, false},
		{marketCodeCapReached, 409, false, true},
		{marketCodeNoBalance, 402, false, true},
		{marketCodeInvalidKey, 401, false, true},
		{marketCodeUnauthed, 401, false, true},
		{marketCodeDisabled, 403, false, true},
		{marketCodeBadZone, 400, false, true},
		{marketCodeBadCount, 400, false, true},
		{marketCodeBadOrderID, 400, false, false},
		{marketCodeSessionOnly, 403, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			e := &marketError{Status: tc.status, Code: tc.code}
			if got := e.Retryable(); got != tc.retryable {
				t.Fatalf("Retryable() = %v, want %v", got, tc.retryable)
			}
			if got := e.Terminal(); got != tc.terminal {
				t.Fatalf("Terminal() = %v, want %v", got, tc.terminal)
			}
		})
	}
}

func TestMarketErrorRetriesTransportAndServerFailures(t *testing.T) {
	// Status 0 means the request never reached the server.
	if e := (&marketError{Status: 0, Msg: "dial tcp: timeout"}); !e.Retryable() {
		t.Fatal("a transport failure should be retryable")
	}
	if e := (&marketError{Status: 500}); !e.Retryable() {
		t.Fatal("a 500 should be retryable")
	}
	if e := (&marketError{Status: 502, Code: "quota_failed"}); !e.Retryable() {
		t.Fatal("a 502 should be retryable")
	}
	// An unrecognised 4xx is a client mistake; repeating it reproduces it.
	if e := (&marketError{Status: 418, Code: "teapot"}); e.Retryable() {
		t.Fatal("an unknown 4xx should not be retried")
	}
}

func TestParseMarketErrorReadsCodeAndRetryAfter(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"45"}},
	}
	e := parseMarketError(resp, []byte(`{"code":"rate_limited","message":"slow down"}`))
	if e.Code != marketCodeRateLimited {
		t.Fatalf("want code rate_limited, got %q", e.Code)
	}
	if e.RetryAfter != 45*time.Second {
		t.Fatalf("want RetryAfter 45s, got %s", e.RetryAfter)
	}
	if e.Msg != "slow down" {
		t.Fatalf("want the upstream message, got %q", e.Msg)
	}
}

// A 429 with no code still needs one so callers can branch on it.
func TestParseMarketErrorDefaultsRateLimitCode(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	e := parseMarketError(resp, []byte(`{}`))
	if e.Code != marketCodeRateLimited {
		t.Fatalf("a 429 without a code should default to rate_limited, got %q", e.Code)
	}
}

// A wrong base URL returns an HTML error page. It must be truncated, not pasted
// whole into the log and the admin panel.
func TestParseMarketErrorTruncatesNonJSONBodies(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}}
	long := strings.Repeat("<html>nginx error page</html>", 200)
	e := parseMarketError(resp, []byte(long))
	if len(e.Msg) > 210 {
		t.Fatalf("message should be truncated, got %d characters", len(e.Msg))
	}
	if !strings.HasSuffix(e.Msg, "…") {
		t.Fatalf("truncated message should be marked with an ellipsis, got %q", e.Msg)
	}
}

func TestMarketErrorReadsAliasErrorField(t *testing.T) {
	// The docs state error is an alias for message; either may carry the text.
	resp := &http.Response{StatusCode: http.StatusConflict, Header: http.Header{}}
	e := parseMarketError(resp, []byte(`{"code":"no_stock","error":"nothing left"}`))
	if e.Msg != "nothing left" {
		t.Fatalf("want the aliased error text, got %q", e.Msg)
	}
}

func TestMarketErrorCodeHelper(t *testing.T) {
	if got := marketErrorCode(&marketError{Code: "no_stock"}); got != "no_stock" {
		t.Fatalf("got %q", got)
	}
	if got := marketErrorCode(http.ErrNotSupported); got != "" {
		t.Fatalf("a non-market error should yield an empty code, got %q", got)
	}
}

// --- guards ---

func TestLocalGuardsRejectsDisabledFeature(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Enabled = false

	s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now())
	if s == nil {
		t.Fatal("a disabled feature must not buy")
	}
}

// The schedule gates the webhook too. If it did not, the window would be
// decorative: webhooks are the main buying path.
func TestLocalGuardsEnforcesTheWindowOnEveryTrigger(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.ScheduleEnabled = true
	cfg.WindowStart = "22:00"
	cfg.WindowEnd = "23:00"

	outside := time.Date(2026, 8, 7, 12, 0, 0, 0, time.Local)
	for _, trigger := range []string{autoBuyTriggerPoll, autoBuyTriggerWebhook, autoBuyTriggerManual} {
		t.Run(trigger, func(t *testing.T) {
			if s := h.localGuards(cfg, config.AutoBuyZoneUS, trigger, outside); s == nil {
				t.Fatalf("%s must be refused outside the window", trigger)
			}
		})
	}

	inside := time.Date(2026, 8, 7, 22, 30, 0, 0, time.Local)
	if s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerWebhook, inside); s != nil {
		t.Fatalf("inside the window should pass, got %q", s.Reason)
	}
}

func TestLocalGuardsEnforcesDailyCreditCeiling(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MaxCreditsPerDay = 100
	cfg.SpentToday = 100

	s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now())
	if s == nil {
		t.Fatal("an exhausted daily ceiling must stop the buy")
	}
	if !strings.Contains(s.Reason, "daily credit ceiling") {
		t.Fatalf("reason should name the ceiling, got %q", s.Reason)
	}
}

func TestLocalGuardsEnforcesPerZoneDailyKeyLimit(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Zones[config.AutoBuyZoneUS].MaxKeysPerDay = 10
	cfg.Zones[config.AutoBuyZoneUS].BoughtToday = 10

	if s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now()); s == nil {
		t.Fatal("a zone at its daily key limit must be refused")
	}
	// The other zone has its own budget and must be unaffected.
	if s := h.localGuards(cfg, config.AutoBuyZoneEU, autoBuyTriggerPoll, time.Now()); s != nil {
		t.Fatalf("eu should be unaffected by the us limit, got %q", s.Reason)
	}
}

func TestLocalGuardsSkipsDisabledZone(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Zones[config.AutoBuyZoneEU].Enabled = false

	if s := h.localGuards(cfg, config.AutoBuyZoneEU, autoBuyTriggerPoll, time.Now()); s == nil {
		t.Fatal("a disabled zone must be refused")
	}
}

func TestLocalGuardsRejectsUnknownZone(t *testing.T) {
	h := newAutoBuyHandler(t)
	if s := h.localGuards(autoBuyTestConfig(), "apac", autoBuyTriggerPoll, time.Now()); s == nil {
		t.Fatal("a zone with no rule must be refused")
	}
}

// The trigger is "not enough usable accounts", so a healthy pool must suppress it.
func TestLocalGuardsSkipsWhenThePoolIsAlreadyHealthy(t *testing.T) {
	h := newAutoBuyHandler(t)
	for _, id := range []string{"a1", "a2", "a3"} {
		if err := config.AddAccount(config.Account{
			ID: id, Enabled: true, AccessToken: "tok-" + id,
		}); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}
	h.pool.Reload()

	cfg := autoBuyTestConfig()
	cfg.MinHealthyAccounts = 2

	s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now())
	if s == nil {
		t.Fatal("three usable accounts against a threshold of two should suppress buying")
	}
	if !strings.Contains(s.Reason, "usable accounts") {
		t.Fatalf("reason should mention usable accounts, got %q", s.Reason)
	}
}

// The count must exclude quota-exhausted accounts. A pool of accounts that have
// all burned their quota needs topping up exactly as much as an empty one, and
// this is the case AvailableCount gets wrong.
func TestHealthyCountExcludesQuotaExhaustedAccounts(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.AddAccount(config.Account{
		ID: "spent", Enabled: true, AccessToken: "tok",
		UsageCurrent: 500, UsageLimit: 500,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "fresh", Enabled: true, AccessToken: "tok2",
		UsageCurrent: 10, UsageLimit: 500,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	h.pool.Reload()

	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("HealthyCount should count only the account with quota left, got %d", got)
	}

	// With the threshold at 2, one healthy account must trigger a buy.
	cfg := autoBuyTestConfig()
	cfg.MinHealthyAccounts = 2
	if s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now()); s != nil {
		t.Fatalf("one usable account against a threshold of two should allow buying, got %q", s.Reason)
	}
}

func TestLocalGuardsEnforcesMaxPoolSize(t *testing.T) {
	h := newAutoBuyHandler(t)
	for _, id := range []string{"p1", "p2"} {
		if err := config.AddAccount(config.Account{ID: id, Enabled: true, AccessToken: "t"}); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}
	h.pool.Reload()

	cfg := autoBuyTestConfig()
	cfg.MaxPoolAccounts = 2

	s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now())
	if s == nil {
		t.Fatal("a pool at its maximum must not grow")
	}
	if !strings.Contains(s.Reason, "already holds") {
		t.Fatalf("reason should name the pool cap, got %q", s.Reason)
	}
}

// A zero threshold means "no opinion" and must not be read as "never buy".
func TestLocalGuardsTreatsZeroThresholdsAsUnset(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MinHealthyAccounts = 0
	cfg.MaxPoolAccounts = 0
	cfg.MaxCreditsPerDay = 0

	if s := h.localGuards(cfg, config.AutoBuyZoneUS, autoBuyTriggerPoll, time.Now()); s != nil {
		t.Fatalf("unset thresholds should not block, got %q", s.Reason)
	}
}

// --- planning against a stub market ---

// stubMarket serves the market endpoints a plan needs.
func stubMarket(t *testing.T, profile, stock string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/my/profile":
			w.Write([]byte(profile))
		case "/api/my/stock":
			w.Write([]byte(stock))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"code":"not_found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const stubProfileRich = `{"profile":{"username":"tester","balance":10000,"keys_held":0,"hold_cap_effective":0}}`

func stubStock(usAvail, usPrice, euAvail, euPrice, max int) string {
	return `{"stock":{"public_available":` + strconv.Itoa(usAvail+euAvail) + `},"zones":[` +
		`{"zone":"us","region":"us-east-1","available":` + strconv.Itoa(usAvail) + `,"unit_price":` + strconv.Itoa(usPrice) + `,"base_price":40},` +
		`{"zone":"eu","region":"eu-central-1","available":` + strconv.Itoa(euAvail) + `,"unit_price":` + strconv.Itoa(euPrice) + `,"base_price":10}],` +
		`"max":` + strconv.Itoa(max) + `,"min_per_order":1,"max_per_order":200,"warranty_minutes":10}`
}

// planFor runs planPurchase against a stub market.
func planFor(t *testing.T, h *Handler, cfg *config.AutoBuyConfig, srv *httptest.Server, zone string) (*autoBuyPlan, *autoBuySkip, error) {
	t.Helper()
	cfg.MarketBaseURL = srv.URL
	m, err := newMarketClient(cfg)
	if err != nil {
		t.Fatalf("newMarketClient: %v", err)
	}
	return h.planPurchase(testCtx(t), m, cfg, zone, "")
}

func TestPlanPurchaseHonoursThePerZonePriceCeiling(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	// us is priced at 30 against a ceiling of 25; eu at 8 against a ceiling of 10.
	// One shared ceiling could not express both, which is why zones carry their own.
	srv := stubMarket(t, stubProfileRich, stubStock(50, 30, 50, 8, 50))

	plan, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped == nil {
		t.Fatalf("us at 30 should be refused by the ceiling of 25, got plan %+v", plan)
	}
	if !strings.Contains(skipped.Reason, "exceeds the ceiling") {
		t.Fatalf("reason should name the ceiling, got %q", skipped.Reason)
	}

	plan, skipped, err = planFor(t, h, cfg, srv, config.AutoBuyZoneEU)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("eu at 8 is under its ceiling of 10 and should proceed, got %q", skipped.Reason)
	}
	if plan.Count != 3 {
		t.Fatalf("eu buyCount is 3, got %d", plan.Count)
	}
}

func TestPlanPurchaseSkipsWhenOutOfStock(t *testing.T) {
	h := newAutoBuyHandler(t)
	srv := stubMarket(t, stubProfileRich, stubStock(0, 20, 0, 5, 0))

	_, skipped, err := planFor(t, h, autoBuyTestConfig(), srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped == nil {
		t.Fatal("no stock should skip")
	}
}

func TestPlanPurchaseRespectsTheBalanceFloor(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MinBalance = 5000
	srv := stubMarket(t, `{"profile":{"balance":100,"keys_held":0,"hold_cap_effective":0}}`, stubStock(50, 20, 50, 5, 50))

	_, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped == nil {
		t.Fatal("a balance below the floor should skip")
	}
	if !strings.Contains(skipped.Reason, "below the configured floor") {
		t.Fatalf("reason should name the floor, got %q", skipped.Reason)
	}
}

// With credits left over but not enough for the full order, buying fewer beats
// buying none.
func TestPlanPurchaseTrimsCountToTheRemainingDailyCredits(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MaxCreditsPerDay = 100
	cfg.SpentToday = 40 // 60 left, at 20 each → 3 affordable
	cfg.Zones[config.AutoBuyZoneUS].BuyCount = 5
	srv := stubMarket(t, stubProfileRich, stubStock(50, 20, 50, 5, 50))

	plan, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("should trim rather than skip, got %q", skipped.Reason)
	}
	if plan.Count != 3 {
		t.Fatalf("60 credits at 20 each affords 3, got %d", plan.Count)
	}
}

// Not even one key affordable is a genuine skip, not a zero-count order.
func TestPlanPurchaseSkipsWhenTheCeilingCannotAffordOneKey(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MaxCreditsPerDay = 100
	cfg.SpentToday = 95 // 5 left, price is 20
	srv := stubMarket(t, stubProfileRich, stubStock(50, 20, 50, 5, 50))

	_, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped == nil {
		t.Fatal("an unaffordable unit price should skip")
	}
}

func TestPlanPurchaseTrimsToStockAndPerOrderMaximum(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Zones[config.AutoBuyZoneUS].BuyCount = 50
	// Only 4 in stock, and max says 2 may be taken at once.
	srv := stubMarket(t, stubProfileRich, stubStock(4, 20, 0, 5, 2))

	plan, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("should trim rather than skip, got %q", skipped.Reason)
	}
	if plan.Count != 2 {
		t.Fatalf("count should be trimmed to the pickup maximum of 2, got %d", plan.Count)
	}
}

// Ordering past the market's holding cap fails with purchase_cap_reached, which
// the docs say is not worth retrying. Trim locally instead.
func TestPlanPurchaseTrimsToTheMarketHoldingCap(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Zones[config.AutoBuyZoneUS].BuyCount = 10
	profile := `{"profile":{"balance":10000,"keys_held":18,"hold_cap_effective":20}}`
	srv := stubMarket(t, profile, stubStock(50, 20, 50, 5, 50))

	plan, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("should trim rather than skip, got %q", skipped.Reason)
	}
	if plan.Count != 2 {
		t.Fatalf("holding room is 2 (18 of 20), got count %d", plan.Count)
	}
}

func TestPlanPurchaseSkipsWhenTheHoldingCapIsFull(t *testing.T) {
	h := newAutoBuyHandler(t)
	profile := `{"profile":{"balance":10000,"keys_held":20,"hold_cap_effective":20}}`
	srv := stubMarket(t, profile, stubStock(50, 20, 50, 5, 50))

	_, skipped, err := planFor(t, h, autoBuyTestConfig(), srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped == nil {
		t.Fatal("a full holding cap should skip")
	}
}

// hold_cap_effective of 0 means unlimited and must not be read as "no room".
func TestPlanPurchaseTreatsZeroHoldCapAsUnlimited(t *testing.T) {
	h := newAutoBuyHandler(t)
	profile := `{"profile":{"balance":10000,"keys_held":900,"hold_cap_effective":0}}`
	srv := stubMarket(t, profile, stubStock(50, 20, 50, 5, 50))

	plan, skipped, err := planFor(t, h, autoBuyTestConfig(), srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("an unlimited hold cap should not block, got %q", skipped.Reason)
	}
	if plan.Count != 5 {
		t.Fatalf("want the configured count of 5, got %d", plan.Count)
	}
}

// A zero ceiling means "any price", spelled as a distinct value rather than a
// very large number.
func TestPlanPurchaseTreatsZeroPriceCeilingAsUnlimited(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.Zones[config.AutoBuyZoneUS].MaxUnitPrice = 0
	srv := stubMarket(t, stubProfileRich, stubStock(50, 999, 0, 5, 50))

	plan, skipped, err := planFor(t, h, cfg, srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("a zero ceiling should accept any price, got %q", skipped.Reason)
	}
	if plan.UnitPrice != 999 {
		t.Fatalf("plan should carry the live price, got %d", plan.UnitPrice)
	}
}

func TestPlanPurchaseGeneratesAValidOrderID(t *testing.T) {
	h := newAutoBuyHandler(t)
	srv := stubMarket(t, stubProfileRich, stubStock(50, 20, 50, 5, 50))

	plan, _, err := planFor(t, h, autoBuyTestConfig(), srv, config.AutoBuyZoneUS)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if !isValidClientOrderID(plan.OrderID) {
		t.Fatalf("plan should carry a valid idempotency key, got %q", plan.OrderID)
	}
}

// The webhook's purchase_order_id is what makes a redelivery idempotent, so it
// must be carried into the order unchanged.
func TestPlanPurchaseReusesASuppliedOrderID(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MarketBaseURL = stubMarket(t, stubProfileRich, stubStock(50, 20, 50, 5, 50)).URL
	m, err := newMarketClient(cfg)
	if err != nil {
		t.Fatalf("newMarketClient: %v", err)
	}

	supplied := "0a1b2c3d4e5f60718293a4b5c6d7e8f9"
	plan, skipped, err := h.planPurchase(testCtx(t), m, cfg, config.AutoBuyZoneUS, supplied)
	if err != nil {
		t.Fatalf("planPurchase: %v", err)
	}
	if skipped != nil {
		t.Fatalf("unexpected skip: %q", skipped.Reason)
	}
	if plan.OrderID != supplied {
		t.Fatalf("the supplied idempotency key must be reused verbatim, got %q", plan.OrderID)
	}
}

func TestPlanPurchaseRejectsAMalformedSuppliedOrderID(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.MarketBaseURL = stubMarket(t, stubProfileRich, stubStock(50, 20, 50, 5, 50)).URL
	m, err := newMarketClient(cfg)
	if err != nil {
		t.Fatalf("newMarketClient: %v", err)
	}

	if _, _, err := h.planPurchase(testCtx(t), m, cfg, config.AutoBuyZoneUS, "not-hex"); err == nil {
		t.Fatal("a malformed idempotency key should be rejected before the round trip")
	}
}

func TestNewMarketClientRequiresAKey(t *testing.T) {
	if _, err := newMarketClient(&config.AutoBuyConfig{}); err == nil {
		t.Fatal("a client without a market key should be refused")
	}
	if _, err := newMarketClient(nil); err == nil {
		t.Fatal("a nil config should be refused")
	}
}
