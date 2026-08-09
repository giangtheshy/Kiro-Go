package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// emptyStreamErr is the error the endpoint loop raises when the upstream answers
// 200 and then closes the stream having sent neither content nor a metering
// event. Kept as a literal rather than built from the production format string so
// that rewording the message has to be acknowledged here.
const emptyStreamErr = "empty stream from CodeWhisperer (no output, no metering)"

func TestEmptyStreamErrorIsRecognised(t *testing.T) {
	if !isEmptyStreamErrorMessage(emptyStreamErr) {
		t.Fatalf("the real empty-stream message was not recognised: %s", emptyStreamErr)
	}
	// The matcher keys off the parenthetical, so it must survive the wrapping that
	// handlers add around upstream errors.
	if !isEmptyStreamErrorMessage("HTTP 500 from Kiro IDE: " + emptyStreamErr) {
		t.Fatal("a wrapped empty-stream message was not recognised")
	}
}

// The matcher gates both an extra upstream call and the skipping of account error
// accounting, so a false positive would silently stop cooling down accounts that
// genuinely misbehave.
func TestEmptyStreamMatcherIsNarrow(t *testing.T) {
	for _, msg := range []string{
		"HTTP 429 from Kiro IDE: quota exhausted",
		"HTTP 400 from Kiro IDE: ValidationException",
		`{"reason":"OVERAGE_REQUEST_LIMIT_EXCEEDED"}`,
		"dial tcp 1.2.3.4:443: connect: connection refused",
		// Deliberately close: a turn that WAS billed but produced nothing. That one
		// must never be retried, so it must not match.
		"upstream billed a turn that produced no content",
		"",
	} {
		if isEmptyStreamErrorMessage(msg) {
			t.Fatalf("unrelated error matched the empty-stream matcher: %q", msg)
		}
	}
}

// An empty stream must reach the flat-history retry: the endpoint loop has already
// retried the identical body on every endpoint by then, so the flattened payload
// is the only remaining variation before the client sees an error.
func TestEmptyStreamTriggersSafePayloadRetry(t *testing.T) {
	if !shouldRetrySafePayload(emptyStreamErr) {
		t.Fatal("an empty stream must be eligible for the flat-history retry")
	}
	// Regression guard: the two pre-existing classes must still qualify.
	if !shouldRetrySafePayload("HTTP 400 from Kiro IDE: ValidationException") {
		t.Fatal("a malformed-payload error stopped qualifying for the safe retry")
	}
	if !shouldRetrySafePayload("HTTP 500 from Kiro IDE: unexpected error when processing the request") {
		t.Fatal("a generic 5xx stopped qualifying for the safe retry")
	}
	// A quota error must still NOT: another account may serve it fine, so
	// rewriting the payload would be the wrong response.
	if shouldRetrySafePayload("HTTP 429 from Kiro IDE: quota exhausted") {
		t.Fatal("a quota error must not trigger a payload rewrite")
	}
}

// An empty stream must not cool the account down. It previously fell through to
// the default branch of handleAccountFailure, which records an account error and
// cools the account after three: one client sending a payload the upstream
// silently rejects would walk the whole pool out of service, breaking accounts
// that were healthy for every other customer.
//
// Cooldown is asserted through HealthyCount rather than the pool's private error
// counter, so this tests the consequence an operator would actually notice.
func TestEmptyStreamDoesNotCoolDownAccount(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		t.Fatal("test setup produced no enabled accounts")
	}
	acc := accounts[0]

	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("expected a healthy pool of 1 before the test, got %d", got)
	}

	// Well past the three-strike threshold.
	for i := 0; i < 6; i++ {
		h.handleAccountFailure(&acc, errors.New(emptyStreamErr))
	}
	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("empty streams took the account out of the pool: healthy=%d", got)
	}
	if got := h.pool.GetNextForModelExcluding("claude-sonnet-4.5", nil); got == nil {
		t.Fatal("the account became unselectable after empty streams")
	}
}

// Contrast case, so the exemption above is proven specific rather than proving
// that account accounting is broken outright.
func TestGenericFailureStillCoolsDownAccount(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		t.Fatal("test setup produced no enabled accounts")
	}
	acc := accounts[0]

	for i := 0; i < 3; i++ {
		h.handleAccountFailure(&acc, errors.New("HTTP 418 from Kiro IDE: teapot"))
	}
	if got := h.pool.HealthyCount(); got != 0 {
		t.Fatalf("three generic failures must cool the account down, healthy=%d", got)
	}
}

