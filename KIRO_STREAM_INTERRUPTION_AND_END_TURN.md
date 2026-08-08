# Hướng dẫn Kỹ thuật Khắc phục Lỗi Kiro/Claude Stream Bị Ngắt & Lỗi `end_turn`
*(Tài liệu Độc lập - Mã nguồn Go Hoàn chỉnh Tích hợp Trực tiếp trong Nội dung)*

Tài liệu này phân tích bản chất kỹ thuật và cung cấp **toàn bộ mã nguồn Go độc lập** (không phụ thuộc bất kỳ thư viện ngoài hay dự án cụ thể nào) để giải quyết hai vấn đề nghiêm trọng khi kết nối với upstream Kiro / AWS CodeWhisperer (Claude):

1. **Stream bị ngắt giữa dòng (Stream Interruption)**: Socket ngắt giữa chừng làm client báo `API Error: Server error mid-response. The response above may be incomplete.` hoặc bị ngắt kết nối do rảnh (`Connection closed mid-response` / Cloudflare 524).
2. **Turn kết thúc sai (`end_turn` trên response rỗng hoặc bị ngắt)**: Client hiểu nhầm câu trả lời đã hoàn tất nên tự động dừng task giữa câu, hoặc báo `API returned an empty or malformed response (HTTP 200)`.

---

## 1. Phân Loại Lỗi Stream & Giải Mã AWS EventStream

### 1.1 Bản chất lỗi tầng Transport
Upstream Kiro trả về dữ liệu chuẩn **AWS EventStream** (Binary Frame) trên HTTP Status 200 và **không có frame kết thúc bắt buộc**. Nếu chỉ kiểm tra `err == io.EOF`, 5 trường hợp sau trông **giống hệt nhau** ở tầng TCP:

- **Kết thúc sạch**: EOF đúng biên frame, không còn byte dư trong buffer.
- **Đứt giữa frame**: EOF nhưng còn byte dở dang trong buffer (`truncated`).
- **Lỗi đọc socket**: Lỗi kết nối mạng/socket reset (`read`).
- **Byte rác / Prelude lỗi**: `totalLen` / `headersLen` vượt ngưỡng an toàn (`corrupt`).
- **Lỗi in-band từ upstream**: Header chứa `:message-type: exception` (`exception`).
- **Response rỗng**: EOF sạch nhưng 0 frame nội dung (`empty`).

Nếu không phân loại, một stream bị ngắt dở (`truncated`) sẽ bị hệ thống hiểu nhầm là thành công và bắn `stop_reason: "end_turn"`. Client sẽ im lặng chấp nhận một câu trả lời bị cắt giữa chừng.

### 1.2 Mã nguồn Go: Bộ đọc & Phân loại AWS EventStream

Dưới đây là mã nguồn phân giải AWS EventStream binary frame, tự động phát hiện exception frame và bắt các lỗi đứt socket mid-stream:

