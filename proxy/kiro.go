// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// maxEventStreamFrameBytes caps the size of a single AWS Event Stream frame we are
// willing to allocate. totalLength is read straight off the wire (4 bytes), so without
// an upper bound a corrupt/hostile frame could make us allocate multiple GiB and OOM the
// whole process from a single response. 32 MiB is far above any legitimate SSE frame.
const maxEventStreamFrameBytes = 32 << 20

const (
	// esPreludeLen is the fixed AWS Event Stream prelude: total_len(4) +
	// headers_len(4) + prelude_crc(4).
	esPreludeLen = 12
	// esMsgCRCLen is the 4-byte message CRC that trails every frame.
	esMsgCRCLen = 4
	// esMinMsgBytes is the smallest byte count a frame can legitimately claim:
	// prelude + trailing CRC, with zero headers and an empty payload.
	esMinMsgBytes = esPreludeLen + esMsgCRCLen
	// maxEventStreamHeaderBytes bounds the header block. Like totalLength, this
	// value is read straight off the wire and must be range-checked before it is
	// used to slice.
	maxEventStreamHeaderBytes = 128 << 10
)

// errCorruptKiroStream means the byte offset is no longer aligned to a frame
// boundary — a prelude was read that cannot describe a real frame. It is a
// distinct sentinel because the correct response differs from a truncated
// stream: nothing after this point can be trusted, so the connection is
// abandoned rather than resynchronized.
var errCorruptKiroStream = errors.New("corrupt upstream event stream")

// errEmptyMeteredKiroTurn means the upstream billed a turn (meteringEvent
// arrived) but produced no text, reasoning or tool call. It must stay distinct
// from an unmetered empty stream: that one is safely retryable, this one is not
// — the turn is already paid for, so re-running it would be charged twice.
var errEmptyMeteredKiroTurn = errors.New("upstream billed a turn that produced no content")

// kiroStreamTimeout bounds the ENTIRE streaming request, including reading the
// response body (http.Client.Timeout is documented to cover the body read). It was
// 5 minutes, which silently truncated long generations: a high thinking budget on
// opus-class models routinely keeps a single turn streaming past that, and the abort
// surfaced to the user as an abrupt mid-response stop. 30 minutes leaves ample
// headroom while still capping a permanently stalled connection.
//
// The pre-body phase stays bounded much more tightly and independently (DialContext
// 10s, ResponseHeaderTimeout in buildKiroTransport), so a dead proxy or unresponsive
// endpoint still fails fast and rotates to the next account rather than waiting out
// this ceiling. Override with KIRO_STREAM_TIMEOUT_SEC (0 removes the deadline).
var kiroStreamTimeout = time.Duration(envInt("KIRO_STREAM_TIMEOUT_SEC", 1800)) * time.Second

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		// Kiro's blessed edge host. Used as a 403 fallback: when the primary
		// q.amazonaws.com endpoint rejects the token, the runtime edge often
		// still accepts it, buying the request time before account rotation.
		URL:       "https://runtime.us-east-1.kiro.dev/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "Kiro Runtime",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   kiroStreamTimeout,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// ResolveAccountProxyURLStrict is like ResolveAccountProxyURL but enforces the
// global RequireProxy flag: when no proxy is configured for the account and
// require-proxy is on, it returns an error instead of "" so the caller fails
// the account (and rotates) rather than connecting directly and leaking the
// real IP. The error message contains "require-proxy" for failover matching.
func ResolveAccountProxyURLStrict(account *config.Account) (string, error) {
	url := ResolveAccountProxyURL(account)
	if url == "" && config.GetRequireProxy() {
		return "", fmt.Errorf("require-proxy: no proxy configured for account")
	}
	return url, nil
}

// proxyRRCounter drives round-robin selection over eligible pooled proxies.
var proxyRRCounter atomic.Uint64

// proxyPoolEligible reports whether a pooled proxy can be picked now: Healthy ||
// cooldown elapsed since LastFailAt; and never when DisabledPermanent. now is
// unix seconds.
func proxyPoolEligible(p config.PooledProxy, now int64) bool {
	if p.DisabledPermanent {
		return false
	}
	if p.Healthy {
		return true
	}
	return now-p.LastFailAt >= int64(config.ProxyUnhealthyCooldown.Seconds())
}

// SelectProxyForAccount returns the proxy URL to use and a poolKey identifying
// the chosen pool entry (empty when not from the pool), so the caller can report
// health back. Order: account override → pool (round-robin over eligible) →
// global proxy → require-proxy error / direct. It reads live pool state via
// config.GetProxyPool() on every call.
func SelectProxyForAccount(account *config.Account) (proxyURL string, poolKey string, err error) {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL, "", nil
	}

	now := time.Now().Unix()
	var eligible []config.PooledProxy
	for _, p := range config.GetProxyPool() {
		if proxyPoolEligible(p, now) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) > 0 {
		idx := proxyRRCounter.Add(1)
		pick := eligible[(idx-1)%uint64(len(eligible))]
		return pick.URL, pick.URL, nil
	}

	if global := config.GetProxyURL(); global != "" {
		return global, "", nil
	}
	if config.GetRequireProxy() {
		return "", "", fmt.Errorf("require-proxy: no proxy configured for account")
	}
	return "", "", nil
}

