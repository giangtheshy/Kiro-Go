package proxy

import (
	"strings"
	"unicode/utf8"

	"kiro-go/config"
	"kiro-go/logger"
)

// When the upstream drops the connection mid-generation, the text already
// delivered stands but the turn is unfinished. Retrying the original request is
// not an option — it would replay the whole answer on top of what the client
// already has. Instead the turn is RESUMED: the delivered text is replayed back
// to the model as its own partial reply, with an instruction to continue from
// exactly where it stopped. The client sees one continuous stream and never
// learns the transport broke.
//
// This is the difference between "the proxy reports the truncation honestly"
// (which OnTruncated already does) and "the truncation does not reach the user".

// continuationInstruction is sent as the user turn of a resume request. It is
// worded to suppress the three things a model does by default when handed its own
// partial output — restart, summarize, apologize — any of which would corrupt the
// stream the client is assembling.
const continuationInstruction = "[system] Your previous message was cut off mid-stream by a transport error before you finished. " +
	"Continue that message from exactly where it stopped — resume mid-sentence if it was cut mid-sentence. " +
	"Do not repeat, restate or summarize what you already wrote, do not start over, do not apologize or mention this interruption. " +
	"Output only the remaining part of the answer. If the message was cut off before a tool call, issue that tool call now."

// maxContinuationAttempts bounds how many times one request resumes after a
// mid-stream cut. Each attempt costs a full upstream turn (billed, and it resends
// the transcript), so the ceiling is deliberately low: repeated cuts on the same
// turn mean the account or route is unhealthy, and rotating beats resuming.
const maxContinuationAttempts = 2

// minContinuationTextBytes is the least delivered text worth resuming from. Below
// this the "continue where you stopped" framing has almost no anchor, and the
// model tends to restart — worse for the client than an honest truncation.
const minContinuationTextBytes = 24

// CallKiroAPIWithContinuation calls the upstream and transparently resumes the
// turn if it is cut off mid-generation.
//
// Handlers keep their own OnTruncated as the give-up signal: it fires only after
// every resume attempt has also been cut short. A turn that completes on a resume
// looks, to the handler, exactly like one that never broke.
func CallKiroAPIWithContinuation(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if callback == nil || payload == nil || !config.GetStreamContinuation() {
		return CallKiroAPI(account, payload, callback)
	}

	var delivered strings.Builder // non-thinking text only: what a resume must not repeat
	var truncated bool

	userOnText := callback.OnText
	userOnTruncated := callback.OnTruncated
	userOnStopReason := callback.OnStopReason

	// pendingTrim holds the tail of already-delivered text that the next chunk
	// must be de-overlapped against. It is set once per resume and cleared after
	// the first chunk of that attempt, because only the seam can overlap.
	var pendingTrim string

	// The upstream's stopReason is buffered per attempt instead of being passed
	// straight through. A turn that gets cut typically reports MAX_TOKENS on the
	// attempt that broke; if a later resume finishes cleanly, forwarding that
	// stale value would label a now-complete answer as truncated. Only the final
	// attempt's verdict describes the turn the client actually receives.
	var lastStopReason string
	flushStopReason := func() {
		if userOnStopReason != nil && lastStopReason != "" {
			userOnStopReason(lastStopReason)
		}
	}

	attemptCallback := *callback
	attemptCallback.OnTruncated = func() { truncated = true }
	attemptCallback.OnStopReason = func(reason string) { lastStopReason = reason }
	attemptCallback.OnText = func(text string, isThinking bool) {
		if isThinking {
			// Thinking output is not part of the answer being resumed, so it is
			// neither recorded as delivered nor overlap-trimmed.
			if userOnText != nil {
				userOnText(text, true)
			}
			return
		}
		if pendingTrim != "" {
			text = trimResumeOverlap(pendingTrim, text)
			pendingTrim = ""
			if text == "" {
				// The chunk was entirely a repeat of what the client already has.
				return
			}
		}
		delivered.WriteString(text)
		if userOnText != nil {
			userOnText(text, false)
		}
	}

	err := CallKiroAPI(account, payload, &attemptCallback)
	if err != nil || !truncated {
		flushStopReason()
		return err
	}

	for attempt := 1; attempt <= maxContinuationAttempts; attempt++ {
		text := delivered.String()
		if len(strings.TrimSpace(text)) < minContinuationTextBytes {
			break
		}

		resumePayload := buildContinuationPayload(payload, text)
		if resumePayload == nil {
			break
		}

		logger.Warnf("[KiroAPI] Resuming a turn cut off after %d bytes (attempt %d/%d) on %s",
			len(text), attempt, maxContinuationAttempts, accountEmailForLog(account))

		truncated = false
		// Cleared so a resume that sends no metadataEvent cannot inherit the
		// previous attempt's verdict — an absent reason must read as absent, which
		// falls the handler back to local inference.
		lastStopReason = ""
		pendingTrim = overlapAnchor(text)

		// A resume failing is not the client's problem: the text already
		// delivered is still valid, so fall back to reporting the truncation
		// rather than surfacing the resume's own error.
		if err := CallKiroAPI(account, resumePayload, &attemptCallback); err != nil {
			logger.Warnf("[KiroAPI] Continuation attempt %d failed: %v", attempt, err)
			break
		}
		if !truncated {
			logger.Infof("[KiroAPI] Turn resumed successfully after %d attempt(s)", attempt)
			flushStopReason()
			return nil
		}
	}

	// Every resume was also cut short. Hand the truncation to the handler so it
	// withholds the "finished" signal, exactly as it would have without resuming.
	flushStopReason()
	if userOnTruncated != nil {
		userOnTruncated()
	}
	return nil
}

