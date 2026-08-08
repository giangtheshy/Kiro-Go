package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// RequestLogEntry is a single served API request, captured for the admin "API Log" view.
// It records which API key was used, the model, token/credit cost and the serving account.
type RequestLogEntry struct {
	Time             int64   `json:"time"`               // Unix seconds
	Status           string  `json:"status"`             // "ok" or "error"
	Endpoint         string  `json:"endpoint,omitempty"` // "claude" or "openai"
	APIKeyID         string  `json:"apiKeyId,omitempty"`
	APIKeyName       string  `json:"apiKeyName,omitempty"`
	APIKeyMasked     string  `json:"apiKeyMasked,omitempty"`
	Model            string  `json:"model,omitempty"`
	AccountID        string  `json:"accountId,omitempty"`
	AccountEmail     string  `json:"accountEmail,omitempty"`
	// InputTokens is the FULL prompt size, cache traffic included. It is what the
	// upstream charged and what the per-key quota counts, so it stays the raw
	// total — the self-service /check view reports this field directly.
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	// UncachedInputTokens is InputTokens with the cache traffic taken out, i.e.
	// the freshly-processed part of the prompt. Derived in logRequest, never set
	// by callers.
	//
	// It exists because "input" means two different things in this codebase and
	// the admin table needs the one that adds up: this field plus CacheRead plus
	// CacheWrite plus Output equals TotalTokens, so an operator can read the
	// breakdown as four disjoint parts. It also matches the input_tokens the
	// client itself receives (see billedClaudeInputTokens), which InputTokens
	// does not.
	UncachedInputTokens int `json:"uncachedInputTokens"`
	CacheReadTokens     int `json:"cacheReadTokens,omitempty"`  // prompt-cache read tokens
	CacheWriteTokens    int `json:"cacheWriteTokens,omitempty"` // prompt-cache write tokens
	TotalTokens         int `json:"totalTokens"`
	Credits          float64 `json:"credits"`
	DurationMs       int64   `json:"durationMs"`
	StatusCode       int     `json:"statusCode,omitempty"` // upstream/HTTP status on error
	Error            string  `json:"error,omitempty"`      // error detail on failure
	// ClientIP is the resolved real client IP address. Populated only when
	// KIRO_TRUST_PROXY is enabled and the IP could be determined. The per-key
	// self-service log endpoint strips this field before returning entries to
	// the key owner: it is admin-only information.
	ClientIP string `json:"clientIp,omitempty"`
}

// requestUsage bundles the token/credit/cache/ip attribution for a single
// successful upstream round. Using a struct avoids a 10+ parameter function
// signature and makes call sites self-documenting.
type requestUsage struct {
	Input      int
	Output     int
	CacheRead  int // prompt-cache read tokens (Anthropic) or cached prompt tokens (OpenAI)
	CacheWrite int // prompt-cache creation tokens (Anthropic)
	Credits    float64
	ClientIP   string
}

// requestLogCapacity is the number of recent request entries retained in memory.
// Per-request only — counters/limits persist on the ApiKeyEntry; this is just a live feed.
const requestLogCapacity = 1000

// requestLogBuffer is a fixed-size in-memory ring of recent requests. Oldest entries are
// overwritten once full. Not persisted: it is a runtime diagnostic feed, gone on restart.
// It also fans new entries out to any live SSE subscribers (the admin "API Log" view).
type requestLogBuffer struct {
	mu          sync.Mutex
	ring        []RequestLogEntry
	next        int
	full        bool
	subscribers map[chan RequestLogEntry]struct{}
}

var requestLog = &requestLogBuffer{
	ring:        make([]RequestLogEntry, requestLogCapacity),
	subscribers: make(map[chan RequestLogEntry]struct{}),
}

