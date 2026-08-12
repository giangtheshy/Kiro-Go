package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Provider protocols. A provider natively serves the client endpoints that speak
// its own protocol, where the request is forwarded verbatim (passthrough). With
// AllowProtocolBridge it also serves the other protocol's endpoint by translating
// the request and the response.
const (
	ProviderProtocolAnthropic = "anthropic"
	ProviderProtocolOpenAI    = "openai"
)

// Bridge modes describe the translation a provider needs to serve a client
// endpoint. BridgeNone is the passthrough case.
const (
	BridgeNone = ""
	// BridgeOpenAIToAnthropic: the client spoke /v1/chat/completions, the provider
	// speaks /v1/messages.
	BridgeOpenAIToAnthropic = "openai->anthropic"
	// BridgeAnthropicToOpenAI: the client spoke /v1/messages, the provider speaks
	// /v1/chat/completions. This is the case that matters for Claude Code, which
	// only ever calls /v1/messages.
	BridgeAnthropicToOpenAI = "anthropic->openai"
	// BridgeResponsesToAnthropic: the client spoke /v1/responses, the provider
	// speaks /v1/messages.
	BridgeResponsesToAnthropic = "responses->anthropic"
	// BridgeResponsesToOpenAI: the client spoke /v1/responses, the provider only
	// implements /v1/chat/completions. This is the common case — most cheap
	// OpenAI-compatible upstreams never implement the Responses API.
	BridgeResponsesToOpenAI = "responses->chat"
)

// ProviderPricing is the credit price per 1M tokens for one traffic class.
// The unit is the same "credit" that ApiKeyEntry.CreditLimit is denominated in,
// so an operator can price external capacity against the Kiro credit they sell.
type ProviderPricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheWrite float64 `json:"cacheWrite"`
	CacheRead  float64 `json:"cacheRead"`
}

// IsZero reports whether no price at all was configured.
func (p ProviderPricing) IsZero() bool {
	return p.Input == 0 && p.Output == 0 && p.CacheWrite == 0 && p.CacheRead == 0
}

// ProviderModel maps a client-visible model name onto the provider's real model.
// Alias is what the caller sends (normally a Kiro model ID such as
// "claude-sonnet-4.5"); Name is what gets written into the forwarded body.
type ProviderModel struct {
	Alias   string           `json:"alias"`
	Name    string           `json:"name"`
	Pricing *ProviderPricing `json:"pricing,omitempty"` // nil = inherit the provider default
}

// Provider is one external OpenAI- or Anthropic-compatible upstream that can serve
// requests when (or before) the Kiro account pool does. Requests are passed through
// unchanged apart from the model field, so the provider must speak the same wire
// protocol as the client endpoint it serves.
type Provider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Protocol string `json:"protocol"` // ProviderProtocolAnthropic | ProviderProtocolOpenAI
	BaseURL  string `json:"baseUrl"`  // e.g. https://api.z.ai/api/anthropic
	APIKey   string `json:"apiKey"`

	Headers  map[string]string `json:"headers,omitempty"`
	ProxyURL string            `json:"proxyUrl,omitempty"` // empty = fall back to the global proxy

	// Priority is the routing tier. Upstreams are tried in ascending tier order and
	// Kiro accounts carry the same field, so an operator can interleave them
	// (e.g. acc1=0, provider=1, acc2=2). Everything at the default 0 means the
	// provider is a plain fallback behind the whole Kiro pool.
	Priority int `json:"priority,omitempty"`

	// Weight is the round-robin share among providers sharing a tier (0 or 1 = normal).
	Weight int `json:"weight,omitempty"`

	// SupportsResponses gates /v1/responses. Only meaningful for the OpenAI protocol,
	// and off by default because most OpenAI-compatible upstreams implement only
	// /v1/chat/completions.
	SupportsResponses bool `json:"supportsResponses,omitempty"`

	// AllowProtocolBridge lets this provider serve the OTHER protocol's endpoint by
	// translating the request and response instead of passing them through.
	//
	// Off by default: passthrough is lossless, whereas a bridge is a best-effort
	// re-shaping that cannot carry everything (see the per-direction notes in
	// proxy/provider_bridge.go). Turning it on is the operator saying "an
	// approximate answer from this provider beats no answer at all", which is
	// exactly the trade a fallback tier exists to make.
	//
	// The practical reason it exists: Claude Code only ever calls /v1/messages, so
	// without a bridge an OpenAI-only provider can never serve Claude Code traffic
	// no matter how it is priced or prioritised.
	AllowProtocolBridge bool `json:"allowProtocolBridge,omitempty"`

	Models  []ProviderModel `json:"models"`
	Pricing ProviderPricing `json:"pricing"`

	// Runtime statistics, updated on the request hot path via markDirtyLocked.
	RequestCount    int64   `json:"requestCount,omitempty"`
	ErrorCount      int64   `json:"errorCount,omitempty"`
	TotalTokens     int64   `json:"totalTokens,omitempty"`
	InputTokens     int64   `json:"inputTokens,omitempty"`
	OutputTokens    int64   `json:"outputTokens,omitempty"`
	CacheWriteTokens int64  `json:"cacheWriteTokens,omitempty"`
	CacheReadTokens  int64  `json:"cacheReadTokens,omitempty"`
	TotalCredits    float64 `json:"totalCredits,omitempty"`
	LastUsed        int64   `json:"lastUsed,omitempty"`
}