// maxProxySwapAttempts caps how many times a single streaming request rotates to
// another pool proxy after a proxy/dial transport failure before giving up and
// letting account-level failover take over. maxRestProxySwapAttempts is the
// smaller cap for the REST/background path.
const (
	maxProxySwapAttempts     = 3
	maxRestProxySwapAttempts = 2
)

const (
	// maxEmptyStreamRetries bounds how many times a single request re-attempts an
	// endpoint that returned a completely empty stream. Three covers the blips
	// seen in practice (they clear within seconds) without letting a sustained
	// outage spin a request forever.
	maxEmptyStreamRetries = 3
	// streamRetryBackoff is multiplied by the attempt number for a linear backoff.
	streamRetryBackoff = 300 * time.Millisecond
)

// shouldSwapProxy decides whether a streaming request should rotate to another
// pool proxy after a transport failure. It is true only for a genuine
// proxy/dial transport error (isProxyErrorMessage), when the failing proxy came
// from the pool (poolKey != "" — account overrides and the global proxy are not
// pool-managed), and while under the swap cap. A nil error (no transport
// failure) or an HTTP-status error (e.g. "HTTP 401 ...") returns false so a
// working proxy is never marked unhealthy for an upstream status.
func shouldSwapProxy(transportErr error, poolKey string, attempts int) bool {
	if transportErr == nil {
		return false
	}
	return isProxyErrorMessage(transportErr.Error()) && poolKey != "" && attempts < maxProxySwapAttempts
}

// doRESTWithProxySwap runs a REST request through a pool-aware proxy with
// bounded proxy-swap failover. It selects a proxy via SelectProxyForAccount
// (honoring the require-proxy gate — a require-proxy error is returned as-is so
// the caller aborts rather than leaking the real IP), issues the request, and
// on a proxy/dial transport failure marks that pool proxy unhealthy and
// re-selects another, up to maxRestProxySwapAttempts. When the request reaches
// upstream through a pool proxy it marks that proxy healthy. HTTP status errors
// (4xx/5xx) come back as a normal *http.Response and never mark a proxy
// unhealthy — only transport failures do. buildReq must construct a FRESH
// *http.Request each call so the body can be re-read across swaps.
func doRESTWithProxySwap(account *config.Account, buildReq func() (*http.Request, error)) (*http.Response, error) {
	attempts := 0
	for {
		proxyURL, poolKey, err := SelectProxyForAccount(account)
		if err != nil {
			return nil, err
		}
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		resp, err := GetRestClientForProxy(proxyURL).Do(req)
		if err != nil {
			if isProxyErrorMessage(err.Error()) && poolKey != "" && attempts < maxRestProxySwapAttempts {
				config.MarkProxyUnhealthy(poolKey)
				attempts++
				logger.Warnf("[Route] REST proxy swap for %s after transport error: %v", accountEmailForLog(account), err)
				continue
			}
			return nil, err
		}
		if poolKey != "" {
			config.MarkProxyHealthy(poolKey)
		}
		return resp, nil
	}
}

// maskProxyForLog returns a log-safe proxy string: scheme://[user:***@]host:port,
// or "direct" when no proxy is configured. Password is never logged.
func maskProxyForLog(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return "direct"
	}
	auth := ""
	if u.User != nil {
		name := u.User.Username()
		if _, hasPw := u.User.Password(); hasPw {
			auth = name + ":***@"
		} else if name != "" {
			auth = name + "@"
		}
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, auth, u.Host)
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		// Cap the connect/proxy-handshake phase so a dead or hung proxy fails
		// fast and the request rotates to another account, instead of hanging
		// for the full stream timeout.
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
		// Bound the wait for response HEADERS only. This is what makes it safe to
		// give the streaming client a long overall Timeout: an endpoint that accepts
		// the connection but never responds fails here in 2 minutes instead of
		// occupying the request for the full kiroStreamTimeout. Once headers arrive
		// this no longer applies, so the body may stream for as long as needed.
		ResponseHeaderTimeout: 120 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   kiroStreamTimeout,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	// RequestContext, when non-nil, is propagated into the upstream HTTP
	// request so that a client disconnect cancels the Kiro API call
	// immediately rather than letting it run to completion and waste credits.
	RequestContext context.Context `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)

	// OnTruncated fires when the stream closed cleanly AFTER emitting content but
	// without a meteringEvent — i.e. the upstream dropped the connection mid-turn.
	// Retrying is not an option at that point (it would append a second, partial
	// answer), so the handler's only correct move is to withhold the "finished"
	// signal from the client: no stop_reason / finish_reason / completed event.
	// Handlers that report a clean stop here cause the silent-truncation bug,
	// where a cut-off answer is indistinguishable from a complete one.
	OnTruncated func()

	// OnStopReason fires when the upstream states, in band, why the turn ended
	// (metadataEvent.stopReason: END_TURN / MAX_TOKENS / TOOL_USE). This is
	// authoritative and must win over any local guess: Kiro enforces its OWN
	// output ceiling, which is unrelated to the client's max_tokens, so a turn
	// cut short by the server looks well under the client's limit and would
	// otherwise be reported as a clean end_turn. The client then believes the
	// answer finished and stops — the silent mid-task stop.
	OnStopReason func(reason string)

	// OnCacheTokens fires once at end of stream when the upstream reported its own
	// prompt-cache split. These numbers are MEASURED, unlike the Claude paths'
	// locally-derived estimate from cache_control breakpoints — and they are the
	// only cache information available to the OpenAI and Responses protocols,
	// which have no cache_control concept at all. read+write is a subset of the
	// input token count, never an addition to it.
	OnCacheTokens func(read, write int)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto": Kiro first, then fallback to others
		return []kiroEndpoint{kiroEndpoints[0], kiroEndpoints[1], kiroEndpoints[2]}
	}

	if !fallback {
		// No fallback: only use the selected endpoint
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// secretPreviewRe masks obvious credential tokens in the content preview so
// debug logs never leak API keys / bearer tokens that appear inside prompts.
var secretPreviewRe = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{6,}|bearer\s+[a-z0-9._-]{8,}|(?:api[_-]?key|token|secret|password)["']?\s*[:=]\s*["']?[a-z0-9._-]{6,})`)

