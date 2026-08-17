package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// Detection has to check the type prefix as well as the name. A client is free
// to define its own tool called web_search; hijacking it would run our search
// instead of theirs.
func TestIsNativeWebSearchTool(t *testing.T) {
	cases := []struct {
		name string
		tool ClaudeTool
		want bool
	}{
		{"native", ClaudeTool{Name: "web_search", Type: "web_search_20250305"}, true},
		{"native with padding", ClaudeTool{Name: "web_search", Type: " web_search_20250305 "}, true},
		{"client tool sharing the name", ClaudeTool{Name: "web_search"}, false},
		{"client tool with unrelated type", ClaudeTool{Name: "web_search", Type: "custom"}, false},
		{"different server tool", ClaudeTool{Name: "code_execution", Type: "code_execution_20250101"}, false},
	}
	for _, c := range cases {
		if got := isNativeWebSearchTool(c.tool); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWebSearchDispatchPredicatesAreExclusive(t *testing.T) {
	native := ClaudeTool{Name: "web_search", Type: "web_search_20250305"}
	other := ClaudeTool{Name: "bash"}

	only := &ClaudeRequest{Tools: []ClaudeTool{native}}
	if !hasWebSearchTool(only) || hasWebSearchAmongTools(only) {
		t.Fatal("a lone native web_search must take the fast path only")
	}

	mixed := &ClaudeRequest{Tools: []ClaudeTool{native, other}}
	if hasWebSearchTool(mixed) || !hasWebSearchAmongTools(mixed) {
		t.Fatal("mixed tools must take the loop only")
	}

	none := &ClaudeRequest{Tools: []ClaudeTool{other}}
	if hasWebSearchTool(none) || hasWebSearchAmongTools(none) {
		t.Fatal("a request without native web_search must take neither path")
	}

	empty := &ClaudeRequest{}
	if hasWebSearchTool(empty) || hasWebSearchAmongTools(empty) {
		t.Fatal("a request with no tools must take neither path")
	}
}

func TestExtractSearchQuery(t *testing.T) {
	cases := []struct {
		name string
		req  *ClaudeRequest
		want string
	}{
		{
			"strips the client prefix",
			&ClaudeRequest{Messages: []ClaudeMessage{
				{Role: "user", Content: "Perform a web search for the query: golang generics"},
			}},
			"golang generics",
		},
		{
			"plain query without the prefix",
			&ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "golang generics"}}},
			"golang generics",
		},
		{
			// Taking the FIRST user turn would search for stale context.
			"multi-turn uses the last user turn",
			&ClaudeRequest{Messages: []ClaudeMessage{
				{Role: "user", Content: "old question"},
				{Role: "assistant", Content: "old answer"},
				{Role: "user", Content: "Perform a web search for the query: current question"},
			}},
			"current question",
		},
		{
			"array content shape",
			&ClaudeRequest{Messages: []ClaudeMessage{
				{Role: "user", Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "array query"},
				}},
			}},
			"array query",
		},
		{
			"typed block shape",
			&ClaudeRequest{Messages: []ClaudeMessage{
				{Role: "user", Content: []ClaudeContentBlock{{Type: "text", Text: "typed query"}}},
			}},
			"typed query",
		},
		{"empty", &ClaudeRequest{}, ""},
	}
	for _, c := range cases {
		if got := extractSearchQuery(c.req); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The success boundary is whether the payload PARSED, not whether it found
// anything. Getting this wrong produces a confident "no results" for a search
// that never ran.
func TestParseSearchResults(t *testing.T) {
	wrap := func(inner string) *McpResponse {
		return &McpResponse{Result: &McpResult{Content: []McpContent{{Type: "text", Text: inner}}}}
	}

	if got := parseSearchResults(wrap(`{"results":[{"title":"T","url":"u"}]}`)); got == nil || len(got.Results) != 1 {
		t.Fatalf("valid payload should parse, got %#v", got)
	}
	// Zero results is a SUCCESS: the search ran and found nothing.
	if got := parseSearchResults(wrap(`{"results":[]}`)); got == nil {
		t.Fatal("an empty result set is a successful search, not a failure")
	} else if len(got.Results) != 0 {
		t.Fatalf("expected zero results, got %d", len(got.Results))
	}
	// Everything below means the search did NOT run.
	if got := parseSearchResults(wrap(`{"error":"quota exceeded"}`)); got != nil {
		t.Fatal("an embedded error must be reported as failure")
	}
	if got := parseSearchResults(wrap(`not json`)); got != nil {
		t.Fatal("unparseable payload must be reported as failure")
	}
	if got := parseSearchResults(&McpResponse{Result: &McpResult{IsError: true}}); got != nil {
		t.Fatal("tool-level isError must be reported as failure")
	}
	if got := parseSearchResults(&McpResponse{Result: &McpResult{Content: []McpContent{{Type: "image"}}}}); got != nil {
		t.Fatal("non-text content must be reported as failure")
	}
	if got := parseSearchResults(nil); got != nil {
		t.Fatal("nil response must be reported as failure")
	}
}

func TestChunkByRunesKeepsMultibyteIntact(t *testing.T) {
	// Byte slicing would split these characters and emit invalid UTF-8.
	s := "tiếng Việt và 日本語テキスト"
	chunks := chunkByRunes(s, 4)
	if strings.Join(chunks, "") != s {
		t.Fatalf("round trip changed the string: %q", strings.Join(chunks, ""))
	}
	for _, c := range chunks {
		if !json.Valid(mustJSON(t, c)) {
			t.Fatalf("chunk is not valid UTF-8 when encoded: %q", c)
		}
	}
	if got := chunkByRunes("", 4); got != nil {
		t.Fatalf("empty input should produce no chunks, got %#v", got)
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestGenerateSearchSummary(t *testing.T) {
	long := strings.Repeat("ñ", 400)
	results := &WebSearchResults{Results: []WebSearchResult{
		{Title: "First", URL: "https://a", Snippet: strPtr(long)},
	}}
	summary := generateSearchSummary("q", results)
	if !strings.Contains(summary, "First") || !strings.Contains(summary, "https://a") {
		t.Fatalf("summary missing result details: %s", summary)
	}
	if !strings.Contains(summary, "...") {
		t.Fatalf("expected the long snippet to be truncated: %s", summary)
	}
	// Rune-safe truncation keeps the string valid UTF-8.
	if !json.Valid(mustJSON(t, summary)) {
		t.Fatal("truncated summary is not encodable")
	}

	empty := generateSearchSummary("q", &WebSearchResults{})
	if !strings.Contains(empty, "No results found") {
		t.Fatalf("expected the empty-result wording, got: %s", empty)
	}
}

// The result block must match the real API exactly: no tool_use_id, [] rather
// than null, and a null page_age when the date is missing.
func TestWebSearchResultContentShape(t *testing.T) {
	results := &WebSearchResults{Results: []WebSearchResult{
		{Title: "Dated", URL: "https://a", Snippet: strPtr("s"), PublishedDate: i64Ptr(1700000000000)},
		{Title: "Undated", URL: "https://b"},
	}}
	raw := mustJSON(t, webSearchResultContent(results))

	var decoded []map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected 2 results, got %d", len(decoded))
	}
	// tool_use_id belongs on the web_search_tool_result wrapper, not on the
	// individual hits inside its content array.
	if _, present := decoded[0]["tool_use_id"]; present {
		t.Fatal("an individual web_search_result must not carry tool_use_id")
	}
	if decoded[0]["encrypted_content"] != "s" {
		t.Fatalf("encrypted_content must be plaintext snippet (verified against 100-score reference API), got %v", decoded[0]["encrypted_content"])
	}
	if decoded[0]["page_age"] == nil {
		t.Fatal("expected a formatted page_age when publishedDate is present")
	}
	if decoded[1]["page_age"] != nil {
		t.Fatalf("missing publishedDate must yield null page_age, got %v", decoded[1]["page_age"])
	}
	if decoded[1]["encrypted_content"] != "" {
		t.Fatalf("missing snippet should yield an empty string, got %v", decoded[1]["encrypted_content"])
	}

	// A nil slice would marshal to null; clients expect [].
	if got := string(mustJSON(t, webSearchResultContent(nil))); got != "[]" {
		t.Fatalf("expected [] for no results, got %s", got)
	}
}

func TestBuildWebSearchContentBlocksOrder(t *testing.T) {
	blocks := buildWebSearchContentBlocks("q", "srvtoolu_x", &WebSearchResults{}, "the answer")
	want := []string{"server_tool_use", "web_search_tool_result", "text"}
	if len(blocks) != len(want) {
		t.Fatalf("expected %d blocks, got %d", len(want), len(blocks))
	}
	for i, w := range want {
		if blocks[i]["type"] != w {
			t.Fatalf("block %d: got %v, want %s", i, blocks[i]["type"], w)
		}
	}
	if blocks[0]["id"] != "srvtoolu_x" {
		t.Fatalf("server_tool_use must carry the tool use id, got %v", blocks[0]["id"])
	}
	// The result block is what links the hits back to the call that produced
	// them; without it a client cannot tell which search a result answered.
	if blocks[1]["tool_use_id"] != "srvtoolu_x" {
		t.Fatalf("web_search_tool_result must reference the server_tool_use id, got %v", blocks[1]["tool_use_id"])
	}
	if blocks[2]["text"] != "the answer" {
		t.Fatalf("the final block must carry the model's answer, got %v", blocks[2]["text"])
	}
}

// A Bedrock id that borrows the Anthropic prefix still announces the backend in
// the middle of the id, so a prefix check alone is not enough.
func TestNormalizeToolUseIDRewritesBedrockPrefixedShape(t *testing.T) {
	got := normalizeToolUseID("toolu_bdrk_01BZSE8P8Bm5SVnRJAUmFySz")
	if got == "toolu_bdrk_01BZSE8P8Bm5SVnRJAUmFySz" {
		t.Fatal("toolu_bdrk_ ids must be rewritten, not passed through")
	}
	if !anthropicIDPattern.MatchString(got) {
		t.Fatalf("rewritten id has the wrong shape: %q", got)
	}
}

func TestCreateMcpRequestIDShapes(t *testing.T) {
	toolUseID, req := createMcpRequest("hello")
	if !strings.HasPrefix(toolUseID, "srvtoolu_") {
		t.Fatalf("client expects the srvtoolu_ prefix, got %q", toolUseID)
	}
	if !strings.HasPrefix(req.ID, "web_search_tooluse_") {
		t.Fatalf("unexpected request id shape: %q", req.ID)
	}
	if req.JSONRPC != "2.0" || req.Method != "tools/call" {
		t.Fatalf("unexpected JSON-RPC envelope: %+v", req)
	}
	if req.Params.Name != webSearchToolName || req.Params.Arguments.Query != "hello" {
		t.Fatalf("unexpected params: %+v", req.Params)
	}

	// IDs must not repeat across calls.
	otherID, otherReq := createMcpRequest("hello")
	if otherID == toolUseID || otherReq.ID == req.ID {
		t.Fatal("request ids must be unique per call")
	}
}

func TestResolveWebSearchMaxUses(t *testing.T) {
	native := func(n int) []ClaudeTool {
		return []ClaudeTool{{Name: "web_search", Type: "web_search_20250305", MaxUses: n}}
	}
	if got := resolveWebSearchMaxUses(native(0)); got != maxWebSearchRounds {
		t.Fatalf("unset max_uses should default to %d, got %d", maxWebSearchRounds, got)
	}
	if got := resolveWebSearchMaxUses(native(2)); got != 2 {
		t.Fatalf("expected the client's budget, got %d", got)
	}
	// A client must not be able to open an unbounded loop.
	if got := resolveWebSearchMaxUses(native(999)); got != maxWebSearchRounds {
		t.Fatalf("expected the budget to be capped at %d, got %d", maxWebSearchRounds, got)
	}
}

func TestShouldSearchRound(t *testing.T) {
	search := KiroToolUse{Name: "web_search"}
	clientTool := KiroToolUse{Name: "bash"}

	if !shouldSearchRound(0, []KiroToolUse{search, search}, 3) {
		t.Fatal("an all-web_search round should search again")
	}
	// One client tool means the turn belongs to the client.
	if shouldSearchRound(0, []KiroToolUse{search, clientTool}, 3) {
		t.Fatal("a round containing a client tool must flush, not search")
	}
	if shouldSearchRound(0, nil, 3) {
		t.Fatal("a round with no tool calls must flush")
	}
	if shouldSearchRound(3, []KiroToolUse{search}, 3) {
		t.Fatal("the round budget must be respected")
	}
}

func TestResolveFlushStopReason(t *testing.T) {
	search := KiroToolUse{Name: "web_search"}
	clientTool := KiroToolUse{Name: "bash"}

	// web_search was consumed internally, so the client has nothing left to do.
	if got := resolveFlushStopReason("", "", []KiroToolUse{search}, nil); got != "end_turn" {
		t.Fatalf("web_search-only should end the turn, got %q", got)
	}
	if got := resolveFlushStopReason("", "", []KiroToolUse{clientTool}, nil); got != "tool_use" {
		t.Fatalf("a client tool requires tool_use, got %q", got)
	}
	content := []map[string]interface{}{{"type": "tool_use", "name": "bash"}}
	if got := resolveFlushStopReason("", "", nil, content); got != "tool_use" {
		t.Fatalf("a client tool in content requires tool_use, got %q", got)
	}
	if got := resolveFlushStopReason("max_tokens", "", []KiroToolUse{clientTool}, nil); got != "max_tokens" {
		t.Fatalf("an explicit override must win, got %q", got)
	}
}

// The websearch loop was the one serving path left without the upstream
// stopReason wiring: a turn Kiro cut at its own output ceiling was flushed as a
// clean end_turn, so the client believed the answer finished and stopped.
func TestResolveFlushStopReasonHonoursUpstream(t *testing.T) {
	search := KiroToolUse{Name: "web_search"}
	clientTool := KiroToolUse{Name: "bash"}

	if got := resolveFlushStopReason("", "MAX_TOKENS", []KiroToolUse{search}, nil); got != "max_tokens" {
		t.Fatalf("upstream MAX_TOKENS must surface as max_tokens, got %q", got)
	}
	if got := resolveFlushStopReason("", "END_TURN", []KiroToolUse{search}, nil); got != "end_turn" {
		t.Fatalf("upstream END_TURN stays end_turn, got %q", got)
	}
	if got := resolveFlushStopReason("", "STOP_SEQUENCE", nil, nil); got != "stop_sequence" {
		t.Fatalf("upstream STOP_SEQUENCE must map through, got %q", got)
	}
	// An unknown or absent reason must not invent a verdict.
	if got := resolveFlushStopReason("", "SOMETHING_NEW", nil, nil); got != "end_turn" {
		t.Fatalf("an unknown upstream reason falls back to end_turn, got %q", got)
	}

	// Precedence: a pending client tool_use outranks the upstream reason. Answering
	// max_tokens here would strand the tool call — the client would never run it.
	if got := resolveFlushStopReason("", "MAX_TOKENS", []KiroToolUse{clientTool}, nil); got != "tool_use" {
		t.Fatalf("a client tool must outrank upstream MAX_TOKENS, got %q", got)
	}
	content := []map[string]interface{}{{"type": "tool_use", "name": "bash"}}
	if got := resolveFlushStopReason("", "MAX_TOKENS", nil, content); got != "tool_use" {
		t.Fatalf("a client tool in content must outrank upstream MAX_TOKENS, got %q", got)
	}

	// Upstream TOOL_USE with no client tool left to forward must NOT claim
	// tool_use: web_search was consumed internally, so there is no tool_use block
	// for the client to act on and it would block forever waiting for one.
	if got := resolveFlushStopReason("", "TOOL_USE", []KiroToolUse{search}, nil); got != "end_turn" {
		t.Fatalf("upstream TOOL_USE without a forwarded tool must end the turn, got %q", got)
	}
}

func TestBuildFlushContentConvertsWebSearchOnly(t *testing.T) {
	presentation := []map[string]interface{}{{"type": "server_tool_use"}}
	toolUses := []KiroToolUse{
		{Name: "web_search", ToolUseID: "t1", Input: map[string]interface{}{"query": "q"}},
		{Name: "bash", ToolUseID: "t2", Input: map[string]interface{}{"cmd": "ls"}},
	}
	content := buildFlushContent(presentation, "answer", toolUses)

	var sawClientTool, sawWebSearchToolUse bool
	for _, c := range content {
		if c["type"] == "tool_use" {
			switch c["name"] {
			case "bash":
				sawClientTool = true
			case "web_search":
				sawWebSearchToolUse = true
			}
		}
	}
	if !sawClientTool {
		t.Fatal("the client's tool must be forwarded")
	}
	if sawWebSearchToolUse {
		t.Fatal("web_search was executed internally and must not reach the client as tool_use")
	}
}

// The Kiro payload converter only understands the []interface{} content shape a
// JSON decode produces. Using []map here would drop tool_use/tool_result
// silently, so this asserts they survive the round trip.
func TestAppendSearchRoundSurvivesKiroConversion(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "find something"},
		},
		Tools: []ClaudeTool{
			{Name: "web_search", Type: "web_search_20250305"},
			{Name: "bash", InputSchema: map[string]interface{}{"type": "object"}},
		},
	}
	round := &webSearchRoundOutcome{
		text: "searching now",
		toolUses: []KiroToolUse{
			{ToolUseID: "toolu_1", Name: "web_search", Input: map[string]interface{}{"query": "golang"}},
		},
	}
	results := []*WebSearchResults{{Results: []WebSearchResult{{Title: "Go", URL: "https://go.dev"}}}}

	presentation := make([]map[string]interface{}, 0)
	appendSearchRound(req, round, results, &presentation)

	if len(req.Messages) != 3 {
		t.Fatalf("expected assistant + user turns appended, got %d messages", len(req.Messages))
	}
	if _, ok := req.Messages[1].Content.([]interface{}); !ok {
		t.Fatalf("assistant content must be []interface{}, got %T", req.Messages[1].Content)
	}
	if _, ok := req.Messages[2].Content.([]interface{}); !ok {
		t.Fatalf("user content must be []interface{}, got %T", req.Messages[2].Content)
	}

	// Presentation is the separate, client-facing track.
	if len(presentation) != 2 ||
		presentation[0]["type"] != "server_tool_use" ||
		presentation[1]["type"] != "web_search_tool_result" {
		t.Fatalf("unexpected presentation blocks: %#v", presentation)
	}

	// The real assertion: the tool result must reach the Kiro payload.
	payload := ClaudeToKiro(req, false)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(raw), "go.dev") {
		t.Fatalf("search results were dropped during Kiro conversion:\n%s", string(raw))
	}
}

