package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// notifier.go is the outbound alert channel for things that happen while nobody
// is watching: an unattended purchase, an error that will not fix itself, and a
// pool with nothing left to serve requests with.
//
// It lives outside autobuy_worker.go because not every notice is about buying.
// Pool exhaustion is worth reporting even with auto-buy switched off entirely —
// it means the proxy has gone dark, which is the operator's problem regardless of
// whether anything is being purchased.
//
// Two transports, one call site. The webhook keeps its original JSON payload so
// anything already consuming it is unaffected; Telegram gets a readable message.

// notifyTimeout bounds a single delivery attempt. Deliberately short: a notice
// is worthless late, and the caller is usually holding a purchase result.
const notifyTimeout = 10 * time.Second

// telegramMaxLen is the Telegram message limit. Text is truncated rather than
// rejected — a clipped alert still wakes someone, a 400 does not.
const telegramMaxLen = 4096

// Notice kinds. Stable strings: they go out as the webhook's "event" field.
const (
	noticeKindPurchase      = "purchase"
	noticeKindError         = "error"
	noticeKindPoolExhausted = "pool_exhausted"
	noticeKindTest          = "test"
)

// notice is one alert, in a form both transports can render.
//
// Title and Lines are pre-rendered text for Telegram; Fields is the JSON body for
// the webhook. They are kept separate rather than derived from one another because
// the webhook payload is a compatibility surface — reformatting the human-readable
// text must not silently change the shape of someone's integration.
type notice struct {
	Kind  string
	Title string
	Lines []string
	// Fields is merged into the webhook payload. "event" and "timeUnix" are added
	// by the sender, so callers should not set them here.
	Fields map[string]any
}

// telegramText renders the notice as HTML for Telegram.
//
// Every interpolated value is HTML-escaped. Account emails and upstream error
// messages are external data, and a single "<" in one of them makes Telegram
// reject the whole message with 400 — losing exactly the alert that mattered.
func (n notice) telegramText() string {
	var b strings.Builder
	b.WriteString("<b>")
	b.WriteString(html.EscapeString(n.Title))
	b.WriteString("</b>")
	for _, line := range n.Lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString("\n")
		b.WriteString(html.EscapeString(line))
	}
	out := b.String()
	if len(out) > telegramMaxLen {
		// Cut on a rune boundary: slicing mid-rune yields invalid UTF-8, which
		// Telegram also rejects. The ellipsis marks the truncation for the reader,
		// and its own length is subtracted first — it is 3 bytes, not 1, so
		// reserving a single byte for it would push the result back over the limit.
		out = truncateRunes(out, telegramMaxLen-len(ellipsis)) + ellipsis
	}
	return out
}

// ellipsis marks a truncated message. Named so its byte length can be reserved
// explicitly rather than assumed to be one.
const ellipsis = "…"

// truncateRunes cuts s to at most max bytes without splitting a rune.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isUTF8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// isUTF8Start reports whether b begins a UTF-8 sequence (i.e. is not a
// continuation byte).
func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// notify delivers a notice to every configured channel.
//
// Fire-and-forget by design: a failed notification must never affect the
// operation that produced it. A purchase that already went through is not undone
// because Telegram was unreachable.
func (h *Handler) notify(cfg *config.AutoBuyConfig, n notice) {
	if cfg == nil {
		return
	}
	if url := strings.TrimSpace(cfg.NotifyWebhook); url != "" {
		payload := n.webhookPayload()
		safeGo(func() {
			if err := postNotifyWebhook(url, payload); err != nil {
				logger.Warnf("[Notify] webhook delivery failed: %v", err)
			}
		})
	}
	if cfg.TelegramConfigured() {
		text := n.telegramText()
		token := cfg.TelegramBotToken
		chatID := cfg.TelegramChatID
		apiBase := cfg.EffectiveTelegramApiBase()
		safeGo(func() {
			if err := sendTelegram(apiBase, token, chatID, text); err != nil {
				logger.Warnf("[Notify] Telegram delivery failed: %v", err)
			}
		})
	}
}

// webhookPayload builds the JSON body, preserving the original field layout.
func (n notice) webhookPayload() map[string]any {
	payload := map[string]any{}
	for k, v := range n.Fields {
		payload[k] = v
	}
	payload["event"] = n.Kind
	payload["timeUnix"] = time.Now().Unix()
	return payload
}

// postNotifyWebhook POSTs the payload and reports a non-2xx as an error, so the
// Test button can tell "delivered" from "the endpoint rejected it".
func postNotifyWebhook(endpoint string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := notifyHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// notifyHTTPClient returns the proxy-aware client used for outbound notices.
//
// It goes through the same egress path as market and Kiro traffic. A configured
// outbound proxy exists to control what leaves the host; letting the notifier
// bypass it would punch a hole in that for no good reason.
func notifyHTTPClient() *http.Client {
	client := GetRestClientForProxy(config.GetProxyURL())
	if client == nil {
		return &http.Client{Timeout: notifyTimeout}
	}
	// Copy rather than mutate: the shared client is cached and reused by the Kiro
	// REST path, and overwriting its Timeout here would change that path's
	// behaviour as a side effect.
	c := *client
	c.Timeout = notifyTimeout
	return &c
}

// telegramResponse is the envelope every Bot API call returns.
type telegramResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// sendTelegram posts one message via the Bot API.
//
// Errors carry Telegram's own description verbatim. "chat not found" and
// "Unauthorized" are different problems with different fixes, and collapsing them
// into "delivery failed" would leave the operator guessing which one they have.
func sendTelegram(apiBase, token, chatID, text string) error {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("telegram: bot token and chat id are both required")
	}
	if config.GetProxyURL() == "" && config.GetRequireProxy() {
		return fmt.Errorf("telegram: require-proxy is on but no proxy is configured")
	}

	body, err := json.Marshal(map[string]any{
		"chat_id": strings.TrimSpace(chatID),
		"text":    text,
		// HTML rather than MarkdownV2: MarkdownV2 requires escaping 18 characters,
		// and any miss is a 400 instead of a delivered alert.
		"parse_mode": "HTML",
		// Link previews would turn a webhook URL in an error message into a large
		// unrelated card.
		"disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("telegram: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	// The token goes in the path, so it must be escaped — and must never be logged.
	endpoint := strings.TrimRight(apiBase, "/") + "/bot" + url.PathEscape(strings.TrimSpace(token)) + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := notifyHTTPClient().Do(req)
	if err != nil {
		// Wrap without the URL: it contains the bot token.
		return fmt.Errorf("telegram: request failed: %w", redactToken(err, token))
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var parsed telegramResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 200 {
			snippet = truncateRunes(snippet, 200) + "…"
		}
		return fmt.Errorf("telegram: HTTP %d with unparseable body: %s", resp.StatusCode, snippet)
	}
	if !parsed.OK {
		desc := strings.TrimSpace(parsed.Description)
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("telegram: %s", desc)
	}
	return nil
}

// redactToken strips a bot token from an error message.
//
// net/http includes the request URL in transport errors, and the token is part of
// that URL — so an unmodified error would put a full bot credential in the log.
func redactToken(err error, token string) error {
	if err == nil {
		return nil
	}
	tok := strings.TrimSpace(token)
	if tok == "" {
		return err
	}
	msg := err.Error()
	for _, form := range []string{tok, url.PathEscape(tok)} {
		if form == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, form, "<redacted>")
	}
	return fmt.Errorf("%s", msg)
}
