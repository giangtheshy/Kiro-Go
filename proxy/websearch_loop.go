package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"

	"github.com/google/uuid"
)

// The agentic loop handles native web_search mixed with the client's own tools.
// The fast path cannot apply: the model has to decide when to search, and the
// client's tools must come back to the client untouched.
//
// Each round buffers one complete Kiro stream, then decides: search again, or
// flush to the client.

// webSearchRoundOutcome is what one upstream round produced.
type webSearchRoundOutcome struct {
	text               string
	toolUses           []KiroToolUse
	inputTokens        int
	credits            float64
	stopReasonOverride string
	// upstreamStopReason carries metadataEvent.stopReason verbatim (END_TURN,
	// MAX_TOKENS, ...) when the upstream sent one. Kiro enforces its OWN output
	// ceiling, unrelated to the client's max_tokens, so a turn it cuts short looks
	// well under the limit and would otherwise be flushed as a clean end_turn —
	// the client believes the answer finished and stops. This is the only in-band
	// signal that distinguishes the two.
	upstreamStopReason string
	// truncated marks a round whose stream ended mid-turn. Its text and toolUses
	// are partial, so they must not drive a search decision or be written into
	// history — see runWebSearchLoop.
	truncated bool
}

// callUpstreamForWebSearch runs one round, buffering the whole stream.
func (h *Handler) callUpstreamForWebSearch(req *ClaudeRequest, thinking bool, apiKeyID string) (*webSearchRoundOutcome, *config.Account, error) {
	payload := ClaudeToKiro(req, thinking)

	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.nextAccountForKey(apiKeyID, req.Model, excluded)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		round := &webSearchRoundOutcome{}
		var sb strings.Builder
		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if !isThinking {
					sb.WriteString(text)
				}
			},
			OnToolUse:   func(tu KiroToolUse) { round.toolUses = append(round.toolUses, tu) },
			OnComplete:  func(inTok, _ int) { round.inputTokens = inTok },
			OnCredits:   func(c float64) { round.credits = c },
			OnTruncated: func() { round.truncated = true },
			OnStopReason: func(reason string) { round.upstreamStopReason = reason },
		}

		if err := CallKiroAPI(account, payload, callback); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			// A refusal is final and was already billed: the upstream read the
			// conversation before declining, so rotating pays another account for
			// the identical verdict. See isTerminalRequestError.
			if isTerminalRequestError(err) {
				break
			}
			continue
		}

		round.text = sb.String()
		return round, account, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no available accounts")
	}
	return nil, nil, lastErr
}

// shouldSearchRound reports whether the loop may run another search.
//
// EVERY tool_use in the round must be web_search. A single client tool means the
// turn belongs to the client — the proxy must flush rather than execute it.
func shouldSearchRound(roundIdx int, toolUses []KiroToolUse, maxUses int) bool {
	if maxUses <= 0 {
		maxUses = maxWebSearchRounds
	}
	if len(toolUses) == 0 || roundIdx >= maxUses {
		return false
	}
	for _, tu := range toolUses {
		if tu.Name != webSearchToolName {
			return false
		}
	}
	return true
}

// appendSearchRound feeds a completed search round back into the conversation and
// accumulates the blocks the client will be shown.
//
// Two parallel tracks, deliberately different:
//   - history (req.Messages) uses ordinary tool_use / tool_result so the MODEL
//     understands what happened;
//   - presentation uses server_tool_use / web_search_tool_result so the CLIENT
//     sees a server tool.
//
// Content must be []interface{}, not []map[string]interface{}: the content
// extractor only handles the shape a JSON decode produces, so typed maps are
// silently dropped during conversion to the Kiro payload.
func appendSearchRound(req *ClaudeRequest, round *webSearchRoundOutcome, searched []*WebSearchResults, presentation *[]map[string]interface{}) {
	assistantContent := make([]interface{}, 0)
	if strings.TrimSpace(round.text) != "" {
		assistantContent = append(assistantContent, map[string]interface{}{
			"type": "text", "text": round.text,
		})
	}
	for _, tu := range round.toolUses {
		input := tu.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		assistantContent = append(assistantContent, map[string]interface{}{
			"type": "tool_use", "id": tu.ToolUseID, "name": tu.Name, "input": input,
		})
	}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: assistantContent})

	userContent := make([]interface{}, 0, len(round.toolUses))
	for i, tu := range round.toolUses {
		var results *WebSearchResults
		if i < len(searched) {
			results = searched[i]
		}
		query := toolUseQuery(tu.Input)
		userContent = append(userContent, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": tu.ToolUseID,
			"content":     generateSearchSummary(query, results),
		})

		srvID, _ := createMcpRequest(query)
		*presentation = append(*presentation,
			map[string]interface{}{
				"type": "server_tool_use", "id": srvID,
				"name":  webSearchToolName,
				"input": map[string]interface{}{"query": query},
			},
			map[string]interface{}{
				"type":    "web_search_tool_result",
				"content": webSearchResultContent(results),
			},
		)
	}
	req.Messages = append(req.Messages, ClaudeMessage{Role: "user", Content: userContent})
}

