package proxy

import (
	"context"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

// apiKeyContextKey is an unexported type used as the context key for the matched ApiKeyEntry
// so it cannot collide with keys defined in other packages.
type apiKeyContextKey struct{}

// authError describes why authentication failed. status is the HTTP status code to send.
//
// When notice is true the key is VALID but blocked (over-limit, disabled, or expired):
// callers render the configured limit-notice message as a normal successful chat reply
// instead of writing an HTTP error. status/code are unused on the notice path.
type authError struct {
	status  int
	code    string
	message string
	notice  bool
}

func (e *authError) Error() string { return e.message }

func newAuthError(status int, code, message string) *authError {
	return &authError{status: status, code: code, message: message}
}

// newNoticeError signals a valid-but-blocked key. The handler turns this into the
// limit-notice chat reply once it has parsed the request (model + stream flag).
func newNoticeError() *authError {
	return &authError{notice: true}
}

// extractProvidedKey reads the API key from Authorization (Bearer ...) or X-Api-Key header.
func extractProvidedKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	return ""
}

// authenticate validates an incoming request against the configured API keys.
//
// Master switch: config.RequireApiKey. When false, requests pass without checking
// any keys, even if entries exist (so the admin UI can hold draft keys without
// affecting public deployments).
//
// When RequireApiKey is true:
//  1. If ApiKeys is non-empty, the provided key MUST match an enabled, in-quota
//     entry. Returns the matched entry (a copy) so callers can attribute usage.
//  2. Else if the legacy single ApiKey field is set, the provided key MUST match it.
//  3. Else (switch on but nothing configured) → fail-closed: every request is rejected.
//     This prevents the prior bug where toggling auth on without keys silently
//     left the service open.
//
// Returns (entry, nil) on success. entry is nil when the legacy single-key path
// is used or when the master switch is off.
func (h *Handler) authenticate(r *http.Request) (*config.ApiKeyEntry, error) {
	if !config.IsApiKeyRequired() {
		return nil, nil
	}

	provided := extractProvidedKey(r)

	if config.HasApiKeys() {
		if provided == "" {
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		entry := config.FindApiKeyByValue(provided)
		if entry == nil {
			// A supplied-but-unknown key is a credential guess; enough of them from
			// one address promotes it to the persistent ban list. A request with NO
			// key is deliberately not counted — that is a misconfigured client, not
			// an attacker, and banning it would create support noise.
			h.registerAuthFailure(r)
			return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
		}
		// The key is genuine: forget any earlier misses from this address so a
		// customer who fixed a typo is not banned by their own stale counter.
		h.clearAuthFailures(r)
		if !entry.Enabled {
			// Valid key, just disabled → friendly notice instead of a 401 that breaks clients.
			return nil, newNoticeError()
		}
		if config.ApiKeyExpired(*entry) {
			return nil, newNoticeError()
		}
		if overToken, overCredit := config.ApiKeyOverLimit(*entry); overToken || overCredit {
			return nil, newNoticeError()
		}
		// IP 限制：优先用硬性白名单 (IPAllowlist)，非空时只有匹配的 IP/CIDR 才放行，
		// 其余一律按“被封锁的 key”处理，返回限额提示而非 401。白名单为空时退回到
		// 软性去重计数 (IPLimit)：滚动窗口内只允许有限个不同 IP，已见过的 IP 始终放行，
		// 一个新 IP 若会超出上限则被拒。两者都返回友好提示，客户端不会因鉴权错误崩溃。
		if len(entry.IPAllowlist) > 0 {
			ip := clientIPFromContext(r.Context())
			if ip == "" {
				ip = h.resolveClientIP(r)
			}
			if !ipMatchesAllowlist(ip, entry.IPAllowlist) {
				return nil, newNoticeError()
			}
		} else if h.ipLimiter != nil && entry.IPLimit > 0 {
			ip := clientIPFromContext(r.Context())
			if ip == "" {
				ip = h.resolveClientIP(r)
			}
			if !h.ipLimiter.allow(entry.ID, ip, entry.IPLimit) {
				return nil, newNoticeError()
			}
		}
		// RPM 限速：不拒绝请求，而是按配置的每分钟速率把请求排队延迟，
		// 让响应均匀分布而非瞬间全部返回。客户端断开时立即放行以免占用资源。
		if h.rpmThrottle != nil {
			if wait := h.rpmThrottle.reserve(entry.ID, entry.RPMLimit); wait > 0 {
				// 在途上限：一个 key 只允许有限个请求同时“睡在”延迟里。超过则立即拒绝，
				// 而不是再挂起一条连接 (否则单个 key 的暴刷会把 goroutine/连接耗尽)。
				if h.guard != nil && !h.guard.enterKeyWait(entry.ID) {
					// Return the token we just reserved: this request is being
					// rejected without waiting, so keeping the deduction would make
					// the effective RPM drift below the configured limit.
					h.rpmThrottle.refund(entry.ID, entry.RPMLimit)
					// Log it: the handler never runs for a rejected request, so nothing
					// downstream writes this to the request log. Without this line the
					// 429 is invisible to the operator. Only reachable when the key has
					// an explicit rpmLimit > 0 — an unlimited key never gets here.
					logger.Warnf("[Guard] per-key in-flight cap hit: key=%s rpmLimit=%d maxWaiters=%d (raise KIRO_PER_KEY_INFLIGHT or the key's rpmLimit)",
						config.MaskApiKey(entry.Key), entry.RPMLimit, h.guard.keyMaxWaiters)
					return nil, newAuthError(http.StatusTooManyRequests, "rate_limit_error", "too many concurrent requests for this API key")
				}
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-r.Context().Done():
					timer.Stop()
				}
				if h.guard != nil {
					h.guard.leaveKeyWait(entry.ID)
				}
			}
		}
		return entry, nil
	}

	// Legacy single-key path.
	expected := config.GetApiKey()
	if expected == "" {
		// Auth required but nothing configured → fail closed.
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "API key authentication is required but no keys are configured")
	}
	if provided == "" || provided != expected {
		if provided != "" {
			h.registerAuthFailure(r)
		}
		return nil, newAuthError(http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
	}
	h.clearAuthFailures(r)
	return nil, nil
}

