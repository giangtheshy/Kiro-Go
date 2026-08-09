package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
)

// Model health tracking: a per-model success-rate timeline for the customer
// portal's status strip.
//
// This does NOT read the request-log ring buffer. That ring holds the last 1000
// requests, which on a busy deployment is minutes of traffic, not a day — a
// timeline built from it would show 24 hours of "no data" with a sliver of
// history at the end. Counters are therefore accumulated per bucket as requests
// are served, which costs two int32 adds per request and bounded memory
// regardless of traffic.
const (
	// modelHealthBucketSeconds is the width of one segment on the strip.
	modelHealthBucketSeconds = 600 // 10 minutes
	// modelHealthBuckets covers 24 hours at the above width.
	modelHealthBuckets = 144
)

// Availability thresholds for one bucket, as success ratios.
//
// A bucket is green ABOVE modelHealthGoodRatio, not at it: 95% even is degraded,
// which matches how the boundary is usually read ("above 95%" is healthy).
const (
	modelHealthGoodRatio = 0.95
	modelHealthWarnRatio = 0.80
)

// trackedHealthModels is the fixed set of models shown on the status strip, in
// display order.
//
// It is a deliberate allowlist rather than "whatever came through": the tracker
// allocates a 144-slot timeline per distinct model name, so keying it off
// arbitrary client input would let anyone grow the map without bound by sending
// requests with made-up model names.
var trackedHealthModels = []string{
	"claude-opus-5",
	"claude-opus-4.8",
	"claude-opus-4.7",
	"claude-sonnet-5",
	"claude-haiku-4.5",
	"gpt-5.6-sol",
}

// healthBucket is one 10-minute window for one model.
//
// idx carries the absolute bucket index the slot currently holds, which is what
// makes the ring self-invalidating: a slot whose idx is not the one being asked
// for is stale data from 24 hours ago, reported as "no data" rather than
// mistaken for the current window. That removes any need to sweep the ring on a
// timer, so a proxy that sat idle overnight cannot report yesterday's numbers as
// today's.
type healthBucket struct {
	idx  int64
	ok   int32
	fail int32
}

type modelHealthTracker struct {
	mu   sync.Mutex
	ring map[string][]healthBucket
}

var modelHealth = newModelHealthTracker()

func newModelHealthTracker() *modelHealthTracker {
	t := &modelHealthTracker{ring: make(map[string][]healthBucket, len(trackedHealthModels))}
	for _, m := range trackedHealthModels {
		t.ring[m] = make([]healthBucket, modelHealthBuckets)
	}
	return t
}

// canonicalHealthModel maps a served model name onto a tracked one, returning ""
// when the model is not on the strip.
//
// The thinking suffix is stripped so "claude-opus-5-thinking" counts towards
// claude-opus-5: it is the same upstream model and splitting the two would
// halve the sample size of both timelines for no benefit.
func canonicalHealthModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	if suffix := strings.ToLower(strings.TrimSpace(config.GetThinkingConfig().Suffix)); suffix != "" {
		m = strings.TrimSuffix(m, suffix)
	}
	for _, tracked := range trackedHealthModels {
		if m == tracked {
			return tracked
		}
	}
	return ""
}

// record folds one served request into the current bucket. Unknown models are
// dropped, so this is safe to call on every request.
func (t *modelHealthTracker) record(model string, ok bool, now time.Time) {
	tracked := canonicalHealthModel(model)
	if tracked == "" {
		return
	}
	idx := now.Unix() / modelHealthBucketSeconds
	pos := int(idx % modelHealthBuckets)

	t.mu.Lock()
	defer t.mu.Unlock()
	ring := t.ring[tracked]
	if ring == nil {
		return
	}
	if ring[pos].idx != idx {
		// The slot belongs to an older window; claim it for this one.
		ring[pos] = healthBucket{idx: idx}
	}
	if ok {
		ring[pos].ok++
	} else {
		ring[pos].fail++
	}
}

