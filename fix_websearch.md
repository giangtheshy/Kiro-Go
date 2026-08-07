# Thêm web_search (Anthropic native) vào Kiro proxy

Tài liệu này mô tả cách hiện thực `web_search` — tool server-side của Anthropic — trên một proxy Kiro. Không phải bản fix; đây là hướng dẫn làm mới từ đầu cho pool chưa có tính năng này.

Toàn bộ tên hàm/biến dưới đây là gợi ý, đặt lại theo convention của bạn tuỳ ý. Code minh hoạ viết bằng Go.

---

# 1. Vì sao cần code riêng

`generateAssistantResponse` của Kiro **không thực thi** `web_search`. Nó chỉ phát ra `tool_use` rồi dừng, chờ ai đó trả `tool_result`. Nếu bạn forward native web_search tool thẳng vào Kiro như một tool bình thường, kết quả là:

- Model phát `tool_use` name=`web_search` về client.
- Client (Claude Code, Claude Desktop) **không** thực thi nó — với client, `web_search` là server tool, nó tưởng server đã chạy.
- Cả hai bên chờ nhau, conversation treo.

Web search thật nằm ở một endpoint khác: **Kiro MCP**. Proxy phải tự gọi endpoint đó, rồi tự tổng hợp response đúng contract của Anthropic.

Nghĩa là proxy đóng hai vai: vừa là client của MCP, vừa là server tool đối với client của nó.

---

# 2. Nhận diện native web_search

Native server tool có `type` mang tiền tố `web_search_` (ví dụ `web_search_20250305`) **và** `name` là `web_search`:

```go
const webSearchToolName = "web_search"

func isNativeWebSearchTool(t ClaudeTool) bool {
    return t.Name == webSearchToolName &&
        strings.HasPrefix(strings.TrimSpace(t.Type), "web_search_")
}
```

Kiểm cả hai điều kiện là bắt buộc. Một client tool tự định nghĩa **có thể** trùng tên `web_search` nhưng không có `type` — tool đó phải đi đường bình thường, nếu không bạn sẽ chặn tool của người ta rồi tự chạy search thay họ.

`ClaudeTool` cần thêm hai field so với tool thường:

```go
type ClaudeTool struct {
    // Chỉ có ở Anthropic server tool, ví dụ "web_search_20250305".
    Type        string      `json:"type,omitempty"`
    Name        string      `json:"name"`
    Description string      `json:"description"`
    InputSchema interface{} `json:"input_schema"`
    // Chỉ có ở native web_search: giới hạn số lần search.
    MaxUses     int         `json:"max_uses,omitempty"`
}
```

## Hai đường, loại trừ nhau

```go
// Chỉ đúng một tool và đó là native web_search → đường nhanh, khỏi gọi Kiro.
func hasWebSearchTool(req *ClaudeRequest) bool {
    if req == nil || len(req.Tools) != 1 {
        return false
    }
    return isNativeWebSearchTool(req.Tools[0])
}

// Native web_search nằm chung với tool khác → cần agentic loop.
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
```

Dispatch đặt **trước** khi convert payload sang Kiro:

```go
// Pure native web_search: relay qua MCP, không cần gọi generateAssistantResponse.
if hasWebSearchTool(&req) {
    h.handleWebSearchRequest(w, &req, estimatedInputTokens, apiKeyID)
    return
}

// Tool trộn: agentic loop tự tiêu hoá web_search, tool của client trả về nguyên vẹn.
if hasWebSearchAmongTools(&req) {
    h.runWebSearchLoop(w, &req, thinking, estimatedInputTokens, apiKeyID)
    return
}

// Còn lại: đường chat bình thường.
kiroPayload := ClaudeToKiro(&req, thinking)
```

---

# 3. Gọi MCP endpoint

## Endpoint và body

MCP là JSON-RPC 2.0 tại `POST https://q.{region}.amazonaws.com/mcp`, method `tools/call`.

```go
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
```

ID phải theo format Kiro IDE dùng, kèm luôn `server_tool_use` id trả cho client:

```go
func createMcpRequest(query string) (string, *McpRequest) {
    requestID := fmt.Sprintf(
        "web_search_tooluse_%s_%d_%s",
        randomAlnum(22),               // [A-Za-z0-9]{22}
        time.Now().UnixMilli(),
        randomLowerAlnum(8),           // [a-z0-9]{8}
    )
    // Client Anthropic mong id server tool có tiền tố srvtoolu_.
    toolUseID := "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
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
```

