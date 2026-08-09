package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

// poolAlertHandler wires a Handler with the pool alert enabled and a stub Telegram
// endpoint, returning the handler and a counter of delivered messages.
func poolAlertHandler(t *testing.T, repeat int) (*Handler, *atomic.Int32) {
	t.Helper()
	h := newAutoBuyHandler(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	cfg := autoBuyTestConfig()
	cfg.NotifyPoolExhausted = true
	cfg.PoolAlertRepeat = repeat
	cfg.TelegramBotToken = "123456:ABC"
	cfg.TelegramChatID = "42"
	cfg.TelegramApiBase = srv.URL
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
	return h, &hits
}

// addAccount inserts one account and reloads the pool so HealthyCount sees it.
func addAccount(t *testing.T, h *Handler, id string, enabled bool) {
	t.Helper()
	if err := config.AddAccount(config.Account{
		ID:      id,
		Email:   id + "@example.com",
		Enabled: enabled,
		// A far-future expiry keeps the account out of the "about to expire" filter.
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	h.pool.Reload()
}

// waitForCount polls until the counter reaches want, or the deadline passes.
func waitForCount(c *atomic.Int32, want int32, timeout time.Duration) int32 {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Load() >= want {
			return c.Load()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c.Load()
}

// A fresh install has no accounts at all. That is setup state, not an outage, and
// alerting on it would greet every new deployment with a false alarm.
func TestPoolAlertStaysQuietWithNoAccountsConfigured(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)

	h.checkPoolExhausted()

	time.Sleep(300 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Fatalf("an empty account list is not an outage, got %d alert(s)", got)
	}
}

func TestPoolAlertStaysQuietWhileAnAccountIsUsable(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-live", true)

	h.checkPoolExhausted()

	time.Sleep(300 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Fatalf("a usable account means no alert, got %d", got)
	}
}

func TestPoolAlertFiresWhenEveryAccountIsDisabled(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-dead", false)

	h.checkPoolExhausted()

	if got := waitForCount(hits, 1, 5*time.Second); got != 1 {
		t.Fatalf("want 1 alert, got %d", got)
	}
}

// The repeats exist because Telegram occasionally drops a message and this is the
// one alert that must land.
func TestPoolAlertRepeatsTheConfiguredNumberOfTimes(t *testing.T) {
	h, hits := poolAlertHandler(t, 3)
	addAccount(t, h, "acc-dead", false)

	h.checkPoolExhausted()

	// Three sends spaced by poolAlertRepeatDelay, so allow for two gaps.
	timeout := 3*poolAlertRepeatDelay + 5*time.Second
	if got := waitForCount(hits, 3, timeout); got != 3 {
		t.Fatalf("want 3 alerts, got %d", got)
	}
}

// The alert is edge-triggered. A level-triggered version would re-send on every
// evaluation, so a pool that stayed empty overnight would deliver an alert per
// poll and train the operator to ignore the channel.
func TestPoolAlertDoesNotRepeatWhileStillExhausted(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-dead", false)

	h.checkPoolExhausted()
	if got := waitForCount(hits, 1, 5*time.Second); got != 1 {
		t.Fatalf("first check should alert once, got %d", got)
	}

	// Further checks with the pool still empty must stay silent.
	for i := 0; i < 3; i++ {
		h.checkPoolExhausted()
	}
	time.Sleep(500 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Fatalf("still-exhausted checks must not re-alert, got %d", got)
	}
}

// Recovery clears the latch, so a second outage is reported. Without that reset the
// pool would only ever alert once per process lifetime.
func TestPoolAlertFiresAgainAfterRecovery(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-dead", false)

	h.checkPoolExhausted()
	if got := waitForCount(hits, 1, 5*time.Second); got != 1 {
		t.Fatalf("first outage should alert, got %d", got)
	}

	// Recover: enable an account and let the check observe it.
	addAccount(t, h, "acc-live", true)
	h.checkPoolExhausted()
	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Fatalf("recovery itself must not alert, got %d", got)
	}

	// Fail again.
	if err := config.SetAccountEnabled("acc-live", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	h.pool.Reload()
	h.checkPoolExhausted()

	if got := waitForCount(hits, 2, 5*time.Second); got != 2 {
		t.Fatalf("a second outage should alert again, got %d", got)
	}
}

// The switch must actually gate the alert: an operator who leaves it off gets
// nothing, even with a fully configured Telegram channel.
func TestPoolAlertRespectsTheDisabledSwitch(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-dead", false)

	cfg := config.GetAutoBuyConfig()
	cfg.NotifyPoolExhausted = false
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	h.checkPoolExhausted()

	time.Sleep(300 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Fatalf("the alert should be off, got %d", got)
	}
}

// The pool alert is independent of buying: an operator can want the "everything is
// down" warning without wanting unattended purchases.
func TestPoolAlertFiresWithAutoBuyDisabled(t *testing.T) {
	h, hits := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-dead", false)

	cfg := config.GetAutoBuyConfig()
	cfg.Enabled = false
	if err := config.SetAutoBuyConfig(cfg); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}

	h.checkPoolExhausted()

	if got := waitForCount(hits, 1, 5*time.Second); got != 1 {
		t.Fatalf("want the alert even with auto-buy off, got %d", got)
	}
}

// summarizeUnavailableAccounts reports reasons rather than addresses: the payload
// can go to an arbitrary endpoint and emails are not needed to act on the alert.
func TestSummarizeUnavailableAccountsOmitsEmails(t *testing.T) {
	h, _ := poolAlertHandler(t, 1)
	addAccount(t, h, "acc-live", true)
	addAccount(t, h, "acc-dead", false)

	got := summarizeUnavailableAccounts()
	if got["enabled"] != 1 {
		t.Fatalf("want one enabled account counted, got %v", got)
	}
	total := 0
	for _, n := range got {
		total += n
	}
	if total != 2 {
		t.Fatalf("every account should be counted once, got %v", got)
	}
	for key := range got {
		if key == "acc-dead@example.com" || key == "acc-live@example.com" {
			t.Fatalf("emails must not appear in the summary, got %v", got)
		}
	}
}
