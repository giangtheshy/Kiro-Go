package proxy

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"kiro-go/config"
)

// filterMetadataFrame is the shape observed in production: the upstream answers
// HTTP 200, streams a metadataEvent stating it will not continue, and never emits
// assistant text. The wording is the vendor's own, which is exactly why it must
// reach the customer rather than being replaced by a proxy-authored guess.
func filterMetadataFrame(t *testing.T) []byte {
	t.Helper()
	return awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
		"filterType": "CYBER",
		"message": "The selected model cannot continue this conversation. Please select a " +
			"different model, or start a new conversation, or rewind the current " +
			"conversation to an earlier point and try a different approach.",
	})
}

// The regression this whole change exists for.
//
// The parser gained a `case "metadataEvent"` that read stopReason and dropped
// every other field. Because the case MATCHED, the frame no longer reached the
// default branch that retains unrecognised bodies — so a content-safety verdict
// was discarded twice over, and the turn surfaced as "empty stream (no output, no
// metering)". The reference build had no metadataEvent case at all, which is why
// it showed the reason and this build did not.
func TestMetadataEventReasonSurvivesParsing(t *testing.T) {
	outcome, err := parseEventStreamTracked(bytes.NewReader(filterMetadataFrame(t)), &KiroStreamCallback{})
	if err != nil {
		t.Fatalf("a metadataEvent under HTTP 200 is not a parse error: %v", err)
	}
	if outcome.Emitted {
		t.Fatal("a filtered turn emits nothing the client can see")
	}
	if outcome.MetadataPayload == "" {
		t.Fatal("the metadataEvent body was dropped — this is the bug that hid the reason")
	}
	if !strings.Contains(outcome.MetadataPayload, "CYBER") {
		t.Fatalf("the filter category must survive, got %q", outcome.MetadataPayload)
	}
	reason := describeUnknownEvents(outcome)
	if !strings.Contains(reason, "cannot continue this conversation") {
		t.Fatalf("the upstream's advice must reach the caller, got %q", reason)
	}
}

