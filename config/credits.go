package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"
)

// Credit top-up limits and retention.
//
// This file implements the "add credits / extend expiry on an EXISTING key"
// operation that a sales bot calls after it has already charged a customer. Two
// properties matter more than anything else here:
//
//  1. ADDITIVE, never absolute. The caller sells an increment and does not know the
//     current limit at request time. An absolute "set creditLimit = 3000" API races:
//     two concurrent orders both read 1000, both want +1000, both write 2000, and the
//     customer paid for 2000 credits but received 1000. So the increment is applied
//     under the same lock that reads the current value.
//
//  2. IDEMPOTENT ACROSS RESTARTS. This is a financial operation. If the caller's
//     request times out it cannot tell "never arrived" from "applied but the response
//     was lost". Retrying must be safe, so every applied top-up is recorded in a
//     persisted ledger keyed by the caller's idempotency key. An in-memory map would
//     lose that guarantee on the first restart.
const (
	// MaxAddCredits bounds a single top-up so a typo (or a float parsed from an
	// unbounded source) cannot push a limit to an absurd value.
	MaxAddCredits = 1_000_000_000
	// MaxAddDays bounds a single expiry extension for the same reason.
	MaxAddDays = 3650
	// CreditTopUpsKept caps the ledger by count so config.json cannot grow forever.
	CreditTopUpsKept = 10_000
	// CreditTopUpTTL is how long a ledger entry is retained. It must comfortably
	// exceed the maximum lifetime of an order in the calling system, otherwise a
	// late retry would be treated as a fresh top-up and applied twice.
	CreditTopUpTTL = 90 * 24 * time.Hour
)

// Top-up source labels, recorded on the ledger entry for auditing.
const (
	TopUpSourceSalesAPI = "sales-api"
	TopUpSourceAdmin    = "admin-panel"
	TopUpSourceBulk     = "admin-bulk"
)

// idempotencyKeyRe constrains keys to a shape that is safe to store and compare.
// Callers derive these from their own order IDs, so the format is deliberately
// permissive about structure but strict about characters and length.
var idempotencyKeyRe = regexp.MustCompile(`^[A-Za-z0-9:_-]{8,100}$`)

// Sentinel errors returned by AddAPIKeyCredits. The HTTP layer maps each to a
// stable machine-readable code that external callers branch on, so these must not
// be collapsed or reworded into one another.
var (
	ErrIdempotencyRequired = errors.New("idempotencyKey is required")
	ErrIdempotencyInvalid  = errors.New("idempotencyKey must be 8-100 chars of [A-Za-z0-9:_-]")
	ErrIdempotencyConflict = errors.New("idempotencyKey was already used with different arguments")
	ErrInvalidCredits      = errors.New("addCredits must be a finite number in (0, 1e9], or 0 when addDays is set")
	ErrInvalidDays         = errors.New("addDays must be an integer in [0, 3650]")
	ErrNothingToApply      = errors.New("either addCredits or addDays must be greater than zero")
	ErrKeyNotFound         = errors.New("api key not found")
	ErrKeyUnlimited        = errors.New("key has an unlimited credit limit; topping it up would silently make it limited")
	ErrKeyNotExpired       = errors.New("key has not expired yet; reclaiming credits from a live key would cut off a paying customer")
)

// ReclaimedLimitFloor is the smallest CreditLimit a reclaim may leave behind.
//
// It exists because CreditLimit == 0 means UNLIMITED throughout this package
// (ApiKeyRemaining returns -1, ApiKeyOverLimit skips the check). Reclaiming from a
// key the customer never used would compute newLimit = CreditsUsed = 0 and hand
// them an unmetered key — the exact opposite of the intent. It would also make the
// key permanently un-toppable, since AddAPIKeyCredits refuses ErrKeyUnlimited.
//
// The value is far below the cost of a single request, so a key left at this floor
// is exhausted for every practical purpose while staying arithmetically limited.
const ReclaimedLimitFloor = 0.01