// Native web_search must never be forwarded to Kiro as a tool, but the model
// still needs a way to ask for a search.
func TestConvertClaudeToolsSwapsNativeWebSearch(t *testing.T) {
	tools := []ClaudeTool{
		{Name: "web_search", Type: "web_search_20250305"},
		{Name: "bash", InputSchema: map[string]interface{}{"type": "object"}},
	}
	converted, _ := convertClaudeTools(tools)

	var webSearchCount, bashCount int
	for _, c := range converted {
		switch c.ToolSpecification.Name {
		case "web_search":
			webSearchCount++
		case "bash":
			bashCount++
		}
	}
	if bashCount != 1 {
		t.Fatalf("the client's tool must survive, got %d", bashCount)
	}
	// Exactly one, and it is the injected function schema rather than the
	// passed-through server tool.
	if webSearchCount != 1 {
		t.Fatalf("expected exactly one injected web_search function, got %d", webSearchCount)
	}
	for _, c := range converted {
		if c.ToolSpecification.Name == "web_search" && c.ToolSpecification.Description == "" {
			t.Fatal("the injected web_search must describe itself to the model")
		}
	}
}

// A client that defines its own web_search must not get a second, injected one.
func TestConvertClaudeToolsDoesNotDuplicateClientWebSearch(t *testing.T) {
	tools := []ClaudeTool{
		{Name: "web_search", Type: "web_search_20250305"},
		{Name: "web_search", InputSchema: map[string]interface{}{"type": "object"}},
	}
	converted, _ := convertClaudeTools(tools)

	count := 0
	for _, c := range converted {
		if c.ToolSpecification.Name == "web_search" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one web_search tool, got %d", count)
	}
}
