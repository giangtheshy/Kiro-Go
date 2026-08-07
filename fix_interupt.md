# Kiro-Go: turn bị ngắt mà proxy báo là hoàn tất

Ba phần dưới cùng một gốc: proxy không phân biệt được "upstream kết thúc turn" với "upstream đứt giữa turn". Cả ba đều nằm ở `proxy/kiro.go` (parser + endpoint loop) và các handler trong `proxy/`.

Mọi vị trí dưới đây gọi theo **tên hàm/biến**, không theo số dòng — số dòng sẽ lệch tùy bản.

---

# 1. Stream đóng sạch mà không có `meteringEvent`

## Triệu chứng

Câu trả lời dừng sau một hai dòng, không có lỗi ở đâu cả. Client nhận `stop_reason: end_turn` bình thường. `failedRequests` không tăng, request log đánh dấu `success` — nên lỗi vô hình trên mọi metric.

Dấu hiệu trong log: một dòng `out=0 text=0B tools=0 credits=0.000` với thời lượng rất ngắn.

## Nguyên nhân

Upstream AWS đôi khi đóng event stream **sạch** — không error, không `io.ErrUnexpectedEOF` — mà chưa gửi `meteringEvent`. `parseEventStream` đọc prelude tiếp theo, gặp `io.EOF` đúng biên frame, break khỏi vòng lặp và `return nil`. Với caller thì đó là thành công.

Vì upstream chỉ bill ở **cuối** turn, sự vắng mặt của `meteringEvent` là tín hiệu in-band duy nhất cho biết turn chưa xong.

Hai biến thể phải xử lý khác nhau:

| | Emitted | Metered | Xử lý đúng |
|---|---|---|---|
| Stream rỗng hoàn toàn | false | false | **Retry được** — client chưa nhận gì nên không thể nhân đôi output |
| Cắt giữa turn | true | false | **Không retry** — sẽ nối thêm một câu trả lời dở dang thứ hai. Chỉ được withhold `stop_reason` + báo lỗi |

Hai giả thuyết **đã loại**, đừng mất thời gian: (1) không phải issue #142 — đường đó có log và có tăng `failedRequests`; (2) không phải `maxPayloadBytes` — nâng 900KB→1400KB cho ra zero dòng `[Truncate]` trên production, và những turn ngắn mà `credits > 0` là câu trả lời ngắn thật, không phải bị proxy cắt.

## Fix — tầng parser (`proxy/kiro.go`)

Thêm hai field vào struct outcome mà `parseEventStreamTracked` trả về:

```go
type StreamOutcome struct {
    // Emitted bật khi có bất cứ thứ gì client thấy được:
    // assistant text, reasoning text, hoặc tool use.
    Emitted bool
    // Metered bật khi upstream gửi meteringEvent. Upstream bill ở cuối turn,
    // nên !Metered nghĩa là connection chết trước khi turn xong.
    Metered bool
}
```

`Emitted = true` ở chỗ forward assistant text, reasoning text, và tool use. `Metered = true` ở chỗ xử lý `meteringEvent`.

## Fix — endpoint loop (`proxy/kiro.go`)

Trong vòng lặp endpoint, sau khi `parseEventStreamTracked` trả `err == nil`:

```go
// Nhánh 1: rỗng hoàn toàn → retry được
if !outcome.Metered && !outcome.Emitted {
    lastErr = fmt.Errorf("empty stream from %s (no output, no metering)", ep.Name)
    if emptyStreamRetries < maxEmptyStreamRetries {
        emptyStreamRetries++
        time.Sleep(streamRetryBackoff * time.Duration(emptyStreamRetries))
        i--       // retry CÙNG endpoint
        continue
    }
    time.Sleep(streamRetryBackoff)
    continue
}

// Nhánh 2: đã emit nhưng không metered → không retry, chỉ báo
if !outcome.Metered {
    logger.Warnf("[KiroAPI] Endpoint %s stream ended without metering after partial output", ep.Name)
    if callback != nil && callback.OnTruncated != nil {
        callback.OnTruncated()
    }
}
return nil
```

Ba điểm dễ làm sai:

**`i--` chứ không phải `continue` trơn.** Account API-key chỉ có **một** CLI endpoint từ `endpointsForAccount`. Dùng `continue` thì vòng lặp kết thúc ngay và fail cứng với `empty stream...` dù chưa thử lại lần nào — trong khi log vẫn ghi "retrying". Phải retry cùng endpoint.