// CreditTopUp is one applied top-up, persisted so a retry with the same
// idempotency key replays instead of applying twice. It never stores the
// plaintext API key.
type CreditTopUp struct {
	IdempotencyKey string  `json:"idempotencyKey"`
	KeyID          string  `json:"keyId"`
	AddCredits     float64 `json:"addCredits,omitempty"`
	AddDays        int     `json:"addDays,omitempty"`
	PreviousLimit  float64 `json:"previousLimit"`
	NewLimit       float64 `json:"newLimit"`
	PreviousExpiry int64   `json:"previousExpiry,omitempty"`
	NewExpiry      int64   `json:"newExpiry,omitempty"`
	Source         string  `json:"source,omitempty"`
	CreatedUnix    int64   `json:"createdUnix"`
}

// AddCreditsResult is the outcome of a top-up, shaped for the JSON response.
// Remaining is derived so the caller does not have to subtract itself.
type AddCreditsResult struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name,omitempty"`
	PreviousCreditLimit float64 `json:"previousCreditLimit"`
	AddedCredits        float64 `json:"addedCredits"`
	CreditLimit         float64 `json:"creditLimit"`
	CreditsUsed         float64 `json:"creditsUsed"`
	Remaining           float64 `json:"remaining"`
	PreviousExpiresAt   int64   `json:"previousExpiresAt,omitempty"`
	ExpiresAt           int64   `json:"expiresAt,omitempty"`
	AddedDays           int     `json:"addedDays,omitempty"`
	Enabled             bool    `json:"enabled"`
	IdempotentReplay    bool    `json:"idempotentReplay"`
}