```go
package kirostream

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	esPreludeLen   = 12 // total_len(4) + headers_len(4) + prelude_crc(4)
	esMsgCRCLen    = 4  // trailing message CRC
	esMinMsgBytes  = esPreludeLen + esMsgCRCLen
	esMaxMsgBytes  = 25 * 1024 * 1024 // 25 MiB
	esMaxHeaderLen = 128 * 1024       // 128 KiB
)

// FrameError mô tả lý do stream dừng bất thường.
type FrameError struct {
	Kind    string // "truncated", "read", "corrupt", "exception", "empty"
	Type    string // Mã lỗi từ upstream (khi Kind == "exception")
	Message string // Thông điệp chi tiết
	Err     error  // Lỗi transport gốc
}

func (e *FrameError) Error() string {
	if e.Message != "" && e.Type != "" {
		return fmt.Sprintf("kiro stream %s (%s): %s", e.Kind, e.Type, e.Message)
	}
	if e.Message != "" {
		return fmt.Sprintf("kiro stream %s: %s", e.Kind, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("kiro stream %s: %v", e.Kind, e.Err)
	}
	return "kiro stream " + e.Kind
}

func (e *FrameError) Unwrap() error { return e.Err }

// StreamErrorMessage chuyển đổi lỗi thành thông điệp trả về cho client.
func StreamErrorMessage(err error) string {
	var fe *FrameError
	if errors.As(err, &fe) {
		if fe.Message != "" {
			return fe.Message
		}
		if fe.Type != "" {
			return "upstream error: " + fe.Type
		}
		switch fe.Kind {
		case "truncated":
			return "upstream connection closed mid-response"
		case "read":
			return "upstream read error"
		case "corrupt":
			return "malformed upstream stream"
		case "empty":
			return "upstream returned an empty response"
		}
	}
	if err != nil {
		return err.Error()
	}
	return "upstream stream error"
}

// FrameReader phân giải từng AWS EventStream frame từ io.Reader.
func FrameReader(r io.Reader, cb func(eventType string, payload []byte)) error {
	var buf []byte
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for len(buf) >= esPreludeLen {
				total := binary.BigEndian.Uint32(buf[0:4])
				headersLen := binary.BigEndian.Uint32(buf[4:8])

				// BẮT BỘC: Guard độ dài chống slice out-of-bounds gây panic goroutine
				if total < esMinMsgBytes || total > esMaxMsgBytes || headersLen > esMaxHeaderLen ||
					headersLen+esMinMsgBytes > total {
					return &FrameError{Kind: "corrupt", Message: "invalid event-stream frame prelude"}
				}
				if uint32(len(buf)) < total {
					break // Chờ thêm bytes cho đủ frame
				}
				headers := buf[esPreludeLen : esPreludeLen+headersLen]
				payload := buf[esPreludeLen+headersLen : total-esMsgCRCLen]

				if ferr := frameException(headers, payload); ferr != nil {
					return ferr
				}
				cb(eventTypeFromHeaders(headers), append([]byte(nil), payload...))
				buf = buf[total:]
			}
		}
		if err != nil {
			if err == io.EOF {
				if len(buf) > 0 {
					return &FrameError{Kind: "truncated", Message: "connection closed mid-frame"}
				}
				return nil // Kết thúc sạch
			}
			return &FrameError{Kind: "read", Err: err}
		}
	}
}

func frameException(headers, payload []byte) *FrameError {
	h := decodeStringHeaders(headers)
	mt := h[":message-type"]
	if mt != "exception" && mt != "error" {
		return nil
	}
	typ := h[":exception-type"]
	if typ == "" {
		typ = h[":error-code"]
	}
	msg := h[":error-message"]
	if msg == "" {
		var o struct {
			Message string `json:"message"`
			Type    string `json:"__type"`
		}
		if json.Unmarshal(payload, &o) == nil {
			if o.Message != "" {
				msg = o.Message
			}
			if typ == "" {
				typ = o.Type
			}
		}
	}
	return &FrameError{Kind: "exception", Type: typ, Message: msg}
}

func decodeStringHeaders(headers []byte) map[string]string {
	out := map[string]string{}
	offset := 0
	for offset < len(headers) {
		nameLen := int(headers[offset])
		offset++
		if nameLen == 0 || nameLen > len(headers)-offset {
			return out
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			return out
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String Header Type
			if len(headers)-offset < 2 {
				return out
			}
			vlen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if vlen > len(headers)-offset {
				return out
			}
			out[name] = string(headers[offset : offset+vlen])
			offset += vlen
		} else {
			switch valueType {
			case 6:
				if len(headers)-offset < 2 {
					return out
				}
				l := int(headers[offset])<<8 | int(headers[offset+1])
				offset += 2 + l
			case 0, 1:
			case 2:
				offset += 1
			case 3:
				offset += 2
			case 4:
				offset += 4
			case 5, 8:
				offset += 8
			case 9:
				offset += 16
			default:
				return out
			}
		}
	}
	return out
}

func eventTypeFromHeaders(headers []byte) string {
	h := decodeStringHeaders(headers)
	return h[":event-type"]
}
```

---

## 2. Xử Lý Socket Timeout & Transparent Pre-Commit Retry

