package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	apiKeyID := apiKeyIDFromContext(r.Context())

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponse(req.PreviousResponseID)
		if loadErr != nil {
			h.sendOpenAIError(w, 404, "invalid_request_error",
				fmt.Sprintf("previous_response_id not found: %v", loadErr))
			return
		}
		// Tenant isolation (M3): only the API key that created a response may chain
		// from it. Reject with the same 404 as "not found" so a caller cannot probe
		// for the existence of another tenant's response IDs. Responses created while
		// key auth was off (empty owner) are only chainable by unauthenticated
		// requests (also empty), preserving single-tenant behaviour.
		if prev.APIKeyID != apiKeyID {
			h.sendOpenAIError(w, 404, "invalid_request_error",
				"previous_response_id not found")
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:    req.Model,
		Messages: finalMessages,
		Stream:   req.Stream,
		Tools:    req.Tools,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	// Apply global/per-key model override (ForceModel > per-key Model > client model).
	actualModel = applyModelOverride(actualModel, apiKeyID, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	respID := generateResponseID()

	// Legacy escape hatch only — see handleClaudeMessagesInternal. A blocked key
	// normally fails at auth with a real HTTP error.
	if limitNoticeRequested(r.Context()) {
		h.sendResponsesNotice(w, actualModel, req.Stream, config.GetLimitNoticeMessage(), respID, &req)
		return
	}

	// The original body is kept so an external OpenAI-compatible provider that
	// advertises /v1/responses support can be handed the request verbatim.
	pc := &passthroughCtx{
		Raw:      body,
		Header:   r.Header,
		Stream:   req.Stream,
		Endpoint: config.ProviderEndpointResponses,
		ClientIP: h.resolveClientIP(r),
	}

	if req.Stream {
		h.handleResponsesStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens,
			apiKeyID, respID, &req, storedInputCopy, storeResponse, pc)
		return
	}

	h.handleResponsesNonStream(w, kiroPayload, actualModel, thinking, estimatedInputTokens,
		apiKeyID, respID, &req, storedInputCopy, storeResponse, pc)
}

func (h *Handler) handleResponsesNonStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
	pc *passthroughCtx,
) {
	startedAt := time.Now()
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		step := h.nextUpstream(apiKeyID, model, pc.Endpoint, excluded)
		if step == nil {
			break
		}
		if step.Provider != nil {
			handled, err := h.serveViaProvider(w, step, pc, model, apiKeyID, startedAt, estimatedInputTokens)
			if handled {
				return
			}
			lastErr = err
			excluded[step.Provider.ID] = true
			continue
		}
		account := step.Account
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var truncated bool
		var upstreamStopReason string

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:    func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete:   func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:    func(c float64) { credits = c },
			OnTruncated:  func() { truncated = true },
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		// Nothing written yet — rotate rather than persist and return a partial
		// response whose envelope would claim Status "completed".
		if truncated {
			lastErr = fmt.Errorf("upstream ended the stream before the turn completed")
			logger.Warnf("[Responses] turn truncated mid-generation on %s, rotating account", accountEmailForLog(account))
			excluded[account.ID] = true
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
		if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:    inputTokens,
			Output:   outputTokens,
			Credits:  credits,
			ClientIP: pc.clientIP(),
		}, model, account, "openai", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req)
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.APIKeyID = apiKeyID
		// The upstream may have stopped at its OWN output ceiling, which sits well
		// below the client's limit. Saying "completed" there tells the client the
		// answer is whole and it stops mid-task with nothing to act on.
		applyResponsesIncomplete(respObj, upstreamStopReason, len(toolUses) > 0, outputTokens, payloadMaxTokens(payload))

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "openai", model, 503, "No available accounts", startedAt, pc.clientIP())
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.recordFailureForApiKey(apiKeyID, "openai", model, status, lastErr.Error(), startedAt, pc.clientIP())
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 1+len(toolUses))

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	return &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             "completed",
		Model:              model,
		Output:             output,
		Usage:              ResponsesUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
}

