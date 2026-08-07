package proxy

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"

	"github.com/google/uuid"
)

// Kiro's generateAssistantResponse does NOT execute web_search. It only emits a
// tool_use and waits for someone to hand back a tool_result. Forwarding the
// native tool through as an ordinary tool therefore deadlocks the conversation:
// the model asks for a search, the client (Claude Code, Claude Desktop) assumes
// the *server* runs server tools and never executes it, and both sides wait.
//
// The actual search lives behind a different endpoint — Kiro MCP — so this proxy
// has to call it itself and then synthesize a response in Anthropic's shape. That
// makes it an MCP client on one side and a server-tool provider on the other.

const webSearchToolName = "web_search"

// webSearchQueryPrefix is the fixed preamble Claude Code / Desktop wrap a search
// request in. Stripped so the upstream receives the bare query.
const webSearchQueryPrefix = "Perform a web search for the query:"

// maxWebSearchRounds caps the agentic loop regardless of what the client asks
// for, so a client cannot open an unbounded search loop against the pool.
const maxWebSearchRounds = 5

// isNativeWebSearchTool reports whether a tool is Anthropic's server-side
// web_search.
//
// BOTH conditions are required. A client is free to define its own tool called
// "web_search" — that one has no Type and must travel the normal path. Matching
// on the name alone would hijack it and run our own search instead of theirs.
func isNativeWebSearchTool(t ClaudeTool) bool {
	return t.Name == webSearchToolName &&
		strings.HasPrefix(strings.TrimSpace(t.Type), "web_search_")
}

// hasWebSearchTool reports the fast path: the request carries exactly one tool
// and it is the native web_search. Nothing needs Kiro's chat endpoint at all.
func hasWebSearchTool(req *ClaudeRequest) bool {
	if req == nil || len(req.Tools) != 1 {
		return false
	}
	return isNativeWebSearchTool(req.Tools[0])
}

// hasWebSearchAmongTools reports the mixed case: native web_search alongside the
// client's own tools, which needs the agentic loop so the model decides when to
// search while the client's tools still reach the client.
func hasWebSearchAmongTools(req *ClaudeRequest) bool {
	if req == nil || len(req.Tools) <= 1 {
		return false
	}
	for _, t := range req.Tools {
		if isNativeWebSearchTool(t) {
			return true
		}
	}
	return false
}

func hasNativeWebSearchInTools(tools []ClaudeTool) bool {
	for _, t := range tools {
		if isNativeWebSearchTool(t) {
			return true
		}
	}
	return false
}

// resolveWebSearchMaxUses reads the client's requested budget, always bounded by
// maxWebSearchRounds.
func resolveWebSearchMaxUses(tools []ClaudeTool) int {
	maxUses := 0
	for _, t := range tools {
		if !isNativeWebSearchTool(t) {
			continue
		}
		if t.MaxUses > maxUses {
			maxUses = t.MaxUses
		}
	}
	if maxUses <= 0 {
		return maxWebSearchRounds
	}
	if maxUses > maxWebSearchRounds {
		return maxWebSearchRounds
	}
	return maxUses
}

// ==================== MCP wire types ====================

type McpRequest struct {
	ID      string       `json:"id"`
	JSONRPC string       `json:"jsonrpc"`
	Method  string       `json:"method"`
	Params  McpReqParams `json:"params"`
}

type McpReqParams struct {
	Name      string       `json:"name"`
	Arguments McpArguments `json:"arguments"`
}

type McpArguments struct {
	Query string `json:"query"`
}

type McpResponse struct {
	Error   *McpError  `json:"error"`
	ID      string     `json:"id"`
	JSONRPC string     `json:"jsonrpc"`
	Result  *McpResult `json:"result"`
}

type McpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type McpResult struct {
	Content []McpContent `json:"content"`
	IsError bool         `json:"isError"`
}

type McpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// WebSearchResults is the payload nested INSIDE result.content[0].text as a JSON
// string — the response is JSON within JSON and needs unmarshalling twice.
type WebSearchResults struct {
	Results      []WebSearchResult `json:"results"`
	TotalResults *int              `json:"totalResults"`
	Query        *string           `json:"query"`
	// Error is a failure reported inside the payload even when JSON-RPC said OK.
	// A pointer so "absent" is distinguishable from "empty", which is what
	// decides success here.
	Error *string `json:"error"`
}

type WebSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Snippet       *string `json:"snippet"`
	PublishedDate *int64  `json:"publishedDate"` // Unix milliseconds
	ID            *string `json:"id"`
	Domain        *string `json:"domain"`
}

// ==================== MCP client ====================

const (
	alnumAlphabet      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	lowerAlnumAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// randomFromAlphabet uses crypto/rand rather than math/rand: this process is long
// lived and math/rand without an explicit seed repeats the same sequence across
// restarts, which would emit colliding request IDs.
func randomFromAlphabet(n int, alphabet string) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand failing is not recoverable here; fall back to a uuid
			// slice, which is still unpredictable enough for a request id.
			return strings.ReplaceAll(uuid.New().String(), "-", "")[:n]
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out)
}

// createMcpRequest builds the JSON-RPC call plus the server_tool_use id handed
// back to the client. The id shapes mirror what the Kiro IDE sends.
func createMcpRequest(query string) (toolUseID string, req *McpRequest) {
	requestID := fmt.Sprintf(
		"web_search_tooluse_%s_%d_%s",
		randomFromAlphabet(22, alnumAlphabet),
		time.Now().UnixMilli(),
		randomFromAlphabet(8, lowerAlnumAlphabet),
	)

	// Anthropic clients expect server tool ids to carry the srvtoolu_ prefix.
	toolUseID = "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if len(toolUseID) > len("srvtoolu_")+32 {
		toolUseID = toolUseID[:len("srvtoolu_")+32]
	}

	return toolUseID, &McpRequest{
		ID:      requestID,
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: McpReqParams{
			Name:      webSearchToolName,
			Arguments: McpArguments{Query: query},
		},
	}
}

// callMcpAPI performs one JSON-RPC tools/call against Kiro's MCP endpoint.
//
// Three independent failure layers each have to be checked: HTTP status,
// JSON-RPC error, and tool-level isError. A 200 does NOT mean the search ran.
func callMcpAPI(account *config.Account, mcpReq *McpRequest) (*McpResponse, error) {
	if account == nil {
		return nil, fmt.Errorf("web_search: no account")
	}

	// The MCP host is keyed to the region of the account's PROFILE, which is not
	// necessarily the region it authenticated in. kiroRegion() resolves the ARN
	// first for exactly this reason; sending the call to a data plane that does
	// not hold the profile gets it rejected.
	profileArn := strings.TrimSpace(account.ProfileArn)
	if profileArn == "" && !account.IsApiKeyCredential() {
		if arn, err := ResolveProfileArn(account); err == nil {
			profileArn = strings.TrimSpace(arn)
			account.ProfileArn = profileArn
		}
	}
	region := kiroRegion(account)
	endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)

	payload, err := json.Marshal(mcpReq)
	if err != nil {
		return nil, fmt.Errorf("web_search: marshal request: %w", err)
	}

	proxyURL, poolKey, err := SelectProxyForAccount(account)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("web_search: build request: %w", err)
	}

	host := ""
	if u, perr := url.Parse(endpoint); perr == nil {
		host = u.Host
	}
	applyKiroBaseHeaders(httpReq, account, buildStreamingHeaderValues(account, host))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	httpReq.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	if profileArn != "" {
		httpReq.Header.Set("x-amzn-kiro-profile-arn", profileArn)
	}

	resp, err := GetRestClientForProxy(proxyURL).Do(httpReq)
	if err != nil {
		if poolKey != "" && isProxyErrorMessage(err.Error()) {
			config.MarkProxyUnhealthy(poolKey)
		}
		return nil, fmt.Errorf("web_search: MCP request failed: %w", err)
	}
	defer resp.Body.Close()
	if poolKey != "" {
		config.MarkProxyHealthy(poolKey)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("web_search: read MCP response: %w", err)
	}

	// Layer 1: HTTP.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web_search: MCP request failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var mcpResp McpResponse
	if err := json.Unmarshal(body, &mcpResp); err != nil {
		return nil, fmt.Errorf("web_search: decode MCP response: %w", err)
	}

	// Layer 2: JSON-RPC.
	if mcpResp.Error != nil {
		return nil, fmt.Errorf("web_search: MCP error: %d - %s", mcpResp.Error.Code, mcpResp.Error.Message)
	}

	// Layer 3: tool-level.
	if mcpResp.Result != nil && mcpResp.Result.IsError {
		return nil, fmt.Errorf("web_search: MCP tool error for %s", webSearchToolName)
	}

	return &mcpResp, nil
}