### 2.1 Chống ngắt socket rảnh (Cloudflare 524 / Idle Timeout) & Retry ngầm
- **SSE Heartbeat Ping (10s)**: Khi Claude suy nghĩ (reasoning) lâu, Kiro không phát ra dữ liệu. Một goroutine ticker độc lập gửi `event: ping\ndata: {"type":"ping"}\n\n` mỗi 10 giây để giữ socket kết nối luôn sống.
- **Pre-Commit Retry Window**: Buffer câu trả lời trong một cửa sổ ngắn (vd: 256 KB hoặc 4 giây). Nếu stream bị ngắt **khi chưa vượt cửa sổ này**, hệ thống xóa buffer và thực hiện retry ngầm request mới tới upstream. Client không nhận thấy lỗi vì chỉ mới nhận được `message_start` và các frame `ping`.

### 2.2 Mã nguồn Go: Heartbeat Ticker & Engine Retry Ngầm

```go
package kirostream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type StreamRetryOptions struct {
	MaxAttempts int
	WindowBytes int
	Budget      time.Duration
}

func (o StreamRetryOptions) normalized() StreamRetryOptions {
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 1
	}
	if o.WindowBytes <= 0 {
		o.WindowBytes = 256 * 1024 // 256 KB
	}
	if o.Budget <= 0 {
		o.Budget = 4 * time.Second
	}
	return o
}

// StreamRetrying phát SSE response với cơ chế Ping 10s và Retry ngầm trong cửa sổ.
func StreamRetrying(
	w http.ResponseWriter,
	opts StreamRetryOptions,
	open func(attempt int) (io.Reader, func(), error),
) {
	opts = opts.normalized()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)

	var writeMu sync.Mutex
	finalized := false

	sse := func(event string, data any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if finalized {
			return
		}
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Goroutine Keepalive Ping: Bắn ping mỗi 10 giây chống CF 524
	pingDone := make(chan struct{})
	var pingStop sync.Once
	stopPing := func() { pingStop.Do(func() { close(pingDone) }) }
	defer stopPing()

	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				sse("ping", map[string]any{"type": "ping"})
			}
		}
	}()

	// Gửi message_start ban đầu cho client
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	sse("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"content": []any{}, "stop_reason": nil,
		},
	})

	// Vòng lặp retry ngầm trong cửa sổ pre-commit
	for attempt := 0; attempt < opts.MaxAttempts; attempt++ {
		body, closeBody, err := open(attempt)
		if err != nil {
			if attempt+1 < opts.MaxAttempts {
				continue
			}
			sse("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": err.Error()},
			})
			return
		}

		var textBuf strings.Builder
		streamErr := FrameReader(body, func(eventType string, payload []byte) {
			if eventType == "assistantResponseEvent" {
				var o struct {
					Content string `json:"content"`
				}
				if json.Unmarshal(payload, &o) == nil && o.Content != "" {
					textBuf.WriteString(o.Content)
					sse("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": 0,
						"delta": map[string]any{"type": "text_delta", "text": o.Content},
					})
				}
			}
		})
		closeBody()

		if streamErr == nil && textBuf.Len() > 0 {
			// Stream thành công
			sse("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
			})
			writeMu.Lock()
			if !finalized {
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				finalized = true
			}
			writeMu.Unlock()
			return
		}

		// Nếu lỗi xảy ra khi chưa emit chữ nào, retry ở attempt tiếp theo
		if textBuf.Len() == 0 && attempt+1 < opts.MaxAttempts {
			continue
		}

		// Đã emit chữ mà vỡ stream -> Báo error SSE
		sse("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": StreamErrorMessage(streamErr)},
		})
		return
	}
}
```

---

## 3. Ghép Stream Tự Động & Nối Câu Trả Lời Giữa Chừng (Stream Continuation & Overlap Trimming)

