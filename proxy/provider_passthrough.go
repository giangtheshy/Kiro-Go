package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// passthroughCtx carries what an external provider needs that the Kiro payload
// has already thrown away: the client's original, untranslated request body and
// the headers that came with it.
type passthroughCtx struct {
	Raw      []byte      // exact body the client sent
	Header   http.Header // original client headers (for anthropic-beta passthrough)
	Stream   bool
	Endpoint string // one of the config.ProviderEndpoint* constants
}

// providerStreamScanBuffer bounds one SSE line. Upstream payloads (large tool
// results, base64 images) routinely exceed bufio's 64KB default.
const providerStreamScanBuffer = 8 << 20 // 8MB

// providerUsage is the token accounting sniffed out of a provider response.
type providerUsage struct {
	InputTokens  int
	OutputTokens int
	CacheWrite   int
	CacheRead    int
	Seen         bool
}

// serveViaProvider forwards the client's original request to an external provider
// and relays the response back verbatim.
//
// handled is true when the response has been written to w, so the caller must
// stop. When it is false nothing at all was written — a connection failure or a
// non-2xx status before any byte was relayed — and err explains why, so the
// caller can mark the provider excluded and move on to the next upstream.
func (h *Handler) serveViaProvider(
	w http.ResponseWriter,
	step *upstreamStep,
	pc *passthroughCtx,
	model, apiKeyID string,
	startedAt time.Time,
	fallbackInputTokens int,
) (handled bool, err error) {
	p := step.Provider

	// The detail stays server-side. A provider failure carries the upstream URL,
	// its HTTP status and a slice of its response body — enough to identify which
	// vendor sits behind this proxy and to expose their error text (which can
	// include account or billing wording). Callers put the returned error straight
	// into the client response, so everything here collapses to one opaque
	// message: a failing provider is indistinguishable from an exhausted pool.
	fail := func(e error) (bool, error) {
		logger.Warnf("[Provider] %s: %v", p.Name, e)
		config.RecordProviderUsage(p.ID, 0, 0, true)
		return false, errNoUpstreamAvailable
	}

	body, err := bridgeRequestBody(step, pc)
	if err != nil {
		return fail(fmt.Errorf("cannot build request body: %w", err))
	}

	proxyURL, poolKey, err := SelectProxyForAccount(&config.Account{ProxyURL: p.ProxyURL})
	if err != nil {
		return fail(err)
	}

	// The upstream path is the provider's own protocol, which is not the client's
	// path when this step is bridged.
	upstreamEndpoint := step.UpstreamEndpoint
	if upstreamEndpoint == "" {
		upstreamEndpoint = pc.Endpoint
	}
	url := p.BaseURL + upstreamEndpoint
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fail(fmt.Errorf("build request failed: %w", err))
	}
	applyProviderHeaders(req, p, pc)

	resp, err := GetClientForProxy(proxyURL).Do(req)
	if err != nil {
		if poolKey != "" && isProxyErrorMessage(err.Error()) {
			config.MarkProxyUnhealthy(poolKey)
		}
		return fail(fmt.Errorf("request to %s failed: %w", url, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fail(fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail))))
	}
	if poolKey != "" {
		config.MarkProxyHealthy(poolKey)
	}

	var usage providerUsage
	if pc.Stream {
		if !h.relayProviderStream(w, resp, p, step, model, fallbackInputTokens, &usage) {
			// The ResponseWriter cannot stream, so nothing was written; let the
			// caller fall through to the next upstream.
			return fail(errors.New("streaming not supported by response writer"))
		}
	} else if err := h.relayProviderJSON(w, resp, step, model, fallbackInputTokens, &usage); err != nil {
		// A bridged body that cannot be translated is detected before anything is
		// written, so this is still a clean "nothing sent" failure and the caller
		// may try the next upstream.
		return fail(err)
	}

	inTok, outTok, credits := providerBilling(p.Protocol, usage, step.Pricing, fallbackInputTokens)
	h.recordSuccessForApiKey(apiKeyID, inTok, outTok, credits, model, providerAsAccount(p), providerLogEndpoint(pc.Endpoint), startedAt, "")
	config.RecordProviderUsage(p.ID, int64(inTok+outTok), credits, false)
	return true, nil
}