Dùng `crypto/rand` cho phần random, không `math/rand`. Server chạy dài, `math/rand` chưa seed hoặc seed trùng sẽ sinh ID lặp lại giữa các process.

## Region lấy từ profile, không lấy từ account

Đây là chỗ dễ sai nhất: **region host MCP là region của profile**, còn `Account.Region` là region xác thực — hai giá trị này có thể khác nhau.

```go
profileArn := strings.TrimSpace(account.ProfileArn)
// Account API-key không dùng profile ARN; account thường thì phải resolve.
if profileArn == "" && !config.IsAPIKeyAccount(account) {
    if arn, err := ResolveProfileArn(account); err == nil {
        profileArn = strings.TrimSpace(arn)
    }
}
region := kiroRegionForProfile(account, profileArn)
endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)
```

Lấy sai region thì request đi tới data plane không giữ profile đó và bị từ chối.

## Headers

Ngoài các header Kiro thường dùng (`Authorization: Bearer`, `User-Agent`, `x-amz-user-agent`, và `tokentype: API_KEY` nếu là account API-key), MCP cần thêm:

```go
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Accept", "*/*")
req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
if profileArn != "" {
    req.Header.Set("x-amzn-kiro-profile-arn", profileArn)
}
```

## Ba tầng lỗi phải kiểm riêng

Response 200 **không** có nghĩa là search thành công. Có ba tầng, kiểm đủ cả ba:

```go
type McpResponse struct {
    Error   *McpError  `json:"error"`
    ID      string     `json:"id"`
    JSONRPC string     `json:"jsonrpc"`
    Result  *McpResult `json:"result"`
}
type McpResult struct {
    Content []McpContent `json:"content"`
    IsError bool         `json:"isError"`
}
type McpContent struct {
    Type string `json:"type"`
    Text string `json:"text"`
}
```

```go
if resp.StatusCode != 200 {                        // tầng 1: HTTP
    return nil, fmt.Errorf("MCP request failed: HTTP %d: %s", resp.StatusCode, string(body))
}
if mcpResp.Error != nil {                          // tầng 2: JSON-RPC error
    return nil, fmt.Errorf("MCP error: %d - %s", code, msg)
}
if mcpResp.Result != nil && mcpResp.Result.IsError { // tầng 3: tool-level error
    return nil, fmt.Errorf("MCP tool error for web_search")
}
```

---

# 4. Parse kết quả search

Kết quả thật nằm **trong** `result.content[0].text` dưới dạng JSON string — tức là JSON lồng trong JSON, phải unmarshal hai lần.

```go
type WebSearchResults struct {
    Results      []WebSearchResult `json:"results"`
    TotalResults *int              `json:"totalResults"`
    Query        *string           `json:"query"`
    Error        *string           `json:"error"`   // lỗi nhúng trong payload
}
type WebSearchResult struct {
    Title         string  `json:"title"`
    URL           string  `json:"url"`
    Snippet       *string `json:"snippet"`
    PublishedDate *int64  `json:"publishedDate"` // Unix milli
    ID            *string `json:"id"`
    Domain        *string `json:"domain"`
}
```

```go
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
    // Payload có thể mang error riêng dù JSON-RPC báo ok.
    if results.Error != nil && strings.TrimSpace(*results.Error) != "" {
        return nil
    }
    return &results
}
```

Dùng con trỏ cho `Snippet`, `PublishedDate`, `Error` để phân biệt "field không có" với "field rỗng". Với `Error` thì khác biệt này quyết định thành/thất bại.

---

# 5. Rotate account và bẫy "empty results"

Search chạy qua pool giống mọi request khác, nhưng có một bẫy riêng.

```go
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
            // HTTP/RPC ok nhưng payload không dùng được. KHÔNG RecordSuccess,
            // KHÔNG trả summary rỗng — đổi account thử lại.
            lastErr = fmt.Errorf("MCP web_search returned unparseable or error search payload")
            excluded[account.ID] = true
            h.handleAccountFailure(account, lastErr)
            continue
        }

        h.pool.RecordSuccess(account.ID)
        return results, toolUseID, account, nil
    }
    if lastErr == nil {
        lastErr = fmt.Errorf("no available accounts for web_search")
    }
    return nil, "", nil, lastErr
}
```

