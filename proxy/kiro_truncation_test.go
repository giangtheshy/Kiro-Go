package proxy

import (
	"bytes"
	"errors"
	"testing"
)

// The tests below cover the three endings a caller cannot otherwise tell apart:
// a stream that delivered nothing, one cut off mid-turn, and one that completed.
// See StreamOutcome for why the distinction matters.

func TestParseEventStreamOutcomeEmptyBody(t *testing.T) {
	outcome, err := parseEventStreamTracked(bytes.NewReader(nil), &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	// An empty body is not a parse failure — it is a turn that never started.
	if err != nil {
		t.Fatalf("empty body should not be a parse error, got %v", err)
	}
	if outcome.Emitted || outcome.Metered {
		t.Fatalf("expected nothing emitted or metered, got %+v", outcome)
	}
}

func TestParseEventStreamOutcomeCutAtFrameBoundary(t *testing.T) {
	// Content arrived, then the connection closed cleanly on a frame boundary
	// with no meteringEvent. This is the silent case: it looks like success.
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "half an ans",
	}))

	var got string
	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(text string, _ bool) { got += text },
	})
	if err != nil {
		t.Fatalf("a clean close on a frame boundary is not an error, got %v", err)
	}
	if got != "half an ans" {
		t.Fatalf("text delivered before the cut must stand, got %q", got)
	}
	if !outcome.Emitted {
		t.Fatal("expected Emitted: the client already saw this text, so a retry would duplicate it")
	}
	if outcome.Metered {
		t.Fatal("expected Metered=false: the upstream never billed this turn")
	}
}

func TestParseEventStreamOutcomeMeteredCountsAsComplete(t *testing.T) {
	var body []byte
	body = append(body, awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "done",
	})...)
	body = append(body, awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage": 2.5,
	})...)

	var credits float64
	outcome, err := parseEventStreamTracked(bytes.NewReader(body), &KiroStreamCallback{
		OnText:    func(string, bool) {},
		OnCredits: func(c float64) { credits = c },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !outcome.Emitted || !outcome.Metered {
		t.Fatalf("expected a complete turn, got %+v", outcome)
	}
	if credits != 2.5 {
		t.Fatalf("expected credits forwarded, got %v", credits)
	}
}

// A metered turn that produced no content must NOT be retried: the upstream has
// already billed it, so resending would pay twice for the same turn.
func TestParseEventStreamOutcomeMeteredWithoutContent(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage": 1.0,
	}))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !outcome.Metered {
		t.Fatal("expected Metered so the caller does not retry an already-billed turn")
	}
	if outcome.Emitted {
		t.Fatal("expected Emitted=false: no content reached the client")
	}
}

// Reasoning text is visible to the client, so it counts as emitted — retrying
// after it would replay thinking output the user already saw.
func TestParseEventStreamOutcomeReasoningCountsAsEmitted(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{
		"text": "thinking out loud",
	}))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !outcome.Emitted {
		t.Fatal("expected reasoning text to count as emitted")
	}
}

func TestParseEventStreamOutcomeToolUseCountsAsEmitted(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "run_diagnosis",
		"input":     `{"target":"foo"}`,
		"stop":      true,
	}))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnToolUse: func(KiroToolUse) {},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !outcome.Emitted {
		t.Fatal("expected a forwarded tool use to count as emitted")
	}
}

// A tool call cut mid-argument must surface as an error. Forwarding it with an
// empty input would make the client execute the tool with parameters the model
// never finished writing.
func TestParseEventStreamRejectsIncompleteToolInput(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "delete_files",
		"input":     `{"path":"/impor`,
	}))

	var forwarded []KiroToolUse
	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnToolUse: func(tu KiroToolUse) { forwarded = append(forwarded, tu) },
	})
	if !errors.Is(err, errIncompleteKiroToolInput) {
		t.Fatalf("expected errIncompleteKiroToolInput, got %v", err)
	}
	if len(forwarded) != 0 {
		t.Fatalf("a tool call with unparseable arguments must never be forwarded, got %d", len(forwarded))
	}
	if outcome.Emitted {
		t.Fatal("expected Emitted=false so the caller may safely retry")
	}
}

// The validation above must not depend on OnToolUse being set. When the JSON
// check sat behind that guard, any handler leaving the callback nil silently
// skipped validation and a truncated call passed for a clean turn.
func TestIncompleteToolInputDetectedWithoutOnToolUseCallback(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "delete_files",
		"input":     `{"path":"/impor`,
	}))

	_, err := parseEventStreamTracked(stream, &KiroStreamCallback{OnText: func(string, bool) {}})
	if !errors.Is(err, errIncompleteKiroToolInput) {
		t.Fatalf("expected validation to run without OnToolUse set, got %v", err)
	}
}

func TestParseEventStreamNilCallbackStillReportsOutcome(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage": 1.0,
	}))

	outcome, err := parseEventStreamTracked(stream, nil)
	if err != nil {
		t.Fatalf("nil callback must be a no-op, got %v", err)
	}
	if !outcome.Metered {
		t.Fatal("expected metering to be tracked even with a nil callback")
	}
}
