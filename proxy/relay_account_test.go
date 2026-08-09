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
		t.Fatalf("status = %d, want 503", got)
	}
}

// A genuine quota error must still classify as quota — the new matcher has to be
// narrow enough not to swallow the case it is ordered in front of.
func TestGenuineQuotaErrorStillClassifiesAsQuota(t *testing.T) {
	msg := "quota exhausted on Kiro IDE"

	if isRelayKeyRejectedError(msg) {
		t.Fatal("a plain quota error must not be read as a rejected key")
	}
	if got := statusForUpstreamError(errors.New(msg)); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", got)
	}
}

// Guarded inside RefreshAccountInfo rather than at its seven call sites, so the
// next call site added cannot reintroduce the problem. It is not only log noise:
// the token-error branch there matches the bare substring "invalid", so a relay
// 404 body containing that word would be misread as an expired token.
func TestRefreshAccountInfoDeclinesRelayAccount(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	_, err := RefreshAccountInfo(&config.Account{
		ID:          "relay-5",
		ApiEndpoint: "https://relay.example/generateAssistantResponse",
	})
	if !errors.Is(err, ErrAccountInfoUnsupported) {
		t.Fatalf("expected ErrAccountInfoUnsupported, got %v", err)
	}
}

// An empty model list means "supports everything" (see accountHasModel), which is
// the same optimistic routing used at cold start. Hardcoding a relay's inventory
// instead would go stale the moment its operator changed it.
func TestRelayModelFetchIsSkippedAndLeavesListEmpty(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	account := &config.Account{
		ID:          "relay-6",
		KiroApiKey:  "relay-key",
		AuthMethod:  "api_key",
		AccessToken: "relay-key",
		ApiEndpoint: "https://relay.example/generateAssistantResponse",
		Enabled:     true,
	}

	// No HTTP server is stubbed: reaching the network here would fail, so a nil
	// error is itself evidence the listing call was skipped.
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		t.Fatalf("model fetch must be skipped for a relay, got %v", err)
	}
	if got := h.pool.GetModelList(account.ID); len(got) != 0 {
		t.Fatalf("relay account model list = %v, want empty so routing stays optimistic", got)
	}
}

// A relative endpoint would be joined against nothing and fail at request time
// with a confusing transport error, long after the operator left the form that
// could have told them.
func TestValidateApiEndpoint(t *testing.T) {
	valid := []string{
		"",
		"   ",
		"https://relay.example/generateAssistantResponse",
		"http://127.0.0.1:8080/generateAssistantResponse",
	}
	for _, raw := range valid {
		if err := config.ValidateApiEndpoint(raw); err != nil {
			t.Fatalf("ValidateApiEndpoint(%q) = %v, want nil", raw, err)
		}
	}

	invalid := []string{
		"relay.example/generateAssistantResponse",
		"/generateAssistantResponse",
		"ftp://relay.example/x",
		"https:///nohost",
	}
	for _, raw := range invalid {
		if err := config.ValidateApiEndpoint(raw); err == nil {
			t.Fatalf("ValidateApiEndpoint(%q) = nil, want an error", raw)
		}
	}
}

// The tier is what makes a relay a standby rather than a live upstream, so it has
// to survive the round trip through config. Tier 99 means "only once everything
// else is exhausted", which is the whole point of adding one.
func TestRelayAccountPersistsEndpointHostAndTier(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID:          "relay-7",
		KiroApiKey:  "relay-key",
		AuthMethod:  "api_key",
		AccessToken: "relay-key",
		ApiEndpoint: "https://relay.example/generateAssistantResponse",
		ApiHost:     "q.vhost.example",
		Priority:    99,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// Re-read through config rather than trusting the value just written.
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init reload: %v", err)
	}
	var found *config.Account
	for _, a := range config.GetAccounts() {
		if a.ID == "relay-7" {
			acc := a
			found = &acc
			break
		}
	}
	if found == nil {
		t.Fatal("relay account did not survive the reload")
	}
	if found.ApiEndpoint != "https://relay.example/generateAssistantResponse" {
		t.Fatalf("ApiEndpoint = %q", found.ApiEndpoint)
	}
	if found.ApiHost != "q.vhost.example" {
		t.Fatalf("ApiHost = %q", found.ApiHost)
	}
	if found.Priority != 99 {
		t.Fatalf("Priority = %d, want 99 so the relay stays a last resort", found.Priority)
	}
	if !found.IsRelayCredential() {
		t.Fatal("reloaded account must still be recognised as a relay")
	}
}
