package config

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Tests for ReclaimAPIKeyCredits — dropping an expired key's unspent credits while
// keeping the key itself alive and toppable. Helpers come from credits_test.go.

// expiredTestKey seeds a single key that lapsed agoSeconds ago.
func expiredTestKey(t *testing.T, limit, used float64, agoSeconds int64) ApiKeyEntry {
	t.Helper()
	_, key := newCreditsTestKey(t, ApiKeyEntry{
		Name:        "reclaim",
		Enabled:     true,
		CreditLimit: limit,
		CreditsUsed: used,
		ExpiresAt:   time.Now().Unix() - agoSeconds,
	})
	return key
}

func TestReclaimLowersLimitToUsageAndReleasesUnspent(t *testing.T) {
	key := expiredTestKey(t, 1000, 200, 4*3600)

	res, err := ReclaimAPIKeyCredits(key.ID, 3*3600)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if res.AlreadyReclaimed {
		t.Fatalf("expected a fresh reclaim, got a no-op")
	}
	if res.CreditLimit != 200 {
		t.Fatalf("CreditLimit = %v, want 200 (the amount already consumed)", res.CreditLimit)
	}
	if res.Reclaimed != 800 {
		t.Fatalf("Reclaimed = %v, want 800", res.Reclaimed)
	}
	if res.Remaining != 0 {
		t.Fatalf("Remaining = %v, want 0", res.Remaining)
	}

	// Usage counters, expiry, key value and Enabled must survive untouched: a reclaim
	// is not a revocation.
	stored := GetApiKeyEntry(key.ID)
	if stored.CreditsUsed != 200 {
		t.Fatalf("CreditsUsed = %v, want 200 (must not be touched)", stored.CreditsUsed)
	}
	if stored.Key != key.Key {
		t.Fatalf("the key value must survive a reclaim")
	}
	if !stored.Enabled {
		t.Fatalf("the key must stay enabled")
	}
	if stored.ExpiresAt != key.ExpiresAt {
		t.Fatalf("ExpiresAt must not be touched")
	}
}

// The whole point of the endpoint: after a reclaim, a paid top-up must still revive
// the key. A limit of 0 would mean UNLIMITED and make AddAPIKeyCredits refuse forever.
func TestReclaimedKeyCanStillBeToppedUp(t *testing.T) {
	key := expiredTestKey(t, 1000, 0, 4*3600)

	if _, err := ReclaimAPIKeyCredits(key.ID, 3*3600); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	stored := GetApiKeyEntry(key.ID)
	if stored.CreditLimit != ReclaimedLimitFloor {
		t.Fatalf("CreditLimit = %v, want the floor %v — a zero limit would mean UNLIMITED",
			stored.CreditLimit, ReclaimedLimitFloor)
	}

	res, err := AddAPIKeyCredits(key.ID, 500, 1, "order:revive0001", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("top-up after a reclaim must work, got: %v", err)
	}
	if res.CreditLimit != ReclaimedLimitFloor+500 {
		t.Fatalf("CreditLimit = %v, want %v", res.CreditLimit, ReclaimedLimitFloor+500)
	}
	if res.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("a lapsed key topped up with days must be pushed into the future")
	}
}

func TestReclaimIsIdempotentWithoutAnIdempotencyKey(t *testing.T) {
	key := expiredTestKey(t, 1000, 200, 4*3600)

	first, err := ReclaimAPIKeyCredits(key.ID, 3*3600)
	if err != nil {
		t.Fatalf("first reclaim: %v", err)
	}
	second, err := ReclaimAPIKeyCredits(key.ID, 3*3600)
	if err != nil {
		t.Fatalf("second reclaim must be a no-op, got: %v", err)
	}
	if !second.AlreadyReclaimed {
		t.Fatalf("expected AlreadyReclaimed on the second call")
	}
	if second.Reclaimed != 0 {
		t.Fatalf("Reclaimed = %v on replay, want 0 — a caller booking this into inventory twice would oversell", second.Reclaimed)
	}
	if second.CreditLimit != first.CreditLimit {
		t.Fatalf("the limit moved on a second reclaim: %v then %v", first.CreditLimit, second.CreditLimit)
	}
}

// A key that lapsed only moments ago must be refused: its credits are still
// spendable by the customer, not stranded.
func TestReclaimRefusesKeyExpiredTooRecently(t *testing.T) {
	key := expiredTestKey(t, 1000, 100, 30*60) // lapsed 30 min ago, policy wants 3h

	_, err := ReclaimAPIKeyCredits(key.ID, 3*3600)
	if !errors.Is(err, ErrKeyNotExpired) {
		t.Fatalf("err = %v, want ErrKeyNotExpired", err)
	}
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 1000 {
		t.Fatalf("CreditLimit = %v, want 1000 — a refused reclaim must not write", stored.CreditLimit)
	}
}

