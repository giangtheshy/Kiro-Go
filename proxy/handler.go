package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	stopPurge       chan struct{}
	shutdownOnce    sync.Once
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	promptCache     *promptCacheTracker
	// tokenRefreshLocks holds one mutex per account ID so a slow token refresh
	// on one account never blocks refreshes (or requests) for other accounts.
	tokenRefreshLocks sync.Map // accountID -> *sync.Mutex
	// Rate-limit / DoS-hardening collaborators (per-key RPM throttle, per-key
	// concurrent-IP cap, application-layer DoS guard, per-key usage stats).
	rpmThrottle *rpmThrottle
	ipLimiter   *ipLimiter
	guard       *dosGuard
	usage       *usageStats
	// adminGuard throttles brute-force guessing of the admin password on /admin/api/*.
	adminGuard *adminAuthGuard
	// adminSessions holds opaque, expiring admin session tokens so the browser never
	// stores the raw admin password (H5) and the plaintext-password cookie is retired (M5).
	adminSessions *adminSessionStore
	// ipBanGate blocks addresses on the persistent ban list and promotes repeat
	// auth failures onto it.
	ipBanGate *ipBanGate
	// keyLogHub fans per-key request records out to live SSE subscribers.
	keyLogHub *keyLogHub
	// streamTickets exchanges an API key for a short-lived token so an
	// EventSource (which cannot set headers) can open the per-key stream.
	streamTickets    *streamTicketStore
	stopAlertWatcher chan struct{}
	// providerCursor drives weighted round-robin across external providers that
	// share a routing tier. See proxy/upstream_router.go.
	providerCursor uint64
}

// safeGo runs fn in a new goroutine with panic containment: a panic inside fn is
// recovered and logged (value + stack) instead of crashing the whole process.
func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[safeGo] recovered from panic in background goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

// tokenRefreshLock returns the per-account refresh mutex, creating it on first use.
func (h *Handler) tokenRefreshLock(accountID string) *sync.Mutex {
	m, _ := h.tokenRefreshLocks.LoadOrStore(accountID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:       time.Now().Unix(),
		stopRefresh:     make(chan struct{}),
		stopStatsSaver:  make(chan struct{}),
		stopPurge:       make(chan struct{}),
		promptCache:     newPromptCacheTracker(defaultPromptCacheTTL),
		rpmThrottle:     newRPMThrottle(),
		ipLimiter:       newIPLimiter(),
		guard:           newDosGuard(loadDosGuardConfig()),
		usage:           newUsageStats(),
		adminGuard:      loadAdminAuthGuard(),
		adminSessions:   newAdminSessionStore(adminSessionTTL),
	}
	h.ipBanGate = newIPBanGate()
	h.keyLogHub = newKeyLogHub()
	h.streamTickets = newStreamTicketStore()
	h.stopAlertWatcher = make(chan struct{})
	// 启动后台刷新
	safeGo(func() { h.backgroundRefresh() })
	// 启动后台统计保存 (每30秒保存一次)
	safeGo(func() { h.backgroundStatsSaver() })
	// 清理过期的 stored responses（>30 天），定时循环而非仅启动时一次
	safeGo(func() { h.backgroundPurge() })
	// 低额度 / 即将过期告警扫描
	safeGo(func() { h.backgroundAlertWatcher() })
	return h
}

// backgroundPurge periodically deletes expired stored responses. It runs once at
// startup and then hourly, so long-lived processes don't accumulate expired files
// on disk until the next restart (M4). Stops on Shutdown.
func (h *Handler) backgroundPurge() {
	purgeExpiredResponses(responsesDefaultTTL)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			purgeExpiredResponses(responsesDefaultTTL)
		case <-h.stopPurge:
			return
		}
	}
}

