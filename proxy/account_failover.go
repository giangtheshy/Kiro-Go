package proxy

import (
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

// noUpstreamAvailableMessage is the single message every upstream-exhaustion
// path reports. It is deliberately identical whether the Kiro pool ran out or an
// external provider failed, so the client cannot tell which upstreams exist
// behind this proxy or why one of them broke.
const noUpstreamAvailableMessage = "No available accounts"

// errNoUpstreamAvailable is the sanitized error a failing external provider
// returns in place of the real one. The real cause — upstream URL, status code,
// and response body — is logged server-side and never travels to the client.
var errNoUpstreamAvailable = errors.New(noUpstreamAvailableMessage)

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota") || strings.Contains(msg, "throttl")
}

// isOverageErrorMessage matches Kiro's overage-limit rejection.
//
// Match on the upstream's own reason codes, NOT on a status code: the real
// rejection arrives as HTTP *400* carrying
// {"__type":"...ServiceQuotaExceededException","reason":"OVERAGE_REQUEST_LIMIT_EXCEEDED"}.
// Keying off "402" therefore never fired, and the error fell through to
// isQuotaErrorMessage — classified as a plain quota problem, so the overage
// refresh path never ran.
func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "overage_request_limit_exceeded") ||
		strings.Contains(msg, "limit for overages") ||
		(strings.Contains(msg, "402") && strings.Contains(msg, "overage"))
}

// isRefusalErrorMessage matches a content-filter refusal. These must never be
// charged to the account: the same payload is refused identically by every
// account, so treating a refusal as an account error would walk the entire pool
// into cooldown over one filtered conversation.
func isRefusalErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "content filter") ||
		strings.Contains(msg, "contentfilter") ||
		strings.Contains(msg, "content_filter") ||
		strings.Contains(msg, "guardrail") ||
		strings.Contains(msg, "blocked by content policy")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

// isProxyErrorMessage matches outbound-proxy / dial failures: a missing required
// proxy (require-proxy), a dead or refusing proxy, or a connect timeout on the
// proxy hop. These are infrastructure failures, not account bans — the account
// is cooled down and the request rotates to the next account. NOTE: keep this
// case ABOVE isAuthErrorMessage in handleAccountFailure so a proxy connect
// failure is never misread as an auth ban and disable the account.
func isProxyErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "require-proxy") ||
		strings.Contains(msg, "proxyconnect") ||
		strings.Contains(msg, "socks") ||
		strings.Contains(msg, "connection refused") ||
		(strings.Contains(msg, "dial tcp") && (strings.Contains(msg, "timeout") ||
			strings.Contains(msg, "refused") ||
			strings.Contains(msg, "connectex") ||
			strings.Contains(msg, "no such host")))
}

// statusForUpstreamError maps an upstream error to the HTTP status the client should see.
// Quota/throttle → 429, overage → 402, auth → 401, everything else → 500.
func statusForUpstreamError(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	// A sanitized provider failure carries no upstream detail to classify, and
	// it means the same thing as an exhausted pool.
	if errors.Is(err, errNoUpstreamAvailable) {
		return http.StatusServiceUnavailable
	}

	msg := err.Error()
	switch {
	// Overage is checked BEFORE quota: the upstream reports it as
	// ServiceQuotaExceededException, which contains "quota", so the quota case
	// would otherwise swallow every overage error and answer 429 instead of 402.
	case isOverageErrorMessage(msg):
		return http.StatusPaymentRequired
	case isQuotaErrorMessage(msg):
		return http.StatusTooManyRequests
	case isAuthErrorMessage(msg):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

func errorTypeForOpenAIStatus(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	default:
		return "server_error"
	}
}

// applyRetryAfterHeader sets Retry-After on quota errors, using the upstream-supplied
// value when the message carries one ("retry after 30"), else a 60s default.
func applyRetryAfterHeader(w http.ResponseWriter, err error) {
	if w == nil || err == nil || !isQuotaErrorMessage(err.Error()) {
		return
	}
	if retryAfter := retryAfterFromError(err.Error()); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
		return
	}
	w.Header().Set("Retry-After", "60")
}

func retryAfterFromError(msg string) string {
	idx := strings.LastIndex(strings.ToLower(msg), "retry after ")
	if idx < 0 {
		return ""
	}
	value := strings.TrimSpace(msg[idx+len("retry after "):])
	if semi := strings.Index(value, ";"); semi >= 0 {
		value = strings.TrimSpace(value[:semi])
	}
	return value
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()

	// A content-filter refusal is a property of the payload, not the account.
	// Every account returns the same verdict, so recording it as an account
	// error would cool down the whole pool over one filtered conversation.
	if isRefusalErrorMessage(errMsg) {
		logger.Warnf("[AccountFailover] Content refusal for %s (not counted against the account): %v", account.Email, err)
		return
	}

	switch {
	case isProxyErrorMessage(errMsg):
		// Proxy/dial failure — cool down and rotate; never disable the account
		// and never fall through to a direct connection.
		logger.Warnf("[AccountFailover] Proxy/dial failure for %s: %v", account.Email, err)
		h.pool.RecordError(account.ID, false)
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
