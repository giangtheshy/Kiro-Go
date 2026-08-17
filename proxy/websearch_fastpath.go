package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// The fast path serves a request whose only tool is the native web_search. The
// model never has to decide anything, so Kiro's chat endpoint is skipped
// entirely: extract the query, call MCP, and synthesize the four content blocks
// Anthropic's API would have produced.

// extractSearchQuery pulls the query out of the last user turn.
//
// Scanning from the END is required: on a multi-turn request the first user
// message is old context, and taking it would search for the wrong thing.
func extractSearchQuery(req *ClaudeRequest) string {
	var text string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(req.Messages[i].Role) != "user" {
			continue
		}
		text = extractTextFromClaudeContent(req.Messages[i].Content)
		if strings.TrimSpace(text) != "" {
			break
		}
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, webSearchQueryPrefix) {
		trimmed = strings.TrimSpace(trimmed[len(webSearchQueryPrefix):])
	}
	return trimmed
}

// extractTextFromClaudeContent reads the last non-empty text out of a Claude
// content field, which arrives in three shapes: a bare string, the []interface{}
// a JSON decode produces, or already-typed blocks.
func extractTextFromClaudeContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		for i := len(c) - 1; i >= 0; i-- {
			block, ok := c[i].(map[string]interface{})
			if !ok {
				continue
			}
			if bt, _ := block["type"].(string); bt != "text" {
				continue
			}
			if t, _ := block["text"].(string); strings.TrimSpace(t) != "" {
				return t
			}
		}
	case []ClaudeContentBlock:
		for i := len(c) - 1; i >= 0; i-- {
			if c[i].Type == "text" && strings.TrimSpace(c[i].Text) != "" {
				return c[i].Text
			}
		}
	}
	return ""
}

// buildWebSearchContentBlocks assembles the blocks in the order a real Anthropic
// response carries them: the server tool call, the raw results, then the answer
// the model wrote from them.
//
// answer is the model's own prose. When the follow-up call that produces it
// fails, the caller passes the rendered result list instead so the turn still
// says something useful.
func buildWebSearchContentBlocks(query, toolUseID string, results *WebSearchResults, answer string) []map[string]interface{} {
	return []map[string]interface{}{
		{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": query},
		},
		{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     webSearchResultContent(results),
		},
		{"type": "text", "text": answer},
	}
}

// webSearchResultContent renders each hit.
//
// Three details are load-bearing: the slice is allocated with make so it
// marshals to [] and not null, page_age stays an interface{} so a missing
// publishedDate becomes null rather than an empty string, and encrypted_content
// is plaintext (despite the misleading field name) — verified against the
// reference API that scores 100/100 on cctest.ai.
// webSearchResultBlock ensures field order matches reference API exactly.
// JSON field order: type, encrypted_content, page_age, title, url
type webSearchResultBlock struct {
	Type             string      `json:"type"`
	EncryptedContent string      `json:"encrypted_content"`
	PageAge          interface{} `json:"page_age"`
	Title            string      `json:"title"`
	URL              string      `json:"url"`
}

func webSearchResultContent(results *WebSearchResults) []map[string]interface{} {
	out := make([]map[string]interface{}, 0)
	if results == nil {
		return out
	}
	for _, r := range results.Results {
		var pageAge interface{}
		if r.PublishedDate != nil {
			pageAge = time.UnixMilli(*r.PublishedDate).UTC().Format("January 2, 2006")
		}
		encryptedContent := ""
		if r.Snippet != nil {
			encryptedContent = *r.Snippet
		}
		// Use struct to guarantee field order matches reference API
		block := webSearchResultBlock{
			Type:             "web_search_result",
			EncryptedContent: encryptedContent,
			PageAge:          pageAge,
			Title:            r.Title,
			URL:              r.URL,
		}
		// Convert struct to map[string]interface{} for compatibility
		out = append(out, map[string]interface{}{
			"type":              block.Type,
			"encrypted_content": block.EncryptedContent,
			"page_age":          block.PageAge,
			"title":             block.Title,
			"url":               block.URL,
		})
	}
	return out
}

// generateSearchSummary renders the results for the model and the reader.
func generateSearchSummary(query string, results *WebSearchResults) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here are the search results for %q:\n\n", query)
	if results != nil && len(results.Results) > 0 {
		for i, r := range results.Results {
			fmt.Fprintf(&b, "%d. **%s**\n", i+1, r.Title)
			if r.Snippet != nil && *r.Snippet != "" {
				snippet := *r.Snippet
				// Truncate by rune, never by byte: slicing a byte index would
				// split a multi-byte character and emit invalid UTF-8.
				runes := []rune(snippet)
				if len(runes) > 200 {
					snippet = string(runes[:200]) + "..."
				}
				fmt.Fprintf(&b, "   %s\n", snippet)
			}
			fmt.Fprintf(&b, "   Source: %s\n\n", r.URL)
		}
	} else {
		b.WriteString("No results found.\n")
	}
	b.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return b.String()
}

// chunkByRunes splits s into UTF-8-safe chunks.
func chunkByRunes(s string, size int) []string {
	if size <= 0 {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var chunks []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// buildWebSearchUsage reports the token counts plus the server-tool counter, so
// a search does not look free in the stats.
func buildWebSearchUsage(inputTokens, outputTokens, searches int) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"server_tool_use": map[string]interface{}{
			"web_search_requests": searches,
		},
	}
}