// stopReason must keep working: it is what stops a server-side cut from being
// reported to the client as a clean end_turn.
func TestMetadataEventStopReasonStillWorks(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
		"stopReason": "MAX_TOKENS",
	}))

	var seen string
	outcome, err := parseEventStreamTracked(stream, &KiroStreamCallback{
		OnStopReason: func(r string) { seen = r },
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if outcome.StopReason != "MAX_TOKENS" || seen != "MAX_TOKENS" {
		t.Fatalf("stopReason regressed: outcome=%q callback=%q", outcome.StopReason, seen)
	}
}

// The raw frame is a JSON blob. Handing that to a customer is barely better than
// handing them nothing, so the readable sentence is lifted out and the category
// kept as a prefix.
func TestFormatUpstreamRefusalReadsLikeTheVendorWroteIt(t *testing.T) {
	raw := `metadataEvent={"filterType":"CYBER","message":"The selected model cannot continue this conversation."}`

	got := formatUpstreamRefusal(raw)
	if !strings.Contains(got, "(CYBER)") {
		t.Fatalf("the category must be surfaced, got %q", got)
	}
	if !strings.Contains(got, "cannot continue this conversation") {
		t.Fatalf("the vendor's sentence must be surfaced, got %q", got)
	}
	if strings.Contains(got, "filterType") || strings.Contains(got, "{") {
		t.Fatalf("raw JSON must not reach the customer, got %q", got)
	}
}

// An unparseable or unfamiliar reason must pass through unchanged. Losing the
// detail because it did not match an expected shape would be worse than showing
// it ugly.
func TestFormatUpstreamRefusalKeepsUnrecognisedShapes(t *testing.T) {
	for _, raw := range []string{
		`someEvent={"unexpected":"layout"}`,  // no known message field
		`truncatedEvent={"message":"cut off`, // snippet cap chopped the JSON
		"bare text with no frame at all",
	} {
		if got := formatUpstreamRefusal(raw); got != raw {
			t.Fatalf("unrecognised reason %q must pass through, got %q", raw, got)
		}
	}
}

// The matcher decides retry vs no-retry, so it must fire on a real verdict.
func TestRefusalDetectionRecognisesFilterVerdicts(t *testing.T) {
	for _, reason := range []string{
		`metadataEvent={"filterType":"CYBER","message":"cannot continue"}`,
		`metadataEvent={"reason":"CONTENT_FILTERED"}`,
		`invalidStateEvent={"reason":"INVALID_STATE"}`,
		`someEvent={"message":"blocked by policy"}`,
		`someEvent={"message":"request violates the usage guidelines"}`,
	} {
		if !looksLikeUpstreamRefusal(reason) {
			t.Fatalf("must be recognised as a refusal: %q", reason)
		}
	}
}

// And it must NOT fire on the metadataEvent that ends every healthy turn.
// Otherwise the first empty stream of any cause would be labelled a refusal and
// answered 400 — turning a retryable blip into a permanent client-side failure.
func TestRefusalDetectionIgnoresOrdinaryMetadata(t *testing.T) {
	for _, reason := range []string{
		"",
		`metadataEvent={"stopReason":"END_TURN"}`,
		`metadataEvent={"conversationId":"abc-123"}`,
		`initial-response={"conversationId":"abc"}`,
		`metadataEvent={"stopReason":"MAX_TOKENS"}`,
	} {
		if looksLikeUpstreamRefusal(reason) {
			t.Fatalf("a normal turn must not look like a refusal: %q", reason)
		}
	}
}

// The client must read the upstream's own words, not the proxy's sentinel text.
// Wrapping with %w would have produced "upstream refused the request from X:
// content filtered by upstream (CYBER): …" — three "upstream"s before the
// sentence that helps.
func TestRefusalErrorLeadsWithTheUpstreamMessage(t *testing.T) {
	err := &refusalError{msg: "content filtered by upstream (CYBER): cannot continue"}

	if !strings.HasPrefix(err.Error(), "content filtered by upstream") {
		t.Fatalf("the upstream's wording must lead, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "upstream refused the request") {
		t.Fatalf("the sentinel's text must not be prefixed onto the message, got %q", err.Error())
	}
	// Classification must still work by sentinel, since there is no longer a
	// fixed phrase to match on.
	if !errors.Is(err, errKiroUpstreamRefusal) {
		t.Fatal("errors.Is must still identify a refusal")
	}
}

// 400, not 500. A 5xx tells the client the failure is transient, so it retries a
// verdict that can never change — which is what made one filtered conversation
// look like a sustained outage.
func TestRefusalAnswers400EvenWithUpstreamWording(t *testing.T) {
	err := &refusalError{msg: "content filtered by upstream (CYBER): cannot continue"}

	if got := statusForUpstreamError(err); got != http.StatusBadRequest {
		t.Fatalf("a refusal must answer 400, got %d", got)
	}
}

// A refusal must not be charged to the account. Every account asks the same
// upstream about the same conversation and gets the same answer, so counting it
// would cool down healthy accounts over one customer's filtered chat.
func TestRefusalDoesNotCoolDownAccount(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		t.Fatal("test setup produced no enabled accounts")
	}
	acc := accounts[0]

	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("expected a healthy pool of 1 before the test, got %d", got)
	}
	// Well past the three-strike cooldown threshold.
	for i := 0; i < 6; i++ {
		h.handleAccountFailure(&acc, &refusalError{msg: "content filtered by upstream (CYBER): nope"})
	}
	if got := h.pool.HealthyCount(); got != 1 {
		t.Fatalf("refusals took the account out of the pool: healthy=%d", got)
	}
}

// A refusal must be terminal for the rotation loop. Rotating costs one BILLED
// turn per account — the upstream read the conversation before declining — for a
// verdict that cannot change.
func TestRefusalIsTerminalForRotation(t *testing.T) {
	refusal := &refusalError{msg: "content filtered by upstream (CYBER): nope"}
	if !isTerminalRequestError(refusal) {
		t.Fatal("a refusal must stop the rotation loop")
	}
	// Contrast: a quota error is exactly what rotation is for.
	if isTerminalRequestError(errors.New("HTTP 429 from Kiro IDE: quota exhausted")) {
		t.Fatal("a quota error must still rotate to another account")
	}
	if isTerminalRequestError(errors.New(emptyStreamErr)) {
		t.Fatal("an unexplained empty stream must still be allowed to retry")
	}
}

// The flat-history fallback must be skipped too: it is a different payload, but
// the conversation it carries is the same one the upstream just refused.
func TestRefusalSkipsFlatHistoryRetry(t *testing.T) {
	refusal := &refusalError{msg: "content filtered by upstream (CYBER): nope"}

	// Guarded by errors.Is in callWithHistoryFallback rather than by the string
	// matchers, because the message is upstream-controlled prose that could
	// coincidentally contain a phrase those matchers look for.
	if !errors.Is(refusal, errKiroUpstreamRefusal) {
		t.Fatal("the fallback guard depends on errors.Is holding")
	}
	// A refusal whose wording happens to contain a transient-5xx phrase must
	// still not be retried — proving the guard is not string-based.
	sneaky := &refusalError{msg: "content filtered: unexpected error when processing the request"}
	if !errors.Is(sneaky, errKiroUpstreamRefusal) {
		t.Fatal("classification must not depend on the upstream's wording")
	}
}
