package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"kiro-go/config"
)

// autobuy_market.go is the client for the kiro-market API. It covers only the
// three calls the unattended buyer needs: read the balance, read stock and price,
// and purchase.
//
// Every response is funnelled through marketError so callers branch on the
// upstream's stable `code` field rather than on prose. The market docs are
// explicit that message text gets reworded but codes do not, and the difference
// between a code that is worth retrying and one that is not is the difference
// between backing off and hammering a rate-limited endpoint all night.

// marketMaxBody caps how much of a market response is read. Its bodies are small
// JSON objects; anything larger is a misconfigured URL pointing somewhere else.
const marketMaxBody = 1 << 20 // 1 MiB

// Market error codes worth distinguishing. See the error table in the market docs.
const (
	// marketCodeRetrySameOrder means stock was taken concurrently and the caller
	// must retry with the SAME client_order_id.
	marketCodeRetrySameOrder = "retry_same_order"
	// marketCodeNoStock is the ordinary empty-shelf case.
	marketCodeNoStock     = "no_stock"
	marketCodeRateLimited = "rate_limited"
	// The next two are terminal: the docs state plainly that retrying is useless.
	marketCodeCapReached  = "purchase_cap_reached"
	marketCodeNoBalance   = "insufficient_balance"
	marketCodeBadZone     = "bad_zone"
	marketCodeBadCount    = "bad_count"
	marketCodeBadOrderID  = "bad_order_id"
	marketCodeInvalidKey  = "invalid_api_key"
	marketCodeUnauthed    = "unauthenticated"
	marketCodeDisabled    = "disabled"
	marketCodeSessionOnly = "session_required"
)

// marketError is a structured failure from the market API.
type marketError struct {
	Status int
	Code   string
	Msg    string
	// RetryAfter carries the Retry-After header on a 429, so a rate-limited
	// caller waits the interval the server asked for instead of guessing.
	RetryAfter time.Duration
}

func (e *marketError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("market: %s (HTTP %d): %s", e.Code, e.Status, e.Msg)
	}
	return fmt.Sprintf("market: HTTP %d: %s", e.Status, e.Msg)
}

// Retryable reports whether trying the same call again could plausibly succeed.
//
// The terminal codes are excluded on the upstream's own advice: a cap that has
// been reached does not un-reach itself, and an empty balance will not refill
// from a retry. Treating them as retryable would turn one bad night into
// thousands of pointless requests against a rate-limited API.
func (e *marketError) Retryable() bool {
	switch e.Code {
	case marketCodeCapReached, marketCodeNoBalance,
		marketCodeBadZone, marketCodeBadCount, marketCodeBadOrderID,
		marketCodeInvalidKey, marketCodeUnauthed, marketCodeDisabled, marketCodeSessionOnly:
		return false
	}
	// 4xx without a recognised code is a client-side mistake; repeating it will
	// reproduce it. 5xx and transport errors are worth another attempt.
	if e.Status >= 400 && e.Status < 500 {
		return e.Code == marketCodeRetrySameOrder || e.Code == marketCodeNoStock || e.Code == marketCodeRateLimited
	}
	return true
}

// Terminal reports whether this error should stop unattended buying rather than
// merely postpone it, so the operator gets one notification instead of a stream.
func (e *marketError) Terminal() bool {
	switch e.Code {
	case marketCodeCapReached, marketCodeNoBalance, marketCodeInvalidKey,
		marketCodeUnauthed, marketCodeDisabled, marketCodeBadZone, marketCodeBadCount:
		return true
	}
	return false
}

// asMarketError extracts a *marketError from err, if it is one.
func asMarketError(err error) (*marketError, bool) {
	me, ok := err.(*marketError)
	return me, ok
}

// marketErrorCode returns the upstream code for err, or "" when err is not a
// market error. Convenient for logging without a type switch at each site.
func marketErrorCode(err error) string {
	if me, ok := asMarketError(err); ok {
		return me.Code
	}
	return ""
}