// registerAuthFailure records one wrong-credential attempt from the request's
// client IP against the auto-ban counter.
func (h *Handler) registerAuthFailure(r *http.Request) {
	if h.ipBanGate == nil {
		return
	}
	if ip := clientIPFromContext(r.Context()); ip != "" {
		h.ipBanGate.registerFail(ip)
	}
}

// clearAuthFailures resets the auto-ban counter for the request's client IP.
func (h *Handler) clearAuthFailures(r *http.Request) {
	if h.ipBanGate == nil {
		return
	}
	if ip := clientIPFromContext(r.Context()); ip != "" {
		h.ipBanGate.clearFails(ip)
	}
}

// withApiKeyContext attaches the matched entry to the request context so downstream
// handlers (recordSuccess, etc.) can credit usage against the correct key.
func withApiKeyContext(r *http.Request, entry *config.ApiKeyEntry) *http.Request {
	if entry == nil {
		return r
	}
	ctx := context.WithValue(r.Context(), apiKeyContextKey{}, entry.ID)
	return r.WithContext(ctx)
}

// apiKeyIDFromContext returns the matched API key ID stored in ctx, or empty string.
func apiKeyIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(apiKeyContextKey{}).(string); ok {
		return v
	}
	return ""
}

// limitNoticeContextKey marks a request whose VALID key is blocked (over-limit/disabled/
// expired). Handlers render the limit-notice chat reply when it is set.
type limitNoticeContextKey struct{}

// withLimitNotice flags the request so the downstream handler emits the notice reply.
func withLimitNotice(r *http.Request) *http.Request {
	ctx := context.WithValue(r.Context(), limitNoticeContextKey{}, true)
	return r.WithContext(ctx)
}

// limitNoticeRequested reports whether the request was flagged by withLimitNotice.
func limitNoticeRequested(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(limitNoticeContextKey{}).(bool)
	return v
}

// clientIPContextKey carries the resolved client IP down to request handlers / auth
// so the request log can record it and rate-limiters can reuse it. It is set once at
// the top of ServeHTTP.
type clientIPContextKey struct{}

// withClientIP attaches the resolved client IP to the request context.
func withClientIP(r *http.Request, ip string) *http.Request {
	if ip == "" {
		return r
	}
	ctx := context.WithValue(r.Context(), clientIPContextKey{}, ip)
	return r.WithContext(ctx)
}

// clientIPFromContext returns the client IP stored in ctx, or empty string.
func clientIPFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(clientIPContextKey{}).(string); ok {
		return v
	}
	return ""
}
