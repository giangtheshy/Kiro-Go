// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	KiroApiKey   string `json:"kiroApiKey,omitempty"`   // API key credential for headless auth (used directly as bearer token)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC), "social" (GitHub/Google), or "api_key"
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub")
	Region       string `json:"region"`                 // AWS region (fallback for both auth and API region)
	AuthRegion   string `json:"authRegion,omitempty"`   // Region for token refresh endpoints; falls back to region
	ApiRegion    string `json:"apiRegion,omitempty"`    // Region for API request hosts; falls back to region
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL

	// External IdP authentication fields (for "Your organization" Kiro SSO flow)
	IssuerURL   string `json:"issuerUrl,omitempty"`   // IdP OIDC issuer (e.g. https://login.microsoftonline.com/<tenant>/v2.0)
	IdPClientID string `json:"idpClientId,omitempty"` // Client ID registered with the external IdP (from Kiro portal)
	Scopes      string `json:"scopes,omitempty"`      // Space-separated scopes for the IdP token endpoint
	LoginHint   string `json:"loginHint,omitempty"`   // User email used as login_hint during IdP authorization

	IdPTokenEndpoint string `json:"idpTokenEndpoint,omitempty"` // Cached IdP token endpoint (resolved via OIDC discovery during login)

	ExpiresAt  int64  `json:"expiresAt,omitempty"`  // Token expiration timestamp (Unix seconds)
	MachineId  string `json:"machineId,omitempty"`  // UUID machine identifier for request tracking
	ProfileArn string `json:"profileArn,omitempty"` // CodeWhisperer/Kiro profile ARN for generation requests

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Priority is the routing tier this account sits in. Upstreams are tried in
	// ascending tier order and external Providers carry the same field, so an
	// operator can interleave them (e.g. acc1=0, provider=1, acc2=2). The default 0
	// puts every account in one tier, which is the historical behaviour: the whole
	// weighted-round-robin pool is tried before any external provider.
	Priority int `json:"priority,omitempty"`

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// IsApiKeyCredential returns true if this account is authenticated via API key.
// An account is an API-key credential if KiroApiKey is non-empty OR if authMethod
// is "api_key" or "apikey" (case-insensitive).
func (a *Account) IsApiKeyCredential() bool {
	if a.KiroApiKey != "" {
		return true
	}
	method := strings.ToLower(a.AuthMethod)
	return method == "api_key" || method == "apikey"
}

// EffectiveAuthRegion returns the effective auth region for this account,
// resolved using the fallback chain:
// account.authRegion > account.region > global authRegion > global region > "us-east-1"
func (a *Account) EffectiveAuthRegion() string {
	if a.AuthRegion != "" {
		return a.AuthRegion
	}
	if a.Region != "" {
		return a.Region
	}
	authRegion := GetGlobalAuthRegion()
	if authRegion != "us-east-1" {
		return authRegion
	}
	globalRegion := GetGlobalRegion()
	if globalRegion != "us-east-1" {
		return globalRegion
	}
	return "us-east-1"
}