// marketProfile is the subset of GET /api/my/profile that gates a purchase.
type marketProfile struct {
	Username string `json:"username"`
	Balance  int    `json:"balance"`
	// HoldCapEffective is the operator's global cap merged with the user's own
	// max_keys_held; 0 means unlimited. KeysHeld is the current active count.
	// Checking these two locally avoids an order that was always going to fail
	// with purchase_cap_reached.
	HoldCapEffective int `json:"hold_cap_effective"`
	KeysHeld         int `json:"keys_held"`
	MaxKeysHeld      int `json:"max_keys_held"`
}

// HoldRoom returns how many more keys may be held, and whether a cap applies.
func (p *marketProfile) HoldRoom() (int, bool) {
	if p == nil || p.HoldCapEffective <= 0 {
		return 0, false
	}
	room := p.HoldCapEffective - p.KeysHeld
	if room < 0 {
		room = 0
	}
	return room, true
}

// marketZoneStock is one zone's availability and price.
type marketZoneStock struct {
	Zone   string `json:"zone"`
	Region string `json:"region"`
	// Available is current stock in this zone.
	Available int `json:"available"`
	// UnitPrice is already decayed to the price that would be charged right now.
	// BasePrice is the undiscounted tier price, kept only for display.
	UnitPrice int `json:"unit_price"`
	BasePrice int `json:"base_price"`
}

// marketStock is the subset of GET /api/my/stock the buyer needs.
type marketStock struct {
	Stock struct {
		PublicAvailable int `json:"public_available"`
		MyPrivate       int `json:"my_private"`
		MyKeys          int `json:"my_keys"`
	} `json:"stock"`
	Zones []marketZoneStock `json:"zones"`
	// Max is the largest single pickup allowed right now. The docs say to check
	// it is > 0 before ordering when polling.
	Max             int `json:"max"`
	MinPerOrder     int `json:"min_per_order"`
	MaxPerOrder     int `json:"max_per_order"`
	WarrantyMinutes int `json:"warranty_minutes"`
}

// ZoneStock returns the entry for a zone, or nil when absent.
func (s *marketStock) ZoneStock(zone string) *marketZoneStock {
	if s == nil {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(zone))
	for i := range s.Zones {
		if strings.EqualFold(s.Zones[i].Zone, want) {
			return &s.Zones[i]
		}
	}
	return nil
}

// marketPurchasedKey is one delivered key.
type marketPurchasedKey struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Region string `json:"region"`
	Zone   string `json:"zone"`
	Free   bool   `json:"free"`
	// Paid is the authoritative per-key charge; the sum over keys equals
	// TotalCredits even when one order spans rounds at different prices.
	Paid          int    `json:"paid"`
	WarrantyUntil string `json:"warranty_until"`
}

// marketPurchase is the response to POST /api/my/purchase.
type marketPurchase struct {
	ClientOrderID string `json:"client_order_id"`
	OrderID       string `json:"order_id"`
	Zone          string `json:"zone"`
	// Purchased is what was actually delivered, which may be fewer than
	// requested: stock is contended and partial fills are normal.
	Purchased int `json:"purchased"`
	UnitPrice int `json:"unit_price"`
	// TotalCredits is the figure to reconcile against. unit_price × count does
	// not hold for a mixed-price order.
	TotalCredits int                  `json:"total_credits"`
	Remaining    int                  `json:"remaining"`
	Keys         []marketPurchasedKey `json:"keys"`
	FreeCount    int                  `json:"free_count"`
}

// marketClient talks to one market account.
type marketClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// newMarketClient builds a client from the auto-buy config, reusing the shared
// proxy-aware REST client so market traffic leaves by the same route as Kiro
// traffic. A configured outbound proxy exists to control egress; letting one
// subsystem bypass it would defeat that.
func newMarketClient(cfg *config.AutoBuyConfig) (*marketClient, error) {
	if cfg == nil {
		return nil, fmt.Errorf("autobuy: no configuration")
	}
	key := strings.TrimSpace(cfg.MarketApiKey)
	if key == "" {
		return nil, fmt.Errorf("autobuy: marketApiKey is not set")
	}
	proxyURL := config.GetProxyURL()
	if proxyURL == "" && config.GetRequireProxy() {
		return nil, fmt.Errorf("autobuy: require-proxy is on but no proxy is configured")
	}
	return &marketClient{
		baseURL: cfg.EffectiveBaseURL(),
		apiKey:  key,
		client:  GetRestClientForProxy(proxyURL),
	}, nil
}

