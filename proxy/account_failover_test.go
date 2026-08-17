package proxy

import "testing"

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		// A hard monthly cap, verbatim from the Kiro runtime. It arrives as 402
		// and mentions neither "quota" nor "429", so it has to be matched on the
		// reason code or the account stays selectable and burns the whole month.
		{name: "monthly cap", fn: isQuotaErrorMessage,
			msg: `HTTP 402 from Kiro Runtime: {"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestIsProxyErrorMessage(t *testing.T) {
	hits := []string{
		"require-proxy: no proxy configured for account",
		"proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused",
		"socks connect tcp: i/o timeout",
		"dial tcp 1.2.3.4:8080: connectex: A connection attempt failed",
	}
	for _, m := range hits {
		if !isProxyErrorMessage(m) {
			t.Fatalf("expected proxy-error match for %q", m)
		}
	}
	misses := []string{
		"HTTP 401 unauthorized",
		"quota exhausted on KiroIDE",
		"temporarily_suspended",
	}
	for _, m := range misses {
		if isProxyErrorMessage(m) {
			t.Fatalf("did not expect proxy-error match for %q", m)
		}
	}
}
