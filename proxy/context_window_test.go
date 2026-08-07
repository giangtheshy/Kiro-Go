package proxy

import "testing"

// TestGetContextWindowSize verifies models are classified into the correct
// context window. This drives the input-token count that clients use to decide
// when to compact; misclassifying opus-4.8 (1M) as 200K under-reports tokens by
// 5x and prevents timely compaction.
func TestGetContextWindowSize(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4.7", 1_000_000},
		{"claude-opus-4.6", 1_000_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-4.8-thinking", 1_000_000},
		{"CLAUDE-OPUS-4.8", 1_000_000},
		// Major-only versions (no minor component). These regressed to 200K
		// because claudeVersionExtractor requires "<major>.<minor>": a bare
		// "claude-opus-5" matched nothing, skipped every numeric branch, and
		// fell through to the default — starving the current flagship model of
		// 800K of context and silently trimming conversation history.
		{"claude-opus-5", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-opus-5-thinking", 1_000_000},
		{"claude-opus-6", 1_000_000},
		{"claude-opus-5.1", 1_000_000},
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-sonnet-4", 200_000},
		{"claude-haiku-4.5", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"unknown-model", 200_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// TestRegisteredModelWindowOverridesHeuristic verifies Kiro's authoritative
// per-model limit (ListAvailableModels' TokenLimits.MaxInputTokens) takes
// precedence over the name heuristic. The heuristic can only ever guess from a
// version string, so a model Kiro reports at a non-standard window — or one whose
// name carries no parseable version at all — would otherwise be sized wrongly.
func TestRegisteredModelWindowOverridesHeuristic(t *testing.T) {
	// The registry is package-global; keep this test's entries out of the
	// heuristic table test above.
	defer clearModelWindows()

	// A model the heuristic would call 200K, reported by Kiro as 500K.
	registerModelWindow("claude-sonnet-4.5", 500_000)
	if got := getContextWindowSize("claude-sonnet-4.5"); got != 500_000 {
		t.Errorf("registered window ignored: got %d, want 500000", got)
	}
	// Dash and dot forms of the same version must resolve to one entry.
	if got := getContextWindowSize("claude-sonnet-4-5"); got != 500_000 {
		t.Errorf("dash form did not match registered dot form: got %d, want 500000", got)
	}
	// A name with no parseable version still gets its authoritative window.
	registerModelWindow("some-internal-alias", 750_000)
	if got := getContextWindowSize("some-internal-alias"); got != 750_000 {
		t.Errorf("unversioned alias window ignored: got %d, want 750000", got)
	}
	// Non-positive limits are ignored so the heuristic still applies.
	registerModelWindow("claude-opus-4.8", 0)
	if got := getContextWindowSize("claude-opus-4.8"); got != 1_000_000 {
		t.Errorf("zero limit should not override heuristic: got %d, want 1000000", got)
	}
}

func clearModelWindows() {
	modelInputWindowsMu.Lock()
	modelInputWindows = map[string]int{}
	modelInputWindowsMu.Unlock()
}
