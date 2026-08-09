package proxy

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// pool_alert.go reports the pool going dark: no account left that can serve a
// request. That is the failure the operator most needs to hear about, because
// every client request is failing until someone acts.
//
// The alert is edge-triggered — it fires on the transition into exhaustion, not
// on the condition itself. A level-triggered version would re-send on every
// evaluation, so a pool that stayed empty for six hours would deliver an alert
// per poll all night and train the operator to ignore the channel.

// poolAlertRepeatDelay spaces the repeated sends.
//
// The repeats exist because this is the one alert that must land and Telegram
// occasionally drops a message. They are spaced rather than sent together: three
// identical messages in the same instant get collapsed by the client into one
// notification and count against the bot's rate limit, which defeats the point.
const poolAlertRepeatDelay = 5 * time.Second

// poolAlertState tracks whether the pool was already known to be exhausted.
//
// Deliberately in memory, not persisted. After a restart the operator wants to
// know the pool is empty NOW; whether a previous process already said so is not
// useful information. Persisting it would mean a restart during an outage
// silences the alert entirely.
type poolAlertState struct {
	mu        sync.Mutex
	exhausted bool
	// sending guards against two concurrent triggers (a disable and a poll tick
	// landing together) both starting a repeat sequence.
	sending bool
}

// checkPoolExhausted evaluates pool health and alerts on the transition into
// "nothing can serve a request".
//
// Called from two places, which between them cover every way the pool can empty:
//   - h.disableAccount, so an auto-disable reports immediately, as asked.
//   - the auto-buy poll sweep, which catches the paths this package cannot hook.
//
// That second case is a real limitation rather than belt-and-braces:
// pool.DisableAccount lives in package pool and cannot call into proxy without an
// import cycle, and RefreshAccountInfo disables accounts from a package-level
// function with no Handler to call through. Those two paths are therefore
// reported at the next sweep instead of instantly.
func (h *Handler) checkPoolExhausted() {
	cfg := config.GetAutoBuyConfig()
	if !cfg.NotifyPoolExhausted {
		return
	}

	// A fresh install has no accounts at all. That is setup state, not an outage,
	// and alerting on it would greet every new deployment with a false alarm.
	total := len(config.GetAccounts())
	if total == 0 {
		return
	}

	healthy := h.pool.HealthyCount()

	h.poolAlert.mu.Lock()
	if healthy > 0 {
		// Recovered (or never gone). Clear the latch so the next exhaustion alerts
		// again — without this, a pool that empties, recovers, and empties again
		// would only ever report the first time.
		h.poolAlert.exhausted = false
		h.poolAlert.mu.Unlock()
		return
	}
	if h.poolAlert.exhausted || h.poolAlert.sending {
		h.poolAlert.mu.Unlock()
		return
	}
	h.poolAlert.exhausted = true
	h.poolAlert.sending = true
	h.poolAlert.mu.Unlock()

	repeat := cfg.EffectivePoolAlertRepeat()
	logger.Errorf("[PoolAlert] no usable account remains out of %d configured; alerting %d time(s)", total, repeat)

	n := notice{
		Kind:  noticeKindPoolExhausted,
		Title: "🚨 Kiro-Go: every account is unavailable",
		Lines: []string{
			fmt.Sprintf("Usable accounts: 0 of %d configured.", total),
			"All accounts are disabled, banned, out of quota, or cooling down.",
			"Client requests are failing until an account recovers or a new one is added.",
			"Time: " + time.Now().Format("2006-01-02 15:04:05 -0700"),
		},
		Fields: map[string]any{
			"healthyAccounts": 0,
			"totalAccounts":   total,
			"disabledSummary": summarizeUnavailableAccounts(),
		},
	}

	safeGo(func() {
		defer func() {
			h.poolAlert.mu.Lock()
			h.poolAlert.sending = false
			h.poolAlert.mu.Unlock()
		}()
		for i := 0; i < repeat; i++ {
			if i > 0 {
				select {
				case <-time.After(poolAlertRepeatDelay):
				case <-h.stopAutoBuy:
					// Shutting down. Stop repeating rather than delaying exit.
					return
				}
			}
			h.notify(cfg, n)
		}
	})
}

// summarizeUnavailableAccounts counts why accounts are out, for the webhook
// payload. Reasons only, no emails: the payload can go to an arbitrary endpoint,
// and account addresses are not needed to act on this alert.
func summarizeUnavailableAccounts() map[string]int {
	out := map[string]int{}
	for _, acc := range config.GetAccounts() {
		switch {
		case !acc.Enabled:
			reason := strings.ToLower(strings.TrimSpace(acc.BanStatus))
			if reason == "" {
				reason = "disabled"
			}
			out[reason]++
		default:
			out["enabled"]++
		}
	}
	return out
}
