package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// unreachableHost points at a closed port, for cases that must fail in the
// transport rather than reach the real Telegram API.
const unreachableHost = "http://127.0.0.1:1"

// --- Telegram rendering ---

// Account emails and upstream error strings are external data. A single "<" in one
// of them makes Telegram reject the whole message with 400, losing exactly the
// alert that mattered, so every interpolated value is escaped.
func TestTelegramTextEscapesInterpolatedValues(t *testing.T) {
	n := notice{
		Kind:  noticeKindError,
		Title: "auto-buy failed <production>",
		Lines: []string{`error: bad <tag> & "quotes"`},
	}
	got := n.telegramText()

	if strings.Contains(got, "<production>") {
		t.Fatalf("the title must be escaped, got %q", got)
	}
	if strings.Contains(got, "<tag>") {
		t.Fatalf("line content must be escaped, got %q", got)
	}
	if !strings.Contains(got, "&lt;production&gt;") {
		t.Fatalf("want the escaped title, got %q", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Fatalf("want the ampersand escaped, got %q", got)
	}
	// The <b> wrapper is ours and must survive escaping of the content inside it.
	if !strings.HasPrefix(got, "<b>") || !strings.Contains(got, "</b>") {
		t.Fatalf("the bold wrapper should remain intact, got %q", got)
	}
}

func TestTelegramTextSkipsBlankLines(t *testing.T) {
	n := notice{Title: "T", Lines: []string{"one", "", "   ", "two"}}
	got := n.telegramText()
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("blank lines should be dropped, got %q", got)
	}
}

// Telegram rejects anything over 4096 characters, so a long message is truncated
// rather than lost.
func TestTelegramTextTruncatesToTheLimit(t *testing.T) {
	n := notice{Title: "T", Lines: []string{strings.Repeat("x", telegramMaxLen*2)}}
	got := n.telegramText()
	if len(got) > telegramMaxLen {
		t.Fatalf("want at most %d bytes, got %d", telegramMaxLen, len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("truncation should be marked for the reader")
	}
}

// Cutting mid-rune yields invalid UTF-8, which Telegram also rejects — so the
// truncation has to land on a boundary even when the text is multi-byte.
func TestTelegramTextTruncationKeepsValidUTF8(t *testing.T) {
	// "ề" is 3 bytes; a naive byte slice would land inside one.
	n := notice{Title: "T", Lines: []string{strings.Repeat("ề", telegramMaxLen)}}
	got := n.telegramText()
	if len(got) > telegramMaxLen {
		t.Fatalf("want at most %d bytes, got %d", telegramMaxLen, len(got))
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("invalid UTF-8 produced at byte %d", i)
		}
	}
}

// --- webhook payload compatibility ---

// The webhook is a compatibility surface: field names and omitempty behaviour must
// not drift, because something out there is already parsing them.
func TestAutoBuyNoticeFieldsPreservesOmitEmpty(t *testing.T) {
	fields := autoBuyNotice{
		Event:     noticeKindPurchase,
		Zone:      "us",
		Purchased: 2,
		Credits:   50,
	}.fields()

	if fields["zone"] != "us" {
		t.Fatalf("zone should survive, got %v", fields["zone"])
	}
	// Zero-valued optional fields were omitted before and must stay omitted.
	for _, absent := range []string{"orderId", "code", "error", "imported", "skipped", "balance"} {
		if _, present := fields[absent]; present {
			t.Fatalf("%q should be omitted when zero, payload was %v", absent, fields)
		}
	}
}

func TestWebhookPayloadStampsEventAndTime(t *testing.T) {
	n := notice{Kind: noticeKindPoolExhausted, Fields: map[string]any{"healthyAccounts": 0}}
	got := n.webhookPayload()
	if got["event"] != noticeKindPoolExhausted {
		t.Fatalf("event should be the notice kind, got %v", got["event"])
	}
	if ts, ok := got["timeUnix"].(int64); !ok || ts == 0 {
		t.Fatalf("timeUnix should be stamped, got %v", got["timeUnix"])
	}
	if got["healthyAccounts"] != 0 {
		t.Fatalf("caller fields should be preserved, got %v", got)
	}
}

// --- webhook delivery ---

func TestPostNotifyWebhookReportsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := postNotifyWebhook(srv.URL, map[string]any{"test": true})
	if err == nil {
		t.Fatal("a 500 should be reported so the Test button can show it")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("the error should name the status, got %v", err)
	}
}

func TestPostNotifyWebhookSendsJSONBody(t *testing.T) {
	var got map[string]any
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	if err := postNotifyWebhook(srv.URL, map[string]any{"event": "test", "n": 1}); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if contentType != "application/json" {
		t.Fatalf("want a JSON content type, got %q", contentType)
	}
	if got["event"] != "test" {
		t.Fatalf("payload should arrive intact, got %v", got)
	}
}