// ResolveModel maps a client-requested model onto this provider's upstream model
// name and the pricing that applies to it. Matching is case-insensitive on the
// alias. ok is false when the provider does not serve the model at all, which is
// how the router decides to skip it.
func (p *Provider) ResolveModel(alias string) (name string, pricing ProviderPricing, ok bool) {
	want := strings.ToLower(strings.TrimSpace(alias))
	if want == "" {
		return "", ProviderPricing{}, false
	}
	for i := range p.Models {
		m := &p.Models[i]
		if strings.ToLower(strings.TrimSpace(m.Alias)) != want {
			continue
		}
		upstream := strings.TrimSpace(m.Name)
		if upstream == "" {
			upstream = strings.TrimSpace(m.Alias)
		}
		if m.Pricing != nil {
			return upstream, *m.Pricing, true
		}
		return upstream, p.Pricing, true
	}
	return "", ProviderPricing{}, false
}

// ResolveEndpoint decides how this provider can serve a client endpoint.
//
// upstreamEndpoint is the path to call on the provider, which is NOT always the
// path the client used: a bridged request is sent to the provider's own protocol.
// bridge is BridgeNone for passthrough, or the translation to apply.
//
// ok is false when the provider cannot serve the endpoint at all, which is how the
// router decides to skip it and try the next upstream.
func (p *Provider) ResolveEndpoint(clientEndpoint string) (upstreamEndpoint, bridge string, ok bool) {
	switch clientEndpoint {
	case ProviderEndpointMessages:
		if p.Protocol == ProviderProtocolAnthropic {
			return ProviderEndpointMessages, BridgeNone, true
		}
		if p.AllowProtocolBridge {
			return ProviderEndpointChatCompletions, BridgeAnthropicToOpenAI, true
		}
	case ProviderEndpointChatCompletions:
		if p.Protocol == ProviderProtocolOpenAI {
			return ProviderEndpointChatCompletions, BridgeNone, true
		}
		if p.AllowProtocolBridge {
			return ProviderEndpointMessages, BridgeOpenAIToAnthropic, true
		}
	case ProviderEndpointResponses:
		// Passthrough first: a provider that really implements the Responses API
		// serves it losslessly, including the parts a bridge cannot carry.
		if p.Protocol == ProviderProtocolOpenAI && p.SupportsResponses {
			return ProviderEndpointResponses, BridgeNone, true
		}
		if p.AllowProtocolBridge {
			// Almost no cheap OpenAI-compatible upstream implements /v1/responses,
			// so without this a Responses client (the current OpenAI SDK default,
			// and Codex) could never reach an external provider at all.
			//
			// What the bridge cannot carry, in either direction: server-side
			// conversation state (previous_response_id / store), signed reasoning
			// items replayed across turns, and OpenAI's server-side built-in tools.
			// The handler flattens history into the request before routing, so a
			// well-behaved client loses nothing that affects the answer.
			if p.Protocol == ProviderProtocolOpenAI {
				return ProviderEndpointChatCompletions, BridgeResponsesToOpenAI, true
			}
			return ProviderEndpointMessages, BridgeResponsesToAnthropic, true
		}
	}
	return "", BridgeNone, false
}

