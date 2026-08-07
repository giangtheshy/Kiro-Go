package config

import (
	"path/filepath"
	"testing"
)

// newProviderTestConfig points the package at a throwaway config file so each
// test starts from a clean provider list.
func newProviderTestConfig(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
}

func sampleProvider() Provider {
	return Provider{
		Name:     "zai",
		Enabled:  true,
		Protocol: ProviderProtocolAnthropic,
		BaseURL:  "https://api.z.ai/api/anthropic/",
		APIKey:   "secret-key",
		Models: []ProviderModel{
			{Alias: "claude-sonnet-4.5", Name: "glm-4.6"},
			{Alias: "claude-opus-4.6"},
		},
		Pricing: ProviderPricing{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.3},
	}
}

func TestAddProviderNormalizesAndPersists(t *testing.T) {
	newProviderTestConfig(t)

	created, err := AddProvider(sampleProvider())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected a generated ID")
	}
	if created.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Fatalf("expected trailing slash trimmed, got %q", created.BaseURL)
	}
	// An omitted upstream name means "same name on both sides".
	if created.Models[1].Name != "claude-opus-4.6" {
		t.Fatalf("expected alias to default as upstream name, got %q", created.Models[1].Name)
	}

	stored := GetProvider(created.ID)
	if stored == nil || stored.APIKey != "secret-key" {
		t.Fatalf("provider not persisted with its key: %+v", stored)
	}
	if len(GetEnabledProviders()) != 1 {
		t.Fatalf("expected one enabled provider")
	}
}

func TestAddProviderRejectsInvalidInput(t *testing.T) {
	newProviderTestConfig(t)

	cases := map[string]func(p *Provider){
		"empty name":        func(p *Provider) { p.Name = "" },
		"unknown protocol":  func(p *Provider) { p.Protocol = "gemini" },
		"relative base url": func(p *Provider) { p.BaseURL = "api.example.com" },
		"no models":         func(p *Provider) { p.Models = nil },
		"empty alias":       func(p *Provider) { p.Models = []ProviderModel{{Alias: "  "}} },
		"duplicate alias": func(p *Provider) {
			p.Models = []ProviderModel{{Alias: "m"}, {Alias: "M"}}
		},
		"negative price": func(p *Provider) { p.Pricing.Input = -1 },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := sampleProvider()
			mutate(&p)
			if _, err := AddProvider(p); err == nil {
				t.Fatalf("expected rejection for %s", name)
			}
		})
	}
}

func TestAddProviderClearsResponsesFlagForAnthropic(t *testing.T) {
	newProviderTestConfig(t)

	p := sampleProvider()
	p.SupportsResponses = true
	created, err := AddProvider(p)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if created.SupportsResponses {
		t.Fatal("supportsResponses only applies to the openai protocol")
	}
}

func TestUpdateProviderKeepsKeyAndCounters(t *testing.T) {
	newProviderTestConfig(t)

	created, err := AddProvider(sampleProvider())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	RecordProviderUsage(created.ID, 1200, 4.5, false)

	patch := sampleProvider()
	patch.Name = "zai-renamed"
	patch.APIKey = "" // the admin UI round-trips a masked value
	if err := UpdateProvider(created.ID, patch); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := GetProvider(created.ID)
	if got == nil {
		t.Fatal("provider vanished")
	}
	if got.Name != "zai-renamed" {
		t.Fatalf("name not applied: %q", got.Name)
	}
	if got.APIKey != "secret-key" {
		t.Fatalf("expected stored key to survive an empty patch, got %q", got.APIKey)
	}
	if got.RequestCount != 1 || got.TotalTokens != 1200 || got.TotalCredits != 4.5 {
		t.Fatalf("counters must not be reset by an edit: %+v", got)
	}
}

func TestDeleteProvider(t *testing.T) {
	newProviderTestConfig(t)

	created, err := AddProvider(sampleProvider())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := DeleteProvider(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if GetProvider(created.ID) != nil {
		t.Fatal("provider still present after delete")
	}
	if err := DeleteProvider(created.ID); err == nil {
		t.Fatal("expected an error deleting a missing provider")
	}
}

func TestResolveModel(t *testing.T) {
	override := ProviderPricing{Input: 1, Output: 2}
	p := Provider{
		Pricing: ProviderPricing{Input: 3, Output: 15},
		Models: []ProviderModel{
			{Alias: "claude-sonnet-4.5", Name: "glm-4.6"},
			{Alias: "cheap", Name: "mini", Pricing: &override},
		},
	}

	name, pricing, ok := p.ResolveModel("Claude-Sonnet-4.5")
	if !ok || name != "glm-4.6" {
		t.Fatalf("expected case-insensitive alias match, got %q ok=%t", name, ok)
	}
	if pricing.Input != 3 {
		t.Fatalf("expected provider-level pricing, got %+v", pricing)
	}

	_, pricing, ok = p.ResolveModel("cheap")
	if !ok || pricing.Input != 1 || pricing.Output != 2 {
		t.Fatalf("expected per-model pricing override, got %+v ok=%t", pricing, ok)
	}

	if _, _, ok = p.ResolveModel("claude-haiku-4.5"); ok {
		t.Fatal("expected an unmapped model to be rejected")
	}
	if _, _, ok = p.ResolveModel(""); ok {
		t.Fatal("expected an empty model to be rejected")
	}
}

func TestServesEndpoint(t *testing.T) {
	anthropic := Provider{Protocol: ProviderProtocolAnthropic}
	openai := Provider{Protocol: ProviderProtocolOpenAI}
	openaiResponses := Provider{Protocol: ProviderProtocolOpenAI, SupportsResponses: true}

	cases := []struct {
		name     string
		p        Provider
		endpoint string
		want     bool
	}{
		{"anthropic serves messages", anthropic, ProviderEndpointMessages, true},
		{"anthropic skips chat", anthropic, ProviderEndpointChatCompletions, false},
		{"anthropic skips responses", anthropic, ProviderEndpointResponses, false},
		{"openai serves chat", openai, ProviderEndpointChatCompletions, true},
		{"openai skips messages", openai, ProviderEndpointMessages, false},
		{"openai skips responses without flag", openai, ProviderEndpointResponses, false},
		{"openai serves responses with flag", openaiResponses, ProviderEndpointResponses, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.ServesEndpoint(tc.endpoint); got != tc.want {
				t.Fatalf("ServesEndpoint(%q) = %t, want %t", tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestRecordProviderUsageFailureOnlyCountsErrors(t *testing.T) {
	newProviderTestConfig(t)

	created, err := AddProvider(sampleProvider())
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	RecordProviderUsage(created.ID, 500, 2, false)
	RecordProviderUsage(created.ID, 999, 9, true)

	got := GetProvider(created.ID)
	if got.RequestCount != 1 {
		t.Fatalf("failed requests must not raise RequestCount, got %d", got.RequestCount)
	}
	if got.ErrorCount != 1 {
		t.Fatalf("expected one error, got %d", got.ErrorCount)
	}
	if got.TotalTokens != 500 || got.TotalCredits != 2 {
		t.Fatalf("failed requests must not be billed: tokens=%d credits=%v", got.TotalTokens, got.TotalCredits)
	}
}
