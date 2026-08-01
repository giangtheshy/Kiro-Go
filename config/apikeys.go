package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	entries, err := AddApiKeys([]ApiKeyEntry{entry})
	if err != nil {
		return ApiKeyEntry{}, err
	}
	return entries[0], nil
}

func AddApiKeys(entries []ApiKeyEntry) ([]ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil, errors.New("config not initialized")
	}
	if len(entries) == 0 {
		return nil, errors.New("no api keys provided")
	}

	seen := make(map[string]bool, len(cfg.ApiKeys)+len(entries))
	for _, existing := range cfg.ApiKeys {
		seen[existing.Key] = true
	}

	now := time.Now().Unix()
	out := make([]ApiKeyEntry, len(entries))
	for i, entry := range entries {
		entry.Key = strings.TrimSpace(entry.Key)
		if entry.Key == "" {
			return nil, errors.New("api key value must not be empty")
		}
		if seen[entry.Key] {
			return nil, errors.New("api key already exists")
		}
		seen[entry.Key] = true
		if entry.ID == "" {
			entry.ID = newUUID()
		}
		if entry.CreatedAt == 0 {
			entry.CreatedAt = now
		}
		// Bound accounts are optional: an empty set means the key routes through the
		// shared pool. When set, drop unknown/duplicate ids so only live accounts remain.
		entry.BoundAccountIDs = sanitizeBoundAccountIDsLocked(entry.BoundAccountIDs)
		entry.Models = sanitizeModelList(entry.Models)
		entry.Model = ""
		out[i] = entry
	}

	oldLen := len(cfg.ApiKeys)
	cfg.ApiKeys = append(cfg.ApiKeys, out...)
	if err := saveLocked(); err != nil {
		cfg.ApiKeys = cfg.ApiKeys[:oldLen]
		return nil, err
	}
	return out, nil
}

// UpdateApiKey applies a patch to an existing API key. Patch semantics:
//   - Name, Key are overwritten when non-empty in patch.
//   - Enabled, TokenLimit, CreditLimit are always overwritten (zero values are valid).
//   - Counters (TokensUsed/CreditsUsed/RequestsCount) are not touched here; use
//     RecordApiKeyUsage or ResetApiKeyUsage instead.
//   - Migrated stays as-is once true; only flips when explicitly set in patch.
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("api key not found")
	}
	if patch.Name != "" {
		cfg.ApiKeys[idx].Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		// Reject duplicates against any other entry.
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].Key == newKey {
				return errors.New("api key value collides with existing entry")
			}
		}
		cfg.ApiKeys[idx].Key = newKey
	}
	cfg.ApiKeys[idx].Enabled = patch.Enabled
	cfg.ApiKeys[idx].TokenLimit = patch.TokenLimit
	cfg.ApiKeys[idx].CreditLimit = patch.CreditLimit
	cfg.ApiKeys[idx].ExpiresAt = patch.ExpiresAt
	cfg.ApiKeys[idx].RPMLimit = patch.RPMLimit
	cfg.ApiKeys[idx].IPLimit = patch.IPLimit
	cfg.ApiKeys[idx].IPAllowlist = patch.IPAllowlist
	cfg.ApiKeys[idx].TPMLimit = patch.TPMLimit
	// Bound accounts are always overwritten from the patch (sanitized). Empty = the key
	// falls back to shared-pool routing; a non-empty set restricts it to those accounts.
	cfg.ApiKeys[idx].BoundAccountIDs = sanitizeBoundAccountIDsLocked(patch.BoundAccountIDs)
	cfg.ApiKeys[idx].Models = sanitizeModelList(patch.Models)
	cfg.ApiKeys[idx].Model = ""
	if patch.Migrated {
		cfg.ApiKeys[idx].Migrated = true
	}
	return saveLocked()
}

// DeleteApiKey removes the API key entry with the given ID. Returns nil even if
// the ID is unknown (idempotent), matching the existing DeleteAccount style.
func DeleteApiKey(id string) error {
	_, err := DeleteApiKeys([]string{id})
	return err
}

