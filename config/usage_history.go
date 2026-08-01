package config

import (
	"sort"
	"time"
)

// UsageDaysKept is how many trailing daily buckets each API key retains. Older
// buckets are dropped on write so config.json cannot grow without bound.
const UsageDaysKept = 30

// UsageDayFormat is the layout of a DailyUsage.Day bucket key. Local time is used
// so "today" matches the operator's wall clock rather than UTC.
const UsageDayFormat = "2006-01-02"

// ModelTallyKept caps the per-model breakdown stored on a key. Model names come
// from client requests, so an unbounded map would be a growth vector; the least
// used entries are dropped once the cap is exceeded.
const ModelTallyKept = 40

// DailyUsage is one calendar day of usage for an API key. It exists because the
// in-memory usageStats in proxy/ is lost on restart, leaving operators with only a
// single cumulative total and no way to see trends or spot a sudden burn spike.
type DailyUsage struct {
	Day       string  `json:"day"`
	Requests  int64   `json:"requests"`
	Failures  int64   `json:"failures"`
	InputTok  int64   `json:"inputTok"`
	OutputTok int64   `json:"outputTok"`
	Credits   float64 `json:"credits"`
}

// ModelTally is the per-model breakdown for an API key, accumulated for the key's
// whole lifetime (not per day).
type ModelTally struct {
	Model     string  `json:"model"`
	Requests  int64   `json:"requests"`
	Failures  int64   `json:"failures"`
	InputTok  int64   `json:"inputTok"`
	OutputTok int64   `json:"outputTok"`
	Credits   float64 `json:"credits"`
}

// todayBucket returns the current local calendar day in UsageDayFormat.
func todayBucket() string {
	return time.Now().Format(UsageDayFormat)
}

// addDailyUsageLocked folds one request into the key's trailing daily buckets.
// MUST be called with cfgLock held.
func addDailyUsageLocked(e *ApiKeyEntry, inTok, outTok int64, credits float64, failed bool) {
	day := todayBucket()
	var bucket *DailyUsage
	if n := len(e.Daily); n > 0 && e.Daily[n-1].Day == day {
		bucket = &e.Daily[n-1]
	} else {
		e.Daily = append(e.Daily, DailyUsage{Day: day})
		if len(e.Daily) > UsageDaysKept {
			e.Daily = append([]DailyUsage(nil), e.Daily[len(e.Daily)-UsageDaysKept:]...)
		}
		bucket = &e.Daily[len(e.Daily)-1]
	}
	if failed {
		bucket.Failures++
		return
	}
	bucket.Requests++
	bucket.InputTok += inTok
	bucket.OutputTok += outTok
	bucket.Credits += credits
}

// addModelTallyLocked folds one request into the key's per-model breakdown.
// MUST be called with cfgLock held.
func addModelTallyLocked(e *ApiKeyEntry, model string, inTok, outTok int64, credits float64, failed bool) {
	if model == "" {
		model = "unknown"
	}
	for i := range e.ByModel {
		if e.ByModel[i].Model != model {
			continue
		}
		if failed {
			e.ByModel[i].Failures++
			return
		}
		e.ByModel[i].Requests++
		e.ByModel[i].InputTok += inTok
		e.ByModel[i].OutputTok += outTok
		e.ByModel[i].Credits += credits
		return
	}
	t := ModelTally{Model: model}
	if failed {
		t.Failures = 1
	} else {
		t.Requests = 1
		t.InputTok = inTok
		t.OutputTok = outTok
		t.Credits = credits
	}
	e.ByModel = append(e.ByModel, t)
	if len(e.ByModel) > ModelTallyKept {
		// Drop the least-used entries rather than the oldest: a one-off typo'd model
		// name should be evicted, not a busy model that happened to appear first.
		sort.SliceStable(e.ByModel, func(i, j int) bool {
			return e.ByModel[i].Requests+e.ByModel[i].Failures > e.ByModel[j].Requests+e.ByModel[j].Failures
		})
		e.ByModel = e.ByModel[:ModelTallyKept]
	}
}

// DailySeries returns the last n calendar days of usage for a key, oldest first,
// with missing days filled in as zero rows so a chart has no gaps.
func DailySeries(keyID string, n int) []DailyUsage {
	if n <= 0 || n > UsageDaysKept {
		n = UsageDaysKept
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	idx := indexOfApiKeyLocked(keyID)
	if idx < 0 {
		return nil
	}
	stored := make(map[string]DailyUsage, len(cfg.ApiKeys[idx].Daily))
	for _, d := range cfg.ApiKeys[idx].Daily {
		// Aggregate rather than overwrite: a clock change could produce two buckets
		// with the same day label, and silently dropping one would understate usage.
		if prev, ok := stored[d.Day]; ok {
			prev.Requests += d.Requests
			prev.Failures += d.Failures
			prev.InputTok += d.InputTok
			prev.OutputTok += d.OutputTok
			prev.Credits += d.Credits
			stored[d.Day] = prev
			continue
		}
		stored[d.Day] = d
	}
	out := make([]DailyUsage, 0, n)
	now := time.Now()
	for i := n - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format(UsageDayFormat)
		if d, ok := stored[day]; ok {
			d.Day = day
			out = append(out, d)
			continue
		}
		out = append(out, DailyUsage{Day: day})
	}
	return out
}

// ModelBreakdown returns a key's per-model tallies, busiest model first.
func ModelBreakdown(keyID string) []ModelTally {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	idx := indexOfApiKeyLocked(keyID)
	if idx < 0 {
		return nil
	}
	out := make([]ModelTally, len(cfg.ApiKeys[idx].ByModel))
	copy(out, cfg.ApiKeys[idx].ByModel)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].InputTok+out[i].OutputTok > out[j].InputTok+out[j].OutputTok
	})
	return out
}
