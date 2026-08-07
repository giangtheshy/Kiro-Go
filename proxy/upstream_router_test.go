package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

const routerTestModel = "claude-sonnet-4.5"

// newRouterHandler wires a Handler onto a throwaway config and the shared pool.
// Accounts are added with a far-future token expiry so the pool does not skip
// them as "about to expire".
func newRouterHandler(t *testing.T, accounts []config.Account, providers []config.Provider) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := accountpool.GetPool()
	for _, acc := range accounts {
		if err := config.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}
	for _, prov := range providers {
		if _, err := config.AddProvider(prov); err != nil {
			t.Fatalf("AddProvider: %v", err)
		}
	}
	p.Reload()
	for _, acc := range accounts {
		p.SetModelList(acc.ID, []string{routerTestModel})
	}
	return &Handler{pool: p}
}

func routerAccount(id string, priority int) config.Account {
	return config.Account{
		ID:          id,
		Email:       id + "@example.com",
		Enabled:     true,
		AccessToken: "token-" + id,
		ExpiresAt:   1 << 40, // far future: never "expiring soon"
		Priority:    priority,
	}
}

func routerProvider(id, name string, priority int, protocol string) config.Provider {
	return config.Provider{
		ID:       id,
		Name:     name,
		Enabled:  true,
		Protocol: protocol,
		BaseURL:  "https://example.invalid",
		APIKey:   "k",
		Priority: priority,
		Models:   []config.ProviderModel{{Alias: routerTestModel, Name: "upstream-model"}},
		Pricing:  config.ProviderPricing{Input: 1, Output: 2},
	}
}

// stepLabel identifies which upstream a step points at, for readable assertions.
func stepLabel(s *upstreamStep) string {
	if s == nil {
		return "<nil>"
	}
	if s.Provider != nil {
		return "provider:" + s.Provider.Name
	}
	return "account:" + s.Account.ID
}

// TestNextUpstreamWalksTiersInOrder is the headline behaviour the feature exists
// for: acc1 -> external provider -> acc2, expressed purely through Priority.
func TestNextUpstreamWalksTiersInOrder(t *testing.T) {
	h := newRouterHandler(t,
		[]config.Account{routerAccount("acc1", 0), routerAccount("acc2", 2)},
		[]config.Provider{routerProvider("prov1", "fallback", 1, config.ProviderProtocolAnthropic)},
	)

	excluded := make(map[string]bool)
	want := []string{"account:acc1", "provider:fallback", "account:acc2"}
	for i, expect := range want {
		step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, excluded)
		if got := stepLabel(step); got != expect {
			t.Fatalf("step %d = %s, want %s", i, got, expect)
		}
		if step.Provider != nil {
			excluded[step.Provider.ID] = true
		} else {
			excluded[step.Account.ID] = true
		}
	}
	if step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, excluded); step != nil {
		t.Fatalf("expected the chain to be exhausted, got %s", stepLabel(step))
	}
}

// TestNextUpstreamDefaultPriorityKeepsAccountsFirst pins the backward-compatible
// default: with everything left at tier 0, the whole Kiro pool is tried before
// any provider, so adding a provider cannot silently divert live traffic.
func TestNextUpstreamDefaultPriorityKeepsAccountsFirst(t *testing.T) {
	h := newRouterHandler(t,
		[]config.Account{routerAccount("acc1", 0), routerAccount("acc2", 0)},
		[]config.Provider{routerProvider("prov1", "fallback", 0, config.ProviderProtocolAnthropic)},
	)

	excluded := make(map[string]bool)
	seenAccounts := 0
	for i := 0; i < 2; i++ {
		step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, excluded)
		if step == nil || step.Account == nil {
			t.Fatalf("step %d: expected an account first, got %s", i, stepLabel(step))
		}
		excluded[step.Account.ID] = true
		seenAccounts++
	}
	if seenAccounts != 2 {
		t.Fatalf("expected both accounts before the provider, got %d", seenAccounts)
	}
	step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, excluded)
	if step == nil || step.Provider == nil {
		t.Fatalf("expected the provider last, got %s", stepLabel(step))
	}
}

// TestNextUpstreamSkipsProviderOnProtocolMismatch covers the core passthrough
// rule: no translation, so an OpenAI provider must never serve /v1/messages.
func TestNextUpstreamSkipsProviderOnProtocolMismatch(t *testing.T) {
	h := newRouterHandler(t, nil,
		[]config.Provider{routerProvider("prov1", "openai-only", 0, config.ProviderProtocolOpenAI)},
	)

	if step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, nil); step != nil {
		t.Fatalf("openai provider must not serve /v1/messages, got %s", stepLabel(step))
	}
	step := h.nextUpstream("", routerTestModel, config.ProviderEndpointChatCompletions, nil)
	if step == nil || step.Provider == nil {
		t.Fatalf("expected the provider on /v1/chat/completions, got %s", stepLabel(step))
	}
	if step.UpstreamModel != "upstream-model" {
		t.Fatalf("expected the mapped upstream model, got %q", step.UpstreamModel)
	}
}