// resolveFlushStopReason decides the stop_reason for the final turn.
//
// Only CLIENT tools produce a tool_use the client must act on; web_search has
// already been consumed internally, so a web_search-only round ends the turn.
//
// Precedence is deliberate. A pending client tool_use outranks the upstream's
// stopReason: the client MUST be told to run that tool, and reporting max_tokens
// instead would strand the tool call. The upstream reason is consulted only once
// no tool is owed, where it is the difference between a turn Kiro cut at its own
// output ceiling and one the model chose to end.
func resolveFlushStopReason(override, upstream string, toolUses []KiroToolUse, content []map[string]interface{}) string {
	if override != "" {
		return override
	}
	for _, c := range content {
		if c["type"] == "tool_use" {
			if name, _ := c["name"].(string); name != webSearchToolName {
				return "tool_use"
			}
		}
	}
	for _, tu := range toolUses {
		if tu.Name != webSearchToolName {
			return "tool_use"
		}
	}
	// No client tool is owed. "tool_use" is therefore unreachable as an answer
	// here even if the upstream says so — claiming it would leave the client
	// blocked on a tool it never received — so only the terminal reasons map.
	switch upstreamStopReasonToClaude(upstream) {
	case "max_tokens":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	}
	return "end_turn"
}

// buildFlushContent assembles the final content: accumulated presentation blocks,
// then this round's text, then any client tool calls left to forward.
func buildFlushContent(presentation []map[string]interface{}, text string, toolUses []KiroToolUse) []map[string]interface{} {
	content := append([]map[string]interface{}{}, presentation...)
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": text})
	}
	for _, tu := range toolUses {
		if tu.Name == webSearchToolName {
			// Already executed internally; it reaches the client as the
			// server_tool_use / web_search_tool_result pair instead.
			continue
		}
		input := tu.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		content = append(content, map[string]interface{}{
			"type": "tool_use", "id": tu.ToolUseID, "name": tu.Name, "input": input,
		})
	}
	return content
}

// runWebSearchLoop drives the mixed-tools path.
func (h *Handler) runWebSearchLoop(
	w http.ResponseWriter,
	req *ClaudeRequest,
	thinking bool,
	estimatedInputTokens int,
	apiKeyID string,
	startedAt time.Time,
	clientIP string,
) {
	// Mutated as search results are folded into the conversation, so the caller's
	// request is left untouched.
	working := *req
	working.Messages = append([]ClaudeMessage(nil), req.Messages...)

	presentation := make([]map[string]interface{}, 0)
	var lastAccount *config.Account
	var totalCredits float64
	var totalInputTokens int
	maxUses := resolveWebSearchMaxUses(req.Tools)
	searchCount := 0

	// <= maxUses so there is one final round to answer with after the last search.
	for roundIdx := 0; roundIdx <= maxUses; roundIdx++ {
		round, account, err := h.callUpstreamForWebSearch(&working, thinking, apiKeyID)
		if err != nil {
			status := statusForUpstreamError(err)
			h.recordFailureForApiKey(apiKeyID, "claude", req.Model, status, err.Error(), startedAt, clientIP)
			h.sendClaudeError(w, status, "api_error", err.Error())
			return
		}
		if account != nil {
			lastAccount = account
		}
		totalCredits += round.credits
		if round.inputTokens > 0 {
			totalInputTokens = round.inputTokens
		}

		// A truncated round carries a PARTIAL tool_use set. Deciding on it would
		// mean either running a search the model never finished asking for, or —
		// worse — seeing zero tool_uses, concluding the model chose to stop, and
		// flushing a half answer as a clean turn. It must also never enter
		// history, since later rounds would build on truncated text. Break out
		// and report the truncation instead.
		if round.truncated {
			logger.Warnf("[WebSearch] round %d truncated mid-turn on %s; flushing with a truncation signal",
				roundIdx, accountEmailForLog(account))
			content := buildFlushContent(presentation, round.text, nil)
			h.flushWebSearchLoop(w, req, content, "", totalInputTokens, estimatedInputTokens,
				searchCount, apiKeyID, startedAt, clientIP, nil, totalCredits, true)
			return
		}

		roundSearchN := countWebSearchToolUses(round.toolUses)
		if shouldSearchRound(roundIdx, round.toolUses, maxUses) && searchCount+roundSearchN <= maxUses {
			searched, searchErr := h.searchAllWebUses(req.Model, round.toolUses)
			if searchErr != nil {
				logWebSearchFailure("", searchErr)
				status, errType := webSearchErrorStatus(searchErr)
				h.recordFailureForApiKey(apiKeyID, "claude", req.Model, status, searchErr.Error(), startedAt, clientIP)
				h.sendClaudeError(w, status, errType, "Web search failed: "+searchErr.Error())
				return
			}
			searchCount += roundSearchN
			appendSearchRound(&working, round, searched, &presentation)
			continue
		}

		content := buildFlushContent(presentation, round.text, round.toolUses)
		stopReason := resolveFlushStopReason(round.stopReasonOverride, round.upstreamStopReason, round.toolUses, content)
		if stopReason == "max_tokens" {
			logger.Warnf("[WebSearch] round %d hit the upstream output ceiling (upstream=%q); reporting max_tokens so the client continues",
				roundIdx, round.upstreamStopReason)
		}
		h.flushWebSearchLoop(w, req, content, stopReason, totalInputTokens, estimatedInputTokens,
			searchCount, apiKeyID, startedAt, clientIP, lastAccount, totalCredits, false)
		return
	}

	// Budget exhausted without the model producing a final answer.
	h.flushWebSearchLoop(w, req, buildFlushContent(presentation, "", nil), "end_turn",
		totalInputTokens, estimatedInputTokens, searchCount, apiKeyID, startedAt, clientIP,
		lastAccount, totalCredits, false)
}

