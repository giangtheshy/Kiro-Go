package proxy

import (
	"strings"
	"testing"
)

// Kiro's ConversationState has no native tool_choice / toolConfig field, so
// the client's intent is emulated by appending a directive to the current
// message and, for "none", dropping the tool specs entirely.

func toolChoiceTools() []ClaudeTool {
	return []ClaudeTool{
		{Name: "Bash", Description: "run a shell command", InputSchema: map[string]interface{}{"type": "object"}},
		{Name: "Read", Description: "read a file", InputSchema: map[string]interface{}{"type": "object"}},
	}
}

// "auto" is a no-op: the model receives the tools and decides whether to call.
func TestToolChoiceAutoIsNoOp(t *testing.T) {
	req := &ClaudeRequest{
		Model:      "claude-opus-4.8",
		Tools:      toolChoiceTools(),
		ToolChoice: map[string]interface{}{"type": "auto"},
		Messages:   []ClaudeMessage{{Role: "user", Content: "help me"}},
	}
	p := ClaudeToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) == 0 {
		t.Fatal("auto: tools must still be sent")
	}
	if strings.Contains(cur.Content, "must") || strings.Contains(cur.Content, "Do not call") {
		t.Fatalf("auto: no forcing directive expected, got %q", cur.Content)
	}
}

// "any" appends a directive that compels a tool call; tools remain on the wire.
func TestToolChoiceAnyAppendsDirective(t *testing.T) {
	req := &ClaudeRequest{
		Model:      "claude-opus-4.8",
		Tools:      toolChoiceTools(),
		ToolChoice: map[string]interface{}{"type": "any"},
		Messages:   []ClaudeMessage{{Role: "user", Content: "help me"}},
	}
	p := ClaudeToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "must respond by calling one of the available tools") {
		t.Fatalf("any: expected forcing directive, got %q", cur.Content)
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) == 0 {
		t.Fatal("any: tools must remain on the wire")
	}
}

// "none" drops the tool specs and appends a "no tools" directive.
func TestToolChoiceNoneDropsTools(t *testing.T) {
	req := &ClaudeRequest{
		Model:      "claude-opus-4.8",
		Tools:      toolChoiceTools(),
		ToolChoice: map[string]interface{}{"type": "none"},
		Messages:   []ClaudeMessage{{Role: "user", Content: "help me"}},
	}
	p := ClaudeToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.Tools) > 0 {
		t.Fatalf("none: tools must be dropped, got %d", len(cur.UserInputMessageContext.Tools))
	}
	if !strings.Contains(cur.Content, "Do not call any tools") {
		t.Fatalf("none: expected 'no tools' directive, got %q", cur.Content)
	}
}

// "tool" with a specific name appends a directive naming that exact tool.
func TestToolChoiceSpecificNamesTheTool(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: toolChoiceTools(),
		ToolChoice: map[string]interface{}{
			"type": "tool",
			"name": "Bash",
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "run something"}},
	}
	p := ClaudeToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "bash") {
		t.Fatalf("specific: expected wire tool name in directive, got %q", cur.Content)
	}
	if !strings.Contains(cur.Content, "must respond by calling the") {
		t.Fatalf("specific: expected forcing directive, got %q", cur.Content)
	}
}

// tool_choice has no effect when no tools are declared — the guards must
// prevent a "call the available tools" directive with an empty toolset.
func TestToolChoiceWithNoToolsIsNoOp(t *testing.T) {
	req := &ClaudeRequest{
		Model:      "claude-opus-4.8",
		ToolChoice: map[string]interface{}{"type": "any"},
		Messages:   []ClaudeMessage{{Role: "user", Content: "help"}},
	}
	p := ClaudeToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if strings.Contains(cur.Content, "must respond by calling") {
		t.Fatalf("no-tools path must not inject a forcing directive, got %q", cur.Content)
	}
}

// "none" must also flatten tool history so the model does not see in-context
// evidence of earlier tool calls from the same session.
func TestToolChoiceNoneFlattensHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: toolChoiceTools(),
		ToolChoice: map[string]interface{}{"type": "none"},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run the tests"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]interface{}{"cmd": "go test"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "tests pass"},
			}},
			{Role: "user", Content: "summarize"},
		},
	}
	p := ClaudeToKiro(req, false)
	for i, h := range p.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil && len(a.ToolUses) > 0 {
			t.Fatalf("none: history[%d] must not carry structured toolUses", i)
		}
	}
}

// OpenAI "required" maps to the same "any" directive.
func TestOpenAIToolChoiceRequiredAppendsDirective(t *testing.T) {
	tool := OpenAITool{Type: "function"}
	tool.Function.Name = "Bash"
	tool.Function.Parameters = map[string]interface{}{"type": "object"}
	req := &OpenAIRequest{
		Model:      "claude-opus-4.8",
		Tools:      []OpenAITool{tool},
		ToolChoice: "required",
		Messages:   []OpenAIMessage{{Role: "user", Content: "run something"}},
	}
	p := OpenAIToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "must respond by calling one of the available tools") {
		t.Fatalf("required: expected forcing directive, got %q", cur.Content)
	}
}

// OpenAI "none" drops tools.
func TestOpenAIToolChoiceNoneDropsTools(t *testing.T) {
	tool := OpenAITool{Type: "function"}
	tool.Function.Name = "Bash"
	tool.Function.Parameters = map[string]interface{}{"type": "object"}
	req := &OpenAIRequest{
		Model:      "claude-opus-4.8",
		Tools:      []OpenAITool{tool},
		ToolChoice: "none",
		Messages:   []OpenAIMessage{{Role: "user", Content: "help"}},
	}
	p := OpenAIToKiro(req, false)
	cur := p.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.Tools) > 0 {
		t.Fatalf("none: tools must be dropped, got %d", len(cur.UserInputMessageContext.Tools))
	}
}

// normalizeClaudeToolChoice unit tests — guard every branch.
func TestNormalizeClaudeToolChoice(t *testing.T) {
	cases := []struct {
		in   interface{}
		mode toolChoiceMode
		name string
	}{
		{nil, toolChoiceAuto, ""},
		{map[string]interface{}{"type": "auto"}, toolChoiceAuto, ""},
		{map[string]interface{}{"type": "any"}, toolChoiceAny, ""},
		{map[string]interface{}{"type": "none"}, toolChoiceNone, ""},
		{map[string]interface{}{"type": "tool", "name": "Bash"}, toolChoiceSpecific, "Bash"},
		{map[string]interface{}{"type": "tool"}, toolChoiceAny, ""},     // no name → any
		{map[string]interface{}{"type": "unknown"}, toolChoiceAuto, ""}, // unknown → auto
	}
	for _, tc := range cases {
		got := normalizeClaudeToolChoice(tc.in)
		if got.mode != tc.mode || got.name != tc.name {
			t.Errorf("normalizeClaudeToolChoice(%v) = {mode:%d name:%q}, want {mode:%d name:%q}",
				tc.in, got.mode, got.name, tc.mode, tc.name)
		}
	}
}