// Shutdown stops the handler's background goroutines and flushes any pending
// hot-path config changes to disk. Safe to call multiple times (idempotent via
// sync.Once); the background savers each persist once more before returning, so
// usage/stats accumulated since the last 30s tick are not lost on SIGTERM.
func (h *Handler) Shutdown() {
	h.shutdownOnce.Do(func() {
		close(h.stopRefresh)
		close(h.stopStatsSaver)
		close(h.stopPurge)
		close(h.stopAlertWatcher)
		if h.ipBanGate != nil {
			h.ipBanGate.close()
		}
		// Capture the latest in-memory global counters and persist synchronously,
		// so the caller can rely on a durable write having happened by the time
		// Shutdown returns (backgroundStatsSaver's own stop-branch flush races us,
		// but saveStats + FlushDirty are both safe to run concurrently).
		h.saveStats()
	})
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || account.AccessToken == "" {
			continue
		}

		// 检查 token 是否需要刷新
		if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
			newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
				h.handleAccountFailure(account, err)
				continue
			}
			account.AccessToken = newAccessToken
			if newRefreshToken != "" {
				account.RefreshToken = newRefreshToken
			}
			account.ExpiresAt = newExpiresAt
			config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			if profileArn != "" {
				account.ProfileArn = profileArn
				config.UpdateAccountProfileArn(account.ID, profileArn)
			}
		}

		// 刷新账户信息
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae != nil && ae.notice {
			return withLimitNotice(r)
		}
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae != nil && ae.notice {
			return withLimitNotice(r)
		}
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// resolvePublicBaseURL returns the externally reachable base URL (scheme://host[:port])
// used to build OAuth redirect_uri values for the SSO loopback callback.
//
// Only config.PublicBaseURL (explicit operator override) is honored. Auto-detecting from
// the admin request Host is intentionally NOT done: the loopback SSO server listens on its
// own port (e.g. 3128), while the admin request arrives on the UI port/domain (e.g. 8080).
// The request host therefore names the wrong endpoint and would produce a redirect_uri the
// callback server never receives. When unset, "" is returned and the SSO flow falls back to
// http://localhost:<loopbackPort> (correct for the pure-local case). For a reverse proxy /
// custom domain, set PublicBaseURL to the domain that routes to the loopback port.
func resolvePublicBaseURL() string {
	return config.GetPublicBaseURL()
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS: only the public API surface (/v1/... consumed by third-party tools) is meant
	// to be called cross-origin. The admin panel is served same-origin, so we do NOT emit
	// a wildcard Access-Control-Allow-Origin for /admin/* — that would invite any website
	// to script the admin API against a logged-in operator's browser.
	if !strings.HasPrefix(path, "/admin") {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
		w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")
	}

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// Resolve the client IP once and stash it on the context so downstream handlers /
	// auth can reuse it (honours the guard's trust-proxy / X-Forwarded-For config;
	// falls back to raw RemoteAddr host when the guard is disabled).
	clientIP := h.resolveClientIP(r)
	r = withClientIP(r, clientIP)

	// Persistent IP ban list. Checked before everything except CORS preflight so a
	// banned address cannot reach auth, the admin panel, or the upstream call path.
	// /health and /status stay reachable so external monitors keep working.
	if h.ipBanGate != nil && path != "/health" && path != "/status" && h.ipBanGate.isBanned(clientIP) {
		// Log it: a banned address is rejected before auth and before the request log,
		// so this used to leave no trace anywhere. Users reporting "429" are sometimes
		// actually hitting this 403 (auto-ban after GetBanThreshold() bad keys within
		// failWindow) — naming the ip lets an operator unban from the admin panel.
		logger.Warnf("[Guard] blocked banned IP: ip=%s path=%s (unban via the admin panel IP ban list)", clientIP, path)
		h.ipBanGate.reject(w)
		return
	}

	// 应用层 DoS 防护：仅作用于会触发上游调用 / 鉴权的公开 API 端点。
	// 管理端点 (/admin/*) 由独立密码保护且非公开分享，不在此限。
	// 在路由前完成：每 IP 拒绝式限速 → 请求体大小上限。全局并发槽故意 NOT 在此获取，
	// 而是在鉴权成功后（RPM 延迟之后）通过 h.acquireGuardedSlot 获取，避免仅被 RPM
	// 延迟的请求长时间占用 KIRO_MAX_CONCURRENT 槽位而饿死其他客户端。
	if h.guard != nil && isGuardedAPIPath(path) {
		if !h.guard.allowIP(clientIP) {
			// Log it. This rejection happens BEFORE authentication, so there is no
			// apiKeyID to attribute it to and it cannot reach the per-key request log
			// — which used to make it completely invisible: users saw 429s while the
			// admin API Log showed nothing at all. The warning names the limit and the
			// env var so an operator can act on it without reading the source.
			// Note KIRO_TRUST_PROXY defaults to false: behind a reverse proxy every
			// client collapses into one bucket, turning this per-IP cap into a global one.
			logger.Warnf("[Guard] per-IP rate limit hit: ip=%s path=%s limit=%d/min (raise KIRO_IP_RPM; set KIRO_TRUST_PROXY=true if behind a reverse proxy)",
				clientIP, path, h.guard.ipRPM)
			h.sendGuardError(w, path, http.StatusTooManyRequests, "rate_limit_error", "Too many requests from your address")
			return
		}
		if h.guard.maxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, h.guard.maxBodyBytes)
		}
	}

	// Maintenance mode blocks inference only. Admin, health, status and the
	// self-service endpoints stay up so an operator can still work and customers
	// can still see why their requests are failing.
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses":
		if h.isMaintenanceMode(w, r, path) {
			return
		}
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	// 自助查询端点：用客户自己的 key 鉴权（非 admin 密码），只返回该 key 用量
	case path == "/v1/key/info" || path == "/key/info":
		h.apiKeySelfInfo(w, r)
	case path == "/v1/key/logs" || path == "/key/logs":
		h.apiKeySelfLogs(w, r)
	case path == "/v1/key/stream-ticket" && r.Method == "POST":
		h.apiKeyStreamTicket(w, r)
	case path == "/v1/key/logs/stream" && r.Method == "GET":
		h.apiKeyLogStream(w, r)
	// 销售机器人 API（管理员密码 / 会话鉴权）
	case strings.HasPrefix(path, "/api/sales/v1/"):
		h.handleSalesAPI(w, r)
	// 公开状态端点（无需鉴权，供监控与机器人轮询）
	case path == "/status" && r.Method == "GET":
		h.handlePublicStatus(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	// 客户自助门户（无需 admin 密码，用自己的 API key 查询用量）
	case path == "/check" || path == "/check/":
		setWebSecurityHeaders(w)
		http.ServeFile(w, r, "web/portal.html")
	// 自助用量仪表盘：GET 提供页面，POST 用客户自己的 key 返回该 key 的用量快照
	case (path == "/usage" || path == "/usage/") && r.Method == "GET":
		h.serveUsagePage(w, r)
	case (path == "/usage" || path == "/usage/" || path == "/v1/usage") && r.Method == "POST":
		h.apiUsageSelfService(w, r)
	// Session login/logout must be reachable WITHOUT a prior session (they sit
	// before the password gate in handleAdminAPI).
	case path == "/admin/api/login":
		h.handleAdminLogin(w, r)
	case path == "/admin/api/logout":
		h.handleAdminLogout(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// resolveClientIP returns the best-effort client IP. It prefers the DoS guard's
// resolver (which honours the trust-proxy / X-Forwarded-For configuration) and
// falls back to the raw RemoteAddr host when the guard is disabled.
func (h *Handler) resolveClientIP(r *http.Request) string {
	if h.guard != nil {
		return h.guard.clientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// acquireGuardedSlot reserves one of the global concurrency slots (KIRO_MAX_CONCURRENT)
// for a guarded API request. It is deliberately invoked AFTER authentication + RPM
// throttling so a request that is merely being RPM-delayed never pins a slot during
// its sleep. For non-guarded paths or when the guard is disabled it is a no-op admit.
// On a full pool it writes a 503 in the endpoint's error shape and returns (nil, false)
// — the caller MUST return without serving. On success it returns a release closure
// that MUST be invoked exactly once (via defer).
func (h *Handler) acquireGuardedSlot(w http.ResponseWriter, path string) (func(), bool) {
	if h.guard == nil || !isGuardedAPIPath(path) {
		return func() {}, true
	}
	release, ok := h.guard.acquireGlobal()
	if !ok {
		h.sendGuardError(w, path, http.StatusServiceUnavailable, "overloaded_error", "Server is at capacity, please retry shortly")
		return nil, false
	}
	return release, true
}

// sendGuardError writes a DoS-guard rejection in the error shape matching the target
// endpoint (OpenAI vs Claude) so clients parse it correctly.
func (h *Handler) sendGuardError(w http.ResponseWriter, path string, status int, errType, message string) {
	if isOpenAIStylePath(path) {
		h.sendOpenAIError(w, status, errType, message)
		return
	}
	h.sendClaudeError(w, status, errType, message)
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.refreshModelsCache()
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// 添加别名模型
	models = append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)

	// Models only reachable through an external provider still have to appear in
	// the catalog, otherwise a client cannot address the fallback capacity at all.
	models = append(models, providerOnlyModels(models, thinkingSuffix)...)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

// providerOnlyModels returns catalog entries for the aliases declared by enabled
// external providers that are not already listed, each with its thinking variant.
func providerOnlyModels(existing []map[string]interface{}, thinkingSuffix string) []map[string]interface{} {
	providers := config.GetEnabledProviders()
	if len(providers) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(existing))
	for _, m := range existing {
		if id, ok := m["id"].(string); ok {
			seen[strings.ToLower(id)] = true
		}
	}

	var out []map[string]interface{}
	for _, p := range providers {
		for _, m := range p.Models {
			for _, id := range []string{m.Alias, m.Alias + thinkingSuffix} {
				key := strings.ToLower(id)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, buildModelInfo(id, "provider:"+p.Name, false))
			}
		}
	}
	return out
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			models = append(models, buildModelInfo(m.ModelId, "anthropic", supportsImage))
			// 自动生成 thinking 变体
			models = append(models, buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage))
		}
	}
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
			},
		},
	}
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	aggregated := make([]ModelInfo, 0)
	for i := range accounts {
		account := &accounts[i]
		if err := h.ensureValidToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}

		models, err := ListAvailableModels(account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// 缓存每账号可用模型，用于路由时过滤
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
			// Record Kiro's authoritative input window so getContextWindowSize
			// stops guessing from the model name.
			if m.TokenLimits != nil {
				registerModelWindow(m.ModelId, m.TokenLimits.MaxInputTokens)
			}
		}
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
		if m.TokenLimits != nil {
			registerModelWindow(m.ModelId, m.TokenLimits.MaxInputTokens)
		}
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCache()
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// noticeOutputTokens returns a rough output-token count for the limit-notice reply so the
// synthetic usage block looks plausible to clients. No billing is attached to it.
func noticeOutputTokens(msg string) int {
	n := len([]rune(msg)) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// sendClaudeNotice returns the limit-notice text as a normal (HTTP 200) assistant reply
// in Claude shape. Used when a valid key is over-limit/disabled/expired so coding clients
// show the message in the chat window instead of erroring out. No upstream call, no billing.
func (h *Handler) sendClaudeNotice(w http.ResponseWriter, model string, stream bool, msg string) {
	outTok := noticeOutputTokens(msg)
	if !stream {
		resp := KiroToClaudeResponse(msg, "", false, nil, 1, outTok, model)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		resp := KiroToClaudeResponse(msg, "", false, nil, 1, outTok, model)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	msgID := "msg_" + uuid.New().String()
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 1, "output_tokens": 0},
		},
	})
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": msg},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})
	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]int{"input_tokens": 1, "output_tokens": outTok},
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// Valid-but-blocked key: render the limit-notice as a normal assistant reply.
	if limitNoticeRequested(r.Context()) {
		h.sendClaudeNotice(w, req.Model, req.Stream, config.GetLimitNoticeMessage())
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	// Apply global/per-key model override (ForceModel > per-key Model > client model).
	req.Model = applyModelOverride(actualModel, apiKeyID, thinkingCfg.Suffix)
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// Native web_search must be dispatched BEFORE the payload is converted: Kiro
	// does not execute server tools, so these requests never reach its chat
	// endpoint in the shape ClaudeToKiro would produce.
	//
	// The two cases are mutually exclusive. Only web_search → answer straight
	// from MCP. Mixed with the client's own tools → the agentic loop, which
	// consumes web_search internally and returns the client's tools untouched.
	if hasWebSearchTool(&req) || hasWebSearchAmongTools(&req) {
		searchStartedAt := time.Now()
		searchClientIP := h.resolveClientIP(r)
		if hasWebSearchTool(&req) {
			h.handleWebSearchRequest(w, &req, estimatedInputTokens, apiKeyID, searchStartedAt, searchClientIP)
		} else {
			h.runWebSearchLoop(w, &req, thinking, estimatedInputTokens, apiKeyID, searchStartedAt, searchClientIP)
		}
		return
	}

	// 转换请求 — build rich (structured history) and safe (flat history) payloads.
	// The rich form is tried first; if the upstream rejects the structured history
	// with a recoverable error and nothing has been written yet, callWithHistoryFallback
	// retries the same account with the flat form automatically.
	safePayload := ClaudeToKiro(&req, thinking)
	richPayload := safePayload
	if config.GetKeepToolHistory() {
		richPayload = ClaudeToKiroWithHistoryMode(&req, thinking, true)
	}

	// The original body is kept so an external Anthropic-compatible provider can
	// be handed the request verbatim instead of the Kiro translation.
	pc := &passthroughCtx{
		Raw:         body,
		Header:      r.Header,
		ClientIP:    h.resolveClientIP(r),
		Stream:      req.Stream,
		Endpoint:    config.ProviderEndpointMessages,
		SafePayload: safePayload,
	}

	// Stream or non-stream
	if req.Stream {
		h.handleClaudeStream(w, richPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, pc)
	} else {
		h.handleClaudeNonStream(w, richPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, pc)
	}
}