// ServesEndpoint reports whether this provider can serve the given client
// endpoint, by passthrough or by bridging.
func (p *Provider) ServesEndpoint(endpoint string) bool {
	_, _, ok := p.ResolveEndpoint(endpoint)
	return ok
}

// Client endpoints a provider may be routed to. The values double as the upstream
// path suffix appended to Provider.BaseURL.
const (
	ProviderEndpointMessages        = "/v1/messages"
	ProviderEndpointChatCompletions = "/v1/chat/completions"
	ProviderEndpointResponses       = "/v1/responses"
)

// GetProviders returns a snapshot of every configured provider.
func GetProviders() []Provider {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]Provider, len(cfg.Providers))
	copy(out, cfg.Providers)
	return out
}

// GetEnabledProviders returns only the providers that are switched on.
func GetEnabledProviders() []Provider {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]Provider, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// GetProvider returns a copy of the provider with the given ID, or nil.
func GetProvider(id string) *Provider {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			cp := cfg.Providers[i]
			return &cp
		}
	}
	return nil
}

// AddProvider validates and appends a new provider, assigning an ID when missing.
func AddProvider(p Provider) (Provider, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return Provider{}, errors.New("config not initialized")
	}
	normalized, err := normalizeProvider(p)
	if err != nil {
		return Provider{}, err
	}
	if normalized.ID == "" {
		normalized.ID = newUUID()
	}
	for _, existing := range cfg.Providers {
		if existing.ID == normalized.ID {
			return Provider{}, errors.New("provider already exists")
		}
	}

	cfg.Providers = append(cfg.Providers, normalized)
	if err := saveLocked(); err != nil {
		cfg.Providers = cfg.Providers[:len(cfg.Providers)-1]
		return Provider{}, err
	}
	return normalized, nil
}

// UpdateProvider replaces the provider with the given ID. An empty APIKey in the
// patch keeps the stored key, so the admin UI can round-trip a masked value without
// wiping the credential.
func UpdateProvider(id string, patch Provider) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("provider not found")
	}

	prev := cfg.Providers[idx]
	patch.ID = id
	if strings.TrimSpace(patch.APIKey) == "" {
		patch.APIKey = prev.APIKey
	}
	normalized, err := normalizeProvider(patch)
	if err != nil {
		return err
	}
	// Counters belong to the runtime, not to the edit form.
	normalized.RequestCount = prev.RequestCount
	normalized.ErrorCount = prev.ErrorCount
	normalized.TotalTokens = prev.TotalTokens
	normalized.TotalCredits = prev.TotalCredits
	normalized.LastUsed = prev.LastUsed

	cfg.Providers[idx] = normalized
	if err := saveLocked(); err != nil {
		cfg.Providers[idx] = prev
		return err
	}
	return nil
}

// DeleteProvider removes a provider by ID.
func DeleteProvider(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			removed := cfg.Providers[i]
			cfg.Providers = append(cfg.Providers[:i], cfg.Providers[i+1:]...)
			if err := saveLocked(); err != nil {
				cfg.Providers = append(cfg.Providers, Provider{})
				copy(cfg.Providers[i+1:], cfg.Providers[i:])
				cfg.Providers[i] = removed
				return err
			}
			return nil
		}
	}
	return errors.New("provider not found")
}

// ResetProviderUsage clears the runtime counters for one provider.
func ResetProviderUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].ID == id {
			cfg.Providers[i].RequestCount = 0
			cfg.Providers[i].ErrorCount = 0
			cfg.Providers[i].TotalTokens = 0
			cfg.Providers[i].TotalCredits = 0
			return saveLocked()
		}
	}
	return errors.New("provider not found")
}

// ProviderUsageBreakdown carries detailed token accounting for a single provider request.
type ProviderUsageBreakdown struct {
	InputTokens     int64
	OutputTokens    int64
	CacheWriteTokens int64
	CacheReadTokens  int64
	Credits         float64
}