func TestReclaimRefusesLiveAndNeverExpiringKeys(t *testing.T) {
	// Still valid for another hour.
	_, live := newCreditsTestKey(t, ApiKeyEntry{
		Enabled: true, CreditLimit: 1000, ExpiresAt: time.Now().Unix() + 3600,
	})
	if _, err := ReclaimAPIKeyCredits(live.ID, 0); !errors.Is(err, ErrKeyNotExpired) {
		t.Fatalf("live key: err = %v, want ErrKeyNotExpired", err)
	}

	// ExpiresAt == 0 never expires, so nothing is stranded.
	_, forever := newCreditsTestKey(t, ApiKeyEntry{
		Key: "sk-forever", Enabled: true, CreditLimit: 1000, ExpiresAt: 0,
	})
	if _, err := ReclaimAPIKeyCredits(forever.ID, 0); !errors.Is(err, ErrKeyNotExpired) {
		t.Fatalf("never-expiring key: err = %v, want ErrKeyNotExpired", err)
	}
}

// An unlimited key has no unspent balance to measure, and lowering it to a finite
// number would be a downgrade nobody agreed to.
func TestReclaimRefusesUnlimitedKey(t *testing.T) {
	key := expiredTestKey(t, 0, 500, 4*3600)

	if _, err := ReclaimAPIKeyCredits(key.ID, 3*3600); !errors.Is(err, ErrKeyUnlimited) {
		t.Fatalf("err = %v, want ErrKeyUnlimited", err)
	}
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 0 {
		t.Fatalf("CreditLimit = %v, want it left unlimited", stored.CreditLimit)
	}
}

// Recorded usage above the limit (a skewed sync) must never raise the limit back up.
func TestReclaimNeverRaisesLimitWhenUsageExceedsIt(t *testing.T) {
	key := expiredTestKey(t, 1000, 1500, 4*3600)

	res, err := ReclaimAPIKeyCredits(key.ID, 3*3600)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !res.AlreadyReclaimed || res.Reclaimed != 0 {
		t.Fatalf("expected a no-op, got AlreadyReclaimed=%v Reclaimed=%v", res.AlreadyReclaimed, res.Reclaimed)
	}
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 1000 {
		t.Fatalf("CreditLimit = %v, want it left at 1000 — a reclaim must never raise a limit", stored.CreditLimit)
	}
}

func TestReclaimKeyNotFound(t *testing.T) {
	newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000})

	if _, err := ReclaimAPIKeyCredits("no-such-key", 0); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("err = %v, want ErrKeyNotFound", err)
	}
}

// A reclaim must survive a restart: the caller books the released credits back into
// its own inventory, so a change that only lived in RAM would be oversold later.
func TestReclaimPersistsAcrossRestart(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	key, err := AddApiKey(ApiKeyEntry{
		Key: "sk-reclaim-restart", Enabled: true,
		CreditLimit: 1000, CreditsUsed: 250,
		ExpiresAt: time.Now().Unix() - 4*3600,
	})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if _, err := ReclaimAPIKeyCredits(key.ID, 3*3600); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	stored := GetApiKeyEntry(key.ID)
	if stored == nil {
		t.Fatalf("the key vanished after a restart")
	}
	if stored.CreditLimit != 250 {
		t.Fatalf("CreditLimit after restart = %v, want 250", stored.CreditLimit)
	}
}

// Concurrent reclaims must converge on one target, and the total released across all
// callers must equal the unspent amount exactly once.
func TestReclaimConcurrentCallsReleaseUnspentOnce(t *testing.T) {
	key := expiredTestKey(t, 1000, 200, 4*3600)

	const n = 8
	var wg sync.WaitGroup
	results := make([]float64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := ReclaimAPIKeyCredits(key.ID, 3*3600)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			results[i] = res.Reclaimed
		}(i)
	}
	wg.Wait()

	var total float64
	for _, r := range results {
		total += r
	}
	if math.Abs(total-800) > 1e-9 {
		t.Fatalf("total reclaimed across %d concurrent calls = %v, want exactly 800", n, total)
	}
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 200 {
		t.Fatalf("CreditLimit = %v, want 200", stored.CreditLimit)
	}
}
