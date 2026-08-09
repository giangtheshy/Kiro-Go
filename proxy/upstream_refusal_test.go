package proxy

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// An unrecognised event frame carries the upstream's own reason for producing no
// output. Before this it was dropped by the dispatch switch, and the turn was
// reported as an unexplained "empty stream (no output, no metering)" — the
// explanation arrived and was thrown away.
func TestParseEventStreamRetainsUnknownEventPayload(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "invalidStateEvent", map[string]interface{}{
		"reason":  "INVALID_STATE",
		"message": "Input contains content that is not allowed.",
	}))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("an event frame under HTTP 200 is not a parse failure: %v", err)
	}
	if outcome.Emitted || outcome.Metered {
		t.Fatal("an invalidStateEvent delivers neither output nor billing")
	}
	if len(outcome.UnknownEvents) != 1 || outcome.UnknownEvents[0] != "invalidStateEvent" {
		t.Fatalf("the event type must be recorded, got %v", outcome.UnknownEvents)
	}
	if !strings.Contains(describeUnknownEvents(outcome), "not allowed") {
		t.Fatalf("the upstream's stated reason must survive, got %q", describeUnknownEvents(outcome))
	}
}

// initial-response opens every stream. Retaining its body would put noise in
// front of the informative frame in every customer-facing message, so its name
// is recorded but its payload is not.
func TestParseEventStreamIgnoresInitialResponseBody(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "initial-response", map[string]interface{}{
		"conversationId": "abc-123",
	}))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(outcome.UnknownEvents) != 1 {
		t.Fatalf("the frame should still be counted, got %v", outcome.UnknownEvents)
	}
	if got := describeUnknownEvents(outcome); got != "" {
		t.Fatalf("a structural frame must not be reported as a reason, got %q", got)
	}
}

// A stream that opened normally and then stated a refusal must report the
// refusal, not the structural frame that preceded it.
func TestParseEventStreamPrefersReasonOverStructuralFrame(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "initial-response", map[string]interface{}{"conversationId": "abc"}),
		awsEventStreamFrame(t, "someFutureRefusalEvent", map[string]interface{}{
			"message": "request blocked by policy",
		}),
	}, nil))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reason := describeUnknownEvents(outcome)
	if !strings.Contains(reason, "blocked by policy") {
		t.Fatalf("the reason frame must be reported, got %q", reason)
	}
	if strings.Contains(reason, "conversationId") {
		t.Fatalf("the structural frame must not appear in the reason, got %q", reason)
	}
}

// A frame body long enough to flood a log line is truncated, but the reason at
// its front — the part that matters — survives.
func TestParseEventStreamCapsUnknownPayloadSnippet(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "verboseRefusalEvent", map[string]interface{}{
		"message": "denied because " + strings.Repeat("x", 4000),
	}))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	reason := describeUnknownEvents(outcome)
	if !strings.Contains(reason, "denied because") {
		t.Fatalf("the front of the reason must survive truncation, got %q", reason)
	}
	// The label and "=" separator sit in front of the capped snippet.
	if len(reason) > maxUnknownPayloadSnippet+len("verboseRefusalEvent=") {
		t.Fatalf("snippet was not capped: %d bytes", len(reason))
	}
}

// A pathological stream of unrecognised frames must not be able to grow the
// retained set without bound.
func TestParseEventStreamBoundsRetainedUnknownFrames(t *testing.T) {
	var frames [][]byte
	for i := 0; i < maxTrackedUnknownEvents*4; i++ {
		frames = append(frames, awsEventStreamFrame(t, "noiseEvent", map[string]interface{}{"n": i}))
	}

	outcome, err := parseEventStreamTracked(bytes.NewReader(bytes.Join(frames, nil)), &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(outcome.UnknownEvents) > maxTrackedUnknownEvents {
		t.Fatalf("retained %d event names, cap is %d", len(outcome.UnknownEvents), maxTrackedUnknownEvents)
	}
	if len(outcome.UnknownPayloads) > maxTrackedUnknownEvents {
		t.Fatalf("retained %d payloads, cap is %d", len(outcome.UnknownPayloads), maxTrackedUnknownEvents)
	}
}

// Recognised frames must not be diverted into the unknown set — that would
// report a perfectly normal turn as carrying an unexplained reason.
func TestParseEventStreamKnownEventsAreNotTrackedAsUnknown(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.0}),
	}, nil))

	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnText: func(string, bool) {},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(outcome.UnknownEvents) != 0 {
		t.Fatalf("known events leaked into the unknown set: %v", outcome.UnknownEvents)
	}
	if !outcome.Emitted || !outcome.Metered {
		t.Fatal("a normal turn must still report as emitted and metered")
	}
}

