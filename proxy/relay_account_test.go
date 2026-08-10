package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// A relay account resolves to exactly ONE endpoint. The other kiroEndpoints
// entries are AWS hosts a relay has nothing to do with, so trying them could only
// fail — and every attempt on the way there is a billed turn.
func TestRelayAccountResolvesToSingleEndpoint(t *testing.T) {
	account := &config.Account{
		ID:          "relay-1",
		KiroApiKey:  "relay-key",
		AuthMethod:  "api_key",
		ApiEndpoint: "https://relay.example/generateAssistantResponse",
	}

	ep, ok := relayEndpointFor(account)
	if !ok {
		t.Fatal("relayEndpointFor must claim an account carrying an ApiEndpoint")
	}
	if ep.URL != account.ApiEndpoint {
		t.Fatalf("relay endpoint URL = %q, want the configured %q", ep.URL, account.ApiEndpoint)
	}
	// X-Amz-Target names an AWS service operation. A relay routes on the URL path,
	// and sending the header could steer it to the wrong handler.
	if ep.AmzTarget != "" {
		t.Fatalf("relay endpoint must not carry an X-Amz-Target, got %q", ep.AmzTarget)
	}
}

// The override must be keyed on ApiEndpoint alone. Tying it to AuthMethod would
// leave a relay quietly talking to AWS if the method were ever edited.
func TestOrdinaryAccountIsNotTreatedAsRelay(t *testing.T) {
	for name, account := range map[string]*config.Account{
		"oauth":             {ID: "a", AccessToken: "tok"},
		"api key, no relay": {ID: "b", KiroApiKey: "k", AuthMethod: "api_key"},
		"whitespace only":   {ID: "c", KiroApiKey: "k", ApiEndpoint: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			if account.IsRelayCredential() {
				t.Fatal("account must not be classified as a relay")
			}
			if _, ok := relayEndpointFor(account); ok {
				t.Fatal("relayEndpointFor must decline a non-relay account")
			}
		})
	}
}

// A relay URL is absolute and operator-supplied. Region rewriting would rebuild
// an AWS host out of an address that only coincidentally resembles one.
func TestRelayURLIsNotRegionalized(t *testing.T) {
	account := &config.Account{
		ID:          "relay-2",
		ApiEndpoint: "https://relay.example/generateAssistantResponse",
		ApiRegion:   "eu-west-1",
	}

	// Guard the premise: this region WOULD rewrite an ordinary AWS host, so the
	// test is proving the relay branch is taken rather than that nothing happens.
	if got := regionalizeURL("https://q.us-east-1.amazonaws.com/x", account); got == "https://q.us-east-1.amazonaws.com/x" {
		t.Fatal("premise broken: regionalizeURL left an AWS host unchanged for a non-default region")
	}

	ep, ok := relayEndpointFor(account)
	if !ok {
		t.Fatal("expected a relay endpoint")
	}
	if strings.Contains(ep.URL, "eu-west-1") || ep.URL != account.ApiEndpoint {
		t.Fatalf("relay URL was rewritten: got %q, want %q", ep.URL, account.ApiEndpoint)
	}
}

// The Host header is overridden while the dialled URL stays put. That split is
// what removes the need for a custom CA: TLS still validates against the real
// host in the URL, so a routing name with no certificate of its own works.
func TestRelayHostHeaderOverridesWithoutChangingDialledHost(t *testing.T) {
	var gotHost, gotAuth, gotTokenType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotAuth = r.Header.Get("Authorization")
		gotTokenType = r.Header.Get("tokentype")
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}))
		w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"unit": "credit", "usage": 0.01}))
	}))
	defer server.Close()

	restore := swapKiroEndpointsForTest(t, server)
	defer restore()

	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	account := &config.Account{
		ID:          "relay-3",
		KiroApiKey:  "relay-key",
		AuthMethod:  "api_key",
		AccessToken: "relay-key",
		ApiEndpoint: server.URL,
		ApiHost:     "q.vhost.example",
		Enabled:     true,
	}

	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "11111111-1111-4111-8111-111111111111"
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-haiku-4.5"

	var text strings.Builder
	err := CallKiroAPI(account, payload, &KiroStreamCallback{
		OnText: func(s string, isThinking bool) { text.WriteString(s) },
	})
	if err != nil {
		t.Fatalf("CallKiroAPI: %v", err)
	}
	if text.String() != "ok" {
		t.Fatalf("streamed text = %q, want %q", text.String(), "ok")
	}
	if gotHost != "q.vhost.example" {
		t.Fatalf("Host header = %q, want the ApiHost override", gotHost)
	}
	if gotAuth != "Bearer relay-key" {
		t.Fatalf("Authorization = %q, want the relay key as bearer", gotAuth)
	}
	if gotTokenType != "API_KEY" {
		t.Fatalf("tokentype = %q, want API_KEY", gotTokenType)
	}
}