// ---- endpoint selection ----

// getSortedEndpoints resolved its indices positionally, from when kiroEndpoints
// held three entries. Inserting "Kiro Runtime" at index 1 shifted them, so
// "codewhisperer" selected Kiro Runtime and "amazonq" selected CodeWhisperer —
// the operator's choice in the panel silently ran a different endpoint, and the
// endpoint named in an error message was not the one configured.
func TestPreferredEndpointSelectsTheNamedEndpoint(t *testing.T) {
	for _, tc := range []struct{ setting, want string }{
		{"kiro", "Kiro IDE"},
		{"codewhisperer", "CodeWhisperer"},
		{"amazonq", "AmazonQ"},
	} {
		got := getSortedEndpoints(tc.setting)
		if len(got) == 0 {
			t.Fatalf("%s: no endpoints returned", tc.setting)
		}
		if got[0].Name != tc.want {
			t.Fatalf("setting %q selected %q, want %q", tc.setting, got[0].Name, tc.want)
		}
	}
}

// Auto mode listed [0],[1],[2] explicitly, so the last declared endpoint was
// unreachable however many were declared.
func TestAutoModeReachesEveryEndpoint(t *testing.T) {
	got := getSortedEndpoints("auto")
	if len(got) != len(kiroEndpoints) {
		t.Fatalf("auto mode returned %d of %d endpoints", len(got), len(kiroEndpoints))
	}
	for i := range kiroEndpoints {
		if got[i].Name != kiroEndpoints[i].Name {
			t.Fatalf("auto mode reordered endpoints at %d: %q vs %q", i, got[i].Name, kiroEndpoints[i].Name)
		}
	}
}

// The returned slice must not alias the package-level table: the endpoint loop
// writes Origin onto the entry it is about to call, which would otherwise mutate
// global state for every later request.
func TestSortedEndpointsDoNotAliasGlobalTable(t *testing.T) {
	original := kiroEndpoints[0].Origin
	got := getSortedEndpoints("auto")
	got[0].Origin = "MUTATED"
	if kiroEndpoints[0].Origin != original {
		kiroEndpoints[0].Origin = original // restore before failing
		t.Fatal("getSortedEndpoints returned a slice aliasing kiroEndpoints")
	}
}

// A preferred endpoint that no longer exists must not leave the caller with
// nothing to try.
func TestUnknownPreferredEndpointFallsBackToAutoOrder(t *testing.T) {
	got := getSortedEndpoints("this-endpoint-was-removed")
	if len(got) != len(kiroEndpoints) {
		t.Fatalf("unknown preference returned %d endpoints, want all %d", len(got), len(kiroEndpoints))
	}
}

func TestIndexOfEndpointReportsMissingByNegativeOne(t *testing.T) {
	if got := indexOfEndpoint("Kiro IDE"); got < 0 {
		t.Fatal("a declared endpoint was reported as missing")
	}
	if got := indexOfEndpoint("nope"); got != -1 {
		t.Fatalf("an undeclared endpoint returned %d, want -1", got)
	}
}

// ---- customer-visible error detail ----

// selfLogsBody calls GET /v1/key/logs as the key owner and returns the raw body.
func selfLogsBody(t *testing.T, h *Handler, key string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/key/logs", nil)
	req.Header.Set("X-Api-Key", key)
	rec := httptest.NewRecorder()
	h.apiKeySelfLogs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("self-logs returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func selfLogEntries(t *testing.T, h *Handler, key string) []apiKeySelfLogEntry {
	t.Helper()
	var payload struct {
		Logs []apiKeySelfLogEntry `json:"logs"`
	}
	body := selfLogsBody(t, h, key)
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode self-logs: %v\n%s", err, body)
	}
	return payload.Logs
}

// seedSelfLogKey returns a handler and a key value whose log rows can be read
// back through the self-service endpoint.
func seedSelfLogKey(t *testing.T, name, key string) (*Handler, string) {
	t.Helper()
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: name, Key: key, Enabled: true}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	requestLog.reset()
	return &Handler{}, key
}

