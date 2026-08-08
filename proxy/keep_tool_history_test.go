package proxy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Kiro's payload has no system-prompt field, so the model's only evidence that
// it issues structured tool calls is the transcript it is handed back. When
// every past assistant tool turn is flattened to prose, the in-context pattern
// becomes "describe the step, results appear in the user turn" — and the model
// imitates that by summarizing and ending the turn instead of calling the next
// tool. These tests pin that correctly-paired cycles keep their structure.

// buildAgenticLoop returns a Claude request representing `steps` completed
// tool cycles (assistant tool_use → user tool_result) plus a trailing cycle
// awaiting its result, which is what an agentic client sends mid-task.
func buildAgenticLoop(steps int) *ClaudeRequest {
	msgs := []ClaudeMessage{{Role: "user", Content: "fix the failing test"}}
	for i := 0; i < steps; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": id, "name": "Bash",
					"input": map[string]interface{}{"command": fmt.Sprintf("step %d", i)}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": id,
					"content": fmt.Sprintf("output of step %d", i)},
			}},
		)
	}
	return &ClaudeRequest{
		Model:    "claude-opus-4.8",
		Tools:    []ClaudeTool{{Name: "Bash", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: msgs,
	}
}

// countStructuredToolUses reports how many history assistant turns still carry
// structured toolUses — i.e. how many of its own tool calls the model can see.
func countStructuredToolUses(payload *KiroPayload) int {
	n := 0
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			n += len(a.ToolUses)
		}
	}
	return n
}

// The regression this whole change exists for: with keepPaired off, a six-step
// loop shows the model just ONE of its six tool calls. It degrades with session
// length, which is why a long task stops mid-way while a short one is fine.
func TestKeepPairedPreservesEveryToolCycle(t *testing.T) {
	req := buildAgenticLoop(6)

	safe := ClaudeToKiroWithHistoryMode(req, false, false)
	rich := ClaudeToKiroWithHistoryMode(req, false, true)

	safeCount := countStructuredToolUses(safe)
	richCount := countStructuredToolUses(rich)

	if safeCount != 1 {
		t.Fatalf("flattened mode should keep exactly the active turn, got %d", safeCount)
	}
	if richCount != 6 {
		t.Fatalf("keepPaired must preserve all six cycles, got %d", richCount)
	}
}

// Flattening a cycle drops the assistant turn entirely (it is left hollow and
// removed), which is what produces runs of consecutive user turns. Keeping the
// pairs structured must keep the alternation intact.
func TestKeepPairedKeepsTurnAlternation(t *testing.T) {
	rich := ClaudeToKiroWithHistoryMode(buildAgenticLoop(4), false, true)

	var consecutiveUsers int
	prevWasUser := false
	for _, h := range rich.ConversationState.History {
		isUser := h.UserInputMessage != nil
		if isUser && prevWasUser {
			consecutiveUsers++
		}
		prevWasUser = isUser
	}
	if consecutiveUsers > 0 {
		t.Fatalf("keepPaired history has %d consecutive user turns; assistant turns were dropped", consecutiveUsers)
	}
}

// A user turn keeping structured toolResults must not ALSO carry the narrated
// text, or the model sees every tool output twice.
func TestKeepPairedDoesNotDoubleReportToolOutput(t *testing.T) {
	rich := ClaudeToKiroWithHistoryMode(buildAgenticLoop(3), false, true)

	for i, h := range rich.ConversationState.History {
		u := h.UserInputMessage
		if u == nil || u.UserInputMessageContext == nil {
			continue
		}
		if len(u.UserInputMessageContext.ToolResults) == 0 {
			continue
		}
		if strings.Contains(u.Content, toolResultsContinuationPrefix) {
			t.Fatalf("history[%d] keeps structured toolResults AND narrates them: %q", i, u.Content)
		}
	}
}

// History must never carry tool SPECS, in either mode: the specs belong on the
// current message only, and duplicating them inflates every turn.
func TestKeepPairedNeverLeavesToolSpecsInHistory(t *testing.T) {
	for _, keepPaired := range []bool{false, true} {
		rich := ClaudeToKiroWithHistoryMode(buildAgenticLoop(3), false, keepPaired)
		for i, h := range rich.ConversationState.History {
			u := h.UserInputMessage
			if u == nil || u.UserInputMessageContext == nil {
				continue
			}
			if len(u.UserInputMessageContext.Tools) > 0 {
				t.Fatalf("keepPaired=%v history[%d] carries %d tool specs",
					keepPaired, i, len(u.UserInputMessageContext.Tools))
			}
		}
	}
}