// countingReader is what lets an empty stream say whether upstream sent zero
// bytes or sent frames that carried nothing usable. Those have different causes.
func TestCountingReaderTalliesBytes(t *testing.T) {
	frame := awsEventStreamFrame(t, "initial-response", map[string]interface{}{"a": "b"})
	counter := &countingReader{r: bytes.NewReader(frame)}

	if _, err := parseEventStreamTracked(counter, &KiroStreamCallback{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if counter.n != int64(len(frame)) {
		t.Fatalf("counted %d bytes, stream carried %d", counter.n, len(frame))
	}
}

// An in-band refusal must be treated exactly like every other refusal: a verdict
// on the payload, never charged to the account that happened to serve it.
// Without this one refused conversation walks the whole pool into cooldown.
func TestInBandRefusalIsRecognisedAsRefusal(t *testing.T) {
	msg := errKiroUpstreamRefusal.Error() + " from CodeWhisperer: invalidStateEvent={\"reason\":\"INVALID_STATE\"}"

	if !isRefusalErrorMessage(msg) {
		t.Fatal("an in-band refusal must be classified as a refusal")
	}
	// Must not be mistaken for the unexplained empty stream: that one is
	// retried, this one is not.
	if isEmptyStreamErrorMessage(msg) {
		t.Fatal("a stated refusal must not be treated as an empty stream")
	}
	if shouldRetrySafePayload(msg) {
		t.Fatal("a stated refusal must not trigger the flat-history retry: the verdict is the same every time")
	}
}

// A refusal answered 5xx is retried by clients, which is how one refused
// conversation becomes an endless run of identical failures that looks like an
// outage. 400 says the request itself is the problem.
func TestRefusalAnswersBadRequestNotServerError(t *testing.T) {
	err := errKiroUpstreamRefusal
	if got := statusForUpstreamError(err); got != http.StatusBadRequest {
		t.Fatalf("a refusal must answer 400, got %d", got)
	}
}

// The matcher must stay narrow. An unexplained empty stream is still retryable
// and must keep reaching the flat-history fallback.
func TestEmptyStreamIsNotClassifiedAsRefusal(t *testing.T) {
	msg := "empty stream from CodeWhisperer (no output, no metering)"

	if isRefusalErrorMessage(msg) {
		t.Fatal("an unexplained empty stream is not a stated refusal")
	}
	if !shouldRetrySafePayload(msg) {
		t.Fatal("an empty stream must still reach the flat-history retry")
	}
}

// describePayloadShape is the only lead available when the upstream states no
// reason: it shows whether the bytes are spread across history or concentrated
// in one oversized tool result.
func TestDescribePayloadShapeLocatesOversizedToolResult(t *testing.T) {
	payload := &KiroPayload{}
	cur := &payload.ConversationState.CurrentMessage.UserInputMessage
	cur.Content = "go on"
	cur.UserInputMessageContext = &UserInputMessageContext{
		ToolResults: []KiroToolResult{
			{ToolUseID: "t1", Content: []KiroResultContent{{Text: strings.Repeat("y", 5000)}}},
			{ToolUseID: "t2", Content: []KiroResultContent{{Text: "small"}}},
		},
	}

	shape := describePayloadShape(payload)
	if !strings.Contains(shape, "toolResults=2/") {
		t.Fatalf("tool result count must be reported, got %q", shape)
	}
	if !strings.Contains(shape, "maxResult=5000B") {
		t.Fatalf("the outlier tool result must be visible, got %q", shape)
	}
}

// A nil payload reaches this from an error path, so it must not panic there.
func TestDescribePayloadShapeHandlesNil(t *testing.T) {
	if got := describePayloadShape(nil); got != "nil" {
		t.Fatalf("expected %q, got %q", "nil", got)
	}
}