// keyIDFor resolves the stored ID for a seeded key value.
func keyIDFor(t *testing.T, key string) string {
	t.Helper()
	entry := config.FindApiKeyByValue(key)
	if entry == nil {
		t.Fatalf("seeded key %q not found", key)
	}
	return entry.ID
}

// The portal reads failures from /v1/key/logs. Before this the endpoint dropped
// the message, so a customer whose requests were all failing saw a ledger of
// zeros with nothing to report. The live SSE stream DID carry the error, so the
// two views disagreed and the detail vanished on reload.
func TestSelfLogsCarryErrorDetail(t *testing.T) {
	h, key := seedSelfLogKey(t, "cust-err", "sk-cust-err")

	logRequest(RequestLogEntry{
		Status:     "error",
		Endpoint:   "claude",
		APIKeyID:   keyIDFor(t, key),
		Model:      "claude-opus-5",
		StatusCode: 500,
		Error:      emptyStreamErr,
	})

	entries := selfLogEntries(t, h, key)
	if len(entries) == 0 {
		t.Fatal("no entries returned for the key")
	}
	e := entries[0]
	if e.Status != "error" {
		t.Fatalf("status not reported: %q", e.Status)
	}
	if e.StatusCode != 500 {
		t.Fatalf("statusCode not reported: %d", e.StatusCode)
	}
	if !strings.Contains(e.Error, "no output, no metering") {
		t.Fatalf("error detail not reported: %q", e.Error)
	}
}

// A successful request must not acquire failure fields: the panel decides whether
// to show itself from exactly these values, so a stray non-empty Error would put
// a red box on a healthy account.
func TestSelfLogsLeaveSuccessesClean(t *testing.T) {
	h, key := seedSelfLogKey(t, "cust-ok", "sk-cust-ok")

	logRequest(RequestLogEntry{
		Status:       "ok",
		Endpoint:     "claude",
		APIKeyID:     keyIDFor(t, key),
		Model:        "claude-opus-5",
		InputTokens:  10,
		OutputTokens: 5,
	})

	entries := selfLogEntries(t, h, key)
	if len(entries) == 0 {
		t.Fatal("no entries returned for the key")
	}
	if entries[0].Error != "" || entries[0].StatusCode != 0 {
		t.Fatalf("a success carried failure fields: %+v", entries[0])
	}
}

// Adding Error/StatusCode widened this payload, so re-assert the boundary rather
// than trusting that the struct was not extended further: the customer must still
// not learn which account served the request, or any other client's address.
func TestSelfLogsStillHideServingIdentity(t *testing.T) {
	h, key := seedSelfLogKey(t, "cust-priv", "sk-cust-priv")

	logRequest(RequestLogEntry{
		Status:       "error",
		APIKeyID:     keyIDFor(t, key),
		Model:        "claude-opus-5",
		AccountEmail: "operator-account@example.com",
		AccountID:    "acct-should-not-leak",
		ClientIP:     "203.0.113.9",
		StatusCode:   500,
		Error:        emptyStreamErr,
	})

	raw := selfLogsBody(t, h, key)
	for _, secret := range []string{
		"operator-account@example.com",
		"acct-should-not-leak",
		"203.0.113.9",
		"clientIp",
		"accountEmail",
	} {
		if strings.Contains(raw, secret) {
			t.Fatalf("customer log leaked %q: %s", secret, raw)
		}
	}
}

// A provider failure is collapsed to the opaque message where it is raised, so
// the customer-facing row must show that rather than a vendor name or URL. This
// pins the behaviour the portal relies on to decide between "wait and retry" and
// "quote this to the operator".
func TestSelfLogsKeepProviderFailuresOpaque(t *testing.T) {
	h, key := seedSelfLogKey(t, "cust-prov", "sk-cust-prov")

	logRequest(RequestLogEntry{
		Status:     "error",
		APIKeyID:   keyIDFor(t, key),
		Model:      "gpt-5.6-sol",
		StatusCode: 503,
		Error:      errNoUpstreamAvailable.Error(),
	})

	entries := selfLogEntries(t, h, key)
	if len(entries) == 0 {
		t.Fatal("no entries returned for the key")
	}
	if entries[0].Error != noUpstreamAvailableMessage {
		t.Fatalf("provider failure must stay opaque, got %q", entries[0].Error)
	}
}