// modelHealthPoint is one segment of the strip. Ratio is nil for a window with
// no traffic, which the UI renders as "no data" — distinct from 0% (every
// request failed), because a quiet window says nothing about availability.
type modelHealthPoint struct {
	Ratio *float64 `json:"ratio"`
	Total int      `json:"total"`
}

type modelHealthSeries struct {
	Model string `json:"model"`
	// Uptime is the ratio across every window that saw traffic, weighted by
	// request count rather than by bucket: a bucket with 3 requests should not
	// swing the headline number as much as one with 3000.
	Uptime *float64           `json:"uptime"`
	Points []modelHealthPoint `json:"points"`
}

// snapshot returns the timelines oldest-first, ending with the window containing
// now.
func (t *modelHealthTracker) snapshot(now time.Time) []modelHealthSeries {
	current := now.Unix() / modelHealthBucketSeconds
	oldest := current - modelHealthBuckets + 1

	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]modelHealthSeries, 0, len(trackedHealthModels))
	for _, model := range trackedHealthModels {
		ring := t.ring[model]
		series := modelHealthSeries{Model: model, Points: make([]modelHealthPoint, 0, modelHealthBuckets)}
		var totalOK, totalReq int64

		for idx := oldest; idx <= current; idx++ {
			pos := int(idx % modelHealthBuckets)
			b := ring[pos]
			if b.idx != idx {
				series.Points = append(series.Points, modelHealthPoint{})
				continue
			}
			total := int(b.ok + b.fail)
			if total == 0 {
				series.Points = append(series.Points, modelHealthPoint{})
				continue
			}
			ratio := float64(b.ok) / float64(total)
			series.Points = append(series.Points, modelHealthPoint{Ratio: &ratio, Total: total})
			totalOK += int64(b.ok)
			totalReq += int64(total)
		}

		if totalReq > 0 {
			uptime := float64(totalOK) / float64(totalReq)
			series.Uptime = &uptime
		}
		out = append(out, series)
	}
	return out
}

// reset clears every timeline. Test seam.
func (t *modelHealthTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, model := range trackedHealthModels {
		t.ring[model] = make([]healthBucket, modelHealthBuckets)
	}
}

// apiKeyModelHealth GET /v1/key/status — the per-model availability timeline
// behind the portal's status strip.
//
// Authenticated with the caller's own API key, like the other /v1/key/* routes.
// It is not public: the timeline exposes when the proxy was struggling and how
// often, which is operational detail about the deployment rather than something
// an anonymous visitor needs. Making it public would be a one-line change to
// this guard if that is ever wanted.
//
// The response carries ratios and per-bucket request counts, never account
// identities or which upstream served the traffic.
func (h *Handler) apiKeyModelHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	provided := extractProvidedKey(r)
	if provided == "" || config.FindApiKeyByValue(provided) == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}

	now := time.Now()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"bucketSeconds": modelHealthBucketSeconds,
		"buckets":       modelHealthBuckets,
		"goodRatio":     modelHealthGoodRatio,
		"warnRatio":     modelHealthWarnRatio,
		// now is the end of the newest window, so the client can label the axis
		// without assuming its own clock agrees with the server's.
		"now":    now.Unix(),
		"models": modelHealth.snapshot(now),
		// poolAvailable answers "will a request work right now?", which the
		// per-model strip cannot: that strip is a history of completed requests,
		// so an empty pool reads there as "no data" — indistinguishable from a
		// quiet model. A customer whose requests are all failing needs to be told
		// the difference between "the service is being topped up" and "your
		// request is the problem".
		//
		// A boolean, not a count: the customer only needs to know whether to
		// wait, and pool size is the operator's business.
		"poolAvailable": h.upstreamAvailable(),
	})
}

// upstreamAvailable reports whether anything could serve a request right now.
//
// Providers count: when one is enabled the routing chain can reach it even with
// every Kiro account cooling down, so requests still succeed and a "please wait"
// banner would be a lie. This deliberately mirrors what nextUpstream can pick
// rather than counting accounts alone.
func (h *Handler) upstreamAvailable() bool {
	if h.pool != nil && h.pool.HealthyCount() > 0 {
		return true
	}
	return len(config.GetEnabledProviders()) > 0
}
