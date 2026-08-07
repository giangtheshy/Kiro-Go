package proxy

import (
	"sort"
	"sync/atomic"

	"kiro-go/config"
)

// upstreamStep is one candidate upstream for a request: either a Kiro account
// (served through the existing translate-and-call path) or an external provider
// (served by passing the client's original body straight through).
// Exactly one of Account / Provider is non-nil.
type upstreamStep struct {
	Account *config.Account

	Provider *config.Provider
	// UpstreamModel is the provider's real model name, written over the "model"
	// field of the forwarded body.
	UpstreamModel string
	Pricing       config.ProviderPricing

	// UpstreamEndpoint is the path to call on the provider. It differs from the
	// client's endpoint when Bridge is set, because a bridged request is sent in
	// the provider's own protocol.
	UpstreamEndpoint string
	// Bridge is config.BridgeNone for passthrough, or the translation to apply.
	Bridge string
}

// nextUpstream picks the next upstream to try for a request.
//
// Kiro accounts and external providers share a Priority field which defines a
// routing tier. Tiers are walked in ascending order, and within a tier accounts
// are tried before providers, so an operator can express a chain such as
// acc1 (tier 0) -> external provider (tier 1) -> acc2 (tier 2).
//
// endpoint is one of the config.ProviderEndpoint* constants; a provider is only
// considered when it speaks that endpoint's protocol (passthrough, never
// translated) and declares a mapping for model.
//
// excluded accumulates the accounts and providers that already failed this
// request. Account IDs and provider IDs are both UUIDs, so one map covers both.
//
// Returns nil when nothing is left to try.
func (h *Handler) nextUpstream(apiKeyID, model, endpoint string, excluded map[string]bool) *upstreamStep {
	providers := eligibleProviders(model, endpoint, excluded)

	// Fast path: no external provider is in play, so routing is exactly what it
	// was before providers existed.
	if len(providers) == 0 {
		if acc := h.nextAccountForKey(apiKeyID, model, excluded); acc != nil {
			return &upstreamStep{Account: acc}
		}
		return nil
	}

	accountTiers := accountTiersByPriority(apiKeyID, excluded)

	for _, tier := range mergedTiers(accountTiers, providers) {
		if allowed := accountTiers[tier]; len(allowed) > 0 {
			if acc := h.pool.GetNextForModelBoundExcluding(model, allowed, excluded); acc != nil {
				return &upstreamStep{Account: acc}
			}
		}
		if step := h.pickProvider(providers[tier], model, endpoint); step != nil {
			return step
		}
	}

	// Every tier is exhausted. Fall back to the unrestricted account selector: it
	// carries a "shortest remaining cooldown" branch that the tier-restricted
	// variant deliberately lacks, and dropping it would regress the pure-Kiro path.
	if acc := h.nextAccountForKey(apiKeyID, model, excluded); acc != nil {
		return &upstreamStep{Account: acc}
	}
	return nil
}

// pickProvider chooses one provider from a tier using weighted round-robin.
//
// clientEndpoint is the endpoint the CLIENT called; the returned step carries the
// endpoint to call upstream, which differs when the provider needs a bridge.
func (h *Handler) pickProvider(tier []config.Provider, model, clientEndpoint string) *upstreamStep {
	if len(tier) == 0 {
		return nil
	}
	var weighted []int
	for i := range tier {
		w := tier[i].Weight
		if w < 1 {
			w = 1
		}
		if w > 100 {
			w = 100
		}
		for j := 0; j < w; j++ {
			weighted = append(weighted, i)
		}
	}
	idx := weighted[int(atomic.AddUint64(&h.providerCursor, 1)%uint64(len(weighted)))]

	p := tier[idx]
	name, pricing, ok := p.ResolveModel(model)
	if !ok {
		return nil
	}
	upstreamEndpoint, bridge, ok := p.ResolveEndpoint(clientEndpoint)
	if !ok {
		// eligibleProviders already filtered on this, so reaching here would mean the
		// two disagreed. Skip rather than send a request to the wrong protocol.
		return nil
	}
	return &upstreamStep{
		Provider:         &p,
		UpstreamModel:    name,
		Pricing:          pricing,
		UpstreamEndpoint: upstreamEndpoint,
		Bridge:           bridge,
	}
}

// eligibleProviders groups the providers that can serve this request by tier.
func eligibleProviders(model, endpoint string, excluded map[string]bool) map[int][]config.Provider {
	all := config.GetEnabledProviders()
	if len(all) == 0 {
		return nil
	}
	out := make(map[int][]config.Provider)
	for _, p := range all {
		if excluded != nil && excluded[p.ID] {
			continue
		}
		if !p.ServesEndpoint(endpoint) {
			continue
		}
		if _, _, ok := p.ResolveModel(model); !ok {
			continue
		}
		out[p.Priority] = append(out[p.Priority], p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// accountTiersByPriority groups enabled account IDs by tier, honouring the API
// key's bound-account set when it has one. A bound key whose bound accounts are
// all gone yields no tiers, which lets nextUpstream fall through to
// nextAccountForKey — the same widen-to-shared-pool rule that path already applies.
func accountTiersByPriority(apiKeyID string, excluded map[string]bool) map[int]map[string]bool {
	var bound map[string]bool
	if apiKeyID != "" {
		if entry := config.GetApiKeyEntry(apiKeyID); entry != nil && len(entry.BoundAccountIDs) > 0 {
			bound = make(map[string]bool, len(entry.BoundAccountIDs))
			for _, id := range entry.BoundAccountIDs {
				bound[id] = true
			}
		}
	}

	out := make(map[int]map[string]bool)
	for _, acc := range config.GetEnabledAccounts() {
		if excluded != nil && excluded[acc.ID] {
			continue
		}
		if bound != nil && !bound[acc.ID] {
			continue
		}
		if out[acc.Priority] == nil {
			out[acc.Priority] = make(map[string]bool)
		}
		out[acc.Priority][acc.ID] = true
	}
	return out
}

// mergedTiers returns every tier present on either side, in ascending order.
func mergedTiers(accounts map[int]map[string]bool, providers map[int][]config.Provider) []int {
	seen := make(map[int]bool, len(accounts)+len(providers))
	for tier := range accounts {
		seen[tier] = true
	}
	for tier := range providers {
		seen[tier] = true
	}
	tiers := make([]int, 0, len(seen))
	for tier := range seen {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)
	return tiers
}