**Phải có chặn trên.** `maxEmptyStreamRetries = 3` là đủ cho các blip đã thấy trên production (chúng tự khỏi trong vài giây) mà không để một outage kéo dài làm request spin vô hạn.

**Refusal phải xử lý TRƯỚC nhánh empty-stream.** Content-filter refusal cũng về với no content + no metering, sẽ khớp nhánh 1 y hệt. Nhưng cùng một payload thì bị từ chối như nhau ở mọi account, nên retry hay rotate đều vô ích — phải return ngay để lời giải thích tới được client. Kiểm cả khi đã emit: upstream có thể filter giữa turn.

Kèm theo: refusal **không** được tính là lỗi account. Nếu tính, một conversation bị filter sẽ lần lượt đẩy cả pool vào cooldown, vì mọi account đều cho cùng verdict. Trong `handleAccountFailure`, thoát sớm nếu error là refusal.

## Fix — tầng response

Thêm callback vào struct:

```go
// OnTruncated fires khi stream đóng sạch sau khi đã emit content nhưng
// không có meteringEvent.
OnTruncated func()
```

Ở Claude stream handler: set `truncated = true` trong callback, rồi khi kết thúc:

```go
if truncated {
    // Vẫn báo usage, nhưng BỎ stop_reason
    h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
        "type":  "message_delta",
        "delta": map[string]interface{}{},
        "usage": buildClaudeUsageMap(...),
    })
    h.sendSSE(w, flusher, "error", map[string]interface{}{
        "type": "error",
        "error": map[string]string{
            "type":    "api_error",
            "message": "upstream ended the stream before the turn completed",
        },
    })
    return   // KHÔNG gửi message_stop
}
```

Anthropic client quyết định "response này có hoàn tất không" từ `message_delta.stop_reason`. Withhold nó, không gửi `message_stop`, text đã gửi được giữ lại — client tự biết turn chưa xong. Log turn ghi `stop=truncated` thay vì `end_turn`.

## Các đường còn phải wire

`OnTruncated` cần wire ở **mọi** handler, không chỉ Claude stream. Nhánh empty-stream trong endpoint loop đã bảo vệ tất cả (nó nằm trong code dùng chung), nhưng ca cắt-giữa-turn thì mỗi handler phải tự báo:

| Handler | Hiện vẫn phát |
|---|---|
| `handleClaudeNonStream` | `stop_reason` bình thường |
| `handleOpenAIStream` | `finish_reason: "stop"` |
| `handleOpenAINonStream` | `finish_reason` bình thường |
| `handleResponsesStream` | `response.completed` |
| `handleResponsesNonStream` | `Status: "completed"` |
| `callUpstreamForWebSearch` | xem phần 2 |

Ưu tiên OpenAI stream/non-stream — hai đường có traffic thật ngoài Claude.

Hướng cho từng loại:

- **Responses API**: dễ nhất, đã có sẵn event `response.failed` — chỉ cần gửi nó thay `response.completed`.
- **OpenAI stream**: OpenAI không có cách "withhold" `finish_reason` như Anthropic. Hai lựa chọn — bỏ chunk `finish_reason` cuối rồi đóng bằng `data: [DONE]`, hoặc gửi `finish_reason: "length"` (giá trị gần nhất về ngữ nghĩa). Nên xác nhận client thật phản ứng thế nào với chunk thiếu `finish_reason` trước khi chốt.
- **Non-stream**: response đã buffer nên gọn hơn — đặt flag rồi trả HTTP error, hoặc `finish_reason: "length"`.

## Test

Parser-level:

- Cắt **giữa** frame → vẫn phải là error (prelude hứa nhiều byte hơn số byte tới).
- Cắt đúng **biên** frame → đây là ca im lặng; assert `Emitted=true, Metered=false`.
- Body rỗng → `Emitted=false, Metered=false`, và **không** phải parse error.
- Có `meteringEvent` nhưng không có content → `Metered=true`, **không** được retry (upstream đã bill, resend là trả tiền hai lần).
- Reasoning text và tool use đều tính là `Emitted` — chúng tới client được nên retry sẽ nhân đôi.
- Callback `nil` vẫn phải là no-op.

Handler-level (dựng upstream giả bằng `httptest`, một endpoint, tắt fallback):

- Turn cắt giữa → assert text vẫn tới client, **không** có `"stop_reason":"end_turn"`, có `event: error`, không có `message_stop`. Lưu ý: `message_start` luôn mang `"stop_reason":null`, nên phải assert riêng trên event `message_delta`, đừng substring cả body.
- Turn metered → giữ nguyên `end_turn` + `message_stop`, không có error event (chống regress).
- Attempt 1 rỗng, attempt 2 có content → assert cùng endpoint được gọi ≥2 lần và content của lần 2 tới client.
- Upstream rỗng mãi → assert đúng `maxEmptyStreamRetries + 1` lần gọi.

