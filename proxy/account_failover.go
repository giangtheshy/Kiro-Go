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

// isRelayKeyRejectedError matches a relay refusing the credential itself.
//
// This needs its own matcher because such a relay answers HTTP *429* for a bad
// key, not 401 — and "429" is exactly what isQuotaErrorMessage looks for. So the
// case MUST be ordered before the quota case in handleAccountFailure, otherwise a
// permanently dead key is read as a temporary quota problem: cooled down, then
// retried forever, with the real reason nowhere in the log.
//
// The distinction matters operationally. A quota clears on its own; a rejected
// key never does, and the only fix is for someone to paste in a new one.
func isRelayKeyRejectedError(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "invalid api key") ||
		strings.Contains(msg, "missing or invalid api key") ||
		strings.Contains(msg, "invalid_api_key")
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

// isEmptyStreamErrorMessage matches the "upstream answered 200 then closed the
// stream saying nothing" failure raised by the endpoint loop in kiro.go.
//
// It is worth its own matcher because the shape of the failure identifies the
// likely cause. A network blip surfaces as a transport error, and a rejected
// request surfaces as a 4xx — an accepted request whose stream carries neither
// output nor a metering event is the upstream refusing a payload without saying
// why. That points at the request body, not at the account, which is why this is
// both retried with the flattened payload and excluded from account error
// counting.
func isEmptyStreamErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no output, no metering")
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
		strings.Contains(msg, "blocked by content policy") ||
		// The in-band form: upstream answered 200 and stated its reason in an
		// event frame rather than as an HTTP error. See errKiroUpstreamRefusal.
		// Matched here so it inherits the same treatment as every other refusal
		// — a verdict on the payload, not a fault of the account serving it.
		strings.Contains(msg, "upstream refused the request")
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
	// Checked by sentinel, not by message. An in-band refusal deliberately
	// carries the UPSTREAM's wording as its message (see refusalError) so the
	// customer reads the vendor's own advice — which means there is no fixed
	// phrase for a string matcher to look for. 400, because a refusal is a
	// verdict on the request: answering 5xx makes clients retry a failure that
	// can never clear, which is precisely what looked like an outage.
	if errors.Is(err, errKiroUpstreamRefusal) {
		return http.StatusBadRequest
	}

	msg := err.Error()
	switch {
	// Ahead of the quota case, same reason as in handleAccountFailure: the message
	// contains "429". 503 rather than 429 or 401 — the credential at fault is the
	// OPERATOR's, not the caller's. 429 would invite a retry of something that can
	// never clear on its own, and 401 would tell the customer their own key is bad,
	// sending them to regenerate a key that was never the problem. 503 is the
	// truthful reading: no upstream can serve this right now.
	case isRelayKeyRejectedError(msg):
		return http.StatusServiceUnavailable
	// Overage is checked BEFORE quota: the upstream reports it as
	// ServiceQuotaExceededException, which contains "quota", so the quota case
	// would otherwise swallow every overage error and answer 429 instead of 402.
	case isOverageErrorMessage(msg):
		return http.StatusPaymentRequired
	// A refusal is a verdict on the request, so it must not answer 5xx. Clients
	// treat 5xx as transient and retry it — which is how one refused
	// conversation turns into a run of identical failures that never clears,
	// exactly the symptom that looks like an outage. 400 tells the client the
	// request itself is the problem and stops the retry loop.
	case isRefusalErrorMessage(msg):
		return http.StatusBadRequest
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

	// Report immediately if that was the last usable account. Reload must come
	// first: HealthyCount reads the pool's cooldown map, and checking before the
	// reload would evaluate a pool that still contains the account just disabled.
	h.checkPoolExhausted()
}