// maxOverlapAnchorBytes bounds how much of the delivered tail is compared against
// the resume's first chunk. The seam is what can overlap, so a bounded window is
// both sufficient and keeps the scan cheap.
const maxOverlapAnchorBytes = 512

// overlapAnchor returns the trailing window of delivered text, cut to a rune
// boundary so a multi-byte character is never split.
func overlapAnchor(s string) string {
	if len(s) <= maxOverlapAnchorBytes {
		return s
	}
	tail := s[len(s)-maxOverlapAnchorBytes:]
	for i := 0; i < len(tail); i++ {
		if utf8.RuneStart(tail[i]) {
			return tail[i:]
		}
	}
	return tail
}

// trimResumeOverlap removes from the front of next any prefix that duplicates the
// tail of prev, which happens when the model repeats a few words before carrying
// on. The longest overlap wins, and only rune-aligned, valid-UTF-8 cuts are
// considered so a Vietnamese or CJK character is never sliced in half.
func trimResumeOverlap(prev, next string) string {
	if prev == "" || next == "" {
		return next
	}
	max := len(prev)
	if len(next) < max {
		max = len(next)
	}
	for k := max; k > 0; k-- {
		if k < len(next) && !utf8.RuneStart(next[k]) {
			continue
		}
		if !utf8.ValidString(next[:k]) {
			continue
		}
		if strings.HasSuffix(prev, next[:k]) {
			return next[k:]
		}
	}
	return next
}

// buildContinuationPayload returns a copy of payload with the delivered text
// appended as the assistant's partial reply and the resume instruction as the new
// current message.
//
// The original payload is never mutated: the caller's retry loop reuses it across
// accounts, so appending in place would compound a resume turn onto every
// subsequent attempt.
func buildContinuationPayload(payload *KiroPayload, delivered string) *KiroPayload {
	if payload == nil {
		return nil
	}

	resume := *payload
	cs := &resume.ConversationState

	// The interrupted exchange becomes history: the user turn that prompted it,
	// then the assistant's partial answer.
	history := make([]KiroHistoryMessage, 0, len(payload.ConversationState.History)+2)
	history = append(history, payload.ConversationState.History...)

	interrupted := payload.ConversationState.CurrentMessage.UserInputMessage

	// Capture the tool specs BEFORE the context is cleared below. The resume turn
	// still needs them so the model can issue the call it was cut off before, and
	// reading them after the clear would silently hand it an empty toolset.
	var resumeTools []KiroToolWrapper
	if ctx := interrupted.UserInputMessageContext; ctx != nil {
		resumeTools = ctx.Tools
	}

	// Moving the interrupted turn into history must not carry its structured tool
	// blocks with it. Upstream accepts at most ONE active tool turn — the last
	// history assistant's toolUses answered by the CURRENT message's toolResults —
	// and the resume turn's assistant (the partial answer) has no toolUses at all.
	// Leaving the results structured would present an unanswered tool turn and get
	// the whole request rejected. sanitizeKiroHistory solves this for the normal
	// path by narrating results into the message text; do the same here so the
	// model still sees the tool output it was reasoning over when it was cut off.
	if ctx := interrupted.UserInputMessageContext; ctx != nil && len(ctx.ToolResults) > 0 {
		names := make(map[string]string, len(ctx.ToolResults))
		for _, h := range payload.ConversationState.History {
			if h.AssistantResponseMessage == nil {
				continue
			}
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				names[tu.ToolUseID] = tu.Name
			}
		}
		interrupted.Content = joinHistoryText(interrupted.Content, narrateToolResults(ctx.ToolResults, names, nil))
	}
	// History never carries tool specs or results, only prose.
	interrupted.UserInputMessageContext = nil

	history = append(history,
		KiroHistoryMessage{UserInputMessage: &interrupted},
		KiroHistoryMessage{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: delivered}},
	)
	cs.History = history

	// The resume turn carries the instruction and nothing else. Tool specs are
	// preserved (captured above) so the model can still issue the call it was cut
	// off before, but toolResults are not re-sent: they now live in history as
	// narrated text, and sending them again would open a second active tool turn,
	// which the upstream rejects.
	resumeMsg := KiroUserInputMessage{
		Content: continuationInstruction,
		ModelID: interrupted.ModelID,
		Origin:  interrupted.Origin,
	}
	if len(resumeTools) > 0 {
		resumeMsg.UserInputMessageContext = &UserInputMessageContext{Tools: resumeTools}
	}
	cs.CurrentMessage.UserInputMessage = resumeMsg

	return &resume
}