func (b *requestLogBuffer) add(e RequestLogEntry) {
	b.mu.Lock()
	b.ring[b.next] = e
	b.next = (b.next + 1) % requestLogCapacity
	if b.next == 0 {
		b.full = true
	}
	subs := make([]chan RequestLogEntry, 0, len(b.subscribers))
	for ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	// Fan out outside the lock and non-blocking: a stalled SSE client drops
	// entries rather than back-pressuring the request path that logged them.
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// subscribe returns a buffered channel fed by add and a cancel function the
// subscriber must call on disconnect.
func (b *requestLogBuffer) subscribe() (<-chan RequestLogEntry, func()) {
	ch := make(chan RequestLogEntry, 64)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subscribers[ch]; ok {
				delete(b.subscribers, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// reset drops the retained history. Live subscribers are left connected — they
// are a feed of new activity, not of the buffer contents.
func (b *requestLogBuffer) reset() {
	b.mu.Lock()
	b.ring = make([]RequestLogEntry, requestLogCapacity)
	b.next = 0
	b.full = false
	b.mu.Unlock()
}

// snapshot returns retained entries newest-first.
func (b *requestLogBuffer) snapshot() []RequestLogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []RequestLogEntry
	if !b.full {
		out = make([]RequestLogEntry, b.next)
		copy(out, b.ring[:b.next])
	} else {
		out = make([]RequestLogEntry, 0, requestLogCapacity)
		out = append(out, b.ring[b.next:]...)
		out = append(out, b.ring[:b.next]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// logRequest appends a served-request entry to the in-memory feed. apiKeyID may be empty
// (unauthenticated / legacy single-key path), in which case key fields are left blank.
func logRequest(e RequestLogEntry) {
	if e.Time == 0 {
		e.Time = time.Now().Unix()
	}
	if e.Status == "" {
		e.Status = "ok"
	}
	e.TotalTokens = e.InputTokens + e.OutputTokens
	// Derived centrally so every serving path gets it, and clamped at zero: the
	// cache counts come from an estimator on some paths and could in principle
	// exceed the recorded prompt size, which must not render as a negative column.
	e.UncachedInputTokens = maxInt(e.InputTokens-e.CacheReadTokens-e.CacheWriteTokens, 0)
	requestLog.add(e)
}

// apiGetRequestLogs GET /admin/api/request-logs - returns the recent per-request feed.
// Optional query params:
//   - apiKeyId: only entries for that key
//   - limit:    cap the number of returned entries (default 200, max requestLogCapacity)
func (h *Handler) apiGetRequestLogs(w http.ResponseWriter, r *http.Request) {
	entries := requestLog.snapshot()

	if keyID := r.URL.Query().Get("apiKeyId"); keyID != "" {
		filtered := entries[:0:0]
		for _, e := range entries {
			if e.APIKeyID == keyID {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > requestLogCapacity {
		limit = requestLogCapacity
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"logs": entries})
}

// apiStreamRequestLogs GET /admin/api/request-logs/stream - live SSE feed of served
// requests, backing the admin "API Log" view so it no longer has to poll.
//
// Unlike the customer stream (/v1/key/logs/stream) this carries the whole
// RequestLogEntry, ClientIP included: it is admin-only information and the admin
// view offers a one-click ban on it. History is NOT replayed here — the view does
// its initial fill with GET /admin/api/request-logs.
func (h *Handler) apiStreamRequestLogs(w http.ResponseWriter, r *http.Request) {
	// handleAdminAPI sets application/json for every route; sseHeadersSet
	// overrides it (and sets X-Accel-Buffering: no).
	flusher, ok := sseHeadersSet(w)
	if !ok {
		return
	}
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	flusher.Flush()

	ch, cancel := requestLog.subscribe()
	defer cancel()

	ping := time.NewTicker(keyLogPingInterval)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-ch:
			if !open {
				return
			}
			if err := sseWriteEvent(w, flusher, map[string]interface{}{
				"type": "requestlog",
				"data": e,
			}); err != nil {
				return
			}
		case <-ping.C:
			if err := sseWritePing(w, flusher); err != nil {
				return
			}
		}
	}
}

// apiClearRequestLogs DELETE /admin/api/request-logs - drops the in-memory request feed.
func (h *Handler) apiClearRequestLogs(w http.ResponseWriter, r *http.Request) {
	requestLog.reset()
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}

// apiKeyUsageView is the per-key usage summary returned by apiGetUsageSummary.
type apiKeyUsageView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	KeyMasked     string  `json:"keyMasked"`
	Enabled       bool    `json:"enabled"`
	TokenLimit    int64   `json:"tokenLimit"`
	CreditLimit   float64 `json:"creditLimit"`
	TokensUsed    int64   `json:"tokensUsed"`
	CreditsUsed   float64 `json:"creditsUsed"`
	RequestsCount int64   `json:"requestsCount"`
	TokensRemain  int64   `json:"tokensRemain"`  // -1 = unlimited
	CreditsRemain float64 `json:"creditsRemain"` // -1 = unlimited
	OverToken     bool    `json:"overToken"`
	OverCredit    bool    `json:"overCredit"`
	Expired       bool    `json:"expired"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
	ExpiresAt     int64   `json:"expiresAt,omitempty"`
}

// apiGetUsageSummary GET /admin/api/usage-summary - per-key credit/token usage vs limits.
// Backs the admin "Usage Check" view so operators can see, at a glance, how much each key
// has consumed and how much quota remains.
func (h *Handler) apiGetUsageSummary(w http.ResponseWriter, r *http.Request) {
	entries := config.ListApiKeys()
	out := make([]apiKeyUsageView, 0, len(entries))

	var totalTokens, totalLimitTokens int64
	var totalCredits, totalLimitCredits float64
	var totalRequests int64

	for _, e := range entries {
		overToken, overCredit := config.ApiKeyOverLimit(e)
		tokensRemain := int64(-1)
		if e.TokenLimit > 0 {
			tokensRemain = e.TokenLimit - e.TokensUsed
			if tokensRemain < 0 {
				tokensRemain = 0
			}
		}
		creditsRemain := float64(-1)
		if e.CreditLimit > 0 {
			creditsRemain = e.CreditLimit - e.CreditsUsed
			if creditsRemain < 0 {
				creditsRemain = 0
			}
		}
		out = append(out, apiKeyUsageView{
			ID:            e.ID,
			Name:          e.Name,
			KeyMasked:     config.MaskApiKey(e.Key),
			Enabled:       e.Enabled,
			TokenLimit:    e.TokenLimit,
			CreditLimit:   e.CreditLimit,
			TokensUsed:    e.TokensUsed,
			CreditsUsed:   e.CreditsUsed,
			RequestsCount: e.RequestsCount,
			TokensRemain:  tokensRemain,
			CreditsRemain: creditsRemain,
			OverToken:     overToken,
			OverCredit:    overCredit,
			Expired:       config.ApiKeyExpired(e),
			LastUsedAt:    e.LastUsedAt,
			ExpiresAt:     e.ExpiresAt,
		})
		totalTokens += e.TokensUsed
		totalLimitTokens += e.TokenLimit
		totalCredits += e.CreditsUsed
		totalLimitCredits += e.CreditLimit
		totalRequests += e.RequestsCount
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"keys": out,
		"totals": map[string]interface{}{
			"tokensUsed":   totalTokens,
			"tokenLimit":   totalLimitTokens,
			"creditsUsed":  totalCredits,
			"creditLimit":  totalLimitCredits,
			"requests":     totalRequests,
			"requestCount": totalRequests,
		},
	})
}

// apiKeySelfInfo is the public self-service payload. It intentionally omits the
// key ID, name, masked value and internal flags — a customer checking their own
// key only needs quota/usage/expiry, not admin metadata.
type apiKeySelfInfo struct {
	Enabled       bool            `json:"enabled"`
	TokenLimit    int64           `json:"tokenLimit"`
	CreditLimit   float64         `json:"creditLimit"`
	TokensUsed    int64           `json:"tokensUsed"`
	CreditsUsed   float64         `json:"creditsUsed"`
	RequestsCount int64           `json:"requestsCount"`
	TokensRemain  int64           `json:"tokensRemain"`  // -1 = unlimited
	CreditsRemain float64         `json:"creditsRemain"` // -1 = unlimited
	OverToken     bool            `json:"overToken"`
	OverCredit    bool            `json:"overCredit"`
	Expired       bool            `json:"expired"`
	ExpiresAt     int64           `json:"expiresAt,omitempty"`
	Valid         bool            `json:"valid"`             // false when the key is disabled/expired/over-limit
	BaseURL       string          `json:"baseURL,omitempty"` // externally reachable API base, so the customer knows where to point their client
	DailyTokens   int64           `json:"dailyTokens"`       // input+output tokens since local midnight (in-memory, resets on restart)
	ByModel       []modelStatView `json:"byModel"`           // per-model breakdown (in-memory, resets on restart)
}

// apiKeySelfLogEntry is one row of a customer's own usage history. Deliberately
// excludes account/key identifiers — a customer only needs to see what they spent.
type apiKeySelfLogEntry struct {
	Time         int64   `json:"time"`
	Model        string  `json:"model,omitempty"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	TotalTokens  int     `json:"totalTokens"`
	Credits      float64 `json:"credits"`
}

// apiKeySelfLogs GET /v1/key/logs — public self-service usage history, scoped to the
// caller's own key (same auth as apiKeySelfInfo). Backs the portal's usage log/ledger.
func (h *Handler) apiKeySelfLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	provided := extractProvidedKey(r)
	if provided == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}
	entry := config.FindApiKeyByValue(provided)
	if entry == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > requestLogCapacity {
		limit = requestLogCapacity
	}

	out := make([]apiKeySelfLogEntry, 0, limit)
	for _, e := range requestLog.snapshot() {
		if e.APIKeyID != entry.ID {
			continue
		}
		out = append(out, apiKeySelfLogEntry{
			Time:         e.Time,
			Model:        e.Model,
			InputTokens:  e.InputTokens,
			OutputTokens: e.OutputTokens,
			TotalTokens:  e.TotalTokens,
			Credits:      e.Credits,
		})
		if len(out) >= limit {
			break
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"logs": out})
}

// apiKeySelfInfo GET/POST /v1/key/info — public self-service usage lookup.
//
// Unlike the /admin/* endpoints (admin-password gated), this authenticates with
// the CUSTOMER'S OWN key (Authorization: Bearer <key> or X-Api-Key) and returns
// only that key's usage. This backs the customer-facing "check your balance"
// portal for an API-reselling setup, so customers never touch the admin console.
//
// Returns 401 for a missing/unknown key. A disabled/expired/over-limit key still
// returns 200 with its usage but Valid=false, so customers can see WHY it stopped.
func (h *Handler) apiKeySelfInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	provided := extractProvidedKey(r)
	if provided == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}
	entry := config.FindApiKeyByValue(provided)
	if entry == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}

	overToken, overCredit := config.ApiKeyOverLimit(*entry)
	expired := config.ApiKeyExpired(*entry)

	tokensRemain := int64(-1)
	if entry.TokenLimit > 0 {
		tokensRemain = entry.TokenLimit - entry.TokensUsed
		if tokensRemain < 0 {
			tokensRemain = 0
		}
	}
	creditsRemain := float64(-1)
	if entry.CreditLimit > 0 {
		creditsRemain = entry.CreditLimit - entry.CreditsUsed
		if creditsRemain < 0 {
			creditsRemain = 0
		}
	}

	usage := h.usage.snapshot(entry.ID)

	json.NewEncoder(w).Encode(apiKeySelfInfo{
		Enabled:       entry.Enabled,
		TokenLimit:    entry.TokenLimit,
		CreditLimit:   entry.CreditLimit,
		TokensUsed:    entry.TokensUsed,
		CreditsUsed:   entry.CreditsUsed,
		RequestsCount: entry.RequestsCount,
		TokensRemain:  tokensRemain,
		CreditsRemain: creditsRemain,
		OverToken:     overToken,
		OverCredit:    overCredit,
		Expired:       expired,
		ExpiresAt:     entry.ExpiresAt,
		Valid:         entry.Enabled && !expired && !overToken && !overCredit,
		BaseURL:       selfServiceBaseURL(r),
		DailyTokens:   usage.DailyTokens,
		ByModel:       usage.ByModel,
	})
}

// selfServiceBaseURL returns the externally reachable API base URL to show the
// customer. Prefers the admin-configured PublicBaseURL; falls back to the scheme
// and host of the incoming request when unset.
func selfServiceBaseURL(r *http.Request) string {
	if u := config.GetPublicBaseURL(); u != "" {
		return u
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	if r.Host == "" {
		return ""
	}
	return scheme + "://" + r.Host
}