**Bẫy:** cách hiện thực sai phổ biến nhất là khi parse thất bại thì trả `*WebSearchResults` rỗng rồi tiếp tục như bình thường. Client nhận một câu trả lời trông hợp lệ nói "không tìm thấy kết quả nào", trong khi thật ra search chưa từng chạy. Không có lỗi ở đâu cả, metric vẫn xanh.

Phân biệt hai ca này:

| Payload | `results` | Xử lý |
|---|---|---|
| `{"results": []}` — parse được, mảng rỗng | non-nil, len 0 | **Thành công.** Search chạy rồi, thật sự không có kết quả |
| Parse lỗi, hoặc có `error` field | nil | **Thất bại.** Đổi account, hết account thì trả HTTP error |

Ranh giới là *parse được hay không*, không phải *có kết quả hay không*.

Khi hết account, trả lỗi thật kèm status map đúng loại, đừng trả body rỗng:

```go
status, errType := 502, "api_error"
if isAuthErrorMessage(err.Error()) {
    status, errType = 401, "authentication_error"
} else if isQuotaErrorMessage(err.Error()) {
    status, errType = 429, "rate_limit_error"
}
h.sendClaudeError(w, status, errType, "Web search failed: "+err.Error())
```

---

# 6. Đường nhanh: chỉ có web_search

Khi request chỉ mang đúng một native web_search tool, không cần gọi Kiro chat chút nào — trích query, gọi MCP, tự tổng hợp response.

## Trích query

Claude Code/Desktop prefix message bằng một câu cố định:

```go
const webSearchQueryPrefix = "Perform a web search for the query:"
```

Quét user turn từ **cuối lên**, không phải lấy message đầu — request multi-turn sẽ lấy sai query nếu lấy message đầu:

```go
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
    // Prefix có thể kèm space phía sau; TrimSpace sau khi cắt xử lý cả hai dạng.
    if strings.HasPrefix(trimmed, webSearchQueryPrefix) {
        trimmed = strings.TrimSpace(trimmed[len(webSearchQueryPrefix):])
    }
    return trimmed
}
```

`Content` của Claude có ba shape: `string`, `[]interface{}` (JSON đã decode), và `[]ClaudeContentBlock` (struct). Handle cả ba, và trong một turn thì quét block từ cuối lên:

```go
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
```

Query rỗng thì trả 400, đừng gọi MCP với query rỗng.

## Bốn content block trả về

Đúng thứ tự này, đây là contract client mong đợi:

```go
func buildWebSearchContentBlocks(query, toolUseID string, results *WebSearchResults) []map[string]interface{} {
    return []map[string]interface{}{
        // 1. Model "quyết định" search.
        {"type": "text", "text": fmt.Sprintf("I'll search for %q.", query)},
        // 2. Lời gọi server tool.
        {
            "id":    toolUseID,
            "type":  "server_tool_use",
            "name":  webSearchToolName,
            "input": map[string]interface{}{"query": query},
        },
        // 3. Kết quả thô.
        {
            "type":    "web_search_tool_result",
            "content": webSearchResultContent(results),
        },
        // 4. Summary cho model/người đọc.
        {"type": "text", "text": generateSearchSummary(query, results)},
    }
}
```

Block kết quả từng phần tử:

```go
func webSearchResultContent(results *WebSearchResults) []map[string]interface{} {
    out := make([]map[string]interface{}, 0)   // slice rỗng, không nil
    if results == nil {
        return out
    }
    for _, r := range results.Results {
        var pageAge interface{}   // interface{} để giữ null khi thiếu
        if r.PublishedDate != nil {
            pageAge = time.UnixMilli(*r.PublishedDate).UTC().Format("January 2, 2006")
        }
        encryptedContent := ""
        if r.Snippet != nil {
            encryptedContent = *r.Snippet
        }
        out = append(out, map[string]interface{}{
            "type":              "web_search_result",
            "title":             r.Title,
            "url":               r.URL,
            "encrypted_content": encryptedContent,
            "page_age":          pageAge,
        })
    }
    return out
}
```

Ba chi tiết dễ sai:

- `web_search_tool_result` **không** có field `tool_use_id`. API thật không gửi, thêm vào là lệch contract.
- Khởi tạo `out` bằng `make(...)` chứ không để nil — nil marshal thành `null`, client mong `[]`.
- `page_age` để `interface{}` để nó ra `null` khi không có `publishedDate`, đừng ép thành string rỗng.

## Summary

```go
func generateSearchSummary(query string, results *WebSearchResults) string {
    var b strings.Builder
    fmt.Fprintf(&b, "Here are the search results for %q:\n\n", query)
    if results != nil && len(results.Results) > 0 {
        for i, r := range results.Results {
            fmt.Fprintf(&b, "%d. **%s**\n", i+1, r.Title)
            if r.Snippet != nil && *r.Snippet != "" {
                snippet := *r.Snippet
                runes := []rune(snippet)          // cắt theo rune, không theo byte
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
```

Cắt theo rune là bắt buộc — cắt theo byte sẽ chẻ đôi ký tự UTF-8 và sinh JSON hỏng với snippet tiếng Việt/Trung/Nhật.

## SSE sequence

Index tăng dần theo block, mỗi block đủ cặp `content_block_start` / `content_block_stop`:

```
message_start                                    stop_reason: null

content_block_start   index 0   text ""
content_block_delta   index 0   text_delta  "I'll search for ..."
content_block_stop    index 0

content_block_start   index 1   server_tool_use (input đầy đủ ngay tại start)
content_block_stop    index 1

content_block_start   index 2   web_search_tool_result (content đầy đủ tại start)
content_block_stop    index 2

content_block_start   index 3   text ""
content_block_delta   index 3   text_delta  (summary, chia chunk ~100 rune)
content_block_delta   index 3   ...
content_block_stop    index 3

message_delta                   stop_reason: "end_turn" + usage
message_stop
```

Điểm khác so với tool_use thường: `server_tool_use` mang `input` **đầy đủ ngay trong** `content_block_start`, **không** phát `input_json_delta`. Với client tool bình thường thì input mới stream dần qua `input_json_delta`.

Chia chunk summary an toàn UTF-8:

```go
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
```

## Usage

Cả SSE và JSON đều nên báo số lần search:

```go
"usage": map[string]interface{}{
    "input_tokens":                inputTokens,
    "output_tokens":               outputTokens,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens":     0,
    "server_tool_use": map[string]interface{}{
        "web_search_requests": 1,
    },
},
```

Đường này không gọi Kiro chat nên credits = 0. Vẫn ghi log usage để search không thành ra miễn phí vô hình trong thống kê.

Bản non-stream trả **cùng** bộ content block, chỉ khác vỏ JSON — dùng lại `buildWebSearchContentBlocks` cho cả hai để chúng không lệch nhau.

---

# 7. Đường trộn tool: agentic loop

Khi native web_search đi cùng tool của client (Bash, Read, exec…), không thể dùng đường nhanh: model cần tự quyết định khi nào search, và tool của client phải về tới client nguyên vẹn.

## Bước chuẩn bị: sửa tool conversion

Hai việc trong hàm convert tool sang định dạng Kiro:

```go
for _, tool := range tools {
    // 1. KHÔNG forward native web_search vào Kiro. Nếu forward, model sẽ phát
    //    client-side tool_use mà host không thực thi được.
    if isNativeWebSearchTool(tool) {
        continue
    }
    // ... convert tool bình thường
}

// 2. Nhưng model vẫn cần biết nó CÓ THỂ search. Inject một function schema
//    thường cho web_search — loop sẽ tự thực thi.
if hasNativeWebSearchInTools(tools) && len(result) > 0 && !hasKiroWebSearchTool(result) {
    w := KiroToolWrapper{}
    w.ToolSpecification.Name = webSearchToolName
    w.ToolSpecification.Description = "Search the web for up-to-date information."
    w.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "query": map[string]interface{}{
                "type":        "string",
                "description": "Search query.",
            },
        },
        "required": []interface{}{"query"},
    }}
    result = append(result, w)
}
```

Ba điều kiện của khối inject đều cần thiết: `hasNativeWebSearchInTools` để chỉ inject khi client thật sự khai báo native, `len(result) > 0` để khỏi inject khi lọc xong không còn tool nào (ca đó đã đi đường nhanh), `!hasKiroWebSearchTool` để không inject trùng khi client tự khai một tool tên `web_search`.