// disableAccountOverage handles an upstream overage-limit rejection by disabling
// the account outright.
//
// A cooldown would be wrong here. Cooldowns exist for failures that clear on
// their own within minutes; an exhausted overage allowance does not — it holds
// until the billing period rolls over or the operator raises the cap. Cooling
// down for an hour and retrying just burns one of the three retry attempts on an
// account that is certain to answer 402 again, on every request, all period.
//
// The snapshot is still fetched first, on a best-effort basis, so the panel shows
// why the account went dark (cap, rate, accumulated overages). A failed fetch
// does not block the disable: the upstream already told us the account is
// unusable, and that verdict does not depend on the snapshot arriving.
func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	if snap, fetchErr := FetchOverageStatus(account); fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
	} else if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
	} else {
		logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
		// Re-read so the disable below is applied on top of the snapshot fields
		// rather than overwriting them with the pre-fetch copy: disableAccount
		// persists a whole Account record, so a stale copy would silently undo the
		// snapshot that was just written.
		if fresh := freshAccountByID(account.ID); fresh != nil {
			account = fresh
		}
	}

	// "SUSPENDED" rather than "BANNED": nothing is wrong with the credentials, the
	// account simply has no allowance left. The panel renders the two differently
	// and the operator's next action is different too — top up or wait, not
	// re-authenticate.
	h.disableAccount(account, "SUSPENDED", "Overage limit reached - upstream rejected the request (OVERAGE_REQUEST_LIMIT_EXCEEDED)")
}