// applyResponsesIncomplete downgrades a response from "completed" to "incomplete"
// when the turn was actually cut off, attaching the reason.
//
// This is the Responses API's only way to say "the output stopped early": there is
// no finish_reason field. It is separate from the truncation path (a dropped
// connection), which is a transport failure and becomes response.failed instead.
// Here the turn ended in an orderly way — the upstream simply hit a limit.
func applyResponsesIncomplete(respObj *ResponsesObject, upstreamStopReason string, hasToolUses bool, outputTokens, maxTokens int) {
	if respObj == nil {
		return
	}
	reason := responsesIncompleteReason(upstreamStopReason, hasToolUses, outputTokens, maxTokens)
	if reason == "" {
		return
	}
	respObj.Status = "incomplete"
	respObj.IncompleteDetails = &ResponsesIncompleteDetails{Reason: reason}
}

// sendResponsesNotice returns the limit-notice text as a normal assistant reply in the
// OpenAI /v1/responses shape. Used when a valid key is over-limit/disabled/expired so
// coding clients show the message in the chat window instead of erroring out. No upstream
// call, no billing.
func (h *Handler) sendResponsesNotice(w http.ResponseWriter, model string, stream bool, msg, respID string, req *ResponsesRequest) {
	outTok := noticeOutputTokens(msg)
	if !stream {
		respObj := buildResponsesObject(respID, model, msg, nil, 1, outTok, req)
		respObj.Instructions = req.Instructions
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		respObj := buildResponsesObject(respID, model, msg, nil, 1, outTok, req)
		respObj.Instructions = req.Instructions
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{"type": "response.created", "response": initial})
	send("response.in_progress", map[string]interface{}{"type": "response.in_progress", "response": initial})

	messageItemID := generateOutputItemID("msg")
	send("response.output_item.added", map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]interface{}{
			"id": messageItemID, "type": "message", "role": "assistant",
			"status": "in_progress", "content": []map[string]interface{}{},
		},
	})
	send("response.content_part.added", map[string]interface{}{
		"type": "response.content_part.added", "item_id": messageItemID,
		"output_index": 0, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": ""},
	})
	send("response.output_text.delta", map[string]interface{}{
		"type": "response.output_text.delta", "item_id": messageItemID,
		"output_index": 0, "content_index": 0, "delta": msg,
	})
	send("response.content_part.done", map[string]interface{}{
		"type": "response.content_part.done", "item_id": messageItemID,
		"output_index": 0, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": msg},
	})
	send("response.output_item.done", map[string]interface{}{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]interface{}{
			"id": messageItemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]interface{}{{"type": "output_text", "text": msg}},
		},
	})

	respObj := buildResponsesObject(respID, model, msg, nil, 1, outTok, req)
	respObj.CreatedAt = createdAt
	respObj.Instructions = req.Instructions
	send("response.completed", map[string]interface{}{"type": "response.completed", "response": respObj})
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) handleResponsesStream(
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
	pc *passthroughCtx,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
	}

	startedAt := time.Now()
	createdAt := startedAt.Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	// response.created is emitted lazily: an external provider serves the whole
	// stream itself (including its own response.created), so we must not have
	// written anything before deciding which upstream takes the request.
	createdSent := false
	ensureCreated := func() {
		if createdSent {
			return
		}
		send("response.created", map[string]interface{}{
			"type":     "response.created",
			"response": initial,
		})
		createdSent = true
	}

	excluded := make(map[string]bool)
	var lastErr error
	responseStarted := false

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		step := h.nextUpstream(apiKeyID, model, pc.Endpoint, excluded)
		if step == nil {
			break
		}
		if step.Provider != nil {
			handled, err := h.serveViaProvider(w, step, pc, model, apiKeyID, startedAt, estimatedInputTokens)
			if handled {
				return
			}
			lastErr = err
			excluded[step.Provider.ID] = true
			continue
		}
		account := step.Account
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		ensureCreated()
		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		var (
			fullText           strings.Builder
			reasoningText      strings.Builder
			toolUses           []KiroToolUse
			inputTokens        int
			outputTokens       int
			credits            float64
			realInputTokens    int
			truncated          bool
			upstreamStopReason string
		)

		messageItemID := generateOutputItemID("msg")
		messageStarted := false
		outputIndex := 0
		contentIndex := 0

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					reasoningText.WriteString(text)
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
			},
			OnToolUse: func(tu KiroToolUse) {
				if messageStarted {
					send("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       messageItemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullText.String(),
						},
					})
					send("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": outputIndex,
						"item": map[string]interface{}{
							"id":     messageItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []map[string]interface{}{{
								"type": "output_text",
								"text": fullText.String(),
							}},
						},
					})
					messageStarted = false
					outputIndex++
				}

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
				responseStarted = true
			},
			OnComplete:   func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:    func(c float64) { credits = c },
			OnTruncated:  func() { truncated = true },
			OnStopReason: func(reason string) { upstreamStopReason = reason },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Streaming path: bytes already sent cannot be taken back, so a mid-turn
		// cut is resumed rather than retried. OnTruncated still fires if every
		// resume also breaks.
		err := CallKiroAPIWithContinuation(account, payload, callback)
		if err != nil {
			if !responseStarted {
				lastErr = err
				excluded[account.ID] = true
				h.handleAccountFailure(account, err)
				continue
			}
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": err.Error(),
					},
				},
			})
			h.recordFailureForApiKey(apiKeyID, "openai", model, 0, err.Error(), startedAt, pc.clientIP())
			return
		}

		// Upstream cut the turn short. Nothing has gone out yet, so rotate and
		// retry on another account rather than emitting a partial response.
		if truncated && !responseStarted {
			lastErr = fmt.Errorf("upstream ended the stream before the turn completed")
			logger.Warnf("[Responses] turn truncated before any output on %s, rotating account", accountEmailForLog(account))
			excluded[account.ID] = true
			continue
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := reasoningText.String()
		if !thinking {
			reasoning = ""
		}

		if messageStarted {
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": finalContent,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": finalContent,
					}},
				},
			})
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		// Content already went out but the turn never completed. The open items
		// above have been closed so the client is not left mid-item, but the
		// terminal event must be response.failed — sending response.completed
		// would assert a partial answer is the whole answer. The response is not
		// persisted either: a stored "completed" record would replay the same lie.
		if truncated {
			logger.Warnf("[Responses] turn truncated mid-generation: model=%s account=%s", model, accountEmailForLog(account))
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": "upstream ended the stream before the turn completed",
					},
				},
			})
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			h.recordFailureForApiKey(apiKeyID, "openai", model, 0, "stream truncated before completion", startedAt, pc.clientIP())
			return
		}

		h.recordSuccessForApiKey(apiKeyID, requestUsage{
			Input:    inputTokens,
			Output:   outputTokens,
			Credits:  credits,
			ClientIP: pc.clientIP(),
		}, model, account, "openai", startedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions
		respObj.APIKeyID = apiKeyID
		// The upstream enforces its own output ceiling, well below the client's, so a
		// turn it cut short must not be announced as completed — the client would
		// treat a partial answer as whole and stop mid-task with nothing to act on.
		applyResponsesIncomplete(respObj, upstreamStopReason, len(toolUses) > 0, outputTokens, payloadMaxTokens(payload))

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		// The terminal event name must agree with the status: the Responses API
		// spells an early stop as response.incomplete. Emitting response.completed
		// with status "incomplete" inside it contradicts itself, and clients key off
		// the event name.
		terminalEvent := "response.completed"
		if respObj.Status == "incomplete" {
			terminalEvent = "response.incomplete"
			reason := ""
			if respObj.IncompleteDetails != nil {
				reason = respObj.IncompleteDetails.Reason
			}
			logger.Warnf("[Responses] turn incomplete (%s): model=%s output=%d max=%d upstream=%q",
				reason, model, outputTokens, payloadMaxTokens(payload), upstreamStopReason)
		}
		send(terminalEvent, map[string]interface{}{
			"type":     terminalEvent,
			"response": respObj,
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.recordFailureForApiKey(apiKeyID, "openai", model, 503, "No available accounts", startedAt, pc.clientIP())
		ensureCreated()
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	status := statusForUpstreamError(lastErr)
	h.recordFailureForApiKey(apiKeyID, "openai", model, status, lastErr.Error(), startedAt, pc.clientIP())
	ensureCreated()
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    errorTypeForOpenAIStatus(status),
				"message": lastErr.Error(),
			},
		},
	})
}
