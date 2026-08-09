package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// baseTime is an arbitrary fixed instant aligned to a bucket boundary, so the
// tests never straddle two windows by accident.
var baseTime = time.Unix(1_700_000_000/modelHealthBucketSeconds*modelHealthBucketSeconds, 0)

func newHealthTracker() *modelHealthTracker { return newModelHealthTracker() }

// findSeries returns the timeline for one model, failing when it is absent.
func findSeries(t *testing.T, all []modelHealthSeries, model string) modelHealthSeries {
	t.Helper()
	for _, s := range all {
		if s.Model == model {
			return s
		}
	}
	t.Fatalf("series for %q not in snapshot", model)
	return modelHealthSeries{}
}

func TestHealthRatioReflectsSuccessAndFailure(t *testing.T) {
	tr := newHealthTracker()
	for i := 0; i < 9; i++ {
		tr.record("claude-opus-5", true, baseTime)
	}
	tr.record("claude-opus-5", false, baseTime)

	s := findSeries(t, tr.snapshot(baseTime), "claude-opus-5")
	last := s.Points[len(s.Points)-1]
	if last.Ratio == nil {
		t.Fatal("expected the newest bucket to carry a ratio")
	}
	if *last.Ratio != 0.9 {
		t.Fatalf("expected ratio 0.9, got %v", *last.Ratio)
	}
	if last.Total != 10 {
		t.Fatalf("expected total 10, got %d", last.Total)
	}
}

// A window with no traffic must stay nil, not 0. Zero means "everything failed",
// which would paint the strip red for a model nobody called.
func TestHealthQuietWindowIsNilNotZero(t *testing.T) {
	tr := newHealthTracker()
	tr.record("claude-opus-5", true, baseTime)

	s := findSeries(t, tr.snapshot(baseTime), "claude-opus-5")
	if len(s.Points) != modelHealthBuckets {
		t.Fatalf("expected %d points, got %d", modelHealthBuckets, len(s.Points))
	}
	for i, p := range s.Points[:len(s.Points)-1] {
		if p.Ratio != nil {
			t.Fatalf("point %d should have no data, got ratio %v", i, *p.Ratio)
		}
	}
	// A model that was never called at all reports no uptime rather than 0%.
	quiet := findSeries(t, tr.snapshot(baseTime), "gpt-5.6-sol")
	if quiet.Uptime != nil {
		t.Fatalf("expected nil uptime for an untouched model, got %v", *quiet.Uptime)
	}
}

// The ring is self-invalidating: a slot still holding a window from more than 24
// hours ago must read as "no data", never as the current window. Without the
// per-slot index check, a proxy idle overnight would report yesterday's numbers.
func TestHealthStaleBucketDoesNotResurface(t *testing.T) {
	tr := newHealthTracker()
	tr.record("claude-opus-5", false, baseTime)

	// Exactly one full ring later, the same slot is reused.
	later := baseTime.Add(modelHealthBuckets * modelHealthBucketSeconds * time.Second)
	s := findSeries(t, tr.snapshot(later), "claude-opus-5")
	for i, p := range s.Points {
		if p.Ratio != nil {
			t.Fatalf("point %d resurfaced stale data (ratio %v)", i, *p.Ratio)
		}
	}
	if s.Uptime != nil {
		t.Fatalf("expected nil uptime once every bucket aged out, got %v", *s.Uptime)
	}
}

func TestHealthTimelineIsOldestFirst(t *testing.T) {
	tr := newHealthTracker()
	old := baseTime
	recent := baseTime.Add(3 * modelHealthBucketSeconds * time.Second)

	tr.record("claude-sonnet-5", false, old) // 0% in the older window
	tr.record("claude-sonnet-5", true, recent)

	s := findSeries(t, tr.snapshot(recent), "claude-sonnet-5")
	n := len(s.Points)
	newest := s.Points[n-1]
	older := s.Points[n-4]

	if newest.Ratio == nil || *newest.Ratio != 1 {
		t.Fatalf("expected the newest bucket to be the recent one at 100%%, got %v", newest.Ratio)
	}
	if older.Ratio == nil || *older.Ratio != 0 {
		t.Fatalf("expected the 4th-from-last bucket to be the older one at 0%%, got %v", older.Ratio)
	}
}

// Uptime is weighted by request count, not averaged per bucket: a window with 1
// request must not swing the headline as much as one with 1000.
func TestHealthUptimeIsRequestWeighted(t *testing.T) {
	tr := newHealthTracker()
	busy := baseTime
	quiet := baseTime.Add(modelHealthBucketSeconds * time.Second)

	for i := 0; i < 999; i++ {
		tr.record("claude-opus-4.8", true, busy)
	}
	tr.record("claude-opus-4.8", false, quiet) // a lone failure in its own window

	s := findSeries(t, tr.snapshot(quiet), "claude-opus-4.8")
	if s.Uptime == nil {
		t.Fatal("expected an uptime value")
	}
	// Bucket-averaged would be (100% + 0%) / 2 = 50%. Request-weighted is 999/1000.
	if *s.Uptime < 0.998 {
		t.Fatalf("expected request-weighted uptime ≈0.999, got %v", *s.Uptime)
	}
}