// relayProviderStream streams the upstream response to the client, sniffing usage
// as it goes. For a passthrough step the bytes are copied verbatim; for a bridged
// step each upstream line is fed through a translator that emits client-shaped SSE
// frames instead.
//
// Returns false only when the ResponseWriter cannot stream at all, which is the
// one case where nothing has been written and the caller may retry elsewhere.
func (h *Handler) relayProviderStream(
	w http.ResponseWriter,
	resp *http.Response,
	p *config.Provider,
	step *upstreamStep,
	clientModel string,
	fallbackInputTokens int,
	usage *providerUsage,
) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	translator := newBridgeStreamTranslator(step.Bridge, clientModel, fallbackInputTokens)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), providerStreamScanBuffer)
	for scanner.Scan() {
		line := scanner.Bytes()
		// Usage is sniffed off the UPSTREAM line, in the upstream's own protocol,
		// before any translation. That keeps billing independent of the bridge.
		observeProviderUsage(p.Protocol, line, usage)

		if translator == nil {
			if _, err := w.Write(append(bytes.Clone(line), '\n')); err != nil {
				// Client hung up. The bytes we already sent are gone; nothing to retry.
				return true
			}
			flusher.Flush()
			continue
		}

		for _, frame := range translator.translate(line) {
			if _, err := w.Write(frame); err != nil {
				return true
			}
		}
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		logger.Warnf("[Provider] %s: stream read error: %v", p.Name, err)
	}

	// Close the translated document even when the upstream stream died early: an
	// Anthropic client rejects a message whose content blocks were never closed,
	// so a truncated upstream must still produce a well-formed tail.
	if translator != nil {
		for _, frame := range translator.finish() {
			if _, err := w.Write(frame); err != nil {
				return true
			}
		}
		flusher.Flush()
	}
	return true
}

// relayProviderJSON writes a non-streaming response, translating it first when the
// step is bridged. An error means nothing was written.
func (h *Handler) relayProviderJSON(
	w http.ResponseWriter,
	resp *http.Response,
	step *upstreamStep,
	clientModel string,
	fallbackInputTokens int,
	usage *providerUsage,
) error {
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Warnf("[Provider] response read error: %v", err)
	}
	observeProviderJSONUsage(payload, usage)

	out := payload
	if step.Bridge != config.BridgeNone {
		translated, err := bridgeJSONResponse(step.Bridge, payload, clientModel, fallbackInputTokens)
		if err != nil {
			return fmt.Errorf("cannot translate %s response: %w", step.Bridge, err)
		}
		out = translated
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(out)
	return nil
}

// applyProviderHeaders sets auth and protocol headers, then lets the provider's
// configured custom headers override anything above them.
func applyProviderHeaders(req *http.Request, p *config.Provider, pc *passthroughCtx) {
	req.Header.Set("Content-Type", "application/json")
	if pc.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	switch p.Protocol {
	case config.ProviderProtocolAnthropic:
		if p.APIKey != "" {
			req.Header.Set("x-api-key", p.APIKey)
		}
		version := pc.Header.Get("anthropic-version")
		if version == "" {
			version = "2023-06-01"
		}
		req.Header.Set("anthropic-version", version)
		if beta := pc.Header.Get("anthropic-beta"); beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
	case config.ProviderProtocolOpenAI:
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
	}

	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
}

// rewriteModelField replaces the "model" field of the client body with the
// provider's real model name, leaving every other field untouched — that is what
// makes this a passthrough rather than a translation.
//
// When includeUsage is set (OpenAI-protocol streaming), it also opts into
// stream_options.include_usage so the upstream reports token counts in the final
// chunk; without it there is nothing to bill against.
func rewriteModelField(raw []byte, upstreamModel string, includeUsage bool) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["model"] = upstreamModel

	if includeUsage {
		opts, _ := doc["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		if _, exists := opts["include_usage"]; !exists {
			opts["include_usage"] = true
			doc["stream_options"] = opts
		}
	}
	return json.Marshal(doc)
}

// --- usage sniffing -------------------------------------------------------

