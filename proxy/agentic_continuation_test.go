package proxy

import (
	"strings"
	"testing"
)

// Kiro's payload has no system-prompt field, so an agentic client's "keep going
// until the task is done" instructions get demoted into buried history and lose
// their steering weight. The model then reads a tool result, writes a summary,
// and ends the turn — a clean end_turn that the transport layer cannot detect,
// because nothing actually broke. These tests pin that the steer is re-anchored
// onto exactly the turns where it applies, and nowhere else.

// exactly one active tool turn: assistant calls, final user message answers.
func toolResultRequest() *ClaudeRequest {
	return &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{{Name: "exec_command", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run the tests"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t1", "name": "exec_command", "input": map[string]interface{}{"cmd": "go test"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "2 failures"},
			}},
		},
	}
}

func TestAgenticContinuationAppendedOnToolResultTurn(t *testing.T) {
	payload := ClaudeToKiro(toolResultRequest(), false)

	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(got, agenticContinuationDirective) {
		t.Fatalf("a tool-results turn must carry the keep-working steer, got:\n%q", got)
	}
	// The steer is additive: the tool output the model has to act on must survive.
	if !strings.Contains(got, "2 failures") {
		t.Fatalf("the tool result itself must still reach the model, got:\n%q", got)
	}
}

// An ordinary chat turn has no next tool step to take, so the steer would be
// noise on every single message.
func TestAgenticContinuationSkippedOnPlainChatTurn(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		Tools:    []ClaudeTool{{Name: "exec_command", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{{Role: "user", Content: "what does this repo do?"}},
	}

	got := ClaudeToKiro(req, false).ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Contains(got, agenticContinuationDirective) {
		t.Fatalf("a plain chat turn must not carry the steer, got:\n%q", got)
	}
	if got != "what does this repo do?" {
		t.Fatalf("a plain chat turn must be passed through unchanged, got:\n%q", got)
	}
}

// With no tools left to call, telling the model to "call the appropriate tools"
// asks for something it cannot do.
func TestAgenticContinuationSkippedWhenNoToolsAvailable(t *testing.T) {
	req := toolResultRequest()
	req.Tools = nil

	got := ClaudeToKiro(req, false).ConversationState.CurrentMessage.UserInputMessage.Content
	if strings.Contains(got, agenticContinuationDirective) {
		t.Fatalf("without callable tools the steer must be withheld, got:\n%q", got)
	}
}

func TestAgenticContinuationAppliedOnOpenAIPath(t *testing.T) {
	// OpenAITool.Function is an anonymous struct, so it is filled after literal
	// construction rather than named inline.
	tool := OpenAITool{Type: "function"}
	tool.Function.Name = "exec_command"
	tool.Function.Parameters = map[string]interface{}{"type": "object"}

	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Tools: []OpenAITool{tool},
		Messages: []OpenAIMessage{
			{Role: "user", Content: "run the tests"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_1", Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "exec_command", Arguments: `{"cmd":"go test"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "2 failures"},
		},
	}

	got := OpenAIToKiro(req, false).ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(got, agenticContinuationDirective) {
		t.Fatalf("the OpenAI path needs the same steer, got:\n%q", got)
	}
}

// The directive must permit stopping, or a model that has genuinely finished
// would be pushed into inventing further tool calls.
func TestAgenticContinuationDirectiveAllowsStopping(t *testing.T) {
	if !strings.Contains(agenticContinuationDirective, "fully complete") {
		t.Fatalf("the steer must leave an explicit way to stop, got:\n%q", agenticContinuationDirective)
	}
}

func TestApplyAgenticContinuationGuards(t *testing.T) {
	cases := []struct {
		name                        string
		hasToolResults, toolsWillGo bool
		want                        bool
	}{
		{"tool results with tools available", true, true, true},
		{"tool results but no tools left", true, false, false},
		{"tools available but no tool results", false, true, false},
		{"neither", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyAgenticContinuation("body", tc.hasToolResults, tc.toolsWillGo)
			if appended := got != "body"; appended != tc.want {
				t.Fatalf("appended=%v, want %v (got %q)", appended, tc.want, got)
			}
		})
	}
}