---

# 2. web_search loop

Đây là đường **tệ nhất**, vì nó không chỉ mất tín hiệu truncation một lần mà nhân lỗi lên qua nhiều round.

## Vấn đề

`runWebSearchLoop` gọi `callUpstreamForWebSearch` tối đa `maxUses + 1` lần, mỗi lần buffer trọn một Kiro stream rồi quyết định: search tiếp, hay flush ra client. Callback trong `callUpstreamForWebSearch` không có `OnTruncated`.

**Ca A — round giữa bị cắt.** `round.text` và `round.toolUses` chỉ có một phần. `shouldSearchRound` xét trên tập tool_use dở dang đó. Nếu stream đứt trước khi `web_search` tool_use kịp về thì `len(toolUses) == 0` → `shouldSearchRound` trả false → loop nhảy sang nhánh terminal và flush câu trả lời dở dang **như thể model đã tự quyết định dừng search**. Không log nào nói round đó bị cắt.

Nặng hơn: `appendSearchRound` ghi `round.text` vào history như một assistant turn hoàn chỉnh. Các round sau build tiếp trên nền text dở dang đó.

**Ca B — round cuối bị cắt.** `resolveFlushStopReason` mặc định trả `"end_turn"`. Cả hai renderer phát giá trị đó vô điều kiện — `renderWebSearchLoopSSE` gửi `stop_reason` rồi `message_stop`, `renderWebSearchLoopJSON` gửi `stop_reason`. Client nhận một turn "hoàn tất".

**Ca C — kế toán sai.** `totalCredits` cộng dồn qua mọi round. Round bị cắt giữa có thể đã bị bill một phần, round rỗng-unmetered thì không. Loop không phân biệt nên `recordSuccessLogUsage` ghi cả conversation là `success` với tổng credits, và `RecordSuccess(lastAccountID)` reset trạng thái lỗi của account.

**Ca D — `OnError` là dead code.** Callback struct **khai báo** field `OnError` nhưng grep toàn bộ `kiro.go` không thấy chỗ nào **gọi** nó. Các handler vẫn set field này và gán `lastErr` trong đó — code chết. Hiện chưa gây hại vì `CallKiroAPI` trả error qua return value và nhánh `if err != nil` xử lý đúng, nhưng ai đọc code cũng sẽ tưởng `OnError` có tác dụng. Chọn một: gọi nó trong parser, hoặc xoá khỏi struct.

## Hướng fix

1. Thêm field `truncated bool` vào struct outcome của round (`webSearchRoundOutcome`), set qua `OnTruncated`.
2. Trong `runWebSearchLoop`, sau khi có `round`: nếu `round.truncated` thì **không** vào nhánh search tiếp — một tập tool_use dở dang không đáng để chạy MCP search, và không được `appendSearchRound` nó vào history. Thoát loop, flush với tín hiệu lỗi.
3. Cho `resolveFlushStopReason` nhận thêm cờ truncated, hoặc thêm tham số `truncated` vào hai renderer để chúng withhold `stop_reason` + phát `event: error` (SSE) giống Claude stream; bản JSON trả `stop_reason: null` hoặc HTTP error.
4. Không gọi `RecordSuccess` khi round cuối bị cắt.

## Test

Test hiện có cho web_search đều là unit test cho hàm thuần (`shouldSearchRound`, `buildFlushContent`, `resolveFlushStopReason`, parse/summary…). Không có test nào dựng upstream giả để chạy `runWebSearchLoop` end-to-end, và không có test nào chạm `Metered` hay truncation.

Cần thêm:

- Round 1 trả web_search tool_use + metering, round 2 cắt giữa → assert không phát `end_turn`, không `message_stop`, text round 1 vẫn còn.
- Round 1 cắt giữa ngay → assert **không** chạy MCP search, và không ghi gì vào history.
- Round rỗng-unmetered → assert được retry ở tầng endpoint loop (đường này đúng sẵn, test để khỏi regress).

---

# 3. Tool call bị cắt giữa argument JSON

## Vấn đề

`finishToolUse` build `KiroToolUse` từ buffer argument. Nếu stream đứt giữa lúc model đang viết argument JSON, buffer không parse được — mà nếu vẫn forward tool call với `input` rỗng thì **client sẽ thực thi tool với tham số model chưa viết xong**.

