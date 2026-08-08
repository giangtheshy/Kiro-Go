package proxy

import (
	"bytes"
	"testing"
)

// The upstream reports its own prompt-cache split. Those numbers are measured,
// unlike the Claude paths' local estimate from cache_control breakpoints — and
// for the OpenAI and Responses protocols, which have no cache_control concept,
// they are the only cache information that exists.
func TestParserSurfacesUpstreamCacheTokens(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
			"usage":                 1.0,
			"uncachedInputTokens":   300,
			"cacheReadInputTokens":  700,
			"cacheWriteInputTokens": 100,
			"outputTokens":          50,
		}),
	}, nil))

	var read, write, inTok, outTok int
	var fired bool
	cb := &KiroStreamCallback{
		OnCacheTokens: func(r, w int) { read, write, fired = r, w, true },
		OnComplete:    func(i, o int) { inTok, outTok = i, o },
	}
	if _, err := parseEventStreamTracked(stream, cb); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !fired {
		t.Fatal("OnCacheTokens must fire when the upstream reports a cache split")
	}
	if read != 700 || write != 100 {
		t.Fatalf("cache split got read=%d write=%d, want 700/100", read, write)
	}
	// The split is a SUBSET of the input count, never an addition to it.
	if inTok != 1100 {
		t.Fatalf("input tokens got %d, want 300+700+100=1100", inTok)
	}
	if outTok != 50 {
		t.Fatalf("output tokens got %d, want 50", outTok)
	}
}

// The regression this fixes. The upstream usually sends a plain inputTokens
// total ALONGSIDE the cache split, and the old code read the split only in the
// fallback branch taken when no total was present — so the measured cache
// numbers were discarded on exactly the requests that carried them.
func TestCacheTokensReadEvenWhenUpstreamAlsoSendsTotal(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
			"usage":                 1.0,
			"inputTokens":           1100,
			"cacheReadInputTokens":  700,
			"cacheWriteInputTokens": 100,
		}),
	}, nil))

	var read, write, inTok int
	cb := &KiroStreamCallback{
		OnCacheTokens: func(r, w int) { read, write = r, w },
		OnComplete:    func(i, _ int) { inTok = i },
	}
	if _, err := parseEventStreamTracked(stream, cb); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if read != 700 || write != 100 {
		t.Fatalf("cache split must survive an explicit total, got read=%d write=%d", read, write)
	}
	// The explicit total still wins for the input count itself.
	if inTok != 1100 {
		t.Fatalf("input tokens got %d, want the reported total 1100", inTok)
	}
}

// snake_case is accepted alongside camelCase, as elsewhere in the token reader.
func TestCacheTokensAcceptSnakeCase(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
			"usage":                       1.0,
			"input_tokens":                500,
			"cache_read_input_tokens":     400,
			"cache_creation_input_tokens": 50,
		}),
	}, nil))

	var read, write int
	cb := &KiroStreamCallback{OnCacheTokens: func(r, w int) { read, write = r, w }}
	if _, err := parseEventStreamTracked(stream, cb); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if read != 400 || write != 50 {
		t.Fatalf("snake_case cache keys got read=%d write=%d, want 400/50", read, write)
	}
}

// A stream that reports no cache split must not fire the callback at all, so a
// path with genuinely no cache activity is distinguishable from one whose
// numbers never arrived. Firing with 0/0 would render a misleading "0 / 0".
func TestNoCacheReportDoesNotFireCallback(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
			"usage":       1.0,
			"inputTokens": 100,
		}),
	}, nil))

	fired := false
	cb := &KiroStreamCallback{OnCacheTokens: func(int, int) { fired = true }}
	if _, err := parseEventStreamTracked(stream, cb); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fired {
		t.Fatal("OnCacheTokens must stay silent when the upstream reported no split")
	}
}

// An upstream that reports a zero split HAS reported one. That is different from
// silence, so the callback fires and the columns render a real zero.
func TestExplicitZeroCacheStillReports(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
			"usage":                 1.0,
			"inputTokens":           100,
			"cacheReadInputTokens":  0,
			"cacheWriteInputTokens": 0,
		}),
	}, nil))

	fired := false
	var read, write int
	cb := &KiroStreamCallback{OnCacheTokens: func(r, w int) { read, write, fired = r, w, true }}
	if _, err := parseEventStreamTracked(stream, cb); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !fired {
		t.Fatal("an explicitly reported zero split must still fire")
	}
	if read != 0 || write != 0 {
		t.Fatalf("got read=%d write=%d, want 0/0", read, write)
	}
}

// A nil OnCacheTokens must not panic: the websearch loop and various probes do
// not set it.
func TestNilCacheTokensCallbackIsSafe(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage":                 1.0,
		"cacheReadInputTokens":  10,
		"cacheWriteInputTokens": 5,
	}))
	if _, err := parseEventStreamTracked(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("a nil OnCacheTokens must be a no-op, got %v", err)
	}
}