// A cycle whose results only PARTIALLY answer the assistant's tool uses is not
// a self-contained pair; upstream rejects it, so it must still be flattened.
func TestKeepPairedFlattensPartiallyAnsweredCycle(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{{Name: "Bash", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "do two things"},
			// Two parallel calls...
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "a1", "name": "Bash", "input": map[string]interface{}{}},
				map[string]interface{}{"type": "tool_use", "id": "a2", "name": "Bash", "input": map[string]interface{}{}},
			}},
			// ...but only one result.
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "a1", "content": "only one answered"},
			}},
			{Role: "user", Content: "carry on"},
		},
	}

	rich := ClaudeToKiroWithHistoryMode(req, false, true)
	if n := countStructuredToolUses(rich); n != 0 {
		t.Fatalf("a half-answered cycle must be flattened, got %d structured toolUses", n)
	}
}

// The OpenAI path shares sanitizeKiroHistory, so it must gain the same benefit.
func TestKeepPairedAppliesToOpenAIPath(t *testing.T) {
	msgs := []OpenAIMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", ToolCalls: []ToolCall{
				newPollToolCall(id, "exec_command", `{"cmd":"x"}`),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: id, Content: fmt.Sprintf("out %d", i)},
		)
	}
	req := &OpenAIRequest{
		Model:    "claude-opus-4.8",
		Tools:    []OpenAITool{{Type: "function"}},
		Messages: msgs,
	}
	req.Tools[0].Function.Name = "exec_command"
	req.Tools[0].Function.Parameters = map[string]interface{}{"type": "object"}

	safe := OpenAIToKiroWithHistoryMode(req, false, false)
	rich := OpenAIToKiroWithHistoryMode(req, false, true)

	if countStructuredToolUses(rich) <= countStructuredToolUses(safe) {
		t.Fatalf("keepPaired must preserve more cycles on the OpenAI path (safe=%d rich=%d)",
			countStructuredToolUses(safe), countStructuredToolUses(rich))
	}
}

// The default entry points must stay on the flattened form, so a caller that has
// not opted in cannot accidentally send the richer payload without a fallback.
func TestDefaultEntryPointsStayFlattened(t *testing.T) {
	if n := countStructuredToolUses(ClaudeToKiro(buildAgenticLoop(4), false)); n != 1 {
		t.Fatalf("ClaudeToKiro must default to flattened history, got %d", n)
	}
}

// Truncation can cut between a kept assistant turn and its results, leaving a
// tool_result with no matching tool_use. Upstream answers that with
// TOOL_USE_RESULT_MISMATCH, so the orphan has to be flattened back to text.
func TestRepairOrphanedStructuredToolPairsFlattensOrphan(t *testing.T) {
	history := []KiroHistoryMessage{
		// The assistant turn that issued "t1" was cut away by truncation.
		{UserInputMessage: &KiroUserInputMessage{
			Content: "",
			UserInputMessageContext: &UserInputMessageContext{
				ToolResults: []KiroToolResult{{
					ToolUseID: "t1",
					Content:   []KiroResultContent{{Text: "SENTINEL_ORPHAN_OUTPUT"}},
				}},
			},
		}},
	}

	repaired := repairOrphanedStructuredToolPairs(history)

	u := repaired[0].UserInputMessage
	if u.UserInputMessageContext != nil && len(u.UserInputMessageContext.ToolResults) > 0 {
		t.Fatal("an orphaned tool result must not stay structured")
	}
	if !strings.Contains(u.Content, "SENTINEL_ORPHAN_OUTPUT") {
		t.Fatalf("orphan output must survive as narrated text, got %q", u.Content)
	}
}

// The mirror case: a correctly paired cycle must survive the repair pass
// untouched, or the repair would undo keepPaired entirely.
func TestRepairOrphanedStructuredToolPairsKeepsValidPair(t *testing.T) {
	history := []KiroHistoryMessage{
		{AssistantResponseMessage: &KiroAssistantResponseMessage{
			ToolUses: []KiroToolUse{{ToolUseID: "t1", Name: "Bash"}},
		}},
		{UserInputMessage: &KiroUserInputMessage{
			UserInputMessageContext: &UserInputMessageContext{
				ToolResults: []KiroToolResult{{
					ToolUseID: "t1",
					Content:   []KiroResultContent{{Text: "ok"}},
				}},
			},
		}},
	}

	repaired := repairOrphanedStructuredToolPairs(history)

	ctx := repaired[1].UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 {
		t.Fatal("a correctly paired tool result must stay structured")
	}
}