## Fix cơ bản

```go
var errIncompleteKiroToolInput = errors.New("upstream stream ended with incomplete tool input")

// trong finishToolUse:
if state.InputBuffer.Len() > 0 {
    if err := json.Unmarshal([]byte(state.InputBuffer.String()), &input); err != nil {
        return fmt.Errorf("%w: %v", errIncompleteKiroToolInput, err)
    }
}
```

Lỗi này xảy ra **trước** khi `OnToolUse` được gọi, nên `outcome.Emitted` vẫn false → endpoint loop được phép retry. Đúng.

Cũng phải flush pending tool ở cuối stream: khi vòng lặp đọc frame kết thúc, nếu còn `currentToolUse` treo thì gọi `finishToolUse` cho nó — nếu không, tool call cuối biến mất khi upstream đóng đúng biên frame.

## Lỗ hở 1 — guard đầu hàm nuốt lỗi

```go
if state == nil || state.Name == "" || callback == nil || callback.OnToolUse == nil {
    return nil
}
```

Hai nhánh trả `nil` **im lặng trước khi kịp validate JSON**:

- `state.Name == ""` — tool use có ID nhưng thiếu name bị bỏ hoàn toàn, không log. Upstream cắt ngay sau `toolUseId` mà chưa gửi `name` thì tool call biến mất và turn vẫn được coi là hoàn tất.
- `callback.OnToolUse == nil` — handler nào không set `OnToolUse` sẽ **không bao giờ** phát hiện tool input dở dang, vì hàm return trước `json.Unmarshal`.

Lỗ thứ hai hiện chưa nổ nếu mọi call site đều set `OnToolUse` (kể cả no-op `func(tu KiroToolUse) {}`). Nhưng đây là bẫy: thêm handler mới mà quên là mất validation, không có gì báo.

Fix: tách validate JSON ra **trước** guard. `OnToolUse == nil` chỉ nên là điều kiện bỏ qua việc *forward*, không phải điều kiện bỏ qua việc *kiểm tra*. Và log warn khi bỏ tool use vì `Name == ""`.

## Lỗ hở 2 — tool thứ hai bị cắt sau khi tool thứ nhất đã ra

Nếu stream forward xong tool call #1 (→ `Emitted = true`) rồi cắt giữa argument của tool #2, error về tới nhánh `if outcome.Emitted { return err }` nên không retry — đúng.

Nhưng phía handler, error đó rơi vào nhánh xử lý lỗi chung. `errIncompleteKiroToolInput` không được handler nào phân biệt với một lỗi mạng thường, nên nó đi vào `handleAccountFailure` → rơi vào `default: RecordError`. Tính là lỗi account dù thật ra là lỗi stream; đủ số lần là account vào cooldown oan.

Cần kiểm thêm: sau khi đã gửi `content_block_start` cho tool #1, handler xử lý error muộn thế nào — có đóng block đang mở, có gửi `event: error`, hay chỉ gọi hàm send-error vào một response đã ghi header rồi. Ca này có thật vì model hay gọi nhiều tool trong một turn.

---

# Điểm cần kiểm khi thêm retry ở bất cứ đâu

Empty-stream retry bằng `i--` xảy ra **bên trong** `CallKiroAPI`, nên nó không reset các biến tích luỹ phía handler. Với nhánh này thì an toàn vì điều kiện retry là `!Emitted`, handler chưa tích luỹ gì.

Nhưng `OnComplete` được gọi ở cuối parser **kể cả** với stream rỗng — nó fire một lần với `(0, 0)` rồi fire lại với giá trị thật ở lần retry. An toàn nếu handler **gán** (`inputTokens = inTok`), sai nếu handler **cộng dồn** (`inputTokens += inTok`). Kiểm từng body `OnComplete` trước khi thêm retry ở đường mới.

---

# Bug liên quan, không cùng gốc

`isOverageErrorMessage` tìm chuỗi `402`, nhưng lỗi overage thật của Kiro về **HTTP 400** với body:

```json
{"__type":"...ServiceQuotaExceededException",
 "message":"You have reached the limit for overages.",
 "reason":"OVERAGE_REQUEST_LIMIT_EXCEEDED"}
```

Nên nó bị phân loại thành `quota` thay vì `overage`, và không kích hoạt đường refresh overage status. Sửa: match `OVERAGE_REQUEST_LIMIT_EXCEEDED` hoặc `limit for overages` thay vì mã số.