### 3.1 Nối câu trả lời dở khi đứt stream ngoài cửa sổ
Nếu stream bị ngắt khi đã lỡ stream một lượng chữ lớn cho client (quá cửa sổ pre-commit):
1. **Không bắn event error về client**.
2. Tự động gửi một request nối tiếp (`AppendContinuationTurn`) tới upstream với câu lệnh hệ thống: *"Tiếp tục câu trả lời từ đúng chỗ bị ngắt, không lặp lại, không xin lỗi..."*.
3. Dùng hàm `TrimResumeOverlap` để cắt sạch các từ trùng lặp ở ranh giới ký tự UTF-8 (rune) giữa 2 segment.
4. Client nhận được một dòng chảy văn bản liên tục, không bị đứt đoạn hay lặp chữ.

### 3.2 Mã nguồn Go: Prompt Nối Tiếp & Thuật Toán Trim Overlap

```go
package kirostream

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// ContinuationInstruction ép model nối tiếp câu trả lời từ đúng điểm bị cắt.
const ContinuationInstruction = "[system] Your previous message was cut off mid-stream by a transport error before you finished. " +
	"Continue that message from exactly where it stopped — resume mid-sentence if it was cut mid-sentence. " +
	"Do not repeat, restate or summarize what you already wrote, do not start over, do not apologize or mention this interruption. " +
	"Output only the remaining part of the answer. If the message was cut off before a tool call, issue that tool call now."

// AppendContinuationTurn tự động chèn assistant turn (đoạn chữ đã phát) và user turn (prompt nối tiếp).
func AppendContinuationTurn(body []byte, delivered string) ([]byte, error) {
	if strings.TrimSpace(delivered) == "" {
		return body, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, err
	}
	var msgs []json.RawMessage
	if raw, ok := root["messages"]; ok {
		_ = json.Unmarshal(raw, &msgs)
	}

	assistant, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": []any{map[string]any{"type": "text", "text": delivered}},
	})
	user, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": ContinuationInstruction}},
	})

	msgs = append(msgs, json.RawMessage(assistant), json.RawMessage(user))
	root["messages"], _ = json.Marshal(msgs)
	return json.Marshal(root)
}

// TrimResumeOverlap cắt bỏ phần trùng lặp ở đầu đoạn chữ mới (next) so với đuôi đoạn chữ cũ (prev).
// Đảm bảo chỉ cắt đúng ranh giới Rune UTF-8, không bao giờ làm rách ký tự tiếng Việt hoặc multi-byte.
func TrimResumeOverlap(prev, next string) string {
	if prev == "" || next == "" {
		return next
	}
	max := len(prev)
	if len(next) < max {
		max = len(next)
	}
	for k := max; k > 0; k-- {
		if k < len(next) && !utf8.RuneStart(next[k]) {
			continue // Tránh cắt giữa chừng ký tự UTF-8
		}
		if !utf8.ValidString(next[:k]) {
			continue
		}
		if strings.HasSuffix(prev, next[:k]) {
			return next[k:]
		}
	}
	return next
}
```

---

## 4. Xử Lý Lỗi `end_turn`, Refusal & Turn Rỗng (Empty Content Protection)

### 4.1 Quy tắc mapping `stop_reason` và Bảo Vệ Response Rỗng
- **Mapping Stop Reason**: `TOOL_USE` -> `tool_use`, `MAX_TOKENS` -> `max_tokens`, mặc định -> `end_turn`.
- **Refusal Category Guard**: Nếu Kiro từ chối trả lời (`refusalCategory` khác rỗng), ép `stop_reason` thành `"refusal"`. Gửi `end_turn` trên response từ chối rỗng sẽ làm client retry liên tục vô ích.
- **Empty Content Protection**: Nếu turn kết thúc sạch (EOF) nhưng **0 có nội dung nào được tạo ra** (text/thinking/tool = 0) và không phải refusal: **BẮT BỘC gửi SSE error** thay vì `message_stop` với `end_turn`. Gửi `end_turn` với content rỗng làm Claude Code báo lỗi `API returned an empty or malformed response (HTTP 200)` và dừng toàn bộ công việc.

### 4.2 Mã nguồn Go: Mapping StopReason & Refusal Guard