// logSuspiciousReq warns when a request burned a large input context but the
// model produced almost nothing and ended its turn without calling a tool —
// typically a thin client prompt (e.g. "continue") over a huge history, where
// the model just acks intent and stops. These waste quota, so surface them.
func logSuspiciousReq(api, model string, inputTokens, outputTokens int, hasTool bool) {
	if inputTokens > 50_000 && outputTokens < 50 && !hasTool {
		logger.Warnf("[SuspiciousReq] high-input low-output api=%s model=%s in=%d out=%d — thin prompt over large context; model acked and ended turn", api, model, inputTokens, outputTokens)
	}
}

// nextAccountForKey picks the next account for a request, honoring the API key's
// bound-account set when present. A key with BoundAccountIDs is restricted to those
// accounts; only when none of them is currently usable (all excluded/cooldown/quota/
// missing-model) does it fall back to the shared pool. Keys with no bound set route
// through the shared pool as before. excluded accumulates accounts that already failed
// this request so the retry loop advances.
func (h *Handler) nextAccountForKey(apiKeyID, model string, excluded map[string]bool) *config.Account {
	if apiKeyID != "" {
		if entry := config.GetApiKeyEntry(apiKeyID); entry != nil && len(entry.BoundAccountIDs) > 0 {
			allowed := make(map[string]bool, len(entry.BoundAccountIDs))
			for _, id := range entry.BoundAccountIDs {
				allowed[id] = true
			}
			if acc := h.pool.GetNextForModelBoundExcluding(model, allowed, excluded); acc != nil {
				return acc
			}
			// No bound account usable → fall back to the shared pool.
		}
	}
	return h.pool.GetNextForModelExcluding(model, excluded)
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, pc *passthroughCtx) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Prevent nginx proxy buffering — without this header nginx buffers the
	// entire SSE response before forwarding it, breaking real-time streaming.
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// Start an SSE-level heartbeat that emits ": ping" comments every 15 s.
	// This keeps the connection alive through nginx / Cloudflare / load
	// balancers that would otherwise close idle connections after ~60–75 s of
	// silence — exactly what happens while the model is thinking deeply.
	stream := newSSEStream(w, flusher)
	stopHeartbeat := stream.startHeartbeat()
	defer stopHeartbeat()

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	startedAt := time.Now()
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	messageStarted := false
	var messageStartUsage promptCacheUsage

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		stream.sendEvent("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		step := h.nextUpstream(apiKeyID, model, pc.Endpoint, excluded)
		if step == nil {
			break
		}
		if step.Provider != nil {
			handled, err := h.serveViaProvider(w, step, pc, model, apiKeyID, startedAt, estimatedInputTokens)
			if handled {
				return
			}
			lastErr = err
			excluded[step.Provider.ID] = true
			continue
		}
		account := step.Account
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		var truncated bool
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			stream.sendEvent("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				stream.sendEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				stream.sendEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				startContentBlock("text")
				stream.sendEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				stream.sendEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				stream.sendEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					stream.sendEvent("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				stream.sendEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				stream.sendEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				stream.sendEvent("content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnTruncated: func() {
				truncated = true
			},
		}

		// History-fallback aware: tries rich (structured) payload first; if the
		// upstream rejects it and nothing has been written yet, retries the same
		// account with the flat safe payload. Inside the retry, continuation is
		// still active, so a mid-stream cut on the safe payload is also resumed.
		safeP := payload
		if pc.SafePayload != nil {
			safeP = pc.SafePayload
		}
		err := callWithHistoryFallback(account, payload, safeP, callback, func() bool { return messageStarted })
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			if !messageStarted {
				continue
			}
			h.recordFailureForApiKey(apiKeyID, "claude", model, 0, err.Error(), startedAt, pc.clientIP())
			stream.sendEvent("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			return
		}

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:      inputTokens,
			Output:     outputTokens,
			CacheRead:  cacheUsage.CacheReadInputTokens,
			CacheWrite: cacheUsage.CacheCreationInputTokens,
			Credits:    credits,
			ClientIP:   pc.clientIP(),
		}, model, account, "claude", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		logSuspiciousReq("claude", model, inputTokens, outputTokens, len(toolUses) > 0)

		ensureMessageStart()

		// The upstream cut the turn short. An Anthropic client decides whether a
		// response finished from message_delta.stop_reason, so report usage but
		// WITHHOLD stop_reason and send no message_stop: the text already
		// delivered stands, and the client can see the turn never completed.
		// Emitting "end_turn" here is what makes truncation invisible.
		if truncated {
			logger.Warnf("[Stream] claude turn truncated mid-generation: model=%s account=%s", model, accountEmailForLog(account))
			stream.sendEvent("message_delta", map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]interface{}{},
				"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
			})
			stream.sendEvent("error", map[string]interface{}{
				"type": "error",
				"error": map[string]string{
					"type":    "api_error",
					"message": "upstream ended the stream before the turn completed",
				},
			})
			return
		}

		stopReason := claudeStopReason(toolUses, outputTokens, payloadMaxTokens(payload))
		if stopReason == "max_tokens" {
			logger.Debugf("[Stream] claude turn truncated at max_tokens: model=%s output=%d max=%d", model, outputTokens, payloadMaxTokens(payload))
		}

		stream.sendEvent("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
		})

		stream.sendEvent("message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "claude", model, 503, "No available accounts", startedAt, pc.clientIP())
		// Use stream.committedNow() to decide how to surface errors: if the
		// heartbeat or a previous SSE event already committed the response, we
		// cannot change the HTTP status, so the error goes in-band via SSE.
		if stream.committedNow() {
			stream.sendEvent("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "No available accounts"},
			})
		} else {
			h.sendClaudeError(w, 503, "api_error", "No available accounts")
		}
		return
	}

	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "claude", model, status, lastErr.Error(), startedAt, pc.clientIP())
	if stream.committedNow() {
		stream.sendEvent("error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": lastErr.Error()},
		})
	} else {
		h.sendClaudeError(w, status, "api_error", lastErr.Error())
	}
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// sseHeartbeatInterval is how often a ": ping" comment is emitted to keep
// idle connections alive through nginx / Cloudflare / load balancers that
// would otherwise time out the connection after 60-75 s of silence.
// The comment form is spec-compliant SSE and ignored by all SSE clients.
const sseHeartbeatInterval = 15 * time.Second

// sseStream wraps a ResponseWriter with a mutex so the heartbeat goroutine
// and the main streaming goroutine never interleave partial writes. The
// committed flag records whether any byte has been flushed; once true the
// HTTP status line is gone and errors must travel in-band via SSE.
type sseStream struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	flusher   http.Flusher
	committed bool
}

func newSSEStream(w http.ResponseWriter, flusher http.Flusher) *sseStream {
	return &sseStream{w: w, flusher: flusher}
}

// send writes one SSE event and flushes. Caller must hold s.mu OR call via
// the public sendEvent wrapper which acquires it.
func (s *sseStream) sendLocked(event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	s.flusher.Flush()
	s.committed = true
}

func (s *sseStream) sendEvent(event string, data interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLocked(event, data)
}