func maskSecrets(s string) string {
	return secretPreviewRe.ReplaceAllString(s, "[REDACTED]")
}

// summarizeKiroPayload returns a compact, single-line description of a request
// payload for debug logging: the request shape (model, history depth, tool
// counts, content size) plus a short, secret-masked preview of the current
// message content. It deliberately avoids dumping the full payload, which can
// be hundreds of KB and contain user secrets.
func summarizeKiroPayload(payload *KiroPayload) string {
	if payload == nil {
		return "<nil>"
	}
	cs := &payload.ConversationState
	uim := &cs.CurrentMessage.UserInputMessage

	tools, toolResults := 0, 0
	if uim.UserInputMessageContext != nil {
		tools = len(uim.UserInputMessageContext.Tools)
		toolResults = len(uim.UserInputMessageContext.ToolResults)
	}

	const previewLen = 200
	preview := uim.Content
	truncated := false
	if len([]rune(preview)) > previewLen {
		preview = string([]rune(preview)[:previewLen])
		truncated = true
	}
	// Collapse whitespace/newlines so the preview stays on one log line.
	preview = strings.Join(strings.Fields(preview), " ")
	preview = maskSecrets(preview)
	if truncated {
		preview += "…"
	}

	convID := cs.ConversationID
	if len(convID) > 8 {
		convID = convID[:8]
	}

	return fmt.Sprintf("conv=%s model=%s task=%s trigger=%s history=%d tools=%d toolResults=%d images=%d contentChars=%d content=%q",
		convID, uim.ModelID, cs.AgentTaskType, cs.ChatTriggerType,
		len(cs.History), tools, toolResults, len(uim.Images), len(uim.Content), preview)
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: log a compact summary (shape + masked content preview) instead of
	// the full payload, which can be hundreds of KB and contain secrets.
	if enabled := logger.GetLevel(); enabled <= logger.LevelDebug {
		logger.Debugf("[KiroAPI] Request: %s", summarizeKiroPayload(payload))
	}

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	// Resolve the outbound proxy FIRST. When require-proxy is on and the account
	// has no proxy, this returns a blocking error so we bail before any network
	// call below (e.g. ResolveProfileArn), preventing a direct-connection IP leak.
	// poolKey (non-empty only when the proxy came from the pool) lets us report
	// health back and rotate to another pool proxy on a transport failure.
	proxyURL, poolKey, proxyErr := SelectProxyForAccount(account)
	if proxyErr != nil {
		return proxyErr
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	// Build endpoint list ordered by configuration.
	endpoints := getSortedEndpoints(config.GetPreferredEndpoint())

	// OUTER proxy-swap loop: the inner loop tries each endpoint over the current
	// proxy. Only a proxy/dial TRANSPORT failure (not an HTTP status) rotates us
	// to another pool proxy — HTTP 4xx/5xx are upstream/account state and must
	// never mark a proxy unhealthy.
	proxyAttempts := 0
	var lastErr error
	for {
		logger.Infof("[Route] ac=%s model=%s proxy=%s", accountEmailForLog(account), currentMessageModelID(payload), maskProxyForLog(proxyURL))
		proxyClient := GetClientForProxy(proxyURL)

		// lastTransportErr captures ONLY proxyClient.Do transport failures for the
		// current proxy — it drives the swap decision. HTTP-status errors set
		// lastErr but never lastTransportErr. reachedUpstream records whether any
		// endpoint got an HTTP response through this proxy: if one did, the proxy
		// demonstrably works, so a transport error on a different endpoint must not
		// mark it unhealthy.
		var lastTransportErr error
		reachedUpstream := false
		// Index loop rather than range: an empty stream retries the SAME endpoint
		// via i--, which a range loop cannot express. That matters because an
		// API-key account resolves to exactly ONE endpoint — advancing to "the
		// next" one would end the loop immediately and fail hard while the log
		// claimed a retry was happening.
		emptyStreamRetries := 0
		for i := 0; i < len(endpoints); i++ {
			ep := endpoints[i]
			// Update the origin field for the selected endpoint.
			payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

			// Target the account's region; endpoint URLs are declared for us-east-1.
			epURL := regionalizeURL(ep.URL, account)
			reqBody, _ := json.Marshal(payload)

			// Propagate the client's request context so a client disconnect
			// cancels the upstream Kiro call immediately and frees the account.
			reqCtx := context.Background()
			if payload != nil && payload.RequestContext != nil {
				reqCtx = payload.RequestContext
			}
			req, err := http.NewRequestWithContext(reqCtx, "POST", epURL, bytes.NewReader(reqBody))
			if err != nil {
				lastErr = err
				continue
			}

			host := ""
			if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
				host = parsedURL.Host
			}
			headerValues := buildStreamingHeaderValues(account, host)

			req.Header.Set("Content-Type", "application/json")
			// The upstream replies with an AWS binary event stream; advertise it
			// explicitly (matching the real Kiro IDE) rather than "*/*". A generic
			// Accept invites an intermediary to negotiate or buffer a different
			// representation, which shows up as a stream that stalls or ends early.
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
			if ep.AmzTarget != "" {
				req.Header.Set("X-Amz-Target", ep.AmzTarget)
			}
			applyKiroBaseHeaders(req, account, headerValues)
			if account.AuthMethod == "external_idp" {
				req.Header.Set("TokenType", "EXTERNAL_IDP")
			}
			req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
			req.Header.Set("x-amzn-codewhisperer-optout", "true")
			req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
			req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

			resp, err := proxyClient.Do(req)
			if err != nil {
				lastErr = err
				lastTransportErr = err
				logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
				continue
			}
			// Got an HTTP response through this proxy — it reached upstream.
			reachedUpstream = true

			if resp.StatusCode == 429 {
				resp.Body.Close()
				logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
				lastErr = fmt.Errorf("quota exhausted on %s", ep.Name)
				continue
			}

			if resp.StatusCode != 200 {
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				lastErr = fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ep.Name, string(errBody))
				// Authentication and payment errors are not retried across
				// endpoints — they signal an account-level problem, not an
				// endpoint-level one. 403 is intentionally NOT in this list:
				// the primary q.amazonaws.com sometimes returns 403 for a
				// token that runtime.kiro.dev still accepts, so 403 is allowed
				// to fall through and try the next endpoint.
				if resp.StatusCode == 401 || resp.StatusCode == 402 {
					return lastErr
				}
				logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
				continue
			}

			// Reached upstream and got a streamable 200 through this proxy — it
			// works, so mark the pool entry healthy once.
			if poolKey != "" {
				config.MarkProxyHealthy(poolKey)
			}
			outcome, parseErr := parseEventStreamTracked(resp.Body, callback)
			resp.Body.Close()

			if parseErr != nil {
				// Nothing reached the client and the failure was a cut-off tool
				// call, so a retry cannot duplicate output. Anything else —
				// exceptions, throttling, content-filter refusals — is returned
				// immediately: the same payload gets the same verdict on every
				// account, so retrying or rotating only delays the explanation
				// the client needs.
				if !outcome.Emitted && errors.Is(parseErr, errIncompleteKiroToolInput) && emptyStreamRetries < maxEmptyStreamRetries {
					emptyStreamRetries++
					lastErr = parseErr
					logger.Warnf("[KiroAPI] Endpoint %s returned incomplete tool input, retrying (%d/%d)", ep.Name, emptyStreamRetries, maxEmptyStreamRetries)
					time.Sleep(streamRetryBackoff * time.Duration(emptyStreamRetries))
					i--
					continue
				}
				return parseErr
			}

			// Stream closed with no output AND no billing: the turn never
			// started. The client has seen nothing, so retrying the same
			// endpoint is safe and usually succeeds — these blips clear within
			// seconds. Bounded so a real outage cannot spin here forever.
			if !outcome.Metered && !outcome.Emitted {
				lastErr = fmt.Errorf("empty stream from %s (no output, no metering)", ep.Name)
				if emptyStreamRetries < maxEmptyStreamRetries {
					emptyStreamRetries++
					logger.Warnf("[KiroAPI] Endpoint %s returned an empty stream, retrying (%d/%d)", ep.Name, emptyStreamRetries, maxEmptyStreamRetries)
					time.Sleep(streamRetryBackoff * time.Duration(emptyStreamRetries))
					i--
					continue
				}
				logger.Warnf("[KiroAPI] Endpoint %s still empty after %d retries, trying next endpoint", ep.Name, maxEmptyStreamRetries)
				time.Sleep(streamRetryBackoff)
				continue
			}

			// Billed, but nothing the client can see was produced. Returning nil
			// here let the handler finish the turn normally, which emits
			// stop_reason "end_turn" over an empty content array — Claude Code
			// reads that as "API returned an empty or malformed response
			// (HTTP 200)" and aborts the whole task. Report it as an error so the
			// handler either rotates to another account (nothing was written yet)
			// or surfaces an in-band SSE error, never a clean empty finish.
			//
			// The turn is NOT retried against the same endpoint: it was already
			// billed, so re-running it here would pay twice for one turn.
			if !outcome.Emitted {
				logger.Warnf("[KiroAPI] Endpoint %s billed a turn that produced no content", ep.Name)
				return fmt.Errorf("%w from %s", errEmptyMeteredKiroTurn, ep.Name)
			}

			// Content was emitted but the turn was never billed: the upstream
			// dropped the connection mid-generation. Retrying would append a
			// second, partial answer on top of what the client already has, so
			// the only correct move is to tell the handler to withhold the
			// "finished" signal.
			if !outcome.Metered {
				logger.Warnf("[KiroAPI] Endpoint %s stream ended without metering after partial output", ep.Name)
				if callback != nil && callback.OnTruncated != nil {
					callback.OnTruncated()
				}
			}
			return nil
		}

		// Inner endpoint loop exhausted. If the failure was a proxy transport
		// error, no endpoint reached upstream through this proxy, and we can
		// still swap, mark the current proxy unhealthy and rotate to another
		// pool proxy. reachedUpstream guards against penalizing a working proxy
		// when one endpoint transport-failed but another got an HTTP response.
		if !reachedUpstream && shouldSwapProxy(lastTransportErr, poolKey, proxyAttempts) {
			config.MarkProxyUnhealthy(poolKey)
			proxyAttempts++
			newURL, newKey, selErr := SelectProxyForAccount(account)
			if selErr != nil {
				return selErr
			}
			proxyURL, poolKey = newURL, newKey
			continue
		}
		break
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

// ==================== Event Stream Parsing ====================

// StreamOutcome reports what a single upstream event stream actually delivered.
// It exists to tell apart three endings that all look identical to a caller
// that only sees `err == nil`:
//
//	Emitted=false Metered=false — nothing arrived at all. Safe to retry: the
//	                              client has seen no bytes, so a second attempt
//	                              cannot duplicate output.
//	Emitted=true  Metered=false — the connection died mid-turn. NOT safe to
//	                              retry (it would append a second, partial
//	                              answer); the turn must be reported as truncated.
//	Metered=true                — the upstream billed the turn, which it only
//	                              does at the end. The turn completed.
type StreamOutcome struct {
	// Emitted is set once anything the client can see has been forwarded:
	// assistant text, reasoning text, or a tool use.
	Emitted bool
	// Metered is set when the upstream sent meteringEvent. Because billing
	// happens at end-of-turn, !Metered means the connection ended before the
	// turn finished — this is the only in-band signal that a stream which
	// closed cleanly was actually cut short.
	Metered bool
	// StopReason carries metadataEvent.stopReason verbatim (END_TURN,
	// MAX_TOKENS, TOOL_USE, ...) when the upstream sent one, else "". Unlike
	// Metered this says WHY the turn ended, which a locally-inferred
	// stop_reason cannot know: Kiro applies its own output limit independent of
	// the client's max_tokens.
	StopReason string
}

// parseEventStream decodes an AWS binary Event Stream response body, discarding
// the outcome. Retained for callers that only care about the error.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	_, err := parseEventStreamTracked(body, callback)
	return err
}