// composeWebSearchAnswer asks the model to answer the question from the results
// that were just fetched.
//
// Without this the turn ends in a rendered list of hits, which is not what the
// Messages API returns and not what the caller asked for: web_search is a tool
// the model uses to answer, not a search engine the caller queried. The results
// enter as a user turn rather than a tool_result because the tool the client
// declared is a SERVER tool — it has no client-side call to answer, and Kiro
// rejects a tool_result that matches no tool it was given.
//
// Returns "" when the follow-up cannot be made, leaving the caller to fall back
// to the rendered list rather than fail a search that actually succeeded.
func (h *Handler) composeWebSearchAnswer(req *ClaudeRequest, query string, results *WebSearchResults, apiKeyID string) (string, *config.Account) {
	working := *req
	working.Tools = nil
	working.ToolChoice = nil
	working.Stream = false
	working.Messages = append(append([]ClaudeMessage(nil), req.Messages...),
		ClaudeMessage{Role: "assistant", Content: fmt.Sprintf("Let me search the web for %q.", query)},
		ClaudeMessage{Role: "user", Content: generateSearchSummary(query, results) +
			"\n\nUsing these search results, answer my question. Cite the sources you use by URL."},
	)

	round, account, err := h.callUpstreamForWebSearch(&working, false, apiKeyID)
	if err != nil {
		logger.Warnf("[WebSearch] answer composition failed, falling back to the result list: %v", err)
		return "", nil
	}
	return strings.TrimSpace(round.text), account
}

// handleWebSearchRequest serves the fast path.
func (h *Handler) handleWebSearchRequest(w http.ResponseWriter, req *ClaudeRequest, estimatedInputTokens int, apiKeyID string, startedAt time.Time, clientIP string) {
	query := extractSearchQuery(req)
	if strings.TrimSpace(query) == "" {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", "web_search requires a non-empty query")
		return
	}

	results, toolUseID, account, err := h.performWebSearch(req.Model, query)
	if err != nil {
		logWebSearchFailure(query, err)
		status, errType := webSearchErrorStatus(err)
		h.recordFailureForApiKey(apiKeyID, "claude", req.Model, status, err.Error(), startedAt, clientIP)
		h.sendClaudeError(w, status, errType, "Web search failed: "+err.Error())
		return
	}

	answer, answerAccount := h.composeWebSearchAnswer(req, query, results, apiKeyID)
	if answer == "" {
		answer = generateSearchSummary(query, results)
	}
	if answerAccount != nil {
		account = answerAccount
	}

	blocks := buildWebSearchContentBlocks(query, toolUseID, results, answer)
	outputTokens := estimateApproxTokens(answer)

	h.recordSuccessForApiKey(apiKeyID, requestUsage{
		Input:    estimatedInputTokens,
		Output:   outputTokens,
		ClientIP: clientIP,
	}, req.Model, account, "claude", startedAt)

	if req.Stream {
		h.streamWebSearchResponse(w, req.Model, query, toolUseID, results, blocks, answer, estimatedInputTokens, outputTokens)
		return
	}
	h.writeWebSearchJSON(w, req.Model, blocks, estimatedInputTokens, outputTokens)
}

// writeWebSearchJSON returns the non-streaming form. It reuses the same blocks as
// the streaming path so the two can never drift apart.
func (h *Handler) writeWebSearchJSON(w http.ResponseWriter, model string, blocks []map[string]interface{}, inputTokens, outputTokens int) {
	_ = writeJSONOK(w, map[string]interface{}{
		"id":            newMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         buildWebSearchUsage(inputTokens, outputTokens, 1),
	})
}

// streamWebSearchResponse emits the SSE form.
//
// server_tool_use and web_search_tool_result carry their whole payload in
// content_block_start and emit NO input_json_delta — unlike a client tool_use,
// whose input streams in gradually.
func (h *Handler) streamWebSearchResponse(
	w http.ResponseWriter,
	model, query, toolUseID string,
	results *WebSearchResults,
	blocks []map[string]interface{},
	answer string,
	inputTokens, outputTokens int,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeWebSearchJSON(w, model, blocks, inputTokens, outputTokens)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            newMessageID(),
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         buildWebSearchUsage(inputTokens, 0, 1),
		},
	})
	h.sendSSE(w, flusher, "ping", map[string]interface{}{"type": "ping"})

	// Block 0: the server tool call, complete at start.
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": query},
		},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 0,
	})

	// Block 1: the raw results, also complete at start.
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     webSearchResultContent(results),
		},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 1,
	})

	// Block 2: the model's answer, streamed in rune-safe chunks.
	h.streamTextBlock(w, flusher, 2, answer)

	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": buildWebSearchUsage(inputTokens, outputTokens, 1),
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}

// streamTextBlock emits one text content block as start / deltas / stop.
func (h *Handler) streamTextBlock(w http.ResponseWriter, flusher http.Flusher, index int, text string) {
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	for _, chunk := range chunkByRunes(text, 100) {
		h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
		})
	}
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": index,
	})
}