// committedNow reports whether any bytes have been flushed to the client.
func (s *sseStream) committedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// startHeartbeat starts a background goroutine that emits a ": ping\n\n"
// SSE comment every sseHeartbeatInterval. Returns a stop function that must
// be called (once) when the stream ends; it blocks until the goroutine exits.
func (s *sseStream) startHeartbeat() (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(sseHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.mu.Lock()
				fmt.Fprint(s.w, ": ping\n\n")
				s.flusher.Flush()
				s.committed = true
				s.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done); <-finished })
	}
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
// Hot-path counter updates (global stats, per-key usage, per-account stats) only
// mark config dirty; this coalesces them into one disk write per tick instead of
// writing the whole config under cfgLock on every request completion.
func (h *Handler) saveStats() {
	config.UpdateStats(
		atomic.LoadInt64(&h.totalRequests),
		atomic.LoadInt64(&h.successRequests),
		atomic.LoadInt64(&h.failedRequests),
		atomic.LoadInt64(&h.totalTokens),
		h.getCredits(),
	)
	if err := config.FlushDirty(); err != nil {
		logger.Warnf("[StatsSaver] Failed to persist config: %v", err)
	}
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
// model is recorded in the per-request log for the admin API Log view.
// u.ClientIP is the resolved real client IP (may be empty if KIRO_TRUST_PROXY is off);
// u.CacheRead/CacheWrite are prompt-cache tokens (non-zero only on Claude paths).
func (h *Handler) recordSuccessForApiKey(apiKeyID string, u requestUsage, model string, account *config.Account, endpoint string, startedAt time.Time) {
	h.recordSuccess(u.Input, u.Output, u.Credits)

	keyName, keyMasked := apiKeyLabels(apiKeyID)
	if apiKeyID != "" {
		if err := config.RecordApiKeyUsage(apiKeyID, int64(u.Input+u.Output), u.Credits, model, false); err != nil {
			logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
		}
		if h.usage != nil {
			h.usage.recordSuccess(apiKeyID, model, int64(u.Input), 0, int64(u.Output))
		}
		if h.keyLogHub != nil {
			h.keyLogHub.publish(apiKeyID, keyLogEntry{
				Status:     "ok",
				Model:      model,
				InTokens:   u.Input,
				OutTokens:  u.Output,
				Credits:    u.Credits,
				DurationMs: durationMs(startedAt),
				ClientIP:   u.ClientIP,
			})
		}
	}

	accountID := ""
	accountEmail := ""
	if account != nil {
		accountID = account.ID
		accountEmail = account.Email
	}

	logRequest(RequestLogEntry{
		Status:           "ok",
		Endpoint:         endpoint,
		APIKeyID:         apiKeyID,
		APIKeyName:       keyName,
		APIKeyMasked:     keyMasked,
		Model:            model,
		AccountID:        accountID,
		AccountEmail:     accountEmail,
		InputTokens:      u.Input,
		OutputTokens:     u.Output,
		CacheReadTokens:  u.CacheRead,
		CacheWriteTokens: u.CacheWrite,
		Credits:          u.Credits,
		DurationMs:       durationMs(startedAt),
		ClientIP:         u.ClientIP,
	})
}

// apiKeyLabels resolves the display name + masked value for a key id (both empty when unknown).
func apiKeyLabels(apiKeyID string) (name, masked string) {
	if apiKeyID == "" {
		return "", ""
	}
	if entry := config.GetApiKeyEntry(apiKeyID); entry != nil {
		return entry.Name, config.MaskApiKey(entry.Key)
	}
	return "", ""
}

func durationMs(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	return time.Since(startedAt).Milliseconds()
}

// recordFailureForApiKey is recordFailure + a failure entry in the per-request log so the
// admin API Log view shows what went wrong (endpoint, model, status code, error detail).
// It also drives the per-key usage history with a failure tick so daily/byModel stay accurate.
func (h *Handler) recordFailureForApiKey(apiKeyID, endpoint, model string, statusCode int, errMsg string, startedAt time.Time, clientIP string) {
	h.recordFailure()
	if apiKeyID != "" {
		if h.usage != nil {
			h.usage.recordFailure(apiKeyID, model)
		}
		if err := config.RecordApiKeyUsage(apiKeyID, 0, 0, model, true); err != nil {
			logger.Warnf("[ApiKey] failed to record failure for key %s: %v", apiKeyID, err)
		}
		if h.keyLogHub != nil {
			h.keyLogHub.publish(apiKeyID, keyLogEntry{
				Status:     "error",
				Model:      model,
				Error:      errMsg,
				DurationMs: durationMs(startedAt),
				ClientIP:   clientIP,
			})
		}
	}
	name, masked := apiKeyLabels(apiKeyID)
	logRequest(RequestLogEntry{
		Status:       "error",
		Endpoint:     endpoint,
		APIKeyID:     apiKeyID,
		APIKeyName:   name,
		APIKeyMasked: masked,
		Model:        model,
		StatusCode:   statusCode,
		Error:        errMsg,
		DurationMs:   durationMs(startedAt),
		ClientIP:     clientIP,
	})
}

func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, pc *passthroughCtx) {
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		step := h.nextUpstream(apiKeyID, model, pc.Endpoint, excluded)
		if step == nil {
			break
		}
		if step.Provider != nil {
			handled, err := h.serveViaProvider(w, step, pc, model, apiKeyID, startedAt, estimatedInputTokens)
			if handled {
				return
			}
			lastErr = err
			excluded[step.Provider.ID] = true
			continue
		}
		account := step.Account
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var truncated bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnTruncated: func() {
				truncated = true
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		// Nothing has been written yet on the non-streaming path, so a truncated
		// turn can still be surfaced as a real HTTP error rather than shipping a
		// half answer labelled complete. Rotate to another account and retry: the
		// client has seen nothing, so there is no partial output to duplicate.
		if truncated {
			lastErr = fmt.Errorf("upstream ended the stream before the turn completed")
			logger.Warnf("[Claude] turn truncated mid-generation on %s, rotating account", accountEmailForLog(account))
			excluded[account.ID] = true
			continue
		}

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:      inputTokens,
			Output:     outputTokens,
			CacheRead:  cacheUsage.CacheReadInputTokens,
			CacheWrite: cacheUsage.CacheCreationInputTokens,
			Credits:    credits,
			ClientIP:   pc.clientIP(),
		}, model, account, "claude", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		logSuspiciousReq("claude", model, inputTokens, outputTokens, len(toolUses) > 0)

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		// KiroToClaudeResponse cannot see max_tokens, so correct the stop_reason here
		// rather than widening its signature (it is also used by the notice paths that
		// have no payload). Without this a truncated answer reports "end_turn" and the
		// client treats a cut-off response as a clean finish.
		resp.StopReason = claudeStopReason(toolUses, outputTokens, payloadMaxTokens(payload))
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "claude", model, 503, "No available accounts", startedAt, pc.clientIP())
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "claude", model, status, lastErr.Error(), startedAt, pc.clientIP())
	h.sendClaudeError(w, status, "api_error", lastErr.Error())
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// sendOpenAINotice returns the limit-notice text as a normal (HTTP 200) assistant reply
// in OpenAI chat-completions shape. No upstream call, no billing.
func (h *Handler) sendOpenAINotice(w http.ResponseWriter, model string, stream bool, msg string) {
	outTok := noticeOutputTokens(msg)
	if !stream {
		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(msg, "", nil, 1, outTok, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(msg, "", nil, 1, outTok, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	chatID := "chatcmpl-" + uuid.New().String()
	created := time.Now().Unix()
	deltaChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]string{"content": msg},
			"finish_reason": nil,
		}},
	}
	data, _ := json.Marshal(deltaChunk)
	fmt.Fprintf(w, "data: %s\n\n", string(data))

	finalChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     1,
			"completion_tokens": outTok,
			"total_tokens":      1 + outTok,
		},
	}
	data, _ = json.Marshal(finalChunk)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// Valid-but-blocked key: render the limit-notice as a normal assistant reply.
	if limitNoticeRequested(r.Context()) {
		h.sendOpenAINotice(w, req.Model, req.Stream, config.GetLimitNoticeMessage())
		return
	}

	apiKeyID := apiKeyIDFromContext(r.Context())

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	// Apply global/per-key model override (ForceModel > per-key Model > client model).
	req.Model = applyModelOverride(actualModel, apiKeyID, thinkingCfg.Suffix)
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	safeOAIPayload := OpenAIToKiro(&req, thinking)
	richOAIPayload := safeOAIPayload
	if config.GetKeepToolHistory() {
		richOAIPayload = OpenAIToKiroWithHistoryMode(&req, thinking, true)
	}

	// The original body is kept so an external OpenAI-compatible provider can be
	// handed the request verbatim instead of the Kiro translation.
	pc := &passthroughCtx{
		Raw:         body,
		Header:      r.Header,
		Stream:      req.Stream,
		Endpoint:    config.ProviderEndpointChatCompletions,
		ClientIP:    h.resolveClientIP(r),
		SafePayload: safeOAIPayload,
	}

	if req.Stream {
		h.handleOpenAIStream(w, richOAIPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, pc)
	} else {
		h.handleOpenAINonStream(w, richOAIPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, pc)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, pc *passthroughCtx) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		step := h.nextUpstream(apiKeyID, model, pc.Endpoint, excluded)
		if step == nil {
			break
		}
		if step.Provider != nil {
			handled, err := h.serveViaProvider(w, step, pc, model, apiKeyID, startedAt, estimatedInputTokens)
			if handled {
				return
			}
			lastErr = err
			excluded[step.Provider.ID] = true
			continue
		}
		account := step.Account
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var truncated bool
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
			OnTruncated: func() {
				truncated = true
			},
		}

		safeP := payload
		if pc.SafePayload != nil {
			safeP = pc.SafePayload
		}
		err := callWithHistoryFallback(account, payload, safeP, callback, func() bool { return responseStarted })
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			if !responseStarted {
				continue
			}
			h.recordFailureForApiKey(apiKeyID, "openai", model, 0, err.Error(), startedAt, pc.clientIP())
			return
		}

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:    inputTokens,
			Output:   outputTokens,
			Credits:  credits,
			ClientIP: pc.clientIP(),
		}, model, account, "openai", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		logSuspiciousReq("openai", model, inputTokens, outputTokens, len(toolCalls) > 0)

		finishReason := "stop"
		if truncated {
			// The upstream died mid-turn. OpenAI's protocol has no way to withhold
			// finish_reason the way Anthropic does — omitting it entirely trips
			// strict clients — so report "length", the only value that means "this
			// output was cut off". Anything else tells the client a partial answer
			// finished cleanly.
			logger.Warnf("[Stream] openai turn truncated mid-generation: model=%s account=%s", model, accountEmailForLog(account))
			finishReason = "length"
		} else if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		} else if hitMaxTokens(outputTokens, payloadMaxTokens(payload)) {
			// Mirror the Claude path: a truncated response must not report "stop",
			// or the client treats a cut-off answer as a complete one.
			finishReason = "length"
		}

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": map[string]int{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "openai", model, 503, "No available accounts", startedAt, pc.clientIP())
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	// Use recordFailureForApiKey, not the bare recordFailure: the latter only bumps the
	// global counter, so an upstream quota 429 on streaming /v1/chat/completions was
	// written nowhere — not the request log, not the per-key SSE feed. Every sibling
	// handler (Claude stream/non-stream, OpenAI non-stream, /v1/responses) logs here.
	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "openai", model, status, lastErr.Error(), startedAt, pc.clientIP())
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, pc *passthroughCtx) {
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		step := h.nextUpstream(apiKeyID, model, pc.Endpoint, excluded)
		if step == nil {
			break
		}
		if step.Provider != nil {
			handled, err := h.serveViaProvider(w, step, pc, model, apiKeyID, startedAt, estimatedInputTokens)
			if handled {
				return
			}
			lastErr = err
			excluded[step.Provider.ID] = true
			continue
		}
		account := step.Account
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var truncated bool

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:   func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete:  func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:   func(c float64) { credits = c },
			OnTruncated: func() { truncated = true },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		// Nothing written yet — rotate and retry rather than return a half answer
		// that the response envelope would label as finished.
		if truncated {
			lastErr = fmt.Errorf("upstream ended the stream before the turn completed")
			logger.Warnf("[OpenAI] turn truncated mid-generation on %s, rotating account", accountEmailForLog(account))
			excluded[account.ID] = true
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:    inputTokens,
			Output:   outputTokens,
			Credits:  credits,
			ClientIP: pc.clientIP(),
		}, model, account, "openai", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		logSuspiciousReq("openai", model, inputTokens, outputTokens, len(toolUses) > 0)

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		// Surface truncation instead of reporting a clean "stop" — see claudeStopReason.
		if len(toolUses) == 0 && hitMaxTokens(outputTokens, payloadMaxTokens(payload)) {
			if choices, ok := resp["choices"].([]map[string]interface{}); ok && len(choices) > 0 {
				choices[0]["finish_reason"] = "length"
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "openai", model, 503, "No available accounts", startedAt, pc.clientIP())
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "openai", model, status, lastErr.Error(), startedAt, pc.clientIP())
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	mu := h.tokenRefreshLock(account.ID)
	mu.Lock()
	defer mu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
	}

	// 更新内存
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// 持久化
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
}