// flushWebSearchLoop writes the final response in whichever form the client asked
// for. truncated withholds the completion signal and reports the failure instead.
func (h *Handler) flushWebSearchLoop(
	w http.ResponseWriter,
	req *ClaudeRequest,
	content []map[string]interface{},
	stopReason string,
	inputTokens, estimatedInputTokens, searches int,
	apiKeyID string,
	startedAt time.Time,
	clientIP string,
	account *config.Account,
	credits float64,
	truncated bool,
) {
	if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputTokens := estimateWebSearchContentTokens(content)

	if truncated {
		// Do NOT record success and do NOT clear the account's error state: the
		// turn did not complete.
		h.recordFailureForApiKey(apiKeyID, "claude", req.Model, 0, "stream truncated before completion", startedAt, clientIP)
	} else {
		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:    inputTokens,
			Output:   outputTokens,
			Credits:  credits,
			ClientIP: clientIP,
		}, req.Model, account, "claude", startedAt)
		if account != nil {
			h.pool.RecordSuccess(account.ID)
			h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		}
	}

	if req.Stream {
		h.renderWebSearchLoopSSE(w, req.Model, content, stopReason, inputTokens, outputTokens, searches, truncated)
		return
	}
	h.renderWebSearchLoopJSON(w, req.Model, content, stopReason, inputTokens, outputTokens, searches, truncated)
}

func (h *Handler) renderWebSearchLoopJSON(
	w http.ResponseWriter,
	model string,
	content []map[string]interface{},
	stopReason string,
	inputTokens, outputTokens, searches int,
	truncated bool,
) {
	body := map[string]interface{}{
		"id":            "msg_" + uuid.New().String(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_sequence": nil,
		"usage":         buildWebSearchUsage(inputTokens, outputTokens, searches),
	}
	if truncated {
		// A null stop_reason is how a buffered response says "this turn did not
		// finish" without discarding the text already produced.
		body["stop_reason"] = nil
		body["error"] = map[string]string{
			"type":    "api_error",
			"message": "upstream ended the stream before the turn completed",
		}
	} else {
		body["stop_reason"] = stopReason
	}
	_ = writeJSONOK(w, body)
}

func (h *Handler) renderWebSearchLoopSSE(
	w http.ResponseWriter,
	model string,
	content []map[string]interface{},
	stopReason string,
	inputTokens, outputTokens, searches int,
	truncated bool,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.renderWebSearchLoopJSON(w, model, content, stopReason, inputTokens, outputTokens, searches, truncated)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	msgID := "msg_" + uuid.New().String()
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         buildWebSearchUsage(inputTokens, 0, searches),
		},
	})

	for idx, block := range content {
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			h.streamTextBlock(w, flusher, idx, text)
		case "tool_use":
			// A client tool streams its input as a json delta, unlike a server tool.
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    block["id"],
					"name":  block["name"],
					"input": map[string]interface{}{},
				},
			})
			inputJSON, _ := json.Marshal(block["input"])
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": string(inputJSON),
				},
			})
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": idx,
			})
		default:
			// server_tool_use and web_search_tool_result carry their full payload
			// in the start frame and emit no deltas.
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": block,
			})
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": idx,
			})
		}
	}

	if truncated {
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{},
			"usage": buildWebSearchUsage(inputTokens, outputTokens, searches),
		})
		h.sendSSE(w, flusher, "error", map[string]interface{}{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": "upstream ended the stream before the turn completed",
			},
		})
		return
	}

	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": buildWebSearchUsage(inputTokens, outputTokens, searches),
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}

// estimateWebSearchContentTokens approximates output size across mixed blocks.
func estimateWebSearchContentTokens(content []map[string]interface{}) int {
	total := 0
	for _, block := range content {
		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok {
				total += estimateApproxTokens(t)
			}
		case "tool_use", "server_tool_use":
			if name, ok := block["name"].(string); ok {
				total += estimateApproxTokens(name)
			}
			if raw, err := json.Marshal(block["input"]); err == nil {
				total += estimateApproxTokens(string(raw))
			}
		case "web_search_tool_result":
			if raw, err := json.Marshal(block["content"]); err == nil {
				total += estimateApproxTokens(string(raw))
			}
		}
	}
	return total
}
