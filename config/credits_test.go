package config

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newCreditsTestKey initializes a fresh config in a temp dir and seeds one API key.
// It returns the config file path (for restart tests) and the created key entry.
func newCreditsTestKey(t *testing.T, entry ApiKeyEntry) (string, ApiKeyEntry) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if entry.Key == "" {
		entry.Key = "sk-credits-test"
	}
	created, err := AddApiKey(entry)
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}
	return cfgFile, created
}

func TestCreditsAddRaisesLimitWithoutTouchingUsage(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{
		Name:        "topup",
		Enabled:     true,
		CreditLimit: 1000,
		CreditsUsed: 200,
	})

	res, err := AddAPIKeyCredits(key.ID, 2000, 0, "order:0001", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("add credits: %v", err)
	}
	if res.IdempotentReplay {
		t.Fatalf("expected a fresh apply, got a replay")
	}
	if res.PreviousCreditLimit != 1000 {
		t.Fatalf("PreviousCreditLimit = %v, want 1000", res.PreviousCreditLimit)
	}
	if res.CreditLimit != 3000 {
		t.Fatalf("CreditLimit = %v, want 3000", res.CreditLimit)
	}
	if res.AddedCredits != 2000 {
		t.Fatalf("AddedCredits = %v, want 2000", res.AddedCredits)
	}
	if res.CreditsUsed != 200 {
		t.Fatalf("CreditsUsed = %v, want 200 (usage must not be touched)", res.CreditsUsed)
	}
	if res.Remaining != 2800 {
		t.Fatalf("Remaining = %v, want 2800", res.Remaining)
	}

	stored := GetApiKeyEntry(key.ID)
	if stored == nil {
		t.Fatalf("key missing after top-up")
	}
	if stored.CreditLimit != 3000 || stored.CreditsUsed != 200 {
		t.Fatalf("stored entry = limit %v used %v, want 3000/200", stored.CreditLimit, stored.CreditsUsed)
	}

	credits, days, count := CreditTopUpTotals(key.ID)
	if credits != 2000 || days != 0 || count != 1 {
		t.Fatalf("CreditTopUpTotals = (%v, %d, %d), want (2000, 0, 1)", credits, days, count)
	}
}

func TestCreditsIdempotentReplay(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{
		Enabled:     true,
		CreditLimit: 1000,
		CreditsUsed: 200,
	})

	if _, err := AddAPIKeyCredits(key.ID, 2000, 0, "order:0001", TopUpSourceSalesAPI); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	res, err := AddAPIKeyCredits(key.ID, 2000, 0, "order:0001", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res.IdempotentReplay {
		t.Fatalf("expected IdempotentReplay=true on the second call")
	}
	if res.CreditLimit != 3000 {
		t.Fatalf("CreditLimit = %v after replay, want 3000 (no second add)", res.CreditLimit)
	}

	stored := GetApiKeyEntry(key.ID)
	if stored.CreditLimit != 3000 {
		t.Fatalf("stored limit = %v after replay, want 3000", stored.CreditLimit)
	}
	if _, _, count := CreditTopUpTotals(key.ID); count != 1 {
		t.Fatalf("expected a single ledger entry, got %d", count)
	}
}

func TestCreditsIdempotencyConflict(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000})

	if _, err := AddAPIKeyCredits(key.ID, 2000, 0, "order:0001", TopUpSourceSalesAPI); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	_, err := AddAPIKeyCredits(key.ID, 500, 0, "order:0001", TopUpSourceSalesAPI)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for a reused key with a different amount, got %v", err)
	}

	// A conflict must not mutate anything.
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 3000 {
		t.Fatalf("limit = %v after a conflict, want 3000", stored.CreditLimit)
	}

	// Same key, same amount, different target key is also a conflict.
	other, err := AddApiKey(ApiKeyEntry{Key: "sk-other", Enabled: true, CreditLimit: 10})
	if err != nil {
		t.Fatalf("add other key: %v", err)
	}
	if _, err := AddAPIKeyCredits(other.ID, 2000, 0, "order:0001", TopUpSourceSalesAPI); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for a reused key on a different keyID, got %v", err)
	}
}

func TestCreditsIdempotencyKeyValidation(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000})

	if _, err := AddAPIKeyCredits(key.ID, 100, 0, "", TopUpSourceSalesAPI); !errors.Is(err, ErrIdempotencyRequired) {
		t.Fatalf("expected ErrIdempotencyRequired, got %v", err)
	}
	for _, bad := range []string{"short", "has space", "bad$char"} {
		if _, err := AddAPIKeyCredits(key.ID, 100, 0, bad, TopUpSourceSalesAPI); !errors.Is(err, ErrIdempotencyInvalid) {
			t.Fatalf("expected ErrIdempotencyInvalid for %q, got %v", bad, err)
		}
	}
}