// parseEventStreamTracked decodes an AWS binary Event Stream response body and
// reports what it delivered. See StreamOutcome for why the caller needs this.
func parseEventStreamTracked(body io.Reader, callback *KiroStreamCallback) (StreamOutcome, error) {
	var outcome StreamOutcome
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var cacheTokens upstreamCacheTokens
	var totalCredits float64
	var currentToolUse *toolUseState
	var lastAssistantContent string
	var lastReasoningContent string

	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		// io.EOF means the connection closed cleanly on a frame boundary.
		// io.ErrUnexpectedEOF means it closed mid-prelude: io.ReadFull only reports
		// io.EOF when ZERO bytes were read, so a close after even 1 of the 12 bytes
		// surfaces as ErrUnexpectedEOF instead. AWS ends the event stream this way
		// often enough on long-lived connections (a high thinking budget keeps the
		// socket open for minutes) that treating it as fatal throws away an otherwise
		// complete response and needlessly rotates the account — the abrupt-cutoff
		// symptom. End the stream the same way a clean EOF would.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return outcome, err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		// A prelude that cannot describe a real frame means the byte offset is no
		// longer on a frame boundary. Skipping ("continue") is not recoverable
		// here: the next read starts 12 bytes into arbitrary payload data, so the
		// parser walks garbage and can spin until the connection dies. Stop and
		// report corruption instead, which lets the caller rotate the account.
		if totalLength < esMinMsgBytes {
			return outcome, fmt.Errorf("%w: frame length %d below the %d-byte minimum", errCorruptKiroStream, totalLength, esMinMsgBytes)
		}
		if totalLength > maxEventStreamFrameBytes {
			return outcome, fmt.Errorf("event stream frame too large: %d bytes (max %d)", totalLength, maxEventStreamFrameBytes)
		}
		// headersLength is attacker/corruption controlled, so it is bounded both
		// against its own ceiling and against the frame it must fit inside.
		if headersLength > maxEventStreamHeaderBytes || headersLength+esMinMsgBytes > totalLength {
			return outcome, fmt.Errorf("%w: headers length %d does not fit a %d-byte frame", errCorruptKiroStream, headersLength, totalLength)
		}

		// Read the remaining message bytes.
		remaining := totalLength - 12
		msgBuf := make([]byte, remaining)
		_, err = io.ReadFull(body, msgBuf)
		// Same rationale as the prelude read above: a frame that started but was cut
		// short by the upstream closing the connection is the end of the stream, not a
		// fatal error. The partial frame is dropped; everything already handed to the
		// callback stands.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return outcome, err
		}

		// No bounds re-check is needed here: the prelude guard above already
		// established headersLength+16 <= totalLength, and len(msgBuf) is
		// totalLength-12, so headersLength <= len(msgBuf)-4 holds by construction.
		headerBytes := msgBuf[0:headersLength]
		eventType := extractStringHeader(headerBytes, ":event-type")
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]

		// AWS Event Stream signals mid-stream failures with :message-type=exception
		// (e.g. ThrottlingException on a 429 after headers are sent). These frames
		// carry no :event-type, so without this check they'd be silently dropped and
		// the stream would end as a false success — leaving the throttled account hot.
		if msgType := extractStringHeader(headerBytes, ":message-type"); msgType == "exception" || msgType == "error" {
			excType := extractStringHeader(headerBytes, ":exception-type")
			if excType == "" {
				excType = extractStringHeader(headerBytes, ":error-code")
			}
			return outcome, fmt.Errorf("upstream stream exception %s: %s", excType, strings.TrimSpace(string(payloadBytes)))
		}

		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens, &cacheTokens)

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, false)
					// The client has now seen bytes: retrying would duplicate them.
					outcome.Emitted = true
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, true)
					outcome.Emitted = true
				}
			}
		case "toolUseEvent":
			next, emitted, err := handleToolUseEvent(event, currentToolUse, callback)
			if emitted {
				outcome.Emitted = true
			}
			if err != nil {
				// Incomplete tool-call arguments. Surfacing this as an error is
				// deliberate: forwarding the call would make the client execute a
				// tool with parameters the model never finished writing.
				return outcome, err
			}
			currentToolUse = next
		case "meteringEvent":
			// Billing only happens at end-of-turn, so this is the in-band marker
			// that the turn actually completed.
			outcome.Metered = true
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		case "metadataEvent":
			// The upstream's own verdict on why generation stopped. Previously
			// dropped on the floor, which is what let a server-side cut be
			// reported to the client as a clean end_turn. See OnStopReason.
			if reason, ok := event["stopReason"].(string); ok && strings.TrimSpace(reason) != "" {
				outcome.StopReason = strings.TrimSpace(reason)
				if callback.OnStopReason != nil {
					callback.OnStopReason(outcome.StopReason)
				}
			}
		}
	}

	// Flush a tool use still in flight when the stream ended. Without this the
	// last tool call of a turn silently disappears whenever the upstream closes
	// on a frame boundary before sending its stop marker.
	if currentToolUse != nil {
		emitted, err := finishToolUse(currentToolUse, callback)
		if emitted {
			outcome.Emitted = true
		}
		if err != nil {
			return outcome, err
		}
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	// Fired before OnComplete so a handler can fold the split into whatever it
	// builds from the token counts.
	if callback.OnCacheTokens != nil && cacheTokens.Reported {
		callback.OnCacheTokens(cacheTokens.Read, cacheTokens.Write)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return outcome, nil
}

// upstreamCacheTokens is the prompt-cache split the upstream reports itself.
//
// This is measured, not estimated. The Claude paths derive cache numbers locally
// from cache_control breakpoints because Anthropic clients declare them, but the
// OpenAI and Responses protocols have no such concept — for those, this is the
// only cache information that exists, and it is better than any guess.
type upstreamCacheTokens struct {
	Read  int
	Write int
	// Reported distinguishes "the upstream said zero" from "the upstream said
	// nothing", so a path with no cache activity is not confused with one whose
	// numbers never arrived.
	Reported bool
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int, cache *upstreamCacheTokens) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		uncached, hasUncached := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, hasRead := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, hasWrite := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")

		// Captured BEFORE the inputTokens branch below. That branch returns early
		// when the upstream also sent a plain total, which it usually does — so
		// reading the split only in the fallback path threw the measured cache
		// numbers away on exactly the requests that had them.
		if cache != nil && (hasRead || hasWrite) {
			cache.Read = cacheRead
			cache.Write = cacheWrite
			cache.Reported = true
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		if hasUncached || hasRead || hasWrite {
			if uncached+cacheRead+cacheWrite > 0 {
				inputTokens = uncached + cacheRead + cacheWrite
				continue
			}
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// modelInputWindows caches the authoritative input-token window per model, as
// reported by Kiro's ListAvailableModels (ModelInfo.TokenLimits.MaxInputTokens).
// getContextWindowSize prefers this over the version-string heuristic so the
// contextUsagePercentage → token conversion uses the exact window Kiro measured
// the percentage against. Empty until the first models-cache refresh; keyed by
// the normalized (dash→dot, lowercased) model ID so dash- and dot-form ids
// collide. Guarded independently because getContextWindowSize runs on the
// streaming hot path, off the models-cache lock.
var (
	modelInputWindowsMu sync.RWMutex
	modelInputWindows   = map[string]int{}
)

// normModelWindowKey normalizes a model id to the dot version form, lowercased
// and trimmed, so "claude-opus-4-8" and "claude-opus-4.8" map to one key.
func normModelWindowKey(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	return claudeVersionPattern.ReplaceAllString(m, "claude-$1-$2.$3")
}

// registerModelWindow records Kiro's real input window for a model. No-op for
// non-positive limits (external/static providers report none) so the version
// heuristic still applies to them.
func registerModelWindow(modelID string, maxInputTokens int) {
	if maxInputTokens <= 0 {
		return
	}
	key := normModelWindowKey(modelID)
	if key == "" {
		return
	}
	modelInputWindowsMu.Lock()
	modelInputWindows[key] = maxInputTokens
	modelInputWindowsMu.Unlock()
}

func lookupModelWindow(model string) (int, bool) {
	key := normModelWindowKey(model)
	if key == "" {
		return 0, false
	}
	modelInputWindowsMu.RLock()
	w, ok := modelInputWindows[key]
	modelInputWindowsMu.RUnlock()
	return w, ok
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Prefers Kiro's authoritative per-model limit (registered from
// ListAvailableModels' TokenLimits.MaxInputTokens) when known, so the value
// matches the denominator Kiro used for contextUsagePercentage exactly. Falls
// back to a version heuristic before the first models-cache refresh (or for
// providers that report no limit): the 1M-token window applies to Claude 4.6 and
// newer, while 4.5 and earlier use 200K. This value converts the upstream
// contextUsagePercentage into an absolute input-token count that clients rely on
// to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time (and over-trims the outgoing payload
// via maxInputTokensForModel, silently dropping conversation history).
func getContextWindowSize(model string) int {
	if w, ok := lookupModelWindow(model); ok {
		return w
	}
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>.<minor>" (dot or dash
// form) and is used to classify 1M-window models by version.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)[.-](\d+)`)

// claudeMajorExtractor matches a bare major-only version (e.g. "claude-opus-5")
// with no minor component, so major >= 5 releases get the 1M window. Without
// this, claudeVersionExtractor fails to match, every numeric branch is skipped,
// and a current flagship model silently falls through to the 200K default.
var claudeMajorExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)\b`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		minor, errMin := strconv.Atoi(match[2])
		if errMaj == nil && errMin == nil {
			// 1M window for Claude >= 4.6 (4.6, 4.7, 4.8, ...) and any major >= 5.
			if major > 4 {
				return true
			}
			if major == 4 && minor >= 6 {
				return true
			}
			return false
		}
	}
	// Major-only version (e.g. claude-opus-5): major >= 5 gets the 1M window.
	if match := claudeMajorExtractor.FindStringSubmatch(m); match != nil {
		if major, err := strconv.Atoi(match[1]); err == nil && major >= 5 {
			return true
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	if chunk == prev {
		return ""
	}

	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}

	return chunk
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

// toolInputAccumulator reconciles the two framings Kiro uses for a tool call's
// argument JSON, which vary by upstream version and cannot be told apart from a
// single fragment:
//
//	delta    — each frame carries the NEXT slice: `{"path":` then `"a.go"}`
//	snapshot — each frame carries the WHOLE value so far: `{"path":` then `{"path":"a.go"}`
//
// Plain concatenation is correct for the first and produces doubled JSON for the
// second (`{"path":{"path":"a.go"}}`), which surfaces to the client as a tool
// call with malformed arguments — the "Error editing file" symptom. So both
// readings are kept and the one that decodes to complete JSON wins at close.
type toolInputAccumulator struct {
	concat      strings.Builder
	last        string
	sawSnapshot bool
}

// Add folds in one argument fragment. A fragment that contains everything seen
// so far is positive evidence of snapshot framing.
func (t *toolInputAccumulator) Add(frag string) {
	if frag == "" {
		return
	}
	if t.last != "" && len(frag) > len(t.last) && strings.HasPrefix(frag, t.last) {
		t.sawSnapshot = true
	}
	if c := t.concat.String(); c != "" && len(frag) >= len(c) && strings.HasPrefix(frag, c) {
		t.sawSnapshot = true
	}
	// A frame repeating the previous one verbatim is only meaningful when that
	// value already stands on its own as complete JSON; otherwise it is an
	// ordinary duplicate delta.
	if frag == t.last && isCompleteToolJSON(frag) {
		t.sawSnapshot = true
	}
	t.concat.WriteString(frag)
	t.last = frag
}

// SetSnapshot records arguments that arrived as a structured object rather than a
// string fragment. Those are always the complete value, so earlier fragments are
// discarded outright.
func (t *toolInputAccumulator) SetSnapshot(js string) {
	t.concat.Reset()
	t.concat.WriteString(js)
	t.last = js
	t.sawSnapshot = true
}

func (t *toolInputAccumulator) Len() int { return t.concat.Len() }

// Resolve returns the reading most likely to be the arguments the model actually
// wrote. Decodability is the primary signal and is checked before the framing
// heuristic, so a stream that mixes both still resolves to valid JSON.
func (t *toolInputAccumulator) Resolve() string {
	concat := strings.TrimSpace(t.concat.String())
	last := strings.TrimSpace(t.last)
	if concat == last {
		return concat
	}
	concatOK, lastOK := isCompleteToolJSON(concat), isCompleteToolJSON(last)
	switch {
	case concatOK && !lastOK:
		return concat
	case lastOK && !concatOK:
		return last
	case t.sawSnapshot:
		return last
	default:
		return concat
	}
}

// isCompleteToolJSON reports whether s is a self-contained JSON object or array.
// Tool arguments are always one of those two, so a bare scalar (a lone `"a.go"`
// delta, which json.Valid accepts) must not count as complete.
func isCompleteToolJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	return json.Valid([]byte(s))
}

type toolUseState struct {
	ToolUseID   string
	Name        string
	Input       toolInputAccumulator
	GeneratedID bool
}

// handleToolUseEvent folds one toolUseEvent frame into the in-flight tool call,
// closing out the previous one when a new call starts. It reports whether a
// completed call was forwarded and propagates incomplete-argument errors rather
// than swallowing them.
func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) (next *toolUseState, emitted bool, err error) {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			if current.GeneratedID && current.Name == name {
				current.ToolUseID = toolUseID
				current.GeneratedID = false
			} else {
				emitted, err = finishToolUse(current, callback)
				if err != nil {
					return nil, emitted, err
				}
				current = &toolUseState{ToolUseID: toolUseID, Name: name}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if name != "" && current != nil && current.Name != name {
		emitted, err = finishToolUse(current, callback)
		if err != nil {
			return nil, emitted, err
		}
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.Input.Add(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.Input.SetSnapshot(string(data))
		}
	}

	if isStop && current != nil {
		stopEmitted, stopErr := finishToolUse(current, callback)
		return nil, emitted || stopEmitted, stopErr
	}

	return current, emitted, nil
}

// errIncompleteKiroToolInput means the stream ended while the model was still
// writing a tool call's argument JSON. It must never be downgraded to "no
// arguments": a client that receives the call anyway would execute the tool with
// parameters the model never finished.
var errIncompleteKiroToolInput = errors.New("upstream stream ended with incomplete tool input")

// finishToolUse validates the buffered tool arguments and forwards the completed
// call. It reports whether anything was actually handed to the client, which the
// stream parser needs to decide if a retry is safe.
//
// Argument validation runs BEFORE every early-return guard. Ordering matters:
// when the JSON check sat behind the `callback.OnToolUse == nil` guard, any
// handler that left that callback unset silently skipped validation entirely,
// and a truncated tool call passed for a clean turn.
func finishToolUse(state *toolUseState, callback *KiroStreamCallback) (emitted bool, err error) {
	if state == nil {
		return false, nil
	}

	var input map[string]interface{}
	if state.Input.Len() > 0 {
		if err := json.Unmarshal([]byte(state.Input.Resolve()), &input); err != nil {
			return false, fmt.Errorf("%w: %v", errIncompleteKiroToolInput, err)
		}
	}

	// A tool use whose name never arrived cannot be forwarded — but dropping it
	// silently makes the turn look complete, so say so.
	if state.Name == "" {
		logger.Warnf("[KiroAPI] Dropping tool use %q: upstream never sent a tool name", state.ToolUseID)
		return false, nil
	}
	if callback == nil || callback.OnToolUse == nil {
		return false, nil
	}

	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
	return true, nil
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

// extractStringHeader returns the value of the named string header (value type 7)
// from AWS Event Stream message headers, or "" if absent.
func extractStringHeader(headers []byte, target string) string {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			if name == target {
				return value
			}
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return ""
}
