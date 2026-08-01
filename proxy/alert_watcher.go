package proxy

import (
	"bytes"
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"time"
)

// alertWatcher scans all API keys on a fixed cadence and fires notifications
// for low-credit keys and upcoming / already-expired keys, and optionally
// disables expired keys automatically.
//
// The goroutine is started by NewHandler and stopped by Shutdown via the
// stopAlertWatcher channel, mirroring the backgroundRefresh pattern.

const alertWatchInterval = 10 * time.Minute

func (h *Handler) backgroundAlertWatcher() {
	ticker := time.NewTicker(alertWatchInterval)
	defer ticker.Stop()
	// In-memory dedupe map so each alert fires at most once per 24 hours per key.
	lastAlerted := make(map[string]time.Time)
	for {
		select {
		case <-h.stopAlertWatcher:
			return
		case <-ticker.C:
			h.runAlertScan(lastAlerted)
		}
	}
}

const alertDedupWindow = 24 * time.Hour

func (h *Handler) runAlertScan(lastAlerted map[string]time.Time) {
	keys := config.ListApiKeys()
	if len(keys) == 0 {
		return
	}
	now := time.Now()

	credPct := config.GetAlertCreditsPercent()
	expiryDays := config.GetAlertExpiryDays()
	webhook := config.GetAlertWebhook()
	autoDisable := config.GetAutoDisableExpired()

	for i := range keys {
		k := &keys[i]
		if !k.Enabled {
			continue
		}

		// --- already expired ---
		if k.ExpiresAt > 0 && now.Unix() >= k.ExpiresAt {
			if autoDisable {
				patch := *k
				patch.Enabled = false
				if err := config.UpdateApiKey(k.ID, patch); err == nil {
					config.RecordAudit(config.AuditEntry{
						Action: config.AuditKeyDisable,
						Actor:  config.AuditActorSystem,
						Target: k.ID,
						Detail: "auto-disabled: key past ExpiresAt",
					})
					logger.Infof("[Alert] auto-disabled expired key %s (%s ownerRef=%s)", k.ID, k.Name, k.OwnerRef)
				}
			}
			if shouldAlert(lastAlerted, k.ID+"_expired", alertDedupWindow, now) {
				fireAlert(webhook, alertPayload{
					Type:      "key_expired",
					KeyID:     k.ID,
					KeyName:   k.Name,
					OwnerRef:  k.OwnerRef,
					ExpiresAt: k.ExpiresAt,
				})
			}
			continue
		}

		// --- expiry soon ---
		if expiryDays > 0 && k.ExpiresAt > 0 {
			daysLeft := float64(k.ExpiresAt-now.Unix()) / 86400
			if daysLeft <= float64(expiryDays) {
				if shouldAlert(lastAlerted, k.ID+"_expiry_soon", alertDedupWindow, now) {
					fireAlert(webhook, alertPayload{
						Type:          "expiry_soon",
						KeyID:         k.ID,
						KeyName:       k.Name,
						OwnerRef:      k.OwnerRef,
						ExpiresAt:     k.ExpiresAt,
						DaysRemaining: daysLeft,
					})
					logger.Warnf("[Alert] key %s (%s) expires in %.1f days (ownerRef=%s)", k.ID, k.Name, daysLeft, k.OwnerRef)
				}
			}
		}

		// --- low credits ---
		if credPct > 0 && k.CreditLimit > 0 {
			remaining := k.CreditLimit - k.CreditsUsed
			pctRemaining := (remaining / k.CreditLimit) * 100
			if pctRemaining <= credPct {
				if shouldAlert(lastAlerted, k.ID+"_credits", alertDedupWindow, now) {
					fireAlert(webhook, alertPayload{
						Type:      "credit_low",
						KeyID:     k.ID,
						KeyName:   k.Name,
						OwnerRef:  k.OwnerRef,
						Remaining: remaining,
					})
					logger.Warnf("[Alert] key %s (%s) has %.1f credits left (%.0f%% of limit ownerRef=%s)",
						k.ID, k.Name, remaining, pctRemaining, k.OwnerRef)
				}
			}
		}
	}
}

// shouldAlert returns true when a dedupe entry for id is absent or older than window.
func shouldAlert(m map[string]time.Time, id string, window time.Duration, now time.Time) bool {
	if last, ok := m[id]; ok && now.Sub(last) < window {
		return false
	}
	m[id] = now
	return true
}

type alertPayload struct {
	Type          string  `json:"type"`
	KeyID         string  `json:"keyId"`
	KeyName       string  `json:"keyName,omitempty"`
	OwnerRef      string  `json:"ownerRef,omitempty"`
	Remaining     float64 `json:"remaining,omitempty"`
	ExpiresAt     int64   `json:"expiresAt,omitempty"`
	DaysRemaining float64 `json:"daysRemaining,omitempty"`
}

// fireAlert posts the payload to webhookURL in a goroutine. A missing URL is a
// no-op; errors are logged but never propagated to the caller.
func fireAlert(webhookURL string, payload alertPayload) {
	if webhookURL == "" {
		return
	}
	go func() {
		b, err := json.Marshal(payload)
		if err != nil {
			logger.Warnf("[Alert] could not marshal payload: %v", err)
			return
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(webhookURL, "application/json", bytes.NewReader(b))
		if err != nil {
			logger.Warnf("[Alert] webhook POST to %s failed: %v", webhookURL, err)
			return
		}
		resp.Body.Close()
	}()
}