// RecordProviderUsage folds one request's outcome into the provider counters. This
// runs on the request hot path, so it only marks the config dirty and lets the
// background flusher persist — same contract as RecordApiKeyUsage.
//
// Deprecated: use RecordProviderUsageDetailed for new call sites.
func RecordProviderUsage(id string, tokens int64, credits float64, failed bool) {
	RecordProviderUsageDetailed(id, ProviderUsageBreakdown{
		InputTokens:  tokens / 2, // rough split for legacy callers
		OutputTokens: tokens / 2,
		Credits:      credits,
	}, failed)
}

// RecordProviderUsageDetailed updates provider stats with full token breakdown.
func RecordProviderUsageDetailed(id string, usage ProviderUsageBreakdown, failed bool) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return
	}
	for i := range cfg.Providers {
		if cfg.Providers[i].ID != id {
			continue
		}
		p := &cfg.Providers[i]
		if failed {
			p.ErrorCount++
		} else {
			p.RequestCount++
			if usage.InputTokens > 0 {
				p.InputTokens += usage.InputTokens
				p.TotalTokens += usage.InputTokens
			}
			if usage.OutputTokens > 0 {
				p.OutputTokens += usage.OutputTokens
				p.TotalTokens += usage.OutputTokens
			}
			if usage.CacheWriteTokens > 0 {
				p.CacheWriteTokens += usage.CacheWriteTokens
			}
			if usage.CacheReadTokens > 0 {
				p.CacheReadTokens += usage.CacheReadTokens
			}
			if usage.Credits > 0 {
				p.TotalCredits += usage.Credits
			}
		}
		p.LastUsed = time.Now().Unix()
		markDirtyLocked()
		return
	}
}

// normalizeProvider trims, defaults and validates a provider before it is stored.
func normalizeProvider(p Provider) (Provider, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return Provider{}, errors.New("provider name must not be empty")
	}

	p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
	if p.Protocol != ProviderProtocolAnthropic && p.Protocol != ProviderProtocolOpenAI {
		return Provider{}, fmt.Errorf("provider protocol must be %q or %q", ProviderProtocolAnthropic, ProviderProtocolOpenAI)
	}
	if p.Protocol != ProviderProtocolOpenAI {
		p.SupportsResponses = false
	}

	p.BaseURL = strings.TrimSpace(p.BaseURL)
	if err := validateProviderBaseURL(p.BaseURL); err != nil {
		return Provider{}, err
	}
	p.BaseURL = strings.TrimSuffix(p.BaseURL, "/")

	p.APIKey = strings.TrimSpace(p.APIKey)
	p.ProxyURL = strings.TrimSpace(p.ProxyURL)
	if p.Priority < 0 {
		p.Priority = 0
	}
	if p.Weight < 0 {
		p.Weight = 0
	}

	if err := validatePricing("provider", p.Pricing); err != nil {
		return Provider{}, err
	}

	models := make([]ProviderModel, 0, len(p.Models))
	seen := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		m.Alias = strings.TrimSpace(m.Alias)
		m.Name = strings.TrimSpace(m.Name)
		if m.Alias == "" {
			return Provider{}, errors.New("model alias must not be empty")
		}
		if m.Name == "" {
			m.Name = m.Alias
		}
		key := strings.ToLower(m.Alias)
		if seen[key] {
			return Provider{}, fmt.Errorf("duplicate model alias %q", m.Alias)
		}
		seen[key] = true
		if m.Pricing != nil {
			if err := validatePricing("model "+m.Alias, *m.Pricing); err != nil {
				return Provider{}, err
			}
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return Provider{}, errors.New("provider must declare at least one model mapping")
	}
	p.Models = models

	headers := make(map[string]string, len(p.Headers))
	for k, v := range p.Headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		headers[k] = strings.TrimSpace(v)
	}
	if len(headers) == 0 {
		headers = nil
	}
	p.Headers = headers

	return p, nil
}

func validateProviderBaseURL(raw string) error {
	if raw == "" {
		return errors.New("provider base URL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid provider base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("provider base URL must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("provider base URL must include a host")
	}
	return nil
}

func validatePricing(what string, p ProviderPricing) error {
	for _, v := range []float64{p.Input, p.Output, p.CacheWrite, p.CacheRead} {
		if v < 0 {
			return fmt.Errorf("%s pricing must not be negative", what)
		}
	}
	return nil
}
