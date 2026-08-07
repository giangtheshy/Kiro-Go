// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
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
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
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
		for _, ep := range endpoints {
			// Update the origin field for the selected endpoint.
			payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

			// Target the account's region; endpoint URLs are declared for us-east-1.
			epURL := regionalizeURL(ep.URL, account)
			reqBody, _ := json.Marshal(payload)
			req, err := http.NewRequest("POST", epURL, bytes.NewReader(reqBody))
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
			req.Header.Set("Accept", "*/*")
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
				// Authentication errors and payment errors are not retried across endpoints.
				if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
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
			err = parseEventStream(resp.Body, callback)
			resp.Body.Close()
			return err
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

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
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
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}
		if totalLength > maxEventStreamFrameBytes {
			return fmt.Errorf("event stream frame too large: %d bytes (max %d)", totalLength, maxEventStreamFrameBytes)
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
			return err
		}

		if headersLength > len(msgBuf)-4 {
			continue
		}

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
			return fmt.Errorf("upstream stream exception %s: %s", excType, strings.TrimSpace(string(payloadBytes)))
		}

		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, true)
				}
			}
		case "toolUseEvent":
			currentToolUse = handleToolUseEvent(event, currentToolUse, callback)
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if currentToolUse != nil {
		finishToolUse(currentToolUse, callback)
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
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

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
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

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// largeContextMinor matches "claude-<family>-<major>.<minor>" (dot or dash form)
// and is used to classify 1M-window models by version.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)[.-](\d+)`)

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

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
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
				finishToolUse(current, callback)
				current = &toolUseState{ToolUseID: toolUseID, Name: name}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if name != "" && current != nil && current.Name != name {
		finishToolUse(current, callback)
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		finishToolUse(current, callback)
		return nil
	}

	return current
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || state.Name == "" || callback == nil || callback.OnToolUse == nil {
		return
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		json.Unmarshal([]byte(state.InputBuffer.String()), &input)
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
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