// AddAPIKeyCredits raises a key's credit limit and/or extends its expiry by the
// given increments, atomically and idempotently.
//
// Exactly one of these happens:
//   - fresh apply: limits are raised, a ledger entry is appended, config is saved
//   - replay: the same idempotencyKey with the same (keyID, addCredits, addDays) was
//     already applied, so the stored outcome is returned with IdempotentReplay=true
//     and nothing is mutated
//   - error: nothing is mutated
//
// What is deliberately NOT touched: CreditsUsed, TokensUsed, RequestsCount, the
// lifetime counters, CreatedAt, LastUsedAt, the key value itself, and Enabled. A
// disabled key gets its limit raised but stays disabled — re-enabling is a separate
// decision for the operator, not a side effect of payment.
//
// Expiry semantics for addDays:
//   - ExpiresAt == 0 (never expires) stays 0. Selling more days to an unlimited-time
//     key must not accidentally give it a deadline.
//   - ExpiresAt already in the past: the new expiry is now + addDays, so a lapsed
//     customer gets the full period they paid for rather than days that already elapsed.
//   - ExpiresAt in the future: extended from the existing expiry.
func AddAPIKeyCredits(keyID string, addCredits float64, addDays int, idempotencyKey, source string) (AddCreditsResult, error) {
	if idempotencyKey == "" {
		return AddCreditsResult{}, ErrIdempotencyRequired
	}
	if !idempotencyKeyRe.MatchString(idempotencyKey) {
		return AddCreditsResult{}, ErrIdempotencyInvalid
	}
	// Reject NaN/Inf before any arithmetic: NaN comparisons are always false, so an
	// unchecked NaN would slip past a naive range test and poison the stored limit.
	if math.IsNaN(addCredits) || math.IsInf(addCredits, 0) || addCredits < 0 || addCredits > MaxAddCredits {
		return AddCreditsResult{}, ErrInvalidCredits
	}
	if addDays < 0 || addDays > MaxAddDays {
		return AddCreditsResult{}, ErrInvalidDays
	}
	if addCredits == 0 && addDays == 0 {
		return AddCreditsResult{}, ErrNothingToApply
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return AddCreditsResult{}, errors.New("config not initialized")
	}

	// Replay check first: an already-applied key must return the stored outcome even
	// if the target key has since been deleted or changed.
	for i := range cfg.CreditTopUps {
		t := &cfg.CreditTopUps[i]
		if t.IdempotencyKey != idempotencyKey {
			continue
		}
		if t.KeyID != keyID || t.AddCredits != addCredits || t.AddDays != addDays {
			return AddCreditsResult{}, fmt.Errorf("%w: recorded for key %s (credits %g, days %d)",
				ErrIdempotencyConflict, t.KeyID, t.AddCredits, t.AddDays)
		}
		res := AddCreditsResult{
			ID:                  t.KeyID,
			PreviousCreditLimit: t.PreviousLimit,
			AddedCredits:        t.AddCredits,
			CreditLimit:         t.NewLimit,
			PreviousExpiresAt:   t.PreviousExpiry,
			ExpiresAt:           t.NewExpiry,
			AddedDays:           t.AddDays,
			IdempotentReplay:    true,
		}
		// Enrich from the live key when it still exists, so the caller sees current
		// usage. A deleted key still replays — just without live counters.
		if idx := indexOfApiKeyLocked(keyID); idx >= 0 {
			e := &cfg.ApiKeys[idx]
			res.Name = e.Name
			res.CreditsUsed = e.CreditsUsed
			res.CreditLimit = e.CreditLimit
			res.ExpiresAt = e.ExpiresAt
			res.Enabled = e.Enabled
			res.Remaining = remainingCredits(e.CreditLimit, e.CreditsUsed)
		} else {
			res.Remaining = remainingCredits(t.NewLimit, 0)
		}
		return res, nil
	}

	idx := indexOfApiKeyLocked(keyID)
	if idx < 0 {
		return AddCreditsResult{}, ErrKeyNotFound
	}
	entry := &cfg.ApiKeys[idx]

	// CreditLimit == 0 means unlimited. 0 + add would quietly convert an unlimited
	// key into a limited one and start rejecting the customer's requests, so refuse.
	// Extending expiry alone is still fine.
	if addCredits > 0 && entry.CreditLimit == 0 {
		return AddCreditsResult{}, ErrKeyUnlimited
	}

	prevLimit := entry.CreditLimit
	prevExpiry := entry.ExpiresAt

	newLimit := prevLimit
	if addCredits > 0 {
		newLimit = prevLimit + addCredits
	}
	newExpiry := extendExpiry(prevExpiry, addDays)

	entry.CreditLimit = newLimit
	entry.ExpiresAt = newExpiry

	if source == "" {
		source = TopUpSourceSalesAPI
	}
	cfg.CreditTopUps = append(cfg.CreditTopUps, CreditTopUp{
		IdempotencyKey: idempotencyKey,
		KeyID:          keyID,
		AddCredits:     addCredits,
		AddDays:        addDays,
		PreviousLimit:  prevLimit,
		NewLimit:       newLimit,
		PreviousExpiry: prevExpiry,
		NewExpiry:      newExpiry,
		Source:         source,
		CreatedUnix:    time.Now().Unix(),
	})
	pruned := pruneCreditTopUpsLocked()

	// Persist before reporting success. Returning 200 for a raise that only exists in
	// RAM is the worst failure mode this endpoint has: the caller marks the order
	// delivered, the process restarts, and the customer's credits are gone.
	if err := saveLocked(); err != nil {
		entry.CreditLimit = prevLimit
		entry.ExpiresAt = prevExpiry
		cfg.CreditTopUps = pruned
		return AddCreditsResult{}, fmt.Errorf("could not persist the new credit limit: %w", err)
	}

	return AddCreditsResult{
		ID:                  entry.ID,
		Name:                entry.Name,
		PreviousCreditLimit: prevLimit,
		AddedCredits:        addCredits,
		CreditLimit:         newLimit,
		CreditsUsed:         entry.CreditsUsed,
		Remaining:           remainingCredits(newLimit, entry.CreditsUsed),
		PreviousExpiresAt:   prevExpiry,
		ExpiresAt:           newExpiry,
		AddedDays:           addDays,
		Enabled:             entry.Enabled,
		IdempotentReplay:    false,
	}, nil
}

