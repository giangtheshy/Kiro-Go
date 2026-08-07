package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// A failing external provider must not describe itself to the client. The error
// serveViaProvider returns is written straight into the client response by every
// caller, so anything it carries — the vendor's hostname, its status code, its
// error text — becomes public. These tests pin the sanitization.

func TestProviderFailureDoesNotLeakUpstreamDetail(t *testing.T) {
	// An upstream whose error body names the vendor and its billing state.
	mux := http.NewServeMux()
	mux.HandleFunc(config.ProviderEndpointMessages, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":{"message":"acme-lm: organization org_9f3 has insufficient credits","code":"billing_hard_limit"}}`)
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
		t.Fatal("expected an error so the caller skips this provider")
	}

	msg := err.Error()
	for _, secret := range []string{
		"acme-lm",              // vendor name from the upstream body
		"org_9f3",              // upstream account identifier
		"insufficient credits", // upstream error text
		"billing_hard_limit",   // upstream error code
		"402",                  // upstream status
		srv.URL,                // upstream hostname/port
		"stub",                 // the provider's configured name
	} {
		if strings.Contains(msg, secret) {
			t.Fatalf("client-visible error leaked %q: %s", secret, msg)
		}
	}

	if !errors.Is(err, errNoUpstreamAvailable) {
		t.Fatalf("expected the generic upstream-exhausted error, got %q", msg)
	}
	// Indistinguishable from the message used when the Kiro pool runs dry, so a
	// caller cannot infer that external providers are configured at all.
	if msg != noUpstreamAvailableMessage {
		t.Fatalf("expected %q, got %q", noUpstreamAvailableMessage, msg)
	}
}

// A transport-level failure (dead host) must be sanitized the same way — the
// dial error would otherwise carry the provider's host and port.
func TestProviderTransportFailureIsSanitized(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	deadURL := srv.URL
	srv.Close() // nothing is listening now

	step := newPassthroughStep(t, httptest.NewServer(http.NewServeMux()), config.ProviderProtocolAnthropic, config.ProviderPricing{})
	step.Provider.BaseURL = deadURL

	pc := &passthroughCtx{
		Raw:      []byte(`{"model":"claude-sonnet-4.5"}`),
		Header:   http.Header{},
		Stream:   true,
		Endpoint: config.ProviderEndpointMessages,
	}

	rec := httptest.NewRecorder()
	handled, err := (&Handler{}).serveViaProvider(rec, step, pc, "claude-sonnet-4.5", "", time.Now(), 0)
	if handled {
		t.Fatal("a dead upstream must not be reported as handled")
	}
	if !errors.Is(err, errNoUpstreamAvailable) {
		t.Fatalf("expected the generic error, got %v", err)
	}
	if strings.Contains(err.Error(), deadURL) {
		t.Fatalf("dial error leaked the upstream address: %v", err)
	}
}

// The sanitized error must still answer 503 rather than falling through to a
// generic 500, so the client sees "try again later" semantics.
func TestSanitizedProviderErrorMapsTo503(t *testing.T) {
	if got := statusForUpstreamError(errNoUpstreamAvailable); got != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for an exhausted-upstream error, got %d", got)
	}
}

// Detail must survive somewhere: an operator still needs to know why a provider
// was skipped, so it belongs in the server log.
func TestProviderFailureDetailIsLogged(t *testing.T) {
	var buf strings.Builder
	logger.SetOutput(&buf)
	defer logger.SetOutput(os.Stderr)

	mux := http.NewServeMux()
	mux.HandleFunc(config.ProviderEndpointMessages, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":"insufficient credits"}`)
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

	_, _ = (&Handler{}).serveViaProvider(httptest.NewRecorder(), step, pc, "claude-sonnet-4.5", "", time.Now(), 0)

	logged := buf.String()
	if !strings.Contains(logged, "insufficient credits") {
		t.Fatalf("operator log must retain the upstream detail, got: %s", logged)
	}
	if !strings.Contains(logged, "stub") {
		t.Fatalf("operator log must identify which provider failed, got: %s", logged)
	}
}
