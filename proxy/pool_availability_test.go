package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// poolAvailability drives /v1/key/status and returns the poolAvailable field
// along with whether the server sent it at all. The distinction matters: the
// portal hides the banner on an absent field rather than guessing an outage.
func poolAvailability(t *testing.T, h *Handler, key string) (available, present bool) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/key/status", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	h.apiKeyModelHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /v1/key/status, got %d (%s)", rec.Code, rec.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := body["poolAvailable"]
	if !ok {
		return false, false
	}
	flag, isBool := raw.(bool)
	if !isBool {
		t.Fatalf("poolAvailable must be a boolean so the portal can tell it from a count, got %T", raw)
	}
	return flag, true
}

// The helper seeds one enabled account, so a healthy pool must report available.
// If this ever reports false the banner would sit on screen permanently and stop
// meaning anything.
func TestPoolAvailableWhenAnAccountCanServe(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-pool-ok", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	available, present := poolAvailability(t, h, "sk-pool-ok")
	if !present {
		t.Fatal("the portal needs poolAvailable to decide whether to warn")
	}
	if !available {
		t.Fatal("a pool with one healthy account must not report itself unavailable")
	}
}

// The case the banner exists for: every account is out, so requests fail and the
// per-model strip has no history to explain why. Nothing is broken on the
// customer's side and there is nothing for them to fix, hence "please wait".
func TestPoolUnavailableWhenEveryAccountIsDisabled(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-pool-empty", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	for _, acc := range config.GetAccounts() {
		if err := config.SetAccountEnabled(acc.ID, false); err != nil {
			t.Fatalf("disable %s: %v", acc.ID, err)
		}
	}
	h.pool.Reload()

	available, present := poolAvailability(t, h, "sk-pool-empty")
	if !present {
		t.Fatal("poolAvailable must be reported even when the pool is empty")
	}
	if available {
		t.Fatal("an empty pool must report unavailable, otherwise the customer is never told to wait")
	}
}

// An enabled provider can serve requests with every Kiro account down, so the
// banner must stay hidden: telling a customer to wait while their requests are
// succeeding is worse than saying nothing.
func TestPoolAvailableWhenOnlyAProviderRemains(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-pool-provider", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	for _, acc := range config.GetAccounts() {
		if err := config.SetAccountEnabled(acc.ID, false); err != nil {
			t.Fatalf("disable %s: %v", acc.ID, err)
		}
	}
	h.pool.Reload()

	if _, err := config.AddProvider(config.Provider{
		Name:     "fallback",
		Enabled:  true,
		Protocol: config.ProviderProtocolAnthropic,
		BaseURL:  "https://example.invalid/api/anthropic",
		APIKey:   "provider-secret",
		Models:   []config.ProviderModel{{Alias: "claude-opus-5", Name: "claude-opus-5"}},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	available, present := poolAvailability(t, h, "sk-pool-provider")
	if !present {
		t.Fatal("poolAvailable must still be reported")
	}
	if !available {
		t.Fatal("an enabled provider can serve the request, so the refill banner must not show")
	}
}

// A disabled provider cannot serve anything, so it must not suppress the banner.
// Without this check the flag would read "available" off mere configuration
// rather than off what can actually be routed to.
func TestDisabledProviderDoesNotCountAsAvailable(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-pool-off", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	for _, acc := range config.GetAccounts() {
		if err := config.SetAccountEnabled(acc.ID, false); err != nil {
			t.Fatalf("disable %s: %v", acc.ID, err)
		}
	}
	h.pool.Reload()

	if _, err := config.AddProvider(config.Provider{
		Name:     "switched-off",
		Enabled:  false,
		Protocol: config.ProviderProtocolAnthropic,
		BaseURL:  "https://example.invalid/api/anthropic",
		APIKey:   "provider-secret",
		Models:   []config.ProviderModel{{Alias: "claude-opus-5", Name: "claude-opus-5"}},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	available, _ := poolAvailability(t, h, "sk-pool-off")
	if available {
		t.Fatal("a disabled provider must not mask an empty pool")
	}
}

// The status payload is customer-facing. Pool size, account identity and provider
// credentials are the operator's business, so the flag must not leak them —
// which is why it is a boolean rather than the count the admin panel shows.
func TestPoolAvailabilityLeaksNoOperatorDetail(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-pool-priv", Enabled: true}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if _, err := config.AddProvider(config.Provider{
		Name:     "secret-vendor",
		Enabled:  true,
		Protocol: config.ProviderProtocolAnthropic,
		BaseURL:  "https://vendor.invalid/api/anthropic",
		APIKey:   "provider-secret",
		Models:   []config.ProviderModel{{Alias: "claude-opus-5", Name: "claude-opus-5"}},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/key/status", nil)
	req.Header.Set("Authorization", "Bearer sk-pool-priv")
	h.apiKeyModelHealth(rec, req)

	body := strings.ToLower(rec.Body.String())
	for _, forbidden := range []string{
		"secret-vendor",   // which vendor sits behind the proxy
		"provider-secret", // its credential
		"vendor.invalid",  // its endpoint
		"test-account",    // account identity
		"poolhealthy",     // a count would tell the customer the pool size
		"accountemail",
	} {
		if strings.Contains(body, strings.ToLower(forbidden)) {
			t.Fatalf("customer-facing status leaked %q: %s", forbidden, rec.Body.String())
		}
	}
}
