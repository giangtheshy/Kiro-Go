package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the three ways a Kiro stream can end badly without the client
// being able to tell. Each one used to look like a clean finish, which is what
// made a cut-off answer indistinguishable from a complete one.

// corruptPrelude returns 12 bytes that cannot describe a real frame: the claimed
// total length is below the 16-byte floor (prelude + trailing CRC).
func corruptPrelude(totalLength, headersLength uint32) []byte {
	p := make([]byte, esPreludeLen)
	binary.BigEndian.PutUint32(p[0:4], totalLength)
	binary.BigEndian.PutUint32(p[4:8], headersLength)
	return p
}

// A prelude that cannot describe a frame means the read offset is no longer on a
// frame boundary. Skipping it would restart the parse 12 bytes into arbitrary
// payload bytes and walk garbage, so the stream must be abandoned instead.
func TestParseEventStreamRejectsCorruptPrelude(t *testing.T) {
	var delivered strings.Builder
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "real text"}),
		corruptPrelude(5, 0),
	}, nil))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(text string, _ bool) { delivered.WriteString(text) },
	})

	if !errors.Is(err, errCorruptKiroStream) {
		t.Fatalf("expected errCorruptKiroStream, got %v", err)
	}
	// Everything handed over before the corruption still stands: the caller needs
	// Emitted to know a retry would duplicate visible output.
	if !outcome.Emitted {
		t.Fatal("expected Emitted=true: text was already forwarded before the bad frame")
	}
	if got := delivered.String(); got != "real text" {
		t.Fatalf("text delivered before the corruption must be kept, got %q", got)
	}
}

// headersLength is read straight off the wire, so a value that cannot fit inside
// the frame it belongs to must be rejected before it is used to slice.
func TestParseEventStreamRejectsHeadersLongerThanFrame(t *testing.T) {
	stream := bytes.NewReader(corruptPrelude(64, 1024))

	if _, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
	}); !errors.Is(err, errCorruptKiroStream) {
		t.Fatalf("expected errCorruptKiroStream for oversized headers, got %v", err)
	}
}

// A turn the upstream billed but which produced nothing must not reach the client
// as a normal finish. Claude Code reads an empty content array closed with
// "end_turn" as `API returned an empty or malformed response (HTTP 200)` and
// aborts the entire task, so this has to surface as an error.
func TestClaudeStreamEmptyMeteredTurnIsNotACleanFinish(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Billed, but not a single content frame.
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	h.handleClaudeMessages(rec, req)

	out, _ := io.ReadAll(rec.Body)
	got := string(out)

	if delta := sseFrameContaining(got, "message_delta"); strings.Contains(delta, `"stop_reason":"end_turn"`) {
		t.Fatalf("an empty billed turn must not report end_turn:\n%s", delta)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Fatalf("message_stop marks a finished turn and must be withheld:\n%s", got)
	}
	// The failure must be visible, either as an HTTP status (nothing flushed yet)
	// or in-band once the response is committed.
	if rec.Code == http.StatusOK && !strings.Contains(got, "event: error") {
		t.Fatalf("expected the empty billed turn to surface as an error, got HTTP %d:\n%s", rec.Code, got)
	}
}

// An already-billed empty turn must not be re-sent to the same endpoint: the
// upstream charged for it, so a retry pays twice for one turn.
func TestEmptyMeteredTurnIsNotRetried(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	h.handleClaudeMessages(httptest.NewRecorder(), req)

	// One attempt per account rotation is expected; the empty-stream retry budget
	// (maxEmptyStreamRetries) must NOT apply to a turn that was billed.
	if calls > maxAccountRetryAttempts {
		t.Fatalf("a billed empty turn must not use the empty-stream retry budget, got %d calls", calls)
	}
}

// Kiro frames tool arguments inconsistently across versions and a single fragment
// cannot say which framing is in use. Concatenating snapshot frames produces
// doubled JSON, which reaches the client as malformed tool arguments.
func TestToolInputAccumulatorReconcilesFraming(t *testing.T) {
	tests := []struct {
		name  string
		frags []string
		want  string
	}{
		{
			name:  "delta framing concatenates",
			frags: []string{`{"path":`, `"a.go"}`},
			want:  `{"path":"a.go"}`,
		},
		{
			// Concatenating these yields {"path":{"path":"a.go"}} — still valid
			// JSON, so decodability alone cannot break the tie. The snapshot
			// evidence has to.
			name:  "snapshot framing keeps only the last frame",
			frags: []string{`{"path":`, `{"path":"a.go"}`},
			want:  `{"path":"a.go"}`,
		},
		{
			name:  "repeated identical complete snapshot",
			frags: []string{`{"path":"a.go"}`, `{"path":"a.go"}`},
			want:  `{"path":"a.go"}`,
		},
		{
			name:  "single complete frame",
			frags: []string{`{"path":"a.go"}`},
			want:  `{"path":"a.go"}`,
		},
		{
			// Only concatenation decodes here, so it wins regardless of framing.
			name:  "multi-fragment delta",
			frags: []string{`{"a":1,`, `"b":`, `2}`},
			want:  `{"a":1,"b":2}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var acc toolInputAccumulator
			for _, f := range tc.frags {
				acc.Add(f)
			}
			if got := acc.Resolve(); got != tc.want {
				t.Fatalf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Structured (non-string) arguments are always the complete value, so any
// fragments seen earlier are superseded rather than appended to.
func TestToolInputAccumulatorSnapshotDiscardsFragments(t *testing.T) {
	var acc toolInputAccumulator
	acc.Add(`{"pa`)
	acc.SetSnapshot(`{"path":"a.go"}`)

	if got := acc.Resolve(); got != `{"path":"a.go"}` {
		t.Fatalf("a structured snapshot must replace earlier fragments, got %q", got)
	}
}

// Tool arguments are always an object or an array. A bare scalar fragment is
// accepted by json.Valid but is not a complete argument set, and treating it as
// one would let a half-written call through as if it had finished.
func TestIsCompleteToolJSONRejectsScalars(t *testing.T) {
	for _, s := range []string{`"a.go"`, `42`, `true`, `null`, ``, `{"path":`} {
		if isCompleteToolJSON(s) {
			t.Fatalf("%q must not count as complete tool JSON", s)
		}
	}
	for _, s := range []string{`{"path":"a.go"}`, `[]`, `{}`} {
		if !isCompleteToolJSON(s) {
			t.Fatalf("%q must count as complete tool JSON", s)
		}
	}
}