// ==================== 管理 API ====================

// passwordMatches compares a supplied admin password against the configured one in
// constant time so the highest-value secret is not exposed to a byte-by-byte timing
// side-channel (matches the API-key path).
func passwordMatches(supplied string) bool {
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(config.GetPassword())) == 1
}

// handleAdminLogin verifies the admin password and, on success, mints an opaque
// session token delivered in an HttpOnly cookie. The password is accepted from the
// JSON body ({"password","remember"}) or the X-Admin-Password header; it is never
// persisted anywhere client-side. Brute-force attempts are throttled per IP.
func (h *Handler) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method Not Allowed"})
		return
	}

	ip := h.resolveClientIP(r)
	if locked, retryAfter := h.adminGuard.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "Too many failed attempts, try again later"})
		return
	}

	password := r.Header.Get("X-Admin-Password")
	remember := r.URL.Query().Get("remember") == "1"
	if password == "" && r.Body != nil {
		var body struct {
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			password = body.Password
			remember = remember || body.Remember
		}
	}

	if !passwordMatches(password) {
		h.adminGuard.recordFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	h.adminGuard.recordSuccess(ip)

	if h.adminSessions == nil {
		// No session store (should not happen in production) — succeed without a
		// cookie so header-based clients still work.
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	token, err := h.adminSessions.mint()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "could not create session"})
		return
	}
	h.setAdminSessionCookie(w, r, token, remember)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// handleAdminLogout revokes the caller's session (if any) and clears the cookie.
