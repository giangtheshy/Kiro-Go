package proxy

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

// Probe: how many upstream HTTP calls does a SINGLE account cost when its
// profile lookup returns an empty list, and does a SECOND request re-probe?
func TestProbeProfileArnRepeatsPerRequest(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	var calls int64
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt64(&calls, 1)
			t.Logf("  call %d -> %s", atomic.LoadInt64(&calls), req.URL.Host+req.URL.Path)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[]}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	acc := config.Account{
		ID:          "acct-probe",
		Email:       "probe@example.com",
		Enabled:     true,
		AccessToken: "tok",
		Region:      "us-east-1",
		Provider:    "IdC",
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	t.Log("REQUEST 1:")
	a1 := acc
	_, err1 := ResolveProfileArn(&a1)
	first := atomic.LoadInt64(&calls)
	t.Logf("REQUEST 1 => err=%v, upstream calls=%d", err1, first)

	atomic.StoreInt64(&calls, 0)
	t.Log("REQUEST 2 (same account, immediately after):")
	a2 := acc
	_, err2 := ResolveProfileArn(&a2)
	second := atomic.LoadInt64(&calls)
	t.Logf("REQUEST 2 => err=%v, upstream calls=%d", err2, second)

	if second > 0 {
		t.Logf("CONFIRMED: no negative cache — request 2 re-probed %d times", second)
	} else {
		t.Log("request 2 was suppressed (negative cache present)")
	}
}