// callWithHistoryFallbackTestable mirrors callWithHistoryFallback's control flow
// with an injectable call func. The production function calls the upstream
// directly, so the decision logic is exercised here in isolation.
func callWithHistoryFallbackTestable(rich, safe *KiroPayload, started func() bool, call func(*KiroPayload) error) error {
	err := call(rich)
	if err == nil {
		return nil
	}
	if rich == safe || !shouldRetrySafePayload(err.Error()) || started() {
		return err
	}
	return call(safe)
}

func TestHistoryFallbackRetriesFlattenedOnMalformedPayload(t *testing.T) {
	rich, safe := &KiroPayload{}, &KiroPayload{}
	var seen []*KiroPayload

	err := callWithHistoryFallbackTestable(rich, safe, func() bool { return false },
		func(p *KiroPayload) error {
			seen = append(seen, p)
			if len(seen) == 1 {
				return errors.New("HTTP 400: TOOL_USE_RESULT_MISMATCH")
			}
			return nil
		})

	if err != nil {
		t.Fatalf("the safe retry should have recovered the request: %v", err)
	}
	if len(seen) != 2 || seen[0] != rich || seen[1] != safe {
		t.Fatalf("expected rich then safe, got %d attempts", len(seen))
	}
}

// Once bytes are on the wire a second attempt would append a duplicate answer,
// so the original error must surface instead.
func TestHistoryFallbackSkippedAfterStreamStarted(t *testing.T) {
	rich, safe := &KiroPayload{}, &KiroPayload{}
	attempts := 0

	err := callWithHistoryFallbackTestable(rich, safe, func() bool { return true },
		func(p *KiroPayload) error {
			attempts++
			return errors.New("HTTP 400: TOOL_USE_RESULT_MISMATCH")
		})

	if attempts != 1 {
		t.Fatalf("a committed stream must not be retried, got %d attempts", attempts)
	}
	if err == nil {
		t.Fatal("expected the original error to surface")
	}
}

// An error a simpler payload cannot fix (quota, auth) must not burn a retry.
func TestHistoryFallbackSkippedOnUnrelatedError(t *testing.T) {
	rich, safe := &KiroPayload{}, &KiroPayload{}
	attempts := 0

	_ = callWithHistoryFallbackTestable(rich, safe, func() bool { return false },
		func(p *KiroPayload) error {
			attempts++
			return errors.New("HTTP 429: too many requests")
		})

	if attempts != 1 {
		t.Fatalf("a quota error is not payload-shaped; expected 1 attempt, got %d", attempts)
	}
}

// With KeepToolHistory off both payloads are the same pointer, so there is
// nothing simpler to fall back to.
func TestHistoryFallbackSkippedWhenPayloadsIdentical(t *testing.T) {
	same := &KiroPayload{}
	attempts := 0

	_ = callWithHistoryFallbackTestable(same, same, func() bool { return false },
		func(p *KiroPayload) error {
			attempts++
			return errors.New("HTTP 400: improperly formed request")
		})

	if attempts != 1 {
		t.Fatalf("identical payloads leave nothing to retry, got %d attempts", attempts)
	}
}

func TestShouldRetrySafePayloadClassification(t *testing.T) {
	retry := []string{
		"HTTP 400: TOOL_USE_RESULT_MISMATCH",
		"HTTP 400: tool_config_missing",
		"HTTP 400: improperly formed request",
		"HTTP 400: content has no matching tool_use",
		"ValidationException: bad shape",
		"HTTP 500: Encountered an unexpected error when processing the request",
	}
	for _, msg := range retry {
		if !shouldRetrySafePayload(msg) {
			t.Errorf("expected a safe-payload retry for %q", msg)
		}
	}

	keep := []string{
		"HTTP 429: throttled",
		"HTTP 403: forbidden",
		"HTTP 400: some unrelated validation detail",
		"context canceled",
	}
	for _, msg := range keep {
		if shouldRetrySafePayload(msg) {
			t.Errorf("must NOT retry the flattened payload for %q", msg)
		}
	}
}