// ReclaimResult is the outcome of a credit reclaim, shaped for the JSON response.
//
// Reclaimed is what the caller actually gets back to resell, which is NOT always
// PreviousCreditLimit - CreditLimit: the floor keeps a sliver behind. Callers that
// credit their own inventory must add Reclaimed and never recompute the difference.
type ReclaimResult struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name,omitempty"`
	PreviousCreditLimit float64 `json:"previousCreditLimit"`
	CreditLimit         float64 `json:"creditLimit"`
	CreditsUsed         float64 `json:"creditsUsed"`
	Reclaimed           float64 `json:"reclaimed"`
	Remaining           float64 `json:"remaining"`
	ExpiresAt           int64   `json:"expiresAt,omitempty"`
	Enabled             bool    `json:"enabled"`
	AlreadyReclaimed    bool    `json:"alreadyReclaimed"`
}

// ReclaimAPIKeyCredits drops an EXPIRED key's unspent credits by lowering its limit
// to what the customer already consumed. The key itself survives: same ID, same
// plaintext value, still limited, still eligible for a paid top-up that revives it.
//
// The seller's problem this solves: an expired key keeps its whole limit reserved
// against a finite upstream budget, so credits nobody can spend sit unsellable for
// as long as the grace window lasts. Reclaiming returns the unspent part to
// inventory early while leaving the customer a key they can pay to restore.
//
// WHY NOT SET THE LIMIT TO ZERO. Zero means UNLIMITED in this package. Writing 0
// would hand out an unmetered key and, because AddAPIKeyCredits rejects unlimited
// keys with ErrKeyUnlimited, would also permanently block the top-up path that makes
// the key worth keeping. The limit lands on CreditsUsed instead, floored at
// ReclaimedLimitFloor for a key that was never used at all.
//
// NO IDEMPOTENCY KEY, unlike AddAPIKeyCredits. This is not a financial increment; it
// is convergence on a computed target. Calling it twice is a no-op that reports
// AlreadyReclaimed=true, so a caller whose response was lost can simply call again.
//
// minExpiredSeconds is how long the key must have been expired ALREADY. The caller
// owns that policy, but it is enforced here so a bug on the caller's side cannot
// strip a key that expired seconds ago and is still inside its refund window. Pass 0
// to require only that the key is expired.
//
// Deliberately NOT touched: CreditsUsed and every other counter, ExpiresAt, Enabled,
// and the key value. Reclaiming is not a revocation — an operator who wants the key
// gone calls DeleteApiKey.
func ReclaimAPIKeyCredits(keyID string, minExpiredSeconds int64) (ReclaimResult, error) {
	if minExpiredSeconds < 0 {
		minExpiredSeconds = 0
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ReclaimResult{}, errors.New("config not initialized")
	}

	idx := indexOfApiKeyLocked(keyID)
	if idx < 0 {
		return ReclaimResult{}, ErrKeyNotFound
	}
	entry := &cfg.ApiKeys[idx]

	// An unlimited key has no unspent balance to measure, and lowering it to a finite
	// number would be a downgrade the customer never agreed to.
	if entry.CreditLimit <= 0 {
		return ReclaimResult{}, ErrKeyUnlimited
	}
	// Never touch a key that can still serve traffic: its credits are not stranded,
	// they are in use.
	if entry.ExpiresAt <= 0 || time.Now().Unix() < entry.ExpiresAt+minExpiredSeconds {
		return ReclaimResult{}, ErrKeyNotExpired
	}

	prevLimit := entry.CreditLimit
	newLimit := math.Max(entry.CreditsUsed, ReclaimedLimitFloor)

	// Already at or below target — including a key whose recorded usage exceeds its
	// limit. Report success without writing: the caller's goal is already true, and a
	// retry must not be an error.
	if newLimit >= prevLimit {
		return ReclaimResult{
			ID:                  entry.ID,
			Name:                entry.Name,
			PreviousCreditLimit: prevLimit,
			CreditLimit:         prevLimit,
			CreditsUsed:         entry.CreditsUsed,
			Reclaimed:           0,
			Remaining:           remainingCredits(prevLimit, entry.CreditsUsed),
			ExpiresAt:           entry.ExpiresAt,
			Enabled:             entry.Enabled,
			AlreadyReclaimed:    true,
		}, nil
	}

	entry.CreditLimit = newLimit
	// Persist before reporting success, same reasoning as AddAPIKeyCredits: a caller
	// that books the reclaimed credits back into its inventory against a change that
	// only exists in RAM will oversell them after the next restart.
	if err := saveLocked(); err != nil {
		entry.CreditLimit = prevLimit
		return ReclaimResult{}, fmt.Errorf("could not persist the reclaimed credit limit: %w", err)
	}

	return ReclaimResult{
		ID:                  entry.ID,
		Name:                entry.Name,
		PreviousCreditLimit: prevLimit,
		CreditLimit:         newLimit,
		CreditsUsed:         entry.CreditsUsed,
		Reclaimed:           prevLimit - newLimit,
		Remaining:           remainingCredits(newLimit, entry.CreditsUsed),
		ExpiresAt:           entry.ExpiresAt,
		Enabled:             entry.Enabled,
		AlreadyReclaimed:    false,
	}, nil
}