type anthropicUsageFields struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type openAIUsageFields struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`

	// /v1/responses uses a different spelling of the same thing.
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	InputTokensDetail *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// observeProviderUsage extracts token counts from one SSE line. Anything that is
// not a JSON data line is ignored.
func observeProviderUsage(protocol string, line []byte, usage *providerUsage) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || !bytes.HasPrefix(payload, []byte("{")) {
		return
	}
	if protocol == config.ProviderProtocolAnthropic {
		observeAnthropicUsage(payload, usage)
		return
	}
	observeOpenAIUsage(payload, usage)
}

// observeProviderJSONUsage extracts token counts from a non-streaming body. It
// probes both shapes so it works for Anthropic, chat/completions and responses
// without the caller having to say which.
func observeProviderJSONUsage(payload []byte, usage *providerUsage) {
	observeAnthropicUsage(payload, usage)
	if !usage.Seen {
		observeOpenAIUsage(payload, usage)
	}
}

func observeAnthropicUsage(payload []byte, usage *providerUsage) {
	var env struct {
		Message *struct {
			Usage *anthropicUsageFields `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsageFields `json:"usage"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	// message_start carries the input side, message_delta the output side, so
	// each field is merged rather than overwritten.
	if env.Message != nil && env.Message.Usage != nil {
		mergeAnthropicUsage(env.Message.Usage, usage)
	}
	if env.Usage != nil {
		mergeAnthropicUsage(env.Usage, usage)
	}
}

func mergeAnthropicUsage(u *anthropicUsageFields, usage *providerUsage) {
	if u.InputTokens > 0 {
		usage.InputTokens = u.InputTokens
		usage.Seen = true
	}
	if u.OutputTokens > 0 {
		usage.OutputTokens = u.OutputTokens
		usage.Seen = true
	}
	if u.CacheCreationInputTokens > 0 {
		usage.CacheWrite = u.CacheCreationInputTokens
		usage.Seen = true
	}
	if u.CacheReadInputTokens > 0 {
		usage.CacheRead = u.CacheReadInputTokens
		usage.Seen = true
	}
}

func observeOpenAIUsage(payload []byte, usage *providerUsage) {
	var env struct {
		Response *struct {
			Usage *openAIUsageFields `json:"usage"`
		} `json:"response"`
		Usage *openAIUsageFields `json:"usage"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Response != nil && env.Response.Usage != nil {
		mergeOpenAIUsage(env.Response.Usage, usage)
	}
	if env.Usage != nil {
		mergeOpenAIUsage(env.Usage, usage)
	}
}

func mergeOpenAIUsage(u *openAIUsageFields, usage *providerUsage) {
	in, out, cached := u.PromptTokens, u.CompletionTokens, 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if in == 0 && u.InputTokens > 0 {
		in = u.InputTokens
	}
	if out == 0 && u.OutputTokens > 0 {
		out = u.OutputTokens
	}
	if cached == 0 && u.InputTokensDetail != nil {
		cached = u.InputTokensDetail.CachedTokens
	}

	if in > 0 {
		usage.InputTokens = in
		usage.Seen = true
	}
	if out > 0 {
		usage.OutputTokens = out
		usage.Seen = true
	}
	if cached > 0 {
		usage.CacheRead = cached
		usage.Seen = true
	}
}

// --- billing --------------------------------------------------------------

// providerBilling converts sniffed usage into billable token counts and credits.
//
// The two protocols disagree on what the input count means, and getting it wrong
// double-charges the customer for cached context:
//   - Anthropic reports input_tokens with cache tokens already excluded.
//   - OpenAI reports prompt_tokens with cached_tokens included, so the cached
//     part has to be subtracted before it is priced at the full input rate.
//
// When the provider reports no usage at all, the caller's own estimate is used so
// a request is never billed as free.
func providerBilling(protocol string, u providerUsage, price config.ProviderPricing, fallbackInputTokens int) (inTokens, outTokens int, credits float64) {
	billableIn := u.InputTokens
	cacheRead := u.CacheRead
	cacheWrite := u.CacheWrite

	if protocol == config.ProviderProtocolOpenAI {
		billableIn = u.InputTokens - u.CacheRead
		if billableIn < 0 {
			billableIn = 0
		}
	}

	if !u.Seen && fallbackInputTokens > 0 {
		billableIn = fallbackInputTokens
	}

	// Reported for stats/logging: the full input side including cache traffic.
	inTokens = billableIn + cacheRead + cacheWrite
	outTokens = u.OutputTokens

	credits = perMillion(billableIn, price.Input) +
		perMillion(u.OutputTokens, price.Output) +
		perMillion(cacheWrite, price.CacheWrite) +
		perMillion(cacheRead, price.CacheRead)
	return inTokens, outTokens, credits
}

func perMillion(tokens int, pricePerMillion float64) float64 {
	if tokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	return float64(tokens) / 1_000_000 * pricePerMillion
}

// providerAsAccount adapts a provider to the *config.Account the request log and
// per-key stats expect, so a provider-served request shows up in the existing
// admin views without new plumbing.
func providerAsAccount(p *config.Provider) *config.Account {
	return &config.Account{ID: p.ID, Email: "provider:" + p.Name}
}

// providerLogEndpoint maps an upstream path to the short endpoint label the
// request log already uses for Kiro traffic.
func providerLogEndpoint(endpoint string) string {
	if endpoint == config.ProviderEndpointMessages {
		return "claude"
	}
	return "openai"
}
