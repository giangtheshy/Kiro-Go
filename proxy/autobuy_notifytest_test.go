package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// postNotifyTest exercises the admin notify-test endpoint with the given body.
func postNotifyTest(t *testing.T, h *Handler, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/autobuy/notify-test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiAutoBuyNotifyTest(rec, req)

	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// The useful case is testing a freshly pasted token before saving it, so
// credentials in the request body take precedence over the stored ones.
func TestNotifyTestUsesCredentialsFromTheRequestBody(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	var gotPath string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"ok":true}`))
	}))
	defer tg.Close()

	code, out := postNotifyTest(t, h, `{"telegramBotToken":"999:UNSAVED","telegramChatId":"7","telegramApiBase":"`+tg.URL+`"}`)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("want ok=true, got %v", out)
	}
	if gotPath != "/bot999:UNSAVED/sendMessage" {
		t.Fatalf("the body token should be used, got path %q", gotPath)
	}
}

// A blank field falls back to what is stored, so an operator can re-test an
// existing configuration without retyping the token.
func TestNotifyTestFallsBackToStoredCredentials(t *testing.T) {
	h := newAutoBuyHandler(t)

	var gotPath string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"ok":true}`))
	}))
	defer tg.Close()

	cfg := autoBuyTestConfig()
	cfg.TelegramBotToken = "111:STORED"
	cfg.TelegramChatID = "42"
	cfg.TelegramApiBase = tg.URL
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	code, out := postNotifyTest(t, h, `{}`)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", code, out)
	}
	if gotPath != "/bot111:STORED/sendMessage" {
		t.Fatalf("the stored token should be used, got path %q", gotPath)
	}
}

// Telegram's own description is passed through: "chat not found" and
// "Unauthorized" need different fixes, and a generic failure would leave the
// operator guessing which one they have.
func TestNotifyTestReportsTheUpstreamErrorVerbatim(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	defer tg.Close()

	code, out := postNotifyTest(t, h, `{"telegramBotToken":"9:X","telegramChatId":"7","telegramApiBase":"`+tg.URL+`"}`)
	// The request itself succeeded; the channel is what failed, so this is a 200
	// carrying ok=false rather than an HTTP error.
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if out["ok"] != false {
		t.Fatalf("want ok=false, got %v", out)
	}
	results, _ := out["results"].(map[string]any)
	tgResult, _ := results["telegram"].(map[string]any)
	msg, _ := tgResult["error"].(string)
	if !strings.Contains(msg, "chat not found") {
		t.Fatalf("want the upstream description, got %q", msg)
	}
}

// Each channel is reported separately: one working and one broken is a state the
// operator needs to see as exactly that.
func TestNotifyTestReportsChannelsIndependently(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer tg.Close()
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hook.Close()

	body := `{"telegramBotToken":"9:X","telegramChatId":"7","telegramApiBase":"` + tg.URL +
		`","notifyWebhook":"` + hook.URL + `"}`
	code, out := postNotifyTest(t, h, body)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	results, _ := out["results"].(map[string]any)

	tgResult, _ := results["telegram"].(map[string]any)
	if tgResult["ok"] != true {
		t.Fatalf("telegram should succeed, got %v", tgResult)
	}
	hookResult, _ := results["webhook"].(map[string]any)
	if hookResult["ok"] != false {
		t.Fatalf("the webhook should be reported as failed, got %v", hookResult)
	}
	// One broken channel makes the overall result false.
	if out["ok"] != false {
		t.Fatalf("want ok=false when any channel fails, got %v", out)
	}
}

// Testing with nothing configured is a mistake worth naming, not a silent success.
func TestNotifyTestRefusesWithNoChannelConfigured(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	code, out := postNotifyTest(t, h, `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%v)", code, out)
	}
}

// A half-filled pair is called out specifically, because "token but no chat id" is
// the most likely way to get this wrong.
func TestNotifyTestFlagsAHalfConfiguredTelegramPair(t *testing.T) {
	h := newAutoBuyHandler(t)
	if err := config.SetAutoBuyConfig(autoBuyTestConfig()); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	code, out := postNotifyTest(t, h, `{"telegramBotToken":"9:X"}`)
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", code, out)
	}
	results, _ := out["results"].(map[string]any)
	tgResult, _ := results["telegram"].(map[string]any)
	if tgResult["ok"] != false {
		t.Fatalf("a half pair must be reported as failing, got %v", tgResult)
	}
}

// The panel must never receive the stored token back, or a masked field would round
// trip a live credential into the browser.
func TestAutoBuyConfigViewMasksTheTelegramToken(t *testing.T) {
	h := newAutoBuyHandler(t)
	cfg := autoBuyTestConfig()
	cfg.TelegramBotToken = "123456:SECRET"
	cfg.TelegramChatID = "42"
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/autobuy/config", nil)
	rec := httptest.NewRecorder()
	h.apiGetAutoBuyConfig(rec, req)

	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("the bot token must not reach the browser: %s", rec.Body.String())
	}

	var out struct {
		Config struct {
			TelegramBotToken    string `json:"telegramBotToken"`
			HasTelegramBotToken bool   `json:"hasTelegramBotToken"`
			TelegramChatID      string `json:"telegramChatId"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Config.TelegramBotToken != "" {
		t.Fatalf("the token field should be blank, got %q", out.Config.TelegramBotToken)
	}
	if !out.Config.HasTelegramBotToken {
		t.Fatal("the panel needs to know a token is stored")
	}
	// The chat id is not a secret and is returned in full so the panel can show it.
	if out.Config.TelegramChatID != "42" {
		t.Fatalf("the chat id should round trip, got %q", out.Config.TelegramChatID)
	}
}