// extendExpiry applies an addDays extension to a current expiry timestamp.
// A zero current expiry (never expires) is preserved as-is; see AddAPIKeyCredits.
func extendExpiry(current int64, addDays int) int64 {
	if addDays <= 0 || current == 0 {
		return current
	}
	base := current
	if now := time.Now().Unix(); base < now {
		base = now
	}
	return base + int64(addDays)*86400
}

// remainingCredits reports how much of a limit is left. A zero limit is unlimited,
// reported as -1 so callers can distinguish "unlimited" from "exhausted".
func remainingCredits(limit, used float64) float64 {
	if limit <= 0 {
		return -1
	}
	return math.Max(0, limit-used)
}

// indexOfApiKeyLocked returns the slice index of the key with the given ID, or -1.
// MUST be called with cfgLock held (the RWMutex is not reentrant).
func indexOfApiKeyLocked(id string) int {
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			return i
		}
	}
	return -1
}

// pruneCreditTopUpsLocked drops ledger entries past CreditTopUpTTL, then trims the
// oldest until at most CreditTopUpsKept remain. It returns the slice as it was
// before pruning so a failed save can restore it exactly.
// MUST be called with cfgLock held.
func pruneCreditTopUpsLocked() []CreditTopUp {
	before := append([]CreditTopUp(nil), cfg.CreditTopUps...)

	cutoff := time.Now().Add(-CreditTopUpTTL).Unix()
	kept := make([]CreditTopUp, 0, len(cfg.CreditTopUps))
	for _, t := range cfg.CreditTopUps {
		if t.CreatedUnix >= cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) > CreditTopUpsKept {
		kept = kept[len(kept)-CreditTopUpsKept:]
	}
	cfg.CreditTopUps = kept
	return before
}

// ListCreditTopUps returns ledger entries, newest first. An empty keyID returns
// entries for every key. A limit <= 0 means no limit.
func ListCreditTopUps(keyID string, limit int) []CreditTopUp {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]CreditTopUp, 0, len(cfg.CreditTopUps))
	for _, t := range cfg.CreditTopUps {
		if keyID != "" && t.KeyID != keyID {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedUnix > out[j].CreatedUnix })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// CreditTopUpTotals sums applied top-ups for one key: how many credits and days
// were ever granted, and how many separate grants there were. Used by the admin
// panel to reconcile a key against an external order system.
func CreditTopUpTotals(keyID string) (credits float64, days int, count int) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return 0, 0, 0
	}
	for _, t := range cfg.CreditTopUps {
		if t.KeyID != keyID {
			continue
		}
		credits += t.AddCredits
		days += t.AddDays
		count++
	}
	return credits, days, count
}