func DeleteApiKeys(ids []string) (int, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, errors.New("config not initialized")
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return 0, errors.New("no api key ids provided")
	}
	original := append([]ApiKeyEntry(nil), cfg.ApiKeys...)
	kept := cfg.ApiKeys[:0]
	deleted := 0
	for _, e := range cfg.ApiKeys {
		if want[e.ID] {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	cfg.ApiKeys = kept
	if deleted == 0 {
		return 0, nil
	}
	if err := saveLocked(); err != nil {
		cfg.ApiKeys = original
		return deleted, err
	}
	return deleted, nil
}

// FindApiKeyByValue returns a copy of the entry whose Key matches the given value,
// or nil if no match. O(n) linear scan.
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if subtle.ConstantTimeCompare([]byte(cfg.ApiKeys[i].Key), []byte(key)) == 1 {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage atomically adds tokens and credits to the entry's counters,
// updates LastUsedAt, and increments RequestsCount. It also folds the usage into the
// per-day bucket and per-model tally maintained by config/usage_history.go, so the
// trends survive a restart. This is a hot-path call, so it only marks the config
// dirty; the backgroundStatsSaver persists via FlushDirty.
//
// model may be empty (treated as "unknown"). failed, when true, only increments the
// failure counters without touching token or credit totals.
func RecordApiKeyUsage(id string, tokens int64, credits float64, model string, failed bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			e := &cfg.ApiKeys[i]
			if !failed {
				if tokens > 0 {
					e.TokensUsed += tokens
					e.LifetimeTokens += tokens
				}
				if credits > 0 {
					e.CreditsUsed += credits
					e.LifetimeCredits += credits
				}
				e.RequestsCount++
				e.LifetimeRequests++
			}
			e.LastUsedAt = time.Now().Unix()
			// Persist rolling usage history (daily buckets + model breakdown).
			inTok := tokens
			if failed {
				inTok = 0
			}
			addDailyUsageLocked(e, inTok, 0, credits, failed)
			addModelTallyLocked(e, model, inTok, 0, credits, failed)
			markDirtyLocked()
			return nil
		}
	}
	return errors.New("api key not found")
}

// ResetApiKeyUsage clears the current-period counters (TokensUsed/CreditsUsed/
// RequestsCount) for the entry, granting a fresh quota. Lifetime counters are left
// untouched so the grand total survives. LastUsedAt is preserved so operators can
// still see when the key was last used.
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// ResetApiKeyUsageAll clears BOTH the current-period counters and the lifetime counters
// for the entry, wiping all recorded usage as if the key were new. LastUsedAt is
// preserved. Use this for the "Reset All" action; use ResetApiKeyUsage for a routine
// per-cycle quota reset that keeps the grand total.
func ResetApiKeyUsageAll(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			cfg.ApiKeys[i].LifetimeTokens = 0
			cfg.ApiKeys[i].LifetimeCredits = 0
			cfg.ApiKeys[i].LifetimeRequests = 0
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// sanitizeBoundAccountIDsLocked trims, de-duplicates, and drops unknown IDs from a
// key's bound-account list, keeping only IDs that match a currently-stored account.
// Order is preserved (first occurrence wins). MUST be called with cfgLock held,
// since it reads cfg.Accounts directly (the RWMutex is not reentrant).
func sanitizeBoundAccountIDsLocked(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	known := make(map[string]bool, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		known[a.ID] = true
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] || !known[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// sanitizeModelList trims, drops empties, and de-duplicates a key's model allowlist,
// preserving order (first occurrence wins). Returns nil for an empty result so the
// field is omitted from JSON. Model IDs are stored verbatim (as chosen in the UI);
// normalization to the canonical upstream name happens later in applyModelOverride.
func sanitizeModelList(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		m := strings.TrimSpace(raw)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GenerateApiKeyValue returns a new random 32-byte hex API key prefixed with "sk-".
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a privacy-mode value like sk-***xxx.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 6 {
		return key
	}
	return key[:3] + "***" + key[len(key)-3:]
}

// ApiKeyExpired reports whether the key has a set expiry (ExpiresAt > 0) that is now
// in the past. Keys with ExpiresAt == 0 never expire.
func ApiKeyExpired(e ApiKeyEntry) bool {
	return e.ExpiresAt > 0 && time.Now().Unix() >= e.ExpiresAt
}

// ApiKeyRemaining returns how many credits remain for a key.
// A zero CreditLimit (unlimited) returns -1.
// A limit with zero usage returns the full limit.
func ApiKeyRemaining(e ApiKeyEntry) float64 {
	if e.CreditLimit <= 0 {
		return -1
	}
	r := e.CreditLimit - e.CreditsUsed
	if r < 0 {
		return 0
	}
	return r
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry. Limits with value 0
// are ignored. The function does not lock; callers should pass a copied entry.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}