// EffectiveApiRegion returns the effective API region for this account,
// resolved using the fallback chain:
// account.apiRegion > account.region > global apiRegion > global region > "us-east-1"
func (a *Account) EffectiveApiRegion() string {
	if a.ApiRegion != "" {
		return a.ApiRegion
	}
	if a.Region != "" {
		return a.Region
	}
	apiRegion := GetGlobalApiRegion()
	if apiRegion != "us-east-1" {
		return apiRegion
	}
	globalRegion := GetGlobalRegion()
	if globalRegion != "us-east-1" {
		return globalRegion
	}
	return "us-east-1"
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// ExpiresAt is an optional expiry timestamp (Unix seconds). 0 = never expires.
	// Once now >= ExpiresAt, the key is rejected at authentication like a disabled key.
	ExpiresAt int64 `json:"expiresAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Rate/abuse limits (0 = unlimited).
	// RPMLimit throttles requests per minute for this key (token-bucket delay, not rejection).
	// IPLimit caps concurrent distinct client IPs using this key.
	// IPAllowlist, when non-empty, restricts the key to the listed client IPs/CIDRs.
	// TPMLimit is display-only metadata (tokens per minute); not enforced here.
	RPMLimit    int      `json:"rpmLimit,omitempty"`
	IPLimit     int      `json:"ipLimit,omitempty"`
	IPAllowlist []string `json:"ipAllowlist,omitempty"`
	TPMLimit    int      `json:"tpmLimit,omitempty"`

	// Customer attribution. These let an operator answer "whose key is this?" from
	// the admin panel alone, without opening the sales system's database. They are
	// opaque labels to this service: nothing routes or authorizes on them.
	// OwnerRef identifies the buyer (e.g. a Telegram user ID), OrderRef the order
	// that produced the key, and Note is free-form operator text.
	OwnerRef string `json:"ownerRef,omitempty"`
	OrderRef string `json:"orderRef,omitempty"`
	Note     string `json:"note,omitempty"`

	// BoundAccountIDs restricts this key to a fixed set of accounts. When non-empty,
	// a request authenticated with this key routes ONLY through these accounts (still
	// subject to per-account cooldown/quota/model filtering). If none of the bound
	// accounts is currently usable, routing falls back to the shared pool so the
	// request is not hard-failed. Empty = no binding (use the shared pool).
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`

	// Model is the legacy single-model override, kept only for migration. New code
	// reads Models instead; on first load a non-empty Model is folded into Models and
	// then cleared. Do not use directly.
	Model string `json:"model,omitempty"`

	// Models is the per-key model allowlist. When non-empty, a request authenticated
	// with this key may use any client-requested model that is in the list; a request
	// for a model NOT in the list is remapped to the first entry. Empty = no restriction
	// (use the client's model). The global ForceModel setting still takes precedence.
	Models []string `json:"models,omitempty"`

	// Current-period usage (cleared by "Reset Usage" for a fresh quota cycle).
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`

	// Lifetime usage. Same additions as the current-period counters, but "Reset Usage"
	// NEVER touches these — they only grow, so an operator always sees the true grand
	// total from the key's creation. They are cleared only by ResetApiKeyUsageAll
	// ("Reset All"). Seeded from the current-period counters on first load (migration)
	// so existing keys don't show a lower lifetime total than what they've already used.
	LifetimeTokens   int64   `json:"lifetimeTokens,omitempty"`
	LifetimeCredits  float64 `json:"lifetimeCredits,omitempty"`
	LifetimeRequests int64   `json:"lifetimeRequests,omitempty"`

	// Daily and ByModel are persisted rolling usage breakdowns kept by
	// config/usage_history.go. They complement the cumulative counters above by
	// letting an operator see per-day trends and model-level consumption without
	// running a separate analytics service. Daily keeps the last UsageDaysKept
	// calendar days; ByModel keeps up to ModelTallyKept entries. Both survive
	// restarts because they live in config.json, unlike the in-memory usageStats
	// in proxy/ which is lost on every restart.
	Daily   []DailyUsage  `json:"daily,omitempty"`
	ByModel []ModelTally  `json:"byModel,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Providers are external OpenAI-/Anthropic-compatible upstreams that serve
	// requests alongside the Kiro pool. See config/providers.go.
	Providers []Provider `json:"providers,omitempty"`

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// StreamContinuation controls whether a turn cut off mid-generation is
	// transparently resumed by replaying the delivered text back to the model and
	// asking it to continue. Defaults to true. Each resume costs an extra upstream
	// turn, so set it to false to trade seamless answers for strictly one billed
	// turn per request.
	StreamContinuation *bool `json:"streamContinuation,omitempty"`

	// KeepToolHistory controls whether correctly-paired assistant tool-call turns
	// keep their structured form in history, giving the model in-context evidence
	// of the invocation→result pattern. Defaults to true. Disable to always use
	// the flattened (safe) payload if the upstream rejects the richer one.
	KeepToolHistory *bool `json:"keepToolHistory,omitempty"`

	// Global default regions. Used as a fallback when an account does not
	// specify its own region. All default to "us-east-1" when empty.
	Region     string `json:"region,omitempty"`     // Default region for both auth and API
	AuthRegion string `json:"authRegion,omitempty"` // Default region for token refresh endpoints
	ApiRegion  string `json:"apiRegion,omitempty"`  // Default region for API request hosts

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// MaxPayloadBytes is the upper bound for the serialized Kiro request body.
	// Requests above this are truncated (oldest history dropped) before dispatch.
	// 0 means "use DefaultMaxPayloadBytes". Read per-request, so changes apply
	// at runtime without a restart. Do not set >~2.15MB: AWS rejects oversized
	// bodies with 400 CONTENT_LENGTH_EXCEEDS_THRESHOLD and there is no shrink-retry.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// RequireProxy, when true, blocks any outbound Kiro request for an account
	// that has neither a per-account proxy nor a global proxy. The request is
	// failed (and the account rotated) instead of connecting directly, so the
	// server's real IP is never exposed. Default false = current behavior.
	RequireProxy bool `json:"requireProxy,omitempty"`

	// ProxyPool is a shared pool of outbound proxies with persisted health.
	// Proxy-level failover picks a healthy proxy; entries that fail are marked
	// unhealthy and skipped for ProxyUnhealthyCooldown before being retried.
	ProxyPool []PooledProxy `json:"proxyPool,omitempty"`

	// PublicBaseURL is the externally reachable base URL that routes to the SSO
	// loopback port (e.g. "https://azr.hian.software" → reverse proxy → container:3128).
	// When set, OAuth redirect_uri values for the Kiro SSO flow are built from it
	// instead of being hardcoded to http://localhost:<loopbackPort>, so logins work
	// through a reverse proxy / custom domain without hand-editing the browser URL.
	// It must point at the loopback port, NOT the admin UI port. Empty = fall back to
	// http://localhost:<loopbackPort> (correct for the pure-local case).
	PublicBaseURL string `json:"publicBaseURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics (persisted across restarts). int64 so long-running 32-bit
	// builds (GOARCH=arm/386) don't wrap TotalTokens at ~2.1B and silently corrupt
	// the persisted counter (L8).
	TotalRequests   int64   `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int64   `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int64   `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int64   `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed

	// LimitNoticeMessage is a friendly in-chat reply shown to a client whose key is
	// blocked (disabled/expired/over-limit/IP-denied) instead of a 401/429 error.
	// Empty falls back to a built-in default at render time.
	LimitNoticeMessage string `json:"limitNoticeMessage,omitempty"`

	// ForceModel, when non-empty, overrides the model of EVERY incoming request
	// (after thinking-suffix parsing) with this Kiro model ID. It takes precedence
	// over a per-key model binding and the client-requested model. Empty = disabled.
	ForceModel string `json:"forceModel,omitempty"`

	// IdentityModel, when non-empty, is the model name the assistant is told to
	// self-identify as. It does NOT change which upstream model serves the request
	// (that is ForceModel's job); it only prepends an identity line to the system
	// prompt so "what model are you?" answers with this name regardless of the real
	// model. Empty = don't inject anything. See applyPromptFilters in proxy/translator.go.
	IdentityModel string `json:"identityModel,omitempty"`

	// --- Sales & operations fields ---

	// BannedIPs holds the persistent IP block list, managed by config/ipban.go.
	// The field is intentionally BanDisabled (not BanEnabled) so that a config
	// written before this feature existed starts with the gate ON.
	BannedIPs    []BannedIP `json:"bannedIps,omitempty"`
	BanThreshold int        `json:"banThreshold,omitempty"` // 0 = use DefaultBanThreshold
	BanDisabled  bool       `json:"banDisabled,omitempty"`  // true = gate off (inverted)

	// CreditTopUps is the idempotency ledger for sales-bot top-up operations.
	// It lives in config.json so idempotency survives restarts.
	CreditTopUps []CreditTopUp `json:"creditTopUps,omitempty"`

	// AuditLog records administrative actions. Capped to AuditLogKept entries.
	AuditLog []AuditEntry `json:"auditLog,omitempty"`

	// Alert configuration: thresholds and destinations for low-credits / expiry-soon
	// notifications. AlertWebhook is a URL that receives POST requests with a JSON
	// payload. Empty AlertWebhook disables HTTP notifications; the alert watcher still
	// emits WARN log lines.
	AlertWebhook          string  `json:"alertWebhook,omitempty"`
	AlertCreditsPercent   float64 `json:"alertCreditsPercent,omitempty"`   // % remaining; 0 → default 10
	AlertExpiryDays       int     `json:"alertExpiryDays,omitempty"`       // days ahead; 0 → default 3
	AutoDisableExpired    bool    `json:"autoDisableExpired,omitempty"`    // disable keys past ExpiresAt

	// Maintenance mode: when true, inference endpoints return a friendly notice
	// instead of forwarding to Kiro. Admin endpoints and /health are unaffected.
	MaintenanceMode    bool   `json:"maintenanceMode,omitempty"`
	MaintenanceMessage string `json:"maintenanceMessage,omitempty"`
}

// PooledProxy is one outbound proxy in the shared pool, carrying persisted
// health so proxy-level failover survives restarts. 池化代理，带持久化健康状态。
type PooledProxy struct {
	URL               string `json:"url"`
	Healthy           bool   `json:"healthy"`
	FailCount         int    `json:"failCount,omitempty"`
	LastFailAt        int64  `json:"lastFailAt,omitempty"`
	LastOKAt          int64  `json:"lastOkAt,omitempty"`
	DisabledPermanent bool   `json:"disabledPermanent,omitempty"`
}

// ProxyUnhealthyCooldown is how long an unhealthy pooled proxy is skipped by
// the selector (in proxy/) before it is retried. 不健康代理的冷却时间。
const ProxyUnhealthyCooldown = 5 * time.Minute

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.2.9"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
	// cfgDirty marks that in-memory cfg has unpersisted changes from a hot-path
	// counter update (RecordApiKeyUsage / UpdateAccountStats). A background flush
	// coalesces these into one disk write instead of writing the full config on
	// every request, which previously serialized all request completions on the
	// exclusive cfgLock + os.WriteFile.
	cfgDirty atomic.Bool
	// passwordOverride holds the ADMIN_PASSWORD env override. It is kept OUT of the
	// persisted Config so a later Save() (e.g. a stats flush) never writes the env
	// secret to config.json in cleartext (L9). When set, it takes priority over the
	// stored cfg.Password. Guarded by cfgLock.
	passwordOverride string
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgPath = path
	return Load()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = &Config{
				Password:      "changeme",
				Port:          8080,
				Host:          "0.0.0.0",
				RequireApiKey: false,
				Accounts:      []Account{},
			}
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfg = &c

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: seed lifetime counters from the current-period counters for keys that
	// predate the lifetime fields. Without this, an existing key with usage would show a
	// lifetime total of 0, lower than what it has already consumed. We detect an unseeded
	// key as one whose lifetime counters are all zero while a current-period counter is
	// non-zero. Runs once; after the save, lifetime >= current so the condition is false.
	lifetimeMigrated := false
	for i := range cfg.ApiKeys {
		k := &cfg.ApiKeys[i]
		if k.LifetimeTokens == 0 && k.LifetimeCredits == 0 && k.LifetimeRequests == 0 &&
			(k.TokensUsed != 0 || k.CreditsUsed != 0 || k.RequestsCount != 0) {
			k.LifetimeTokens = k.TokensUsed
			k.LifetimeCredits = k.CreditsUsed
			k.LifetimeRequests = k.RequestsCount
			lifetimeMigrated = true
		}
	}
	if lifetimeMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: fold the legacy single Model override into the Models allowlist. A
	// one-element allowlist reproduces the old "force this model" behavior exactly
	// (any client model not equal to it is remapped to it). Clear the legacy field so
	// future saves don't re-emit it.
	modelMigrated := false
	for i := range cfg.ApiKeys {
		k := &cfg.ApiKeys[i]
		if strings.TrimSpace(k.Model) != "" {
			if len(k.Models) == 0 {
				k.Models = []string{strings.TrimSpace(k.Model)}
			}
			k.Model = ""
			modelMigrated = true
		}
	}
	if modelMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// markDirtyLocked defers persistence to the background flusher instead of writing
// to disk inline. Hot-path counter updates (per-request usage/stats) use this so
// they don't hold cfgLock across an os.WriteFile of the whole config, which would
// serialize every request completion and block all readers. Caller MUST hold cfgLock.
func markDirtyLocked() {
	cfgDirty.Store(true)
}

// FlushDirty persists cfg to disk only if a markDirtyLocked update is pending.
// Called periodically by the proxy's background stats saver and once on shutdown.
func FlushDirty() error {
	if !cfgDirty.Swap(false) {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil
	}
	if err := Save(); err != nil {
		cfgDirty.Store(true) // retry on next tick
		return err
	}
	return nil
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability.
//
// The write is atomic: the payload is written to a sibling temp file (fsync'd)
// and then renamed over the live config. os.Rename is atomic within a filesystem
// (and on Windows uses MoveFileEx with MOVEFILE_REPLACE_EXISTING), so a crash /
// power loss / ENOSPC mid-write can never leave a truncated or empty config.json
// that would fail Load() on the next boot and lose all account tokens & API keys.
//
// Callers already hold cfgLock, so there is no concurrent Save() to race on the
// fixed temp name.
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := cfgPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	// Flush to stable storage before the rename so the renamed file is complete.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// SetPassword records the ADMIN_PASSWORD env override. It is stored separately from
// the persisted Config (see passwordOverride) so a subsequent Save() never writes the
// env secret to config.json in cleartext (L9). The override takes priority over the
// stored password for the lifetime of the process.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	passwordOverride = password
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if passwordOverride != "" {
		return passwordOverride
	}
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

// GetGlobalRegion returns the global default region. Defaults to "us-east-1".
func GetGlobalRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Region == "" {
		return "us-east-1"
	}
	return cfg.Region
}

// GetGlobalAuthRegion returns the global default auth region. Defaults to "us-east-1".
func GetGlobalAuthRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AuthRegion == "" {
		return "us-east-1"
	}
	return cfg.AuthRegion
}

// GetGlobalApiRegion returns the global default API region. Defaults to "us-east-1".
func GetGlobalApiRegion() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ApiRegion == "" {
		return "us-east-1"
	}
	return cfg.ApiRegion
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i] = account
			return Save()
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].Enabled = enabled
			if !enabled {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			return Save()
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return nil
}

// UpdateAccountProfileArnWithRegion persists a resolved profile ARN and, when
// apiRegion is non-empty, pins the account's data-plane region to it. Kiro /
// Q Developer profiles are regional (e.g. KiroProfile-eu-central-1), and the
// region that owns a profile can differ from the SSO/auth region. Recording the
// API region alongside the ARN ensures subsequent data-plane calls target the
// correct regional endpoint without re-probing. The auth region (account.Region)
// is intentionally left untouched so OIDC token refresh keeps working.
func UpdateAccountProfileArnWithRegion(id, profileArn, apiRegion string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			if apiRegion != "" {
				cfg.Accounts[i].ApiRegion = apiRegion
			}
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			return Save()
		}
	}
	return nil
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
		// An explicit UI password change supersedes the env override, otherwise
		// GetPassword would keep returning the stale ADMIN_PASSWORD value (L9).
		passwordOverride = ""
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
		// An explicit UI password change supersedes the env override, otherwise
		// GetPassword would keep returning the stale ADMIN_PASSWORD value (L9).
		passwordOverride = ""
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int64, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return saveLocked()
}

func GetStats() (int64, int64, int64, int64, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

// UpdateAccountStats updates an account's per-request counters. This runs on the
// request hot path, so it only marks the config dirty (like RecordApiKeyUsage);
// the backgroundStatsSaver coalesces the write via FlushDirty. It must NOT call
// saveLocked() inline — that would marshal and rewrite the whole config on every
// completed request. See CLAUDE.md ("never write config on the request hot path").
func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			markDirtyLocked()
			return nil
		}
	}
	return nil
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// defaultLimitNoticeMessage is served when no custom LimitNoticeMessage is configured.
const defaultLimitNoticeMessage = "Your API key has reached its limit or has expired. Please contact the administrator to renew it."

// GetLimitNoticeMessage returns the configured limit-notice message, or the built-in
// default when none is set.
func GetLimitNoticeMessage() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if strings.TrimSpace(cfg.LimitNoticeMessage) == "" {
		return defaultLimitNoticeMessage
	}
	return cfg.LimitNoticeMessage
}

// SetLimitNoticeMessage persists the limit-notice message.
func SetLimitNoticeMessage(msg string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LimitNoticeMessage = msg
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetStreamContinuation returns whether a turn cut off mid-generation is
// transparently resumed. Defaults to true: without it, every upstream mid-stream
// drop surfaces to the user as an answer that stops in the middle of a sentence.
// The cfg == nil guard is not defensive padding: these getters are reached from
// the translator, which runs in unit tests that never call Init(). Without it the
// first tool-results turn in such a test panics on a nil dereference.
func GetStreamContinuation() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.StreamContinuation == nil {
		return true
	}
	return *cfg.StreamContinuation
}

// UpdateStreamContinuation sets the stream continuation switch and persists the change.
func UpdateStreamContinuation(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.StreamContinuation = &enabled
	return Save()
}

// GetKeepToolHistory returns whether structured tool history is preserved for
// correctly-paired turns. Defaults to true.
func GetKeepToolHistory() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.KeepToolHistory == nil {
		return true
	}
	return *cfg.KeepToolHistory
}

// UpdateKeepToolHistory sets the keep-tool-history switch and persists it.
func UpdateKeepToolHistory(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.KeepToolHistory = &enabled
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetRequireProxy 返回是否强制所有出站请求走代理
func GetRequireProxy() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.RequireProxy
}

// UpdateRequireProxy 设置 require-proxy 开关并持久化
func UpdateRequireProxy(v bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.RequireProxy = v
	return Save()
}

// GetProxyPool 返回共享代理池的副本，调用方不得修改内部状态。
func GetProxyPool() []PooledProxy {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	pool := make([]PooledProxy, len(cfg.ProxyPool))
	copy(pool, cfg.ProxyPool)
	return pool
}

// AddProxyToPool 按 URL 去重加入代理池；已存在则视为成功的空操作。
// 新条目 Healthy=true 且 LastOKAt=now。
func AddProxyToPool(url string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for _, p := range cfg.ProxyPool {
		if p.URL == url {
			return nil
		}
	}
	cfg.ProxyPool = append(cfg.ProxyPool, PooledProxy{
		URL:      url,
		Healthy:  true,
		LastOKAt: time.Now().Unix(),
	})
	return Save()
}

// RemoveProxyFromPool 从代理池移除指定 URL；不存在则空操作。
func RemoveProxyFromPool(url string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, p := range cfg.ProxyPool {
		if p.URL == url {
			cfg.ProxyPool = append(cfg.ProxyPool[:i], cfg.ProxyPool[i+1:]...)
			return Save()
		}
	}
	return nil
}

// MarkProxyUnhealthy 记录一次失败：FailCount++、LastFailAt=now、Healthy=false。
// FailCount 每次都在内存中自增，但仅在 healthy->unhealthy 的状态跳变时返回
// changed=true 并持久化，避免失败风暴引发写入风暴。
func MarkProxyUnhealthy(url string) (changed bool, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.ProxyPool {
		if cfg.ProxyPool[i].URL != url {
			continue
		}
		wasHealthy := cfg.ProxyPool[i].Healthy
		cfg.ProxyPool[i].FailCount++
		cfg.ProxyPool[i].LastFailAt = time.Now().Unix()
		cfg.ProxyPool[i].Healthy = false
		if wasHealthy {
			return true, Save()
		}
		return false, nil
	}
	return false, nil
}

// MarkProxyHealthy 记录恢复：重置 FailCount、Healthy=true、LastOKAt=now。
// 仅在 unhealthy->healthy 跳变时返回 changed=true 并持久化。
func MarkProxyHealthy(url string) (changed bool, err error) {
	// Fast path: a proxy that is already healthy with no accumulated failures
	// needs no write. Every successful pooled request calls this, so an RLock
	// here keeps the high-QPS success path from serializing on the write lock.
	cfgLock.RLock()
	for i := range cfg.ProxyPool {
		if cfg.ProxyPool[i].URL == url {
			if cfg.ProxyPool[i].Healthy && cfg.ProxyPool[i].FailCount == 0 {
				cfgLock.RUnlock()
				return false, nil
			}
			break
		}
	}
	cfgLock.RUnlock()

	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.ProxyPool {
		if cfg.ProxyPool[i].URL != url {
			continue
		}
		wasHealthy := cfg.ProxyPool[i].Healthy
		cfg.ProxyPool[i].FailCount = 0
		cfg.ProxyPool[i].Healthy = true
		cfg.ProxyPool[i].LastOKAt = time.Now().Unix()
		if !wasHealthy {
			return true, Save()
		}
		return false, nil
	}
	return false, nil
}

// SetProxyPoolDisabled 永久禁用/启用某个池化代理并持久化。
func SetProxyPoolDisabled(url string, disabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.ProxyPool {
		if cfg.ProxyPool[i].URL == url {
			cfg.ProxyPool[i].DisabledPermanent = disabled
			return Save()
		}
	}
	return nil
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// DefaultMaxPayloadBytes is the serialized-request byte cap used when the setting
// is unset (0). 2,000,000 sits safely below the ~2.15MB AWS upstream ceiling while
// leaving room for headers and serialization overhead.
const DefaultMaxPayloadBytes = 2_000_000

// GetMaxPayloadBytes returns the configured request byte cap, falling back to
// DefaultMaxPayloadBytes when unset or non-positive.
func GetMaxPayloadBytes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.MaxPayloadBytes <= 0 {
		return DefaultMaxPayloadBytes
	}
	return cfg.MaxPayloadBytes
}

// UpdateMaxPayloadBytes sets the request byte cap and persists the change.
func UpdateMaxPayloadBytes(n int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.MaxPayloadBytes = n
	return Save()
}

// GetPublicBaseURL returns the configured externally reachable base URL
// (no trailing slash), or "" when unset. Used to build OAuth redirect_uri
// values that work behind a reverse proxy / custom domain.
func GetPublicBaseURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return strings.TrimRight(cfg.PublicBaseURL, "/")
}

// UpdatePublicBaseURL sets the public base URL and persists the change.
func UpdatePublicBaseURL(u string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	return Save()
}

// GetForceModel returns the global force-model override (empty = disabled). When set,
// every request's model is rewritten to this Kiro model ID regardless of what the
// client sent or which model a key is bound to.
func GetForceModel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ForceModel)
}

// SetForceModel sets (or clears, when passed "") the global force-model override.
func SetForceModel(m string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ForceModel = strings.TrimSpace(m)
	return Save()
}

// GetIdentityModel returns the model name the assistant should self-identify as
// (empty = disabled). This only affects the injected identity line in the system
// prompt, not which upstream model actually serves the request.
func GetIdentityModel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.IdentityModel)
}

// SetIdentityModel sets (or clears, when passed "") the self-identify model name.
func SetIdentityModel(m string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.IdentityModel = strings.TrimSpace(m)
	return Save()
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