// parseSearchResults unwraps the doubly-encoded payload.
//
// Returning nil means the search did NOT run — the caller must rotate accounts
// rather than present an empty result set. A parsed payload with zero results is
// a SUCCESS (the search ran and found nothing); the boundary is whether it
// parsed, not whether it found anything.
func parseSearchResults(mcpResp *McpResponse) *WebSearchResults {
	if mcpResp == nil || mcpResp.Result == nil || len(mcpResp.Result.Content) == 0 {
		return nil
	}
	if mcpResp.Result.IsError {
		return nil
	}
	content := mcpResp.Result.Content[0]
	if content.Type != "text" {
		return nil
	}
	var results WebSearchResults
	if err := json.Unmarshal([]byte(content.Text), &results); err != nil {
		return nil
	}
	if results.Error != nil && strings.TrimSpace(*results.Error) != "" {
		return nil
	}
	return &results
}

// performWebSearch runs one search through the account pool, rotating on failure.
func (h *Handler) performWebSearch(model, query string) (*WebSearchResults, string, *config.Account, error) {
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		toolUseID, mcpReq := createMcpRequest(query)
		mcpResp, err := callMcpAPI(account, mcpReq)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		results := parseSearchResults(mcpResp)
		if results == nil {
			// HTTP and JSON-RPC said OK but the payload is unusable. Returning an
			// empty result set here would be the worst outcome: the client gets a
			// confident "no results found" for a search that never ran, and no
			// error surfaces anywhere. Treat it as a failure and rotate.
			lastErr = fmt.Errorf("web_search: MCP returned an unparseable or error search payload")
			excluded[account.ID] = true
			h.handleAccountFailure(account, lastErr)
			continue
		}

		h.pool.RecordSuccess(account.ID)
		return results, toolUseID, account, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("web_search: no available accounts")
	}
	return nil, "", nil, lastErr
}

// searchAllWebUses runs every web_search tool_use of a round, preserving order so
// each result lines up with the tool_use that asked for it.
func (h *Handler) searchAllWebUses(model string, toolUses []KiroToolUse) ([]*WebSearchResults, error) {
	out := make([]*WebSearchResults, 0, len(toolUses))
	for _, tu := range toolUses {
		if tu.Name != webSearchToolName {
			out = append(out, nil)
			continue
		}
		query := toolUseQuery(tu.Input)
		if strings.TrimSpace(query) == "" {
			out = append(out, nil)
			continue
		}
		results, _, _, err := h.performWebSearch(model, query)
		if err != nil {
			return nil, err
		}
		out = append(out, results)
	}
	return out, nil
}

func toolUseQuery(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	if q, ok := input["query"].(string); ok {
		return q
	}
	return ""
}

func countWebSearchToolUses(toolUses []KiroToolUse) int {
	n := 0
	for _, tu := range toolUses {
		if tu.Name == webSearchToolName {
			n++
		}
	}
	return n
}

// webSearchErrorStatus maps a search failure onto the status and Anthropic error
// type the client should see, so a quota problem does not surface as a 502.
func webSearchErrorStatus(err error) (int, string) {
	if err == nil {
		return http.StatusBadGateway, "api_error"
	}
	msg := err.Error()
	switch {
	case isAuthErrorMessage(msg):
		return http.StatusUnauthorized, "authentication_error"
	case isQuotaErrorMessage(msg):
		return http.StatusTooManyRequests, "rate_limit_error"
	default:
		return http.StatusBadGateway, "api_error"
	}
}

func logWebSearchFailure(query string, err error) {
	logger.Warnf("[WebSearch] query=%q failed: %v", truncateForLog(query, 80), err)
}

func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