## Cấu trúc loop

Mỗi round buffer trọn một Kiro stream rồi quyết định: search tiếp, hay flush ra client.

```go
const maxWebSearchRounds = 5

type webSearchRoundOutcome struct {
    text               string
    toolUses           []KiroToolUse
    inputTokens        int
    credits            float64
    stopReasonOverride string
}
```

```go
func (h *Handler) runWebSearchLoop(w http.ResponseWriter, req *ClaudeRequest, thinking bool, estimatedInputTokens int, apiKeyID string) {
    // Bản copy để mutate khi feed kết quả search vào history.
    working := *req
    working.Messages = append([]ClaudeMessage(nil), req.Messages...)

    presentation := make([]map[string]interface{}, 0)  // block trả client
    var lastAccountID string
    var totalCredits float64
    maxUses := resolveWebSearchMaxUses(req.Tools)
    searchCount := 0

    // <= maxUses: cho phép một vòng nữa để flush sau round search cuối.
    for roundIdx := 0; roundIdx <= maxUses; roundIdx++ {
        round, account, err := h.callUpstreamForWebSearch(&working, thinking, estimatedInputTokens)
        if err != nil { /* map status, sendClaudeError, return */ }
        if account != nil {
            lastAccountID = account.ID
        }
        totalCredits += round.credits

        // Search tiếp chỉ khi MỌI tool_use đều là web_search và còn budget.
        roundSearchN := countWebSearchToolUses(round.toolUses)
        if shouldSearchRound(roundIdx, round.toolUses, maxUses) && searchCount+roundSearchN <= maxUses {
            searched, searchErr := h.searchAllWebUses(req.Model, round.toolUses)
            if searchErr != nil { /* sendClaudeError, return */ }
            searchCount += roundSearchN
            appendSearchRound(&working, round, searched, &presentation)
            continue
        }

        // Round cuối: chạy web_search còn lại, tool client pass-through.
        // ... build content, resolve stop_reason, render SSE hoặc JSON
        return
    }
}
```

Điều kiện tiếp tục: **mọi** tool_use trong round đều phải là web_search. Chỉ cần một tool của client xuất hiện là phải flush — tool đó thuộc về client, proxy không được tự chạy.

```go
func shouldSearchRound(roundIdx int, toolUses []KiroToolUse, maxUses int) bool {
    if maxUses <= 0 {
        maxUses = maxWebSearchRounds
    }
    if len(toolUses) == 0 || roundIdx >= maxUses {
        return false
    }
    for _, tu := range toolUses {
        if tu.Name != webSearchToolName {
            return false   // có tool client → flush
        }
    }
    return true
}
```

Budget luôn phải có chặn trên, kể cả khi client xin nhiều hơn:

```go
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
        return maxWebSearchRounds   // client không được mở loop vô hạn
    }
    return maxUses
}
```

## Feed kết quả vào history

Sau mỗi round search, thêm hai message vào working copy: assistant (text + tool_use) và user (tool_result).

```go
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

        // Song song: tích luỹ block để trình bày cho client.
        srvID, _ := createMcpRequest(query)
        *presentation = append(*presentation,
            map[string]interface{}{
                "type": "server_tool_use", "id": srvID,
                "name": webSearchToolName,
                "input": map[string]interface{}{"query": query},
            },
            map[string]interface{}{
                "type": "web_search_tool_result",
                "content": webSearchResultContent(results),
            },
        )
    }
    req.Messages = append(req.Messages, ClaudeMessage{Role: "user", Content: userContent})
}
```

**Quan trọng:** dùng `[]interface{}` cho `Content`, không dùng `[]map[string]interface{}`. Hàm extract content của bạn rất có thể chỉ nhận shape `[]interface{}` (shape mà JSON decode sinh ra) — đưa `[]map` vào thì tool_use/tool_result bị bỏ im lặng khi convert sang payload Kiro. Tốt nhất là sửa hàm extract nhận cả hai shape làm phòng tuyến thứ hai.

Hai luồng chạy song song, đừng lẫn:

- **History** (`req.Messages`) — dùng `tool_use` / `tool_result` như tool thường, để model hiểu.
- **Presentation** (`presentation`) — dùng `server_tool_use` / `web_search_tool_result`, để client hiểu là server tool.