// A rejected relay key arrives as HTTP 429 — not 401 — with the reason in the
// body. Discarding that body reported a permanently dead key as "quota
// exhausted", so it was cooled down and retried forever with the real reason
// nowhere in the log.
func TestRelay429BodyReachesTheError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer server.Close()

	restore := swapKiroEndpointsForTest(t, server)
	defer restore()

	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	account := &config.Account{
		ID:          "relay-4",
		KiroApiKey:  "dead-key",
		AuthMethod:  "api_key",
		AccessToken: "dead-key",
		ApiEndpoint: server.URL,
		Enabled:     true,
	}

	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = "11111111-1111-4111-8111-111111111111"
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"
	payload.ConversationState.CurrentMessage.UserInputMessage.ModelID = "claude-haiku-4.5"

	err := CallKiroAPI(account, payload, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for a rejected key")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error must carry the upstream reason, got %q", err.Error())
	}
}

// The rejection message contains "429", which isQuotaErrorMessage matches. So the
// relay case has to be ordered ahead of it — otherwise a key that can never work
// again is classified as a quota problem that clears on its own.
func TestRejectedRelayKeyOutranksQuotaClassification(t *testing.T) {
	msg := "HTTP 429 from Relay: {\"message\":\"invalid api key\"}"

	if !isQuotaErrorMessage(msg) {
		t.Fatal("premise broken: this message no longer looks like a quota error, so ordering would not matter")
	}
	if !isRelayKeyRejectedError(msg) {
		t.Fatal("a rejected relay key must be recognised")
	}
	// 503, not 429 or 401: the credential at fault is the operator's. 429 invites a
	// retry that can never succeed, and 401 would send the customer off to
	// regenerate a key that was never the problem.
	if got := statusForUpstreamError(errors.New(msg)); got != http.StatusServiceUnavailable {
		t.Fatalf("a relay key rejection must answer 503 so clients do not retry, got %d", got)
	}
}

// When isRelayKeyRejectedError misses a shape, the message must still reach the
// error rather than being truncated to a hardcoded label.
func TestRelayRejectionPassesThroughWhenNotRecognised(t *testing.T) {
	msg := "HTTP 418 from Relay: Teapot refused to brew coffee"

	if isRelayKeyRejectedError(msg) {
		t.Fatal("premise broken: this message is now recognised, test would not prove the fallback")
	}
	// Falls through to isQuotaErrorMessage (which does not match, since there is
	// no quota keyword), then to the default branch, which preserves the string.
	if got := statusForUpstreamError(errors.New(msg)); got != http.StatusInternalServerError {
		t.Fatalf("an unrecognised relay error must fall through to 500, got %d", got)
	}
}

// REST operations return ErrModelListingUnsupported / ErrAccountInfoUnsupported
// for relay accounts. Those sentinels must not be charged to the account or leak
// into a cooldown — they are expected behaviour, not failures.
func TestRelayRESTUnsupportedDoesNotCoolDownAccount(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		t.Fatal("test setup produced no enabled accounts")
	}
	acc := accounts[0]
	acc.ApiEndpoint = "https://relay.test/generateAssistantResponse"
	if err := config.UpdateAccount(acc.ID, acc); err != nil {
		t.Fatalf("mark account as relay: %v", err)
	}
	h.pool.Reload()

	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("expected a healthy pool of 1 before the test, got %d", got)
	}

	// The two REST sentinels, hit multiple times to exceed the cooldown threshold.
	for i := 0; i < 6; i++ {
		h.handleAccountFailure(&acc, ErrModelListingUnsupported)
		h.handleAccountFailure(&acc, ErrAccountInfoUnsupported)
	}

	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("REST-unsupported errors took the account out of the pool: healthy=%d", got)
	}
}

// Model listing, overage fetch, and account-info refresh all reach the real AWS
// host. A relay's key is meaningless there and would answer 403, which matches
// isAuthErrorMessage and triggers a ban. The sentinels guard all three.
func TestRelayAccountDoesNotTriggerAuthBanOnBackgroundRefresh(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		t.Fatal("test setup produced no enabled accounts")
	}
	acc := accounts[0]
	acc.ApiEndpoint = "https://relay.test/generateAssistantResponse"
	acc.KiroApiKey = "ksk_relay"
	acc.AuthMethod = "api_key"
	if err := config.UpdateAccount(acc.ID, acc); err != nil {
		t.Fatalf("mark account as relay: %v", err)
	}
	h.pool.Reload()

	// These three are the functions backgroundRefresh calls. The guard must sit
	// upstream of handleAccountFailure for all of them.
	if _, err := ListAvailableModels(&acc); !errors.Is(err, ErrModelListingUnsupported) {
		t.Fatalf("ListAvailableModels must return ErrModelListingUnsupported for a relay, got %v", err)
	}
	if _, err := RefreshAccountInfo(&acc); !errors.Is(err, ErrAccountInfoUnsupported) {
		t.Fatalf("RefreshAccountInfo must return ErrAccountInfoUnsupported for a relay, got %v", err)
	}
	if _, err := GetUsageLimits(&acc); !errors.Is(err, ErrModelListingUnsupported) {
		t.Fatalf("GetUsageLimits must return ErrModelListingUnsupported for a relay, got %v", err)
	}

	fresh := freshAccountByID(acc.ID)
	if fresh == nil {
		t.Fatal("account disappeared")
	}
	if !fresh.Enabled {
		t.Fatalf("account was disabled: banStatus=%q banReason=%q", fresh.BanStatus, fresh.BanReason)
	}
}