// freshAccountByID re-reads an account from config by ID, returning nil when it
// is gone. Config is the source of truth here, not the pool: the pool holds a
// snapshot taken at the last Reload, so it would not carry a field written
// moments ago.
func freshAccountByID(id string) *config.Account {
	for _, acc := range config.GetAccounts() {
		if acc.ID == id {
			fresh := acc
			return &fresh
		}
	}
	return nil
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()

	// A content-filter refusal is a property of the payload, not the account.
	// Every account returns the same verdict, so recording it as an account
	// error would cool down the whole pool over one filtered conversation.
	//
	// The sentinel is checked alongside the message matcher because an in-band
	// refusal carries the upstream's own wording (see refusalError), leaving no
	// fixed phrase to match on.
	if errors.Is(err, errKiroUpstreamRefusal) || isRefusalErrorMessage(errMsg) {
		logger.Warnf("[AccountFailover] Content refusal for %s (not counted against the account): %v", account.Email, err)
		return
	}

	// REST operations unsupported by relay accounts (model listing, overage fetch)
	// are not account failures. The sentinel guards all of them; checking it here
	// closes off every path where such an error could leak into a cooldown or ban.
	if errors.Is(err, ErrModelListingUnsupported) || errors.Is(err, ErrAccountInfoUnsupported) {
		return
	}

	// An empty stream is a property of the payload too, and it used to fall
	// through to the default branch and be charged to the account. That inverted
	// the problem: one client sending a payload the upstream silently rejects
	// would walk the whole pool into cooldown, taking down accounts that were
	// perfectly healthy for everyone else. Log it loudly — a sustained run of
	// these is a real signal — but do not cool the account down over it.
	if isEmptyStreamErrorMessage(errMsg) {
		logger.Warnf("[AccountFailover] Empty upstream stream for %s (not counted against the account; suspect the request payload): %v", account.Email, err)
		return
	}

	switch {
	case isRelayKeyRejectedError(errMsg):
		// Ordered ahead of the quota case on purpose: the message contains "429",
		// which isQuotaErrorMessage matches. See isRelayKeyRejectedError.
		h.disableAccount(account, "BANNED", "Upstream rejected the API key - a new key is required")
	case isProxyErrorMessage(errMsg):
		// Proxy/dial failure — cool down and rotate; never disable the account
		// and never fall through to a direct connection.
		logger.Warnf("[AccountFailover] Proxy/dial failure for %s: %v", account.Email, err)
		h.pool.RecordError(account.ID, false)
	case isOverageErrorMessage(errMsg):
		// No RecordError: the account is disabled outright, so it leaves the pool
		// entirely. Recording a cooldown on top would only inflate the error count
		// of an account that is no longer selectable anyway.
		h.disableAccountOverage(account)
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

// isMalformedPayloadError returns true for 400-class upstream errors that signal
// the request body was structurally wrong — specifically those that a simpler
// (flat-history) payload could fix. ValidationException and ModelError are broad
// Bedrock-class errors also included.
func isMalformedPayloadError(msg string) bool {
	msg = strings.ToLower(msg)
	if strings.Contains(msg, "validationexception") || strings.Contains(msg, "modelerror") {
		return true
	}
	if !strings.Contains(msg, "http 400") {
		return false
	}
	return strings.Contains(msg, "improperly formed") ||
		strings.Contains(msg, "content_length_exceeds_threshold") ||
		strings.Contains(msg, "tool_use_result_mismatch") ||
		strings.Contains(msg, "tool_config_missing") ||
		strings.Contains(msg, "toolconfig field must be defined") ||
		strings.Contains(msg, "has no matching tool_use") ||
		strings.Contains(msg, "unexpected tool_use_id")
}

// isGenericUpstreamServerError matches the opaque 5xx AWS returns for a payload
// it dislikes but won't diagnose.
func isGenericUpstreamServerError(msg string) bool {
	lower := strings.ToLower(msg)
	for _, s := range []string{"http 500", "http 501", "http 502", "http 503", "http 504", "http 505"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return strings.Contains(lower, "unexpected error when processing the request")
}

// shouldRetrySafePayload reports whether an upstream error is worth one
// same-account retry with the flattened (safe) payload.
// An empty stream is included because it is the silent form of the same problem
// the other two matchers catch loudly: the upstream disliked the payload. The
// endpoint loop already retried the identical body three times and walked every
// endpoint, so a fourth identical attempt is pointless — but the flattened
// history is a genuinely DIFFERENT request, and it is the only remaining lever
// before the client sees an error.
func shouldRetrySafePayload(msg string) bool {
	return isMalformedPayloadError(msg) ||
		isGenericUpstreamServerError(msg) ||
		isEmptyStreamErrorMessage(msg)
}

// isTerminalRequestError reports whether retrying an upstream failure on a
// DIFFERENT account is pointless because the verdict belongs to the request.
//
// Rotation exists to route around an unhealthy account. A content-safety refusal
// is not that: every account asks the same upstream, which reads the same
// conversation and returns the same answer. Rotating anyway costs one billed turn
// per account — the refusal is metered, the upstream did read the conversation —
// and marks healthy accounts as failures on the way, for a result the client was
// always going to get. It also delays the one thing that helps: telling the
// customer what the upstream actually said.
func isTerminalRequestError(err error) bool {
	return errors.Is(err, errKiroUpstreamRefusal)
}

// callWithHistoryFallback calls CallKiroAPIWithContinuation with the richer
// payload first and, if the upstream rejects it with a recoverable error AND
// nothing has been streamed yet, retries the SAME account once with the
// flattened safe payload. When rich == safe (KeepToolHistory off) no fallback
// is attempted.
func callWithHistoryFallback(account *config.Account, rich, safe *KiroPayload, callback *KiroStreamCallback, started func() bool) error {
	err := CallKiroAPIWithContinuation(account, rich, callback)
	if err == nil {
		return nil
	}
	// A stated refusal short-circuits before the string matchers below, for two
	// reasons. It is final — the same conversation earns the same verdict from a
	// flattened payload too, so the retry only doubles the latency before the
	// client hears why. And its message is UPSTREAM-CONTROLLED prose (see
	// refusalError), so feeding it to phrase matchers is unsound: a refusal that
	// happens to contain "unexpected error when processing the request" would
	// otherwise be mistaken for a transient 5xx.
	if errors.Is(err, errKiroUpstreamRefusal) {
		return err
	}
	if rich == safe || !shouldRetrySafePayload(err.Error()) || started() {
		return err
	}
	logger.Warnf("[HistoryFallback] Rich payload rejected (%v); retrying same account with flattened history", err)
	return CallKiroAPIWithContinuation(account, safe, callback)
}