// do issues a request and decodes a successful JSON body into out.
func (m *marketClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("autobuy: encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("autobuy: build request: %w", err)
	}
	req.Header.Set("X-API-Key", m.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.client.Do(req)
	if err != nil {
		// A transport failure is retryable: status 0 signals "never reached the
		// server", which Retryable treats as worth another attempt.
		return &marketError{Status: 0, Msg: err.Error()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, marketMaxBody))
	if err != nil {
		return &marketError{Status: resp.StatusCode, Msg: "read body: " + err.Error()}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseMarketError(resp, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &marketError{Status: resp.StatusCode, Msg: "decode body: " + err.Error()}
	}
	return nil
}

// parseMarketError builds a marketError from a non-2xx response.
func parseMarketError(resp *http.Response, raw []byte) *marketError {
	out := &marketError{Status: resp.StatusCode}

	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		out.Code = strings.TrimSpace(payload.Code)
		out.Msg = strings.TrimSpace(payload.Message)
		if out.Msg == "" {
			out.Msg = strings.TrimSpace(payload.Error)
		}
	}
	if out.Msg == "" {
		// Fall back to a bounded snippet so an HTML error page from a wrong URL
		// does not end up in the log or the admin panel in full.
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		out.Msg = snippet
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		if out.Code == "" {
			out.Code = marketCodeRateLimited
		}
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
				out.RetryAfter = time.Duration(secs) * time.Second
			}
		}
	}
	return out
}

// Profile fetches the account balance and holding counters.
func (m *marketClient) Profile(ctx context.Context) (*marketProfile, error) {
	var resp struct {
		Profile marketProfile `json:"profile"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/my/profile", nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Profile, nil
}

// Stock fetches current availability and live per-zone pricing.
func (m *marketClient) Stock(ctx context.Context) (*marketStock, error) {
	var out marketStock
	if err := m.do(ctx, http.MethodGet, "/api/my/stock", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Purchase claims count keys from zone.
//
// clientOrderID is the idempotency key and must be 32 hex characters. Passing the
// same one twice returns the original result byte-for-byte without charging
// again, which is what makes a webhook redelivery and a retry_same_order retry
// safe to perform.
func (m *marketClient) Purchase(ctx context.Context, zone string, count int, clientOrderID string) (*marketPurchase, error) {
	body := map[string]any{
		"count":           count,
		"zone":            strings.ToLower(strings.TrimSpace(zone)),
		"client_order_id": clientOrderID,
	}
	var out marketPurchase
	if err := m.do(ctx, http.MethodPost, "/api/my/purchase", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OrderKeys re-fetches a delivered order by its order number.
//
// This is the only way to obtain the full key text for a reserved delivery: the
// key-list endpoint returns prefixes only, and the delivery notification carries
// no keys. Skipping it strands a batch that was already charged.
//
// Unlike Purchase this is a read, so it is safe to call repeatedly.
func (m *marketClient) OrderKeys(ctx context.Context, orderID string) (*marketPurchase, error) {
	id := strings.TrimSpace(orderID)
	if id == "" {
		return nil, fmt.Errorf("autobuy: order id is required")
	}
	var out marketPurchase
	if err := m.do(ctx, http.MethodGet, "/api/my/orders/"+url.PathEscape(id)+"/keys", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// newClientOrderID generates a 32-hex-character idempotency key.
//
// crypto/rand rather than math/rand: a predictable id could collide with another
// order and silently replay someone else's purchase result.
func newClientOrderID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("autobuy: generate order id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// isValidClientOrderID reports whether s is the 32-hex-character form the market
// API requires, so a malformed webhook value is caught before the round trip
// instead of coming back as 400 bad_order_id.
func isValidClientOrderID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