## Flush

```go
func resolveFlushStopReason(override string, toolUses []KiroToolUse, content []map[string]interface{}) string {
    if override != "" {
        return override
    }
    // Chỉ tool CLIENT mới cho ra tool_use; web_search đã tiêu hoá nội bộ.
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
    return "end_turn"
}
```

Round chỉ có web_search → `end_turn` (client không phải làm gì). Có tool client → `tool_use` (client cần chạy rồi gọi lại).

Content cuối = presentation tích luỹ + text round cuối + tool_use còn lại:

```go
content := append([]map[string]interface{}{}, presentation...)
if strings.TrimSpace(text) != "" {
    content = append(content, map[string]interface{}{"type": "text", "text": text})
}
for i, tu := range toolUses {
    if tu.Name == webSearchToolName {
        // chuyển thành server_tool_use + web_search_tool_result
        continue
    }
    // tool client giữ nguyên dạng tool_use
}
```

Khi render SSE, `server_tool_use` và `web_search_tool_result` chỉ cần `content_block_start` + `content_block_stop` (nội dung đầy đủ ngay tại start); `tool_use` của client thì cần thêm `input_json_delta`; `text` cần `text_delta`.

---

# 8. Checklist test

Hàm thuần, test trực tiếp:

- `isNativeWebSearchTool` — nhận native, **từ chối** client tool tên `web_search` mà không có `type`.
- `hasWebSearchTool` / `hasWebSearchAmongTools` — loại trừ nhau, cả hai false khi không có tool nào.
- `extractSearchQuery` — có prefix, không prefix, content dạng array, multi-turn lấy turn cuối, rỗng.
- `parseSearchResults` — payload hợp lệ, `{"results":[]}` là **thành công**, error nhúng, `isError`, content không phải text, nil.
- `chunkByRunes` — chuỗi multibyte không bị chẻ ký tự.
- `generateSearchSummary` — có kết quả, không kết quả, snippet dài bị cắt theo rune.
- `resolveWebSearchMaxUses` — không set, set nhỏ, set lớn hơn `maxWebSearchRounds` bị cap.
- `shouldSearchRound` — toàn web_search, có tool client, tool rỗng, quá số round.
- `resolveFlushStopReason` — web_search-only ra `end_turn`, có tool client ra `tool_use`, override thắng.
- `buildFlushContent` — web_search thành `server_tool_use`, tool client giữ nguyên.

Test tích hợp, cần upstream giả:

- `appendSearchRound` xong thì tool_use/tool_result **sống sót** qua bước convert sang payload Kiro. Đây là test đáng giá nhất — nó bắt đúng bug shape `[]interface{}` ở trên.
- MCP trả 200 nhưng payload không parse được → rotate account, không trả summary rỗng.
- Hết account → HTTP error, không phải body rỗng.
- SSE đường nhanh đúng thứ tự event và index.
- Round trộn: web_search tiêu hoá nội bộ, tool client về tới client.

---

# 9. Tóm tắt các bẫy

| Bẫy | Hậu quả |
|---|---|
| Forward native web_search vào Kiro như tool thường | Conversation treo, client không chạy được server tool |
| Chỉ kiểm `name == "web_search"`, bỏ `type` | Chặn tool của client, tự chạy search thay họ |
| Parse lỗi → trả results rỗng | Client tưởng "không có kết quả", search chưa từng chạy, không lỗi ở đâu |
| Region lấy từ `Account.Region` | MCP request tới data plane không giữ profile, bị từ chối |
| Thiếu header `x-amzn-kiro-profile-arn` | MCP từ chối account thường (account API-key thì không cần) |
| Thêm `tool_use_id` vào `web_search_tool_result` | Lệch contract Anthropic |
| Cắt snippet theo byte | JSON hỏng với text non-ASCII |
| `math/rand` cho request ID | ID lặp giữa các process |
| Dùng `[]map` thay `[]interface{}` cho message content | tool_use/tool_result bị bỏ im lặng khi convert |
| Không cap `max_uses` | Client mở loop không giới hạn |
| Lấy message đầu làm query | Sai query ở request multi-turn |
| `presentation` để nil thay vì `make(...)` | Marshal ra `null`, client mong `[]` |