func TestNextUpstreamResponsesRequiresOptIn(t *testing.T) {
	provider := routerProvider("prov1", "openai-only", 0, config.ProviderProtocolOpenAI)
	h := newRouterHandler(t, nil, []config.Provider{provider})

	if step := h.nextUpstream("", routerTestModel, config.ProviderEndpointResponses, nil); step != nil {
		t.Fatalf("expected /v1/responses to be skipped without opt-in, got %s", stepLabel(step))
	}

	provider.SupportsResponses = true
	if err := config.UpdateProvider("prov1", provider); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	step := h.nextUpstream("", routerTestModel, config.ProviderEndpointResponses, nil)
	if step == nil || step.Provider == nil {
		t.Fatalf("expected the provider once opted in, got %s", stepLabel(step))
	}
}

func TestNextUpstreamSkipsProviderWithoutModelMapping(t *testing.T) {
	h := newRouterHandler(t, nil,
		[]config.Provider{routerProvider("prov1", "fallback", 0, config.ProviderProtocolAnthropic)},
	)

	if step := h.nextUpstream("", "claude-haiku-4.5", config.ProviderEndpointMessages, nil); step != nil {
		t.Fatalf("expected an unmapped model to be skipped, got %s", stepLabel(step))
	}
}

// TestNextUpstreamHonoursBoundAccounts checks a key restricted to acc2 does not
// get acc1 handed to it just because acc1 sits in an earlier tier.
func TestNextUpstreamHonoursBoundAccounts(t *testing.T) {
	h := newRouterHandler(t,
		[]config.Account{routerAccount("acc1", 0), routerAccount("acc2", 0)},
		[]config.Provider{routerProvider("prov1", "fallback", 1, config.ProviderProtocolAnthropic)},
	)

	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-bound", BoundAccountIDs: []string{"acc2"}})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	step := h.nextUpstream(entry.ID, routerTestModel, config.ProviderEndpointMessages, nil)
	if got := stepLabel(step); got != "account:acc2" {
		t.Fatalf("expected the bound account, got %s", got)
	}
}

// TestNextUpstreamFallsBackWhenNoProviders asserts the fast path: with no
// provider configured at all, selection goes straight through the original
// account selector.
func TestNextUpstreamFallsBackWhenNoProviders(t *testing.T) {
	h := newRouterHandler(t, []config.Account{routerAccount("acc1", 5)}, nil)

	step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, nil)
	if got := stepLabel(step); got != "account:acc1" {
		t.Fatalf("expected the account regardless of its tier, got %s", got)
	}
}

func TestNextUpstreamSkipsDisabledProvider(t *testing.T) {
	provider := routerProvider("prov1", "fallback", 0, config.ProviderProtocolAnthropic)
	provider.Enabled = false
	h := newRouterHandler(t, nil, []config.Provider{provider})

	if step := h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, nil); step != nil {
		t.Fatalf("expected a disabled provider to be skipped, got %s", stepLabel(step))
	}
}

// TestNextUpstreamAccountCooldownDoesNotEscalateToProvider is a regression test
// for the asymmetry between GetNextForModelExcluding (had a cooldown-shortest
// fallback) and GetNextForModelBoundExcluding (previously had none). Before the
// fix, a tier-0 account on a 1-minute cooldown would cause nextUpstream to skip
// it and escalate to a tier-2 provider — even though the operator's intent was
// "use the account first, provider only as a true last resort".
func TestNextUpstreamAccountCooldownDoesNotEscalateToProvider(t *testing.T) {
	h := newRouterHandler(t,
		[]config.Account{routerAccount("acc1", 0)},
		[]config.Provider{routerProvider("prov1", "fallback", 2, config.ProviderProtocolAnthropic)},
	)

	p := accountpool.GetPool()

	// Baseline: healthy account wins.
	if got := stepLabel(h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, nil)); got != "account:acc1" {
		t.Fatalf("baseline: got %s, want account:acc1", got)
	}

	// Put the account on a short cooldown (3 non-quota errors).
	p.RecordError("acc1", false)
	p.RecordError("acc1", false)
	p.RecordError("acc1", false)

	// The account must still win over a provider at a higher tier — the cooldown
	// fallback should return the cooldown-shortest account, not the provider.
	got := stepLabel(h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, nil))
	if got != "account:acc1" {
		t.Fatalf("with short cooldown: got %s, want account:acc1 (provider must not win over a temporarily cooled account)", got)
	}

	// A 429 quota error triggers a 1-hour cooldown. A quota-blocked account is
	// excluded from the pool entirely (isQuotaBlocked/Reload), so the provider
	// becomes the correct fallback. Simulate that by putting the account on a
	// quota cooldown AND having it blocked in the pool via AllowOverUsage=false
	// with an exhausted limit.
	//
	// We can't easily simulate isQuotaBlocked here because it depends on config
	// fields (usageCurrent/usageLimit), but the quota-cooldown path at least
	// verifies that the extended cooldown alone is insufficient to skip to provider
	// when the account is still in the pool (not quota-blocked).
	p.RecordError("acc1", true) // 1-hour cooldown
	got = stepLabel(h.nextUpstream("", routerTestModel, config.ProviderEndpointMessages, nil))
	if got != "account:acc1" {
		t.Fatalf("with quota cooldown (account still in pool): got %s, want account:acc1", got)
	}
}