// The thinking variant is the same upstream model; splitting it would halve the
// sample size of both timelines.
func TestHealthThinkingSuffixFoldsIntoBaseModel(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	tr := newHealthTracker()
	tr.record("claude-opus-5-thinking", true, baseTime)
	tr.record("CLAUDE-OPUS-5", true, baseTime)

	s := findSeries(t, tr.snapshot(baseTime), "claude-opus-5")
	last := s.Points[len(s.Points)-1]
	if last.Total != 2 {
		t.Fatalf("expected both spellings to fold into one series, got total %d", last.Total)
	}
}

// Untracked names must be dropped rather than allocating a timeline: the map is
// keyed by client-supplied input, so accepting arbitrary names would let anyone
// grow it without bound.
func TestHealthUntrackedModelIsDropped(t *testing.T) {
	tr := newHealthTracker()
	tr.record("some-made-up-model", true, baseTime)
	tr.record("", true, baseTime)

	if got := len(tr.ring); got != len(trackedHealthModels) {
		t.Fatalf("expected the ring to stay at %d models, got %d", len(trackedHealthModels), got)
	}
	snap := tr.snapshot(baseTime)
	if len(snap) != len(trackedHealthModels) {
		t.Fatalf("expected %d series, got %d", len(trackedHealthModels), len(snap))
	}
}

func TestHealthSnapshotCoversEveryTrackedModel(t *testing.T) {
	tr := newHealthTracker()
	snap := tr.snapshot(baseTime)
	seen := make(map[string]bool, len(snap))
	for _, s := range snap {
		seen[s.Model] = true
		if len(s.Points) != modelHealthBuckets {
			t.Fatalf("%s: expected %d points, got %d", s.Model, modelHealthBuckets, len(s.Points))
		}
	}
	for _, m := range trackedHealthModels {
		if !seen[m] {
			t.Fatalf("model %q missing from the snapshot", m)
		}
	}
}

// logRequest is the single point every served request passes through, so the
// strip must fill from it without the serving paths knowing it exists.
func TestLogRequestFeedsModelHealth(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	requestLog.reset()
	modelHealth.reset()
	t.Cleanup(func() { modelHealth.reset(); requestLog.reset() })

	logRequest(RequestLogEntry{Model: "claude-opus-5", Status: "ok"})
	logRequest(RequestLogEntry{Model: "claude-opus-5", Status: "error"})

	s := findSeries(t, modelHealth.snapshot(time.Now()), "claude-opus-5")
	last := s.Points[len(s.Points)-1]
	if last.Ratio == nil || *last.Ratio != 0.5 {
		t.Fatalf("expected 0.5 from one ok and one error, got %v", last.Ratio)
	}
}

func TestModelHealthEndpointRequiresAKey(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	h.apiKeyModelHealth(rec, httptest.NewRequest(http.MethodGet, "/v1/key/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a key, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/key/status", nil)
	req.Header.Set("X-Api-Key", "sk-not-a-real-key")
	h.apiKeyModelHealth(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unknown key, got %d", rec.Code)
	}
}

func TestModelHealthEndpointReturnsTimeline(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-health-probe"})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	_ = entry

	modelHealth.reset()
	t.Cleanup(func() { modelHealth.reset() })
	modelHealth.record("claude-opus-5", true, time.Now())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/key/status", nil)
	req.Header.Set("Authorization", "Bearer sk-health-probe")
	h.apiKeyModelHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		BucketSeconds int                 `json:"bucketSeconds"`
		Buckets       int                 `json:"buckets"`
		GoodRatio     float64             `json:"goodRatio"`
		WarnRatio     float64             `json:"warnRatio"`
		Now           int64               `json:"now"`
		Models        []modelHealthSeries `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.BucketSeconds != modelHealthBucketSeconds || body.Buckets != modelHealthBuckets {
		t.Fatalf("unexpected bucket geometry: %d x %d", body.Buckets, body.BucketSeconds)
	}
	if body.GoodRatio != modelHealthGoodRatio || body.WarnRatio != modelHealthWarnRatio {
		t.Fatalf("thresholds not surfaced: good=%v warn=%v", body.GoodRatio, body.WarnRatio)
	}
	if body.Now == 0 {
		t.Fatal("expected the server clock to be surfaced so the client can label the axis")
	}
	if len(body.Models) != len(trackedHealthModels) {
		t.Fatalf("expected %d models, got %d", len(trackedHealthModels), len(body.Models))
	}
	if body.Models[0].Model != trackedHealthModels[0] {
		t.Fatalf("expected display order preserved, got %q first", body.Models[0].Model)
	}
}

// The payload backs a customer-facing page, so it must not name accounts.
func TestModelHealthPayloadCarriesNoAccountIdentity(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	if _, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-health-leak"}); err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID: "acct-secret", Email: "operator-private@example.com", Enabled: true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	modelHealth.reset()
	t.Cleanup(func() { modelHealth.reset() })
	logRequest(RequestLogEntry{
		Model: "claude-opus-5", Status: "ok",
		AccountID: "acct-secret", AccountEmail: "operator-private@example.com",
		ClientIP: "203.0.113.9",
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/key/status", nil)
	req.Header.Set("Authorization", "Bearer sk-health-leak")
	h.apiKeyModelHealth(rec, req)

	body := rec.Body.String()
	for _, secret := range []string{"operator-private@example.com", "acct-secret", "203.0.113.9"} {
		if strings.Contains(body, secret) {
			t.Fatalf("status payload leaked %q: %s", secret, body)
		}
	}
}