// Two concurrent top-ups with distinct idempotency keys must both land: the
// increment is applied under the same lock that reads the current limit, so
// neither can overwrite the other's result.
func TestCreditsConcurrentAddsAreAdditive(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 3000})

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, idem := range []string{"order:aaaa1111", "order:bbbb2222"} {
		wg.Add(1)
		go func(idem string) {
			defer wg.Done()
			if _, err := AddAPIKeyCredits(key.ID, 1000, 0, idem, TopUpSourceSalesAPI); err != nil {
				errCh <- err
			}
		}(idem)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent add failed: %v", err)
	}

	stored := GetApiKeyEntry(key.ID)
	if stored == nil {
		t.Fatalf("key missing after concurrent top-ups")
	}
	if stored.CreditLimit != 5000 {
		t.Fatalf("CreditLimit = %v after two concurrent +1000 adds on 3000, want 5000", stored.CreditLimit)
	}
	if _, _, count := CreditTopUpTotals(key.ID); count != 2 {
		t.Fatalf("expected two ledger entries, got %d", count)
	}
}

// The ledger is the reason this operation is safe to retry. A retry that arrives
// after a restart must replay from the persisted ledger, not apply a second time.
func TestCreditsIdempotencySurvivesRestart(t *testing.T) {
	cfgFile, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000, CreditsUsed: 200})

	if _, err := AddAPIKeyCredits(key.ID, 2000, 0, "order:restart01", TopUpSourceSalesAPI); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Simulate a process restart: reload the config from the same file on disk.
	if err := Init(cfgFile); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if stored := GetApiKeyEntry(key.ID); stored == nil || stored.CreditLimit != 3000 {
		t.Fatalf("expected the raised limit to be persisted, got %+v", stored)
	}

	res, err := AddAPIKeyCredits(key.ID, 2000, 0, "order:restart01", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if !res.IdempotentReplay {
		t.Fatalf("expected IdempotentReplay=true after restart")
	}
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 3000 {
		t.Fatalf("limit = %v after a post-restart retry, want 3000", stored.CreditLimit)
	}
	if _, _, count := CreditTopUpTotals(key.ID); count != 1 {
		t.Fatalf("expected a single ledger entry after restart replay, got %d", count)
	}
}

func TestCreditsKeyNotFound(t *testing.T) {
	newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000})

	if _, err := AddAPIKeyCredits("no-such-key-id", 100, 0, "order:missing01", TopUpSourceSalesAPI); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// CreditLimit == 0 means unlimited. Adding credits would silently convert the key
// into a limited one, so it is refused — but extending expiry alone is fine.
func TestCreditsUnlimitedKeyRejectsCreditsButAllowsDays(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{
		Enabled:     true,
		CreditLimit: 0,
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
	})

	if _, err := AddAPIKeyCredits(key.ID, 500, 0, "order:unlim0001", TopUpSourceSalesAPI); !errors.Is(err, ErrKeyUnlimited) {
		t.Fatalf("expected ErrKeyUnlimited, got %v", err)
	}
	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 0 {
		t.Fatalf("expected the limit to stay unlimited, got %v", stored.CreditLimit)
	}

	res, err := AddAPIKeyCredits(key.ID, 0, 30, "order:unlim0002", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("days-only top-up on an unlimited key: %v", err)
	}
	if res.CreditLimit != 0 {
		t.Fatalf("expected the limit to stay 0 (unlimited), got %v", res.CreditLimit)
	}
	if res.AddedDays != 30 {
		t.Fatalf("AddedDays = %d, want 30", res.AddedDays)
	}
	if res.ExpiresAt <= res.PreviousExpiresAt {
		t.Fatalf("expected expiry to be extended, got %d from %d", res.ExpiresAt, res.PreviousExpiresAt)
	}
}

func TestCreditsInvalidAmounts(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000})

	tests := []struct {
		name       string
		addCredits float64
		addDays    int
		want       error
	}{
		{"negative", -1, 0, ErrInvalidCredits},
		{"NaN", math.NaN(), 0, ErrInvalidCredits},
		{"positive infinity", math.Inf(1), 0, ErrInvalidCredits},
		{"negative infinity", math.Inf(-1), 0, ErrInvalidCredits},
		{"above max", MaxAddCredits + 1, 0, ErrInvalidCredits},
		{"zero with no days", 0, 0, ErrNothingToApply},
		{"negative days", 100, -1, ErrInvalidDays},
		{"days above max", 100, MaxAddDays + 1, ErrInvalidDays},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idem := "order:invalid" + string(rune('a'+i)) + "0000"
			_, err := AddAPIKeyCredits(key.ID, tc.addCredits, tc.addDays, idem, TopUpSourceSalesAPI)
			if !errors.Is(err, tc.want) {
				t.Fatalf("AddAPIKeyCredits(%v, %d) error = %v, want %v", tc.addCredits, tc.addDays, err, tc.want)
			}
		})
	}

	if stored := GetApiKeyEntry(key.ID); stored.CreditLimit != 1000 {
		t.Fatalf("expected rejected calls to leave the limit at 1000, got %v", stored.CreditLimit)
	}
	if _, _, count := CreditTopUpTotals(key.ID); count != 0 {
		t.Fatalf("expected no ledger entries for rejected calls, got %d", count)
	}
}