```go
package kirostream

import "strings"

// MapStopReason chuyển đổi Kiro stopReason sang Anthropic stop_reason.
func MapStopReason(kiro string) string {
	switch strings.ToUpper(kiro) {
	case "TOOL_USE":
		return "tool_use"
	case "MAX_TOKENS":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// MapStopReasonRefusal ép stop_reason thành "refusal" nếu bị model từ chối.
func MapStopReasonRefusal(kiro, refusalCategory string) string {
	if strings.TrimSpace(refusalCategory) != "" {
		return "refusal"
	}
	return MapStopReason(kiro)
}

// HasProducedContent kiểm tra xem turn có tạo ra nội dung thực sự hay không.
func HasProducedContent(textLen, thinkingLen, toolCallsCount int) bool {
	return textLen > 0 || thinkingLen > 0 || toolCallsCount > 0
}
```

---

## 5. Xử Lý Lỗi Tool Argument JSON Hỏng (Tool Input Reconciler)

### 5.1 Giải quyết lẫn lộn Delta vs Snapshot của Kiro
Kiro phát ra `toolUseEvent` bất nhất giữa các version: có lúc là delta mảnh (`{"path":` rồi `"a.go"}`), có lúc là cumulative snapshot (`{"path":` rồi `{"path":"a.go"}`). Nối chuỗi thông thường làm trùng lặp JSON, gây ra lỗi `Error editing file` trên Claude Code.

Bộ tích lũy `ToolInput` lưu cả hai cách hiểu và chọn kết quả giải mã ra JSON object/array hoàn chỉnh khi đóng block.

### 5.2 Mã nguồn Go: Tool Input Accumulator & JSON Reconciler

```go
package kirostream

import (
	"encoding/json"
	"strings"
)

// ToolInput tự động hòa giải giữa Snapshot-framing và Delta-framing.
type ToolInput struct {
	concat      strings.Builder
	last        string
	count       int
	sawSnapshot bool
}

func (t *ToolInput) Add(frag string) {
	if frag == "" {
		return
	}
	if t.last != "" && len(frag) > len(t.last) && strings.HasPrefix(frag, t.last) {
		t.sawSnapshot = true
	}
	if c := t.concat.String(); c != "" && len(frag) >= len(c) && strings.HasPrefix(frag, c) {
		t.sawSnapshot = true
	}
	if frag == t.last && isCompleteToolJSON(frag) {
		t.sawSnapshot = true
	}
	t.count++
	t.concat.WriteString(frag)
	t.last = frag
}

func isCompleteToolJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return false
	}
	return json.Valid([]byte(s))
}

func (t *ToolInput) Resolve() string {
	concat := strings.TrimSpace(t.concat.String())
	last := strings.TrimSpace(t.last)
	if concat == last {
		return concat
	}
	concatOK, lastOK := isCompleteToolJSON(concat), isCompleteToolJSON(last)
	switch {
	case concatOK && !lastOK:
		return concat
	case lastOK && !concatOK:
		return last
	case t.sawSnapshot:
		return last
	default:
		return concat
	}
}

func (t *ToolInput) Reset() {
	t.concat.Reset()
	t.last = ""
	t.count = 0
	t.sawSnapshot = false
}
```

---

## 6. Danh Sách Kiểm Tra Khi Tích Hợp (Integration Checklist)

1. [x] **Heartbeat SSE Ping**: Luôn có goroutine ticker 10s phát `ping` giữ kết nối sống.
2. [x] **EventStream Prelude Guard**: Luôn kiểm tra `headersLen + esMinMsgBytes > total` tránh panic memory.
3. [x] **Turn Rỗng Protection**: Không bao giờ phát `end_turn` nếu 0 content block được tạo ra (trừ refusal).
4. [x] **Refusal Handling**: Chuyển `refusalCategory` thành `stop_reason: "refusal"`.
5. [x] **Tool Argument Resolving**: Gom fragments qua `ToolInput` và chỉ `Resolve()` ra JSON hợp lệ khi kết thúc block.
6. [x] **Rune-Aligned Overlap Trimming**: Dùng `utf8.RuneStart` khi cắt chuỗi lặp giữa 2 segment nối tiếp.
