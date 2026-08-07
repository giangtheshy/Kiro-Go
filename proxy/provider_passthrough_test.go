package proxy

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// newPassthroughStep builds an upstreamStep pointing at a stub provider served by
// srv, so a test exercises the real HTTP path without touching a live API.
func newPassthroughStep(t *testing.T, srv *httptest.Server, protocol string, pricing config.ProviderPricing) *upstreamStep {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	created, err := config.AddProvider(config.Provider{
		Name:     "stub",
		Enabled:  true,
		Protocol: protocol,
		BaseURL:  srv.URL,
		APIKey:   "stub-key",
		Models:   []config.ProviderModel{{Alias: "claude-sonnet-4.5", Name: "upstream-model"}},
		Pricing:  pricing,
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	return &upstreamStep{Provider: created2ptr(created), UpstreamModel: "upstream-model", Pricing: pricing}
}

func created2ptr(p config.Provider) *config.Provider { return &p }

func sseServer(t *testing.T, path string, lines []string, capture *[]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			*capture = body
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, line := range lines {
			io.WriteString(w, line+"\n")
		}
	})
	return httptest.NewServer(mux)
}

// TestServeViaProviderRewritesOnlyTheModel is the heart of passthrough: the body
// reaches the provider byte-identical except for the model name.
func TestServeViaProviderRewritesOnlyTheModel(t *testing.T) {
	var got []byte
	srv := sseServer(t, config.ProviderEndpointMessages, []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":20}}`,
	}, &got)
	defer srv.Close()

	step := newPassthroughStep(t, srv, config.ProviderProtocolAnthropic, config.ProviderPricing{})
	pc := &passthroughCtx{
		Raw:      []byte(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"metadata":{"user_id":"u1"}}`),
		Header:   http.Header{},
		Stream:   true,
		Endpoint: config.ProviderEndpointMessages,
	}

	h := &Handler{}
	rec := httptest.NewRecorder()
	handled, err := h.serveViaProvider(rec, step, pc, "claude-sonnet-4.5", "", time.Now(), 0)
	if !handled || err != nil {
		t.Fatalf("expected the provider to handle the request, handled=%t err=%v", handled, err)
	}

	var sent map[string]any
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if sent["model"] != "upstream-model" {
		t.Fatalf("model not rewritten: %v", sent["model"])
	}
	if sent["max_tokens"] != float64(64) || sent["stream"] != true {
		t.Fatalf("unrelated fields were altered: %v", sent)
	}
	if meta, _ := sent["metadata"].(map[string]any); meta == nil || meta["user_id"] != "u1" {
		t.Fatalf("nested fields were dropped: %v", sent["metadata"])
	}
}

func TestServeViaProviderRelaysStreamVerbatim(t *testing.T) {
	lines := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		``,
		`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
	}
	srv := sseServer(t, config.ProviderEndpointMessages, lines, nil)
	defer srv.Close()

	step := newPassthroughStep(t, srv, config.ProviderProtocolAnthropic, config.ProviderPricing{})
	pc := &passthroughCtx{
		Raw:      []byte(`{"model":"claude-sonnet-4.5"}`),
		Header:   http.Header{},
		Stream:   true,
		Endpoint: config.ProviderEndpointMessages,
	}

	rec := httptest.NewRecorder()
	handled, err := (&Handler{}).serveViaProvider(rec, step, pc, "claude-sonnet-4.5", "", time.Now(), 0)
	if !handled || err != nil {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if want := strings.Join(lines, "\n") + "\n"; rec.Body.String() != want {
		t.Fatalf("stream was not relayed verbatim:\n got %q\nwant %q", rec.Body.String(), want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected an SSE content type, got %q", ct)
	}
}

// TestServeViaProviderNonSuccessLeavesResponseUntouched guarantees the caller can
// still fall through to the next upstream after a provider error.
func TestServeViaProviderNonSuccessLeavesResponseUntouched(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(config.ProviderEndpointMessages, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	step := newPassthroughStep(t, srv, config.ProviderProtocolAnthropic, config.ProviderPricing{})
	pc := &passthroughCtx{
		Raw:      []byte(`{"model":"claude-sonnet-4.5"}`),
		Header:   http.Header{},
		Stream:   true,
		Endpoint: config.ProviderEndpointMessages,
	}

	rec := httptest.NewRecorder()
	handled, err := (&Handler{}).serveViaProvider(rec, step, pc, "claude-sonnet-4.5", "", time.Now(), 0)
	if handled {
		t.Fatal("a non-2xx upstream must not be reported as handled")
	}
	if err == nil {
		t.Fatal("expected an error explaining the skip")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("nothing may be written on the failure path, got %q", rec.Body.String())
	}
}

func TestServeViaProviderSetsProtocolHeaders(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		endpoint string
		check    func(t *testing.T, hdr http.Header)
	}{
		{
			protocol: config.ProviderProtocolAnthropic,
			endpoint: config.ProviderEndpointMessages,
			check: func(t *testing.T, hdr http.Header) {
				if hdr.Get("x-api-key") != "stub-key" {
					t.Fatalf("missing x-api-key, got %q", hdr.Get("x-api-key"))
				}
				if hdr.Get("anthropic-version") != "2023-06-01" {
					t.Fatalf("missing default anthropic-version, got %q", hdr.Get("anthropic-version"))
				}
			},
		},
		{
			protocol: config.ProviderProtocolOpenAI,
			endpoint: config.ProviderEndpointChatCompletions,
			check: func(t *testing.T, hdr http.Header) {
				if hdr.Get("Authorization") != "Bearer stub-key" {
					t.Fatalf("missing bearer auth, got %q", hdr.Get("Authorization"))
				}
			},
		},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			var seen http.Header
			mux := http.NewServeMux()
			mux.HandleFunc(tc.endpoint, func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				io.WriteString(w, `{}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			step := newPassthroughStep(t, srv, tc.protocol, config.ProviderPricing{})
			pc := &passthroughCtx{
				Raw:      []byte(`{"model":"claude-sonnet-4.5"}`),
				Header:   http.Header{},
				Endpoint: tc.endpoint,
			}
			rec := httptest.NewRecorder()
			if handled, err := (&Handler{}).serveViaProvider(rec, step, pc, "claude-sonnet-4.5", "", time.Now(), 0); !handled || err != nil {
				t.Fatalf("handled=%t err=%v", handled, err)
			}
			tc.check(t, seen)
		})
	}
}

// TestRewriteModelFieldInjectsIncludeUsage: without stream_options.include_usage an
// OpenAI-compatible upstream reports no tokens, so the request could not be billed.
func TestRewriteModelFieldInjectsIncludeUsage(t *testing.T) {
	decode := func(t *testing.T, raw string, includeUsage bool) map[string]any {
		t.Helper()
		out, err := rewriteModelField([]byte(raw), "up", includeUsage)
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if doc["model"] != "up" {
			t.Fatalf("model not rewritten: %v", doc["model"])
		}
		return doc
	}

	doc := decode(t, `{"model":"a","stream":true}`, true)
	if opts, _ := doc["stream_options"].(map[string]any); opts == nil || opts["include_usage"] != true {
		t.Fatalf("expected include_usage to be injected, got %v", doc["stream_options"])
	}

	// An explicit client choice is never overridden.
	doc = decode(t, `{"model":"a","stream_options":{"include_usage":false}}`, true)
	if opts, _ := doc["stream_options"].(map[string]any); opts["include_usage"] != false {
		t.Fatalf("client's include_usage=false must be respected, got %v", doc["stream_options"])
	}

	// Non-streaming requests must not grow a stream_options block.
	doc = decode(t, `{"model":"a"}`, false)
	if _, exists := doc["stream_options"]; exists {
		t.Fatal("stream_options must not be added to a non-streaming request")
	}
}

func TestObserveProviderUsage(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		lines    []string
		want     providerUsage
	}{
		{
			name:     "anthropic sse merges start and delta",
			protocol: config.ProviderProtocolAnthropic,
			lines: []string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":30,"cache_read_input_tokens":70}}}`,
				`data: {"type":"message_delta","usage":{"output_tokens":42}}`,
			},
			want: providerUsage{InputTokens: 100, OutputTokens: 42, CacheWrite: 30, CacheRead: 70, Seen: true},
		},
		{
			name:     "openai sse final usage chunk",
			protocol: config.ProviderProtocolOpenAI,
			lines: []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":25,"prompt_tokens_details":{"cached_tokens":200}}}`,
				`data: [DONE]`,
			},
			want: providerUsage{InputTokens: 500, OutputTokens: 25, CacheRead: 200, Seen: true},
		},
		{
			name:     "responses sse completed event",
			protocol: config.ProviderProtocolOpenAI,
			lines: []string{
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":80,"output_tokens":12,"input_tokens_details":{"cached_tokens":30}}}}`,
			},
			want: providerUsage{InputTokens: 80, OutputTokens: 12, CacheRead: 30, Seen: true},
		},
		{
			name:     "non-data lines are ignored",
			protocol: config.ProviderProtocolAnthropic,
			lines:    []string{`event: ping`, `: keepalive`, ``},
			want:     providerUsage{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got providerUsage
			for _, line := range tc.lines {
				observeProviderUsage(tc.protocol, []byte(line), &got)
			}
			if got != tc.want {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestObserveProviderJSONUsage(t *testing.T) {
	var anthropic providerUsage
	observeProviderJSONUsage([]byte(`{"usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":4}}`), &anthropic)
	if anthropic != (providerUsage{InputTokens: 11, OutputTokens: 3, CacheRead: 4, Seen: true}) {
		t.Fatalf("anthropic json usage = %+v", anthropic)
	}

	var openai providerUsage
	observeProviderJSONUsage([]byte(`{"usage":{"prompt_tokens":90,"completion_tokens":7}}`), &openai)
	if openai != (providerUsage{InputTokens: 90, OutputTokens: 7, Seen: true}) {
		t.Fatalf("openai json usage = %+v", openai)
	}
}

// TestProviderBilling pins the protocol difference that would otherwise
// double-charge for cached context: Anthropic excludes cache tokens from
// input_tokens, OpenAI includes them in prompt_tokens.
func TestProviderBilling(t *testing.T) {
	price := config.ProviderPricing{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.3}

	t.Run("anthropic input excludes cache", func(t *testing.T) {
		u := providerUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheWrite: 1_000_000, CacheRead: 1_000_000, Seen: true}
		in, out, credits := providerBilling(config.ProviderProtocolAnthropic, u, price, 0)
		if in != 3_000_000 || out != 1_000_000 {
			t.Fatalf("token totals = %d/%d", in, out)
		}
		if math.Abs(credits-(3+15+3.75+0.3)) > 1e-9 {
			t.Fatalf("credits = %v, want %v", credits, 3+15+3.75+0.3)
		}
	})

	t.Run("openai prompt tokens include cache", func(t *testing.T) {
		u := providerUsage{InputTokens: 1_000_000, OutputTokens: 0, CacheRead: 400_000, Seen: true}
		in, _, credits := providerBilling(config.ProviderProtocolOpenAI, u, price, 0)
		// 600k billed at the input rate, 400k at the cache-read rate.
		want := 0.6*3 + 0.4*0.3
		if math.Abs(credits-want) > 1e-9 {
			t.Fatalf("credits = %v, want %v", credits, want)
		}
		// The reported input total is still the full prompt, cache included.
		if in != 1_000_000 {
			t.Fatalf("reported input tokens = %d, want 1000000", in)
		}
	})

	t.Run("cached tokens larger than prompt never go negative", func(t *testing.T) {
		u := providerUsage{InputTokens: 100, CacheRead: 500, Seen: true}
		_, _, credits := providerBilling(config.ProviderProtocolOpenAI, u, price, 0)
		want := 500.0 / 1_000_000 * 0.3
		if math.Abs(credits-want) > 1e-9 {
			t.Fatalf("credits = %v, want %v", credits, want)
		}
	})

	t.Run("missing usage falls back to the estimate", func(t *testing.T) {
		in, _, credits := providerBilling(config.ProviderProtocolAnthropic, providerUsage{}, price, 2_000_000)
		if in != 2_000_000 {
			t.Fatalf("expected the estimate to be billed, got %d", in)
		}
		if math.Abs(credits-6) > 1e-9 {
			t.Fatalf("credits = %v, want 6", credits)
		}
	})

	t.Run("reported usage wins over the estimate", func(t *testing.T) {
		u := providerUsage{InputTokens: 1_000_000, Seen: true}
		in, _, _ := providerBilling(config.ProviderProtocolAnthropic, u, price, 9_000_000)
		if in != 1_000_000 {
			t.Fatalf("expected the reported usage, got %d", in)
		}
	})
}

func TestProviderAsAccountLabelsTheLog(t *testing.T) {
	acc := providerAsAccount(&config.Provider{ID: "pid", Name: "zai"})
	if acc.ID != "pid" || acc.Email != "provider:zai" {
		t.Fatalf("unexpected log attribution: %+v", acc)
	}
}

func TestProviderLogEndpoint(t *testing.T) {
	if got := providerLogEndpoint(config.ProviderEndpointMessages); got != "claude" {
		t.Fatalf("messages => %q, want claude", got)
	}
	for _, ep := range []string{config.ProviderEndpointChatCompletions, config.ProviderEndpointResponses} {
		if got := providerLogEndpoint(ep); got != "openai" {
			t.Fatalf("%s => %q, want openai", ep, got)
		}
	}
}

// TestServeViaProviderBillsTheApiKey walks the full path and checks the credits
// actually land on the customer's key.
func TestServeViaProviderBillsTheApiKey(t *testing.T) {
	srv := sseServer(t, config.ProviderEndpointMessages, []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1000000}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":1000000}}`,
	}, nil)
	defer srv.Close()

	step := newPassthroughStep(t, srv, config.ProviderProtocolAnthropic, config.ProviderPricing{Input: 3, Output: 15})
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-provider-billing"})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	pc := &passthroughCtx{
		Raw:      []byte(`{"model":"claude-sonnet-4.5"}`),
		Header:   http.Header{},
		Stream:   true,
		Endpoint: config.ProviderEndpointMessages,
	}
	rec := httptest.NewRecorder()
	if handled, err := (&Handler{}).serveViaProvider(rec, step, pc, "claude-sonnet-4.5", entry.ID, time.Now(), 0); !handled || err != nil {
		t.Fatalf("handled=%t err=%v", handled, err)
	}

	billed := config.GetApiKeyEntry(entry.ID)
	if billed == nil {
		t.Fatal("api key vanished")
	}
	if math.Abs(billed.CreditsUsed-18) > 1e-9 {
		t.Fatalf("creditsUsed = %v, want 18", billed.CreditsUsed)
	}
	if billed.TokensUsed != 2_000_000 {
		t.Fatalf("tokensUsed = %d, want 2000000", billed.TokensUsed)
	}

	prov := config.GetProvider(step.Provider.ID)
	if prov == nil || prov.RequestCount != 1 || math.Abs(prov.TotalCredits-18) > 1e-9 {
		t.Fatalf("provider counters not updated: %+v", prov)
	}
}

// TestServeViaProviderTargetsTheDeclaredEndpoint guards against a base URL that
// already carries a path being mangled.
func TestServeViaProviderTargetsTheDeclaredEndpoint(t *testing.T) {
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	base, _ := url.JoinPath(srv.URL, "api", "anthropic")
	created, err := config.AddProvider(config.Provider{
		Name:     "prefixed",
		Enabled:  true,
		Protocol: config.ProviderProtocolAnthropic,
		BaseURL:  base + "/",
		APIKey:   "k",
		Models:   []config.ProviderModel{{Alias: "claude-sonnet-4.5", Name: "up"}},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	step := &upstreamStep{Provider: created2ptr(created), UpstreamModel: "up"}
	pc := &passthroughCtx{
		Raw:      []byte(`{"model":"claude-sonnet-4.5"}`),
		Header:   http.Header{},
		Endpoint: config.ProviderEndpointMessages,
	}
	if handled, err := (&Handler{}).serveViaProvider(httptest.NewRecorder(), step, pc, "claude-sonnet-4.5", "", time.Now(), 0); !handled || err != nil {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if gotPath != "/api/anthropic/v1/messages" {
		t.Fatalf("upstream path = %q, want /api/anthropic/v1/messages", gotPath)
	}
}
