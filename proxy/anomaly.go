package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"sort"
	"time"
)

// anomaly.go computes an anomaly score for each API key by aggregating signals
// from data that is already tracked: the ipLimiter's distinct-IP set, the
// in-memory usageStats error rate, the request log burst RPM, and the
// persisted DailyUsage credit burn rate.
//
// No new tracking state is added — the anomaly view is a read-only projection.

type anomalyView struct {
	KeyID            string        `json:"keyId"`
	KeyName          string        `json:"keyName,omitempty"`
	OwnerRef         string        `json:"ownerRef,omitempty"`
	Score            float64       `json:"score"`
	Reasons          []string      `json:"reasons"`
	DistinctIPs      int           `json:"distinctIPs"`
	TopIPs           []ipSeenEntry `json:"topIPs"`
	ErrorRate        float64       `json:"errorRate"`
	BurstRPM         int           `json:"burstRpm"`
	CreditsBurnRatio float64       `json:"creditsBurnRatio"`
	CreditLimit      float64       `json:"creditLimit"`
	CreditsUsed      float64       `json:"creditsUsed"`
	ExpiresAt        int64         `json:"expiresAt,omitempty"`
	Enabled          bool          `json:"enabled"`
}

// computeAnomalies returns a list of keys sorted by score descending.
// Only keys with score > 0 or at least one reason are included.
func (h *Handler) computeAnomalies() []anomalyView {
	keys := config.ListApiKeys()
	if len(keys) == 0 {
		return nil
	}

	// Snapshot request log for burst RPM (last 60s).
	recentLog := requestLog.snapshot()
	// Count by keyID in the last 60s.
	cutoff := time.Now().Unix() - 60
	burstCount := make(map[string]int)
	for i := range recentLog {
		if recentLog[i].Time >= cutoff {
			burstCount[recentLog[i].APIKeyID]++
		}
	}

	var out []anomalyView
	for i := range keys {
		k := &keys[i]
		view := anomalyView{
			KeyID:       k.ID,
			KeyName:     k.Name,
			OwnerRef:    k.OwnerRef,
			CreditLimit: k.CreditLimit,
			CreditsUsed: k.CreditsUsed,
			ExpiresAt:   k.ExpiresAt,
			Enabled:     k.Enabled,
		}
		score := 0.0

		// --- distinct IPs ---
		var seenIPs []ipSeenEntry
		if h.ipLimiter != nil {
			seenIPs = h.ipLimiter.snapshot(k.ID)
		}
		view.DistinctIPs = len(seenIPs)
		// Top 5 IPs for the admin panel.
		if len(seenIPs) > 5 {
			view.TopIPs = seenIPs[:5]
		} else {
			view.TopIPs = seenIPs
		}
		if view.DistinctIPs >= 20 {
			score += 40
			view.Reasons = append(view.Reasons, "high distinct-IP count (≥20)")
		} else if view.DistinctIPs >= 10 {
			score += 20
			view.Reasons = append(view.Reasons, "elevated distinct-IP count (≥10)")
		}

		// --- error rate ---
		if h.usage != nil {
			snap := h.usage.snapshot(k.ID)
			var total, failures int64
			for _, m := range snap.ByModel {
				total += m.Requests + m.Failures
				failures += m.Failures
			}
			if total > 10 {
				view.ErrorRate = float64(failures) / float64(total) * 100
				if view.ErrorRate >= 50 {
					score += 30
					view.Reasons = append(view.Reasons, "error rate ≥50%")
				} else if view.ErrorRate >= 25 {
					score += 15
					view.Reasons = append(view.Reasons, "error rate ≥25%")
				}
			}
		} else {
			// Fall back to persisted ByModel if usageStats unavailable.
			var total, failures int64
			for _, m := range k.ByModel {
				total += m.Requests + m.Failures
				failures += m.Failures
			}
			if total > 10 {
				view.ErrorRate = float64(failures) / float64(total) * 100
				if view.ErrorRate >= 50 {
					score += 20
					view.Reasons = append(view.Reasons, "error rate ≥50% (persisted)")
				}
			}
		}

		// --- burst RPM ---
		view.BurstRPM = burstCount[k.ID]
		if view.BurstRPM >= 30 {
			score += 25
			view.Reasons = append(view.Reasons, "burst RPM ≥30 in last 60s")
		} else if view.BurstRPM >= 15 {
			score += 10
			view.Reasons = append(view.Reasons, "burst RPM ≥15 in last 60s")
		}

		// --- credit burn spike ---
		// Compare today's credits vs. 7-day average.
		if k.CreditLimit > 0 {
			daily := config.DailySeries(k.ID, 8)
			if len(daily) >= 2 {
				today := daily[len(daily)-1]
				var sumPrev float64
				count := 0
				for j := 0; j < len(daily)-1; j++ {
					sumPrev += daily[j].Credits
					count++
				}
				if count > 0 && sumPrev/float64(count) > 0 {
					avgPrev := sumPrev / float64(count)
					view.CreditsBurnRatio = today.Credits / avgPrev
					if view.CreditsBurnRatio >= 5 {
						score += 20
						view.Reasons = append(view.Reasons, "credit burn 5× daily average")
					} else if view.CreditsBurnRatio >= 3 {
						score += 10
						view.Reasons = append(view.Reasons, "credit burn 3× daily average")
					}
				}
			}
		}

		// --- near limit ---
		if k.CreditLimit > 0 {
			remaining := k.CreditLimit - k.CreditsUsed
			pct := remaining / k.CreditLimit * 100
			if pct <= 5 {
				score += 10
				view.Reasons = append(view.Reasons, "≤5% credits remaining")
			}
		}

		// --- disabled ---
		if !k.Enabled {
			score += 5
			view.Reasons = append(view.Reasons, "key is disabled")
		}

		if score > 0 || len(view.Reasons) > 0 {
			view.Score = score
			out = append(out, view)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// apiGetAnomalies handles GET /admin/api/anomalies.
func (h *Handler) apiGetAnomalies(w http.ResponseWriter, r *http.Request) {
	anomalies := h.computeAnomalies()
	if anomalies == nil {
		anomalies = []anomalyView{}
	}
	if err := writeJSONOK(w, map[string]interface{}{
		"anomalies": anomalies,
		"count":     len(anomalies),
	}); err != nil {
		logger.Warnf("[Anomaly] failed to write response: %v", err)
	}
}