func (h *Handler) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if h.adminSessions != nil {
		if c, err := r.Cookie(adminSessionCookieName); err == nil {
			h.adminSessions.revoke(c.Value)
		}
	}
	h.clearAdminSessionCookie(w, r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// adminAuthorized reports whether a request may access the admin API. A request is
// authorized by EITHER a valid opaque session cookie (the browser flow — the raw
// password is never stored client-side) OR a correct X-Admin-Password header (for
// curl / programmatic clients). The legacy plaintext-password cookie is no longer
// accepted (M5). suppliedPassword returns any header password so the caller can
// decide whether a rejection counts as a brute-force attempt.
func (h *Handler) adminAuthorized(r *http.Request) (ok bool, suppliedPassword string) {
	if h.adminSessions != nil {
		if c, err := r.Cookie(adminSessionCookieName); err == nil && h.adminSessions.valid(c.Value) {
			return true, ""
		}
	}
	pw := r.Header.Get("X-Admin-Password")
	if pw != "" {
		return passwordMatches(pw), pw
	}
	return false, ""
}

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// Brute-force throttle: reject early when this client IP is currently locked out
	// from too many wrong password attempts.
	ip := h.resolveClientIP(r)
	if locked, retryAfter := h.adminGuard.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "Too many failed attempts, try again later"})
		return
	}

	authorized, suppliedPassword := h.adminAuthorized(r)
	if !authorized {
		// Only a WRONG password counts as a brute-force attempt; a missing or stale
		// session cookie does not (the frontend simply redirects to the login page).
		if suppliedPassword != "" {
			h.adminGuard.recordFailure(ip)
		}
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	if suppliedPassword != "" {
		h.adminGuard.recordSuccess(ip)
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	// External providers. The /test suffix must be matched before the generic
	// /providers/{id} prefix routes below, or it would be read as an ID.
	case path == "/providers" && r.Method == "GET":
		h.apiGetProviders(w, r)
	case path == "/providers" && r.Method == "POST":
		h.apiAddProvider(w, r)
	case strings.HasPrefix(path, "/providers/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/providers/"), "/test")
		h.apiTestProvider(w, r, id)
	case strings.HasPrefix(path, "/providers/") && r.Method == "PUT":
		h.apiUpdateProvider(w, r, strings.TrimPrefix(path, "/providers/"))
	case strings.HasPrefix(path, "/providers/") && r.Method == "DELETE":
		h.apiDeleteProvider(w, r, strings.TrimPrefix(path, "/providers/"))

	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/auth/local-cache/scan" && r.Method == "GET":
		h.apiScanLocalCache(w, r)
	case path == "/auth/local-cache/import" && r.Method == "POST":
		h.apiImportLocalCache(w, r)
	case path == "/auth/apikeys-batch" && r.Method == "POST":
		h.apiImportApiKeys(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/request-logs/stream" && r.Method == "GET":
		h.apiStreamRequestLogs(w, r)
	case path == "/request-logs" && r.Method == "GET":
		h.apiGetRequestLogs(w, r)
	case path == "/request-logs" && r.Method == "DELETE":
		h.apiClearRequestLogs(w, r)
	case path == "/usage-summary" && r.Method == "GET":
		h.apiGetUsageSummary(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs/stream" && r.Method == "GET":
		h.apiStreamLogs(w, r)
	case path == "/logs/level" && r.Method == "GET":
		h.apiGetLogLevel(w, r)
	case path == "/logs/level" && r.Method == "POST":
		h.apiSetLogLevel(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/proxy/import" && r.Method == "POST":
		h.apiImportProxies(w, r)
	case path == "/proxy/pool" && r.Method == "GET":
		h.apiGetProxyPool(w, r)
	case path == "/proxy/pool" && r.Method == "POST":
		h.apiAddProxyPool(w, r)
	case path == "/proxy/pool" && r.Method == "DELETE":
		h.apiRemoveProxyPool(w, r)
	case path == "/proxy/pool/toggle" && r.Method == "POST":
		h.apiToggleProxyPool(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case path == "/api-keys/bulk" && r.Method == "POST":
		h.apiBulkCreateApiKeys(w, r)
	case path == "/api-keys/bulk" && r.Method == "DELETE":
		h.apiBulkDeleteApiKeys(w, r)
	case path == "/api-keys/export" && r.Method == "POST":
		h.apiExportApiKeys(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-all") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-all")
		h.apiResetApiKeyUsageAll(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))

	// IP 封禁 / 审计 / 异常检测 / 维护模式 / 充值台账
	case path == "/ip-bans" && r.Method == "GET":
		h.apiGetIPBans(w, r)
	case path == "/ip-bans" && r.Method == "POST":
		h.apiAddIPBan(w, r)
	case path == "/ip-bans" && r.Method == "DELETE":
		h.apiClearIPBans(w, r)
	case path == "/ip-bans/settings" && r.Method == "PATCH":
		h.apiUpdateIPBanSettings(w, r)
	case strings.HasPrefix(path, "/ip-bans/") && r.Method == "DELETE":
		h.apiDeleteIPBan(w, r, strings.TrimPrefix(path, "/ip-bans/"))
	case path == "/anomalies" && r.Method == "GET":
		h.apiGetAnomalies(w, r)
	case path == "/audit-log" && r.Method == "GET":
		h.apiGetAuditLog(w, r)
	case path == "/maintenance" && r.Method == "POST":
		h.apiSetMaintenanceMode(w, r)
	case path == "/credit-topups" && r.Method == "GET":
		h.salesTopupsAdmin(w, r)
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"priority":          a.Priority,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}

	// Handle API-key credential creation
	if account.KiroApiKey != "" || strings.EqualFold(account.AuthMethod, "api_key") || strings.EqualFold(account.AuthMethod, "apikey") {
		// Reject empty API key
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		// Normalize authMethod and set expiry
		account.AuthMethod = "api_key"
		account.ExpiresAt = 0
		account.AccessToken = account.KiroApiKey // Set accessToken to kiroApiKey for pool compatibility
		// Don't call RefreshToken for API-key accounts
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token（或是 API-key 账号），立即拉取并缓存模型列表
	if account.Enabled && (account.AccessToken != "" || account.IsApiKeyCredential()) {
		acc := account
		safeGo(func() {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["priority"].(float64); ok && v >= 0 {
		existing.Priority = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		acc := *existing
		safeGo(func() {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			a := acc
			safeGo(func() {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// 刷新 token
			if account.RefreshToken != "" {
				if newAccess, newRefresh, newExpires, profileArn, err := auth.RefreshToken(account); err == nil {
					account.AccessToken = newAccess
					if newRefresh != "" {
						account.RefreshToken = newRefresh
					}
					account.ExpiresAt = newExpires
					config.UpdateAccountToken(id, newAccess, newRefresh, newExpires)
					if profileArn != "" {
						account.ProfileArn = profileArn
						config.UpdateAccountProfileArn(id, profileArn)
					}
					h.pool.UpdateToken(id, newAccess, newRefresh, newExpires)
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginHint string `json:"loginHint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	redirectBase := resolvePublicBaseURL()
	sessionID, authorizeURL, loopbackPort, err := auth.StartKiroSsoLogin(req.LoginHint, redirectBase)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeURL,
		"loopbackPort": loopbackPort,
	})
}

func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.SessionID == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId is required"})
		return
	}

	status, result, err := auth.PollKiroSsoLogin(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "pending",
		})
		return
	}

	// Flow completed — tạo account
	if result.AccessToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No access token received"})
		return
	}

	// Lấy user info từ login hint (có thể cập nhật sau qua RefreshAccountInfo)
	email := result.UserEmail
	if email == "" {
		email = result.LoginHint
	}

	account := config.Account{
		ID:               auth.GenerateAccountID(),
		Email:            email,
		AccessToken:      result.AccessToken,
		RefreshToken:     result.RefreshToken,
		AuthMethod:       "external_idp",
		Provider:         "MicrosoftEntra",
		IssuerURL:        result.IssuerURL,
		IdPClientID:      result.IdPClientID,
		Scopes:           result.Scopes,
		LoginHint:        result.LoginHint,
		IdPTokenEndpoint: result.IdPTokenEndpoint,
		Region:           "us-east-1",
		ExpiresAt:        time.Now().Unix() + int64(result.ExpiresIn),
		Enabled:          true,
		MachineId:        config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()

	// Resolve profile ARN + fetch account info in background.
	// external_idp accounts: resolve profileArn via ListAvailableProfiles (with
	// TokenType: EXTERNAL_IDP header) first — this is the correct health-check.
	// GetUsageLimits is optional metadata; failure does NOT ban the account.
	safeGo(func() {
		// Step 1: Resolve profile ARN (required for runtime generate endpoint)
		if _, err := ResolveProfileArn(&account); err != nil {
			logger.Warnf("[apiPollKiroSso] Profile ARN resolve failed for external_idp account %s: %v", account.Email, err)
		}
		// Step 2: Usage/subscription info (best-effort, non-fatal)
		if _, err := RefreshAccountInfo(&account); err != nil {
			logger.Warnf("[apiPollKiroSso] Account info refresh failed for external_idp account %s: %v", account.Email, err)
		}
		h.fetchAndCacheAccountModels(&account)
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "completed",
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	auth.CancelKiroSsoLogin(req.SessionID)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req credentialImportPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 设置默认值
	if req.Region == "" {
		req.Region = "us-east-1"
	}

	// Handle API-key credential import (no refresh token required)
	if req.KiroApiKey != "" || strings.EqualFold(req.AuthMethod, "api_key") || strings.EqualFold(req.AuthMethod, "apikey") {
		if req.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}

		account := config.Account{
			ID:          auth.GenerateAccountID(),
			Nickname:    req.Nickname,
			KiroApiKey:  req.KiroApiKey,
			AccessToken: req.KiroApiKey, // mirror for pool compatibility
			AuthMethod:  "api_key",
			Region:      req.Region,
			AuthRegion:  req.AuthRegion,
			ApiRegion:   req.ApiRegion,
			ExpiresAt:   0,
			Enabled:     true,
			MachineId:   config.GenerateMachineId(),
		}

		// Best-effort: fetch account info to populate email/usage.
		if info, infoErr := RefreshAccountInfo(&account); infoErr == nil && info != nil {
			account.Email = info.Email
			account.UserId = info.UserId
			account.SubscriptionType = info.SubscriptionType
			account.SubscriptionTitle = info.SubscriptionTitle
			account.DaysRemaining = info.DaysRemaining
			account.UsageCurrent = info.UsageCurrent
			account.UsageLimit = info.UsageLimit
			account.UsagePercent = info.UsagePercent
			account.NextResetDate = info.NextResetDate
			account.LastRefresh = info.LastRefresh
		}

		if err := config.AddAccount(account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		h.pool.Reload()
		acc := account
		safeGo(func() {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new api_key account %s: %v", acc.Email, err)
			}
		})

		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"account": map[string]interface{}{
				"id":    account.ID,
				"email": account.Email,
			},
		})
		return
	}

	account, status, err := h.importOAuthCredential(req)
	if err != nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// credentialImportPayload is the request body shared by the credential import
// endpoint and the local-cache importer.
type credentialImportPayload struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider"`
	Region       string `json:"region"`
	AuthRegion   string `json:"authRegion"`
	ApiRegion    string `json:"apiRegion"`
	KiroApiKey   string `json:"kiroApiKey"`
	Nickname     string `json:"nickname"`
	IssuerURL    string `json:"issuerUrl"`
	IdPClientID  string `json:"idpClientId"`
	Scopes       string `json:"scopes"`
	LoginHint    string `json:"loginHint"`
}

// importOAuthCredential performs the refresh-token based credential import
// (idc / social / external_idp). It returns the created account, or an HTTP
// status code plus error on failure. Shared by the HTTP handler and the
// local-cache auto-import so both paths behave identically.
func (h *Handler) importOAuthCredential(req credentialImportPayload) (*config.Account, int, error) {
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	if req.RefreshToken == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("refreshToken is required")
	}
	if req.AuthMethod == "" {
		if req.ClientID != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}
	// 标准化 authMethod
	switch strings.ToLower(req.AuthMethod) {
	case "idc", "builderid", "enterprise":
		req.AuthMethod = "idc"
	case "social", "google", "github":
		req.AuthMethod = "social"
	case "external_idp", "externalidp":
		req.AuthMethod = "external_idp"
	default:
		if req.ClientID != "" && req.ClientSecret != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}

	// Backend-side external_idp detection: an older cached frontend (or a JSON
	// export missing the authMethod field) can mislabel a Microsoft Entra
	// account as "social", which then refreshes against the wrong endpoint and
	// fails with 401 "Bad credentials". Trust the IdP signals over the label:
	// presence of issuerUrl / idpClientId / a Microsoft-style provider means
	// this is an External IdP account regardless of what authMethod claims.
	if req.AuthMethod != "external_idp" {
		if req.IssuerURL != "" || req.IdPClientID != "" ||
			strings.Contains(strings.ToLower(req.Provider), "entra") ||
			strings.Contains(strings.ToLower(req.Provider), "microsoft") {
			req.AuthMethod = "external_idp"
		}
	}

	// SSRF guard at the trust boundary: for external_idp accounts the very first thing
	// we do below is a token refresh that runs OIDC discovery + a POST against the
	// (untrusted) issuer URL. Enforce the external-IdP allow-list here, before that
	// request, so a malicious import (admin JSON or a poisoned local-cache entry)
	// cannot point the refresh at an internal address (e.g. cloud metadata) and
	// exfiltrate the refresh token. This is the primary M2 fix; the auth-package
	// refresh path re-validates as defense-in-depth.
	if req.AuthMethod == "external_idp" {
		if err := auth.ValidateExternalIdpURL(req.IssuerURL); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid external IdP issuer URL: %w", err)
		}
	}

	// 用 refreshToken 刷新获取新的 accessToken。导入必须以一次成功的刷新为前提：
	// 本地缓存里的 accessToken 不携带可信的过期时间，盲猜短 TTL 会让账号在选号时
	// 永远被跳过，导致后台/按需刷新都无法触发（详见 ensureValidToken 与 Pick 的过期判定）。
	tempAccount := &config.Account{
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Region:       req.Region,
		IssuerURL:    req.IssuerURL,
		IdPClientID:  req.IdPClientID,
		Scopes:       req.Scopes,
	}
	accessToken, newRefreshToken, expiresAt, newProfileArn, err := auth.RefreshToken(tempAccount)
	if err != nil {
		// Refresh may fail if token already consumed elsewhere (token rotation).
		// If import payload includes an accessToken, use it directly and let
		// background refresh handle renewal later.
		if req.AccessToken != "" {
			logger.Warnf("[ImportCredentials] Token refresh failed for %s (using provided accessToken): %v", req.AuthMethod, err)
			accessToken = req.AccessToken
			newRefreshToken = req.RefreshToken
			expiresAt = 0 // imported token may be stale; let pool refresh immediately
			newProfileArn = ""
		} else {
			return nil, http.StatusBadRequest, fmt.Errorf("Token refresh failed: %s", err.Error())
		}
	}
	if newRefreshToken != "" {
		req.RefreshToken = newRefreshToken
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)
	// external_idp: GetUserInfo may fail for Microsoft-issued tokens; fallback to loginHint
	if email == "" && req.LoginHint != "" {
		email = req.LoginHint
	}

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		Nickname:     req.Nickname,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Provider:     req.Provider,
		Region:       req.Region,
		AuthRegion:   req.AuthRegion,
		ApiRegion:    req.ApiRegion,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   newProfileArn,
		IssuerURL:    req.IssuerURL,
		IdPClientID:  req.IdPClientID,
		Scopes:       req.Scopes,
		LoginHint:    req.LoginHint,
	}

	if err := config.AddAccount(account); err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// external_idp: resolve profileArn after import (ListAvailableProfiles with TokenType header)
	if req.AuthMethod == "external_idp" {
		acc := account
		safeGo(func() {
			if _, err := ResolveProfileArn(&acc); err != nil {
				logger.Warnf("[ImportCredentials] Profile ARN resolve failed for external_idp account %s: %v", acc.Email, err)
			}
		})
	}

	h.pool.Reload()
	return &account, http.StatusOK, nil
}

// apiScanLocalCache handles GET /admin/api/auth/local-cache/scan.
//
// It scans the local AWS SSO cache (~/.aws/sso/cache) for Kiro IDE credentials
// and returns a non-secret summary of each discovered identity so the UI can
// present them for one-click import. Secrets are never included in the response;
// each entry carries a fingerprint used to reference it in the import call.
//
// Returns available=false (not an error) when no local cache exists, so the UI
// can hide the feature on containerized/remote deployments.
func (h *Handler) apiScanLocalCache(w http.ResponseWriter, r *http.Request) {
	dir, _ := auth.LocalCacheDir()
	creds, err := auth.ScanLocalKiroCredentials()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	type item struct {
		Fingerprint string `json:"fingerprint"`
		SourceFile  string `json:"sourceFile"`
		AuthMethod  string `json:"authMethod"`
		Provider    string `json:"provider"`
		Region      string `json:"region"`
		LoginHint   string `json:"loginHint,omitempty"`
		HasClient   bool   `json:"hasClient"`
		Importable  bool   `json:"importable"`
		Reason      string `json:"reason,omitempty"`
	}
	items := make([]item, 0, len(creds))
	for _, c := range creds {
		it := item{
			Fingerprint: c.Fingerprint,
			SourceFile:  c.SourceFile,
			AuthMethod:  c.AuthMethod,
			Provider:    c.Provider,
			Region:      c.Region,
			LoginHint:   c.LoginHint,
			HasClient:   c.HasClient,
			Importable:  true,
		}
		// IdC tokens need a client registration to refresh; flag if missing.
		if c.AuthMethod == "idc" && !c.HasClient {
			it.Importable = false
			it.Reason = "missing clientId/clientSecret (client registration file not found)"
		}
		items = append(items, it)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"available": len(creds) > 0,
		"cacheDir":  dir,
		"count":     len(items),
		"accounts":  items,
	})
}

// apiImportLocalCache handles POST /admin/api/auth/local-cache/import.
//
// Body: {"fingerprints": ["tok-abcd1234", ...]} — the fingerprints returned by
// the scan endpoint. When omitted or empty, every importable credential found
// in the cache is imported. Each credential is run through the same OAuth import
// path as manual credential import, so region probing and profile resolution
// apply automatically.
func (h *Handler) apiImportLocalCache(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprints []string `json:"fingerprints"`
	}
	// Body is optional; ignore decode errors and treat as "import all".
	_ = json.NewDecoder(r.Body).Decode(&req)

	creds, err := auth.ScanLocalKiroCredentials()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(creds) == 0 {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "no local Kiro credentials found"})
		return
	}

	want := make(map[string]bool, len(req.Fingerprints))
	for _, f := range req.Fingerprints {
		want[f] = true
	}

	type result struct {
		Fingerprint string `json:"fingerprint"`
		SourceFile  string `json:"sourceFile"`
		Success     bool   `json:"success"`
		AccountID   string `json:"accountId,omitempty"`
		Email       string `json:"email,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	var results []result
	imported := 0
	for _, c := range creds {
		if len(want) > 0 && !want[c.Fingerprint] {
			continue
		}
		res := result{Fingerprint: c.Fingerprint, SourceFile: c.SourceFile}

		if c.AuthMethod == "idc" && !c.HasClient {
			res.Error = "missing clientId/clientSecret"
			results = append(results, res)
			continue
		}

		account, _, importErr := h.importOAuthCredential(credentialImportPayload{
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			AuthMethod:   c.AuthMethod,
			Provider:     c.Provider,
			Region:       c.Region,
			IssuerURL:    c.IssuerURL,
			IdPClientID:  c.IdPClientID,
			Scopes:       c.Scopes,
			LoginHint:    c.LoginHint,
		})
		if importErr != nil {
			res.Error = importErr.Error()
			results = append(results, res)
			continue
		}
		res.Success = true
		res.AccountID = account.ID
		res.Email = account.Email
		imported++
		results = append(results, res)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  imported > 0,
		"imported": imported,
		"total":    len(results),
		"results":  results,
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests,
		"successRequests": h.successRequests,
		"failedRequests":  h.failedRequests,
		"totalTokens":     h.totalTokens,
		"totalCredits":    h.totalCredits,
		"uptime":          time.Now().Unix() - h.startTime,

		"maintenance":        config.GetMaintenanceMode(),
		"maintenanceMessage": config.GetMaintenanceMessage(),
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":             config.GetApiKey(),
		"requireApiKey":      config.IsApiKeyRequired(),
		"port":               config.GetPort(),
		"host":               config.GetHost(),
		"allowOverUsage":     config.GetAllowOverUsage(),
		"maxPayloadBytes":    config.GetMaxPayloadBytes(),
		"publicBaseURL":      config.GetPublicBaseURL(),
		"limitNoticeMessage": config.GetLimitNoticeMessage(),
		"forceModel":         config.GetForceModel(),
		"identityModel":      config.GetIdentityModel(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey             *string `json:"apiKey,omitempty"`
		RequireApiKey      *bool   `json:"requireApiKey,omitempty"`
		Password           string  `json:"password,omitempty"`
		AllowOverUsage     *bool   `json:"allowOverUsage,omitempty"`
		MaxPayloadBytes    *int    `json:"maxPayloadBytes,omitempty"`
		PublicBaseURL      *string `json:"publicBaseURL,omitempty"`
		LimitNoticeMessage *string `json:"limitNoticeMessage,omitempty"`
		ForceModel         *string `json:"forceModel,omitempty"`
		IdentityModel      *string `json:"identityModel,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}

	// maxPayloadBytes is read per-request by truncatePayloadToLimit, so the new
	// value takes effect on the next request — no restart or pool reload needed.
	if req.MaxPayloadBytes != nil {
		if err := config.UpdateMaxPayloadBytes(*req.MaxPayloadBytes); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.PublicBaseURL != nil {
		if err := config.UpdatePublicBaseURL(*req.PublicBaseURL); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.LimitNoticeMessage != nil {
		if err := config.SetLimitNoticeMessage(strings.TrimSpace(*req.LimitNoticeMessage)); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.ForceModel != nil {
		if err := config.SetForceModel(strings.TrimSpace(*req.ForceModel)); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.IdentityModel != nil {
		if err := config.SetIdentityModel(strings.TrimSpace(*req.IdentityModel)); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
		if err != nil {
			return err
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(id, profileArn)
		}
		return nil
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"issuerUrl":         account.IssuerURL,
		"idpClientId":       account.IdPClientID,
		"scopes":            account.Scopes,
		"loginHint":         account.LoginHint,
		"region":            account.Region,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
		if m.TokenLimits != nil {
			registerModelWindow(m.ModelId, m.TokenLimits.MaxInputTokens)
		}
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

// setWebSecurityHeaders adds anti-clickjacking and defense-in-depth headers to the
// HTML admin panel and self-service portal (M6).
//
//   - X-Frame-Options: DENY and CSP frame-ancestors 'none' stop the pages from being
//     embedded in an iframe, blocking clickjacking of a logged-in admin.
//   - X-Content-Type-Options: nosniff prevents MIME-sniffing of served assets.
//   - Referrer-Policy: no-referrer keeps admin URLs/keys out of Referer headers.
//
// The CSP intentionally does NOT lock down script-src/style-src: the panel uses inline
// <script> blocks, ~80 inline event handlers, and a runtime Tailwind build, so a strict
// policy would break it. It restricts the safe-to-restrict directives (framing, objects,
// base URI). A full script/style lockdown would require reworking the frontend to use
// nonces and external handlers.
func setWebSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; object-src 'none'; base-uri 'none'")
}

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	setWebSecurityHeaders(w)
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	setWebSecurityHeaders(w)
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// logEntryJSON is the wire shape for a captured log line.
type logEntryJSON struct {
	Ts    int64  `json:"ts"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

func toLogEntryJSON(e logger.Entry) logEntryJSON {
	return logEntryJSON{
		Ts:    e.Time.UnixMilli(),
		Level: logger.LevelName(e.Level),
		Text:  e.Text,
	}
}

// apiGetLogs GET /admin/api/logs - returns retained log history (SSE fallback).
func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	hist := logger.History()
	out := make([]logEntryJSON, 0, len(hist))
	for _, e := range hist {
		out = append(out, toLogEntryJSON(e))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"level": logger.LevelName(logger.GetLevel()),
		"logs":  out,
	})
}

// apiStreamLogs GET /admin/api/logs/stream - Server-Sent Events stream of log lines.
func (h *Handler) apiStreamLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Streaming not supported"})
		return
	}

	// handleAdminAPI sets application/json; override for SSE.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEntry := func(e logger.Entry) {
		data, _ := json.Marshal(toLogEntryJSON(e))
		fmt.Fprintf(w, "data: %s\n\n", data)
	}

	// Backfill retained history first, then stream live entries.
	for _, e := range logger.History() {
		writeEntry(e)
	}
	flusher.Flush()

	ch, cancel := logger.Subscribe()
	defer cancel()

	ctx := r.Context()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			writeEntry(e)
			// Coalesce: drain everything already queued and write it in one
			// batch so a burst costs a single flush (one socket write) instead
			// of one per line.
			for drained := true; drained; {
				select {
				case e2 := <-ch:
					writeEntry(e2)
				default:
					drained = false
				}
			}
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// apiGetLogLevel GET /admin/api/logs/level - returns the active log level.
func (h *Handler) apiGetLogLevel(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"level": logger.LevelName(logger.GetLevel())})
}

// apiSetLogLevel POST /admin/api/logs/level - changes the active log level at runtime and persists it.
func (h *Handler) apiSetLogLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	lvl, ok := logger.ParseLevel(req.Level)
	if !ok {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid level, must be: debug, info, warn, or error"})
		return
	}
	logger.SetLevel(lvl)
	if err := config.UpdateLogLevel(logger.LevelName(lvl)); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"level": logger.LevelName(lvl)})
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"proxyURL":     config.GetProxyURL(),
		"requireProxy": config.GetRequireProxy(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL     string `json:"proxyURL"`
		RequireProxy *bool  `json:"requireProxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	if req.RequireProxy != nil {
		if err := config.UpdateRequireProxy(*req.RequireProxy); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// validProxyScheme 校验代理 URL 前缀，与 apiUpdateProxy 保持一致。
func validProxyScheme(u string) bool {
	return strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "socks5://") ||
		strings.HasPrefix(u, "socks5h://")
}

// apiGetProxyPool 返回共享代理池（含完整 URL，客户端负责展示脱敏）
func (h *Handler) apiGetProxyPool(w http.ResponseWriter, r *http.Request) {
	pool := config.GetProxyPool()
	entries := make([]map[string]any, 0, len(pool))
	for _, p := range pool {
		entries = append(entries, map[string]any{
			"url":               p.URL,
			"healthy":           p.Healthy,
			"failCount":         p.FailCount,
			"lastFailAt":        p.LastFailAt,
			"lastOkAt":          p.LastOKAt,
			"disabledPermanent": p.DisabledPermanent,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"pool": entries})
}

// apiAddProxyPool 向代理池批量添加，接受 {url} 或 {urls:[...]}
func (h *Handler) apiAddProxyPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string   `json:"url"`
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	if len(urls) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "url or urls required"})
		return
	}

	existing := make(map[string]bool)
	for _, p := range config.GetProxyPool() {
		existing[p.URL] = true
	}

	added, skipped := 0, 0
	for _, u := range urls {
		if !validProxyScheme(u) || existing[u] {
			skipped++
			continue
		}
		if err := config.AddProxyToPool(u); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		existing[u] = true
		added++
	}
	json.NewEncoder(w).Encode(map[string]int{
		"added":   added,
		"skipped": skipped,
		"total":   len(config.GetProxyPool()),
	})
}

// apiRemoveProxyPool 从代理池移除指定 URL
func (h *Handler) apiRemoveProxyPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.URL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "url required"})
		return
	}
	if err := config.RemoveProxyFromPool(req.URL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiToggleProxyPool 永久禁用/启用某个池化代理
func (h *Handler) apiToggleProxyPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.URL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "url required"})
		return
	}
	if err := config.SetProxyPoolDisabled(req.URL, req.Disabled); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
		IssuerURL    string `json:"issuerUrl,omitempty"`
		IdPClientID  string `json:"idpClientId,omitempty"`
		Scopes       string `json:"scopes,omitempty"`
		LoginHint    string `json:"loginHint,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ExpiresAt:    a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:   authMethod,
				Provider:     a.Provider,
				IssuerURL:    a.IssuerURL,
				IdPClientID:  a.IdPClientID,
				Scopes:       a.Scopes,
				LoginHint:    a.LoginHint,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
