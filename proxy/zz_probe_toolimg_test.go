package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

var probeOut []string

func probeLog(format string, args ...interface{}) {
	probeOut = append(probeOut, strings.TrimRight(fmt.Sprintf(format, args...), "\n"))
	_ = os.WriteFile("probe_result.txt", []byte(strings.Join(probeOut, "\n")+"\n"), 0o644)
}

const probeImg = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func probeDump(t *testing.T, p *KiroPayload) string {
	t.Helper()
	b, _ := json.Marshal(p)
	return strings.ReplaceAll(string(b), probeImg, "<IMG>")
}

// Claim 1: an ORPHANED tool_result (no matching assistant toolUses) that carries
// BOTH text and an image loses its text entirely.
func TestZZProbeOrphanMixedLosesText(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{{
			Role: "user",
			Content: []interface{}{map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": "tool_2",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "SENTINEL_SCREENSHOT_TEXT"},
					map[string]interface{}{"type": "image", "source": map[string]interface{}{
						"type": "base64", "media_type": "image/png", "data": probeImg,
					}},
				},
			}},
		}},
	}
	dump := probeDump(t, ClaudeToKiro(req, false))
	probeLog("text survives anywhere: %v", strings.Contains(dump, "SENTINEL_SCREENSHOT_TEXT"))
	probeLog("payload: %s", dump)
}

// Control: the SAME tool_result, but PAIRED with an assistant toolUse, so it is
// an active tool turn. If text survives here but not above, the loss is specific
// to the orphan path.
func TestZZProbePairedMixedKeepsText(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "shoot"},
			{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "tool_use", "id": "tool_2", "name": "screenshot",
				"input": map[string]interface{}{},
			}}},
			{Role: "user", Content: []interface{}{map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": "tool_2",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "SENTINEL_SCREENSHOT_TEXT"},
					map[string]interface{}{"type": "image", "source": map[string]interface{}{
						"type": "base64", "media_type": "image/png", "data": probeImg,
					}},
				},
			}}},
		},
	}
	dump := probeDump(t, ClaudeToKiro(req, false))
	probeLog("text survives anywhere: %v", strings.Contains(dump, "SENTINEL_SCREENSHOT_TEXT"))
}

// Claim 1b: a TEXT-ONLY orphan tool_result also loses its text when it is the
// last message AND something else already filled currentContent.
func TestZZProbeOrphanTextOnlyWithUserText(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "and now?"},
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "tool_9",
					"content":     "SENTINEL_TOOL_OUTPUT",
				},
			},
		}},
	}
	dump := probeDump(t, ClaudeToKiro(req, false))
	probeLog("text survives anywhere: %v", strings.Contains(dump, "SENTINEL_TOOL_OUTPUT"))
	probeLog("payload: %s", dump)
}

// Claim 2: for the OpenAI path, the flushed tool-result history entry keeps its
// image but sanitizeKiroHistory deliberately clears UserInputMessageContext, so
// the failing test's precondition can never hold.
func TestZZProbeOpenAIFlushedToolHistoryShape(t *testing.T) {
	const dataURL = "data:image/png;base64," + probeImg
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_img", Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "read", Arguments: `{"path":"a.png"}`},
			}}},
			{Role: "tool", ToolCallID: "call_img", Content: []interface{}{
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
			}},
			{Role: "user", Content: "what do you see?"},
		},
	}
	p := OpenAIToKiro(req, false)
	for i, h := range p.ConversationState.History {
		if h.UserInputMessage == nil {
			probeLog("hist[%d] assistant content=%q", i, h.AssistantResponseMessage.Content)
			continue
		}
		probeLog("hist[%d] user content=%q ctxNil=%v nImages=%d", i,
			h.UserInputMessage.Content,
			h.UserInputMessage.UserInputMessageContext == nil,
			len(h.UserInputMessage.Images))
	}
	probeLog("current nImages=%d", len(p.ConversationState.CurrentMessage.UserInputMessage.Images))
}