// --- Telegram delivery ---

func TestSendTelegramPostsExpectedRequest(t *testing.T) {
	var path string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &payload)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if err := sendTelegram(srv.URL, "123456:ABC", "-100999", "hello"); err != nil {
		t.Fatalf("sendTelegram: %v", err)
	}
	if path != "/bot123456:ABC/sendMessage" {
		t.Fatalf("unexpected path %q", path)
	}
	if payload["chat_id"] != "-100999" {
		t.Fatalf("chat id should be forwarded, got %v", payload["chat_id"])
	}
	// HTML rather than MarkdownV2: MarkdownV2 needs 18 characters escaped and any
	// miss is a 400 instead of a delivered alert.
	if payload["parse_mode"] != "HTML" {
		t.Fatalf("want HTML parse mode, got %v", payload["parse_mode"])
	}
	if payload["disable_web_page_preview"] != true {
		t.Fatalf("link previews should be off, got %v", payload["disable_web_page_preview"])
	}
}

// Telegram's own description distinguishes problems that need different fixes, so
// it is surfaced verbatim rather than collapsed into "delivery failed".
func TestSendTelegramSurfacesUpstreamDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()

	err := sendTelegram(srv.URL, "123456:ABC", "42", "hi")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("the upstream description should be preserved, got %v", err)
	}
}

func TestSendTelegramRequiresBothCredentials(t *testing.T) {
	if err := sendTelegram(unreachableHost, "", "42", "hi"); err == nil {
		t.Fatal("a missing token should be refused before any request")
	}
	if err := sendTelegram(unreachableHost, "123456:ABC", "", "hi"); err == nil {
		t.Fatal("a missing chat id should be refused before any request")
	}
}

// A transport error from net/http includes the request URL, and the bot token is
// part of that URL — so an unredacted error would put a live credential in the log.
func TestSendTelegramRedactsTheTokenFromTransportErrors(t *testing.T) {
	const token = "123456:SUPERSECRET"
	err := sendTelegram(unreachableHost, token, "42", "hi")
	if err == nil {
		t.Fatal("expected a transport error against a closed port")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("the bot token must not appear in an error, got %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("want the redaction marker, got %v", err)
	}
}

func TestSendTelegramHandlesUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>gateway error</html>"))
	}))
	defer srv.Close()

	err := sendTelegram(srv.URL, "123456:ABC", "42", "hi")
	if err == nil {
		t.Fatal("a non-JSON body should still be an error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("the status should be reported, got %v", err)
	}
}

// --- notify() fan-out ---

func TestNotifyDeliversToBothChannels(t *testing.T) {
	h := newAutoBuyHandler(t)

	var wg sync.WaitGroup
	wg.Add(2)
	var hookHits, tgHits atomic.Int32

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookHits.Add(1)
		wg.Done()
	}))
	defer hook.Close()
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tgHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
		wg.Done()
	}))
	defer tg.Close()

	cfg := autoBuyTestConfig()
	cfg.NotifyWebhook = hook.URL
	cfg.TelegramBotToken = "123456:ABC"
	cfg.TelegramChatID = "42"
	cfg.TelegramApiBase = tg.URL

	h.notify(cfg, notice{Kind: noticeKindTest, Title: "hi"})

	if !waitGroupDone(&wg, 5*time.Second) {
		t.Fatalf("both channels should be hit; webhook=%d telegram=%d", hookHits.Load(), tgHits.Load())
	}
}

// A half-configured Telegram pair must not be attempted: it would fail on every
// notice and bury the real alert in delivery warnings.
func TestNotifySkipsHalfConfiguredTelegram(t *testing.T) {
	h := newAutoBuyHandler(t)

	var tgHits atomic.Int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tgHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer tg.Close()

	cfg := autoBuyTestConfig()
	cfg.TelegramBotToken = "123456:ABC"
	cfg.TelegramChatID = "" // missing half
	cfg.TelegramApiBase = tg.URL

	h.notify(cfg, notice{Kind: noticeKindTest, Title: "hi"})

	// Give any (incorrectly) spawned goroutine time to land before asserting.
	time.Sleep(300 * time.Millisecond)
	if got := tgHits.Load(); got != 0 {
		t.Fatalf("no Telegram call should be made, got %d", got)
	}
}

// A notice with no channel configured must be a silent no-op, not a panic: most
// deployments never set either field.
func TestNotifyWithNoChannelsIsANoOp(t *testing.T) {
	h := newAutoBuyHandler(t)
	h.notify(autoBuyTestConfig(), notice{Kind: noticeKindTest, Title: "hi"})
	h.notify(nil, notice{Kind: noticeKindTest, Title: "hi"})
}

// waitGroupDone waits for wg with a timeout, so a missing delivery fails the test
// instead of hanging it.
func waitGroupDone(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