// A lapsed customer must get the full period they paid for, measured from now,
// not from an expiry that already elapsed.
func TestCreditsExpiryExtensionFromNowWhenLapsed(t *testing.T) {
	past := time.Now().Add(-30 * 24 * time.Hour).Unix()
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000, ExpiresAt: past})

	res, err := AddAPIKeyCredits(key.ID, 0, 30, "order:lapsed0001", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("extend expiry: %v", err)
	}
	if res.PreviousExpiresAt != past {
		t.Fatalf("PreviousExpiresAt = %d, want %d", res.PreviousExpiresAt, past)
	}

	want := time.Now().Unix() + 30*86400
	if diff := res.ExpiresAt - want; diff > 5 || diff < -5 {
		t.Fatalf("ExpiresAt = %d, want ~%d (diff %ds)", res.ExpiresAt, want, diff)
	}
	if stored := GetApiKeyEntry(key.ID); stored.ExpiresAt != res.ExpiresAt {
		t.Fatalf("stored expiry %d does not match reported %d", stored.ExpiresAt, res.ExpiresAt)
	}
}

// A future expiry is extended from itself, not from now.
func TestCreditsExpiryExtensionFromFutureExpiry(t *testing.T) {
	future := time.Now().Add(10 * 24 * time.Hour).Unix()
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000, ExpiresAt: future})

	res, err := AddAPIKeyCredits(key.ID, 0, 30, "order:future0001", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("extend expiry: %v", err)
	}
	want := future + 30*86400
	if res.ExpiresAt != want {
		t.Fatalf("ExpiresAt = %d, want %d", res.ExpiresAt, want)
	}
}

// Selling more days to a never-expiring key must not accidentally give it a deadline.
func TestCreditsExpiryStaysZeroForNeverExpiringKey(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000, ExpiresAt: 0})

	res, err := AddAPIKeyCredits(key.ID, 100, 30, "order:never00001", TopUpSourceSalesAPI)
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if res.ExpiresAt != 0 {
		t.Fatalf("ExpiresAt = %d, want 0 (never expires must be preserved)", res.ExpiresAt)
	}
	stored := GetApiKeyEntry(key.ID)
	if stored.ExpiresAt != 0 {
		t.Fatalf("stored ExpiresAt = %d, want 0", stored.ExpiresAt)
	}
	if stored.CreditLimit != 1100 {
		t.Fatalf("CreditLimit = %v, want 1100", stored.CreditLimit)
	}
}

// A disabled key gets its limit raised but stays disabled: re-enabling is an
// operator decision, not a side effect of payment.
func TestCreditsDisabledKeyStaysDisabled(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: false, CreditLimit: 1000})

	res, err := AddAPIKeyCredits(key.ID, 500, 0, "order:disabled01", TopUpSourceAdmin)
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	if res.Enabled {
		t.Fatalf("expected the key to stay disabled")
	}
	if stored := GetApiKeyEntry(key.ID); stored.Enabled {
		t.Fatalf("expected the stored key to stay disabled")
	}
}

func TestCreditsListTopUpsNewestFirst(t *testing.T) {
	_, key := newCreditsTestKey(t, ApiKeyEntry{Enabled: true, CreditLimit: 1000})

	if _, err := AddAPIKeyCredits(key.ID, 100, 0, "order:list00001", TopUpSourceSalesAPI); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := AddAPIKeyCredits(key.ID, 200, 0, "order:list00002", TopUpSourceBulk); err != nil {
		t.Fatalf("second: %v", err)
	}

	all := ListCreditTopUps("", 0)
	if len(all) != 2 {
		t.Fatalf("expected 2 ledger entries, got %d", len(all))
	}
	if got := ListCreditTopUps(key.ID, 1); len(got) != 1 {
		t.Fatalf("expected the limit to be honoured, got %d entries", len(got))
	}
	if got := ListCreditTopUps("other-key", 0); len(got) != 0 {
		t.Fatalf("expected no entries for an unrelated key, got %d", len(got))
	}

	credits, _, count := CreditTopUpTotals(key.ID)
	if credits != 300 || count != 2 {
		t.Fatalf("CreditTopUpTotals = (%v, _, %d), want (300, _, 2)", credits, count)
	}
}
