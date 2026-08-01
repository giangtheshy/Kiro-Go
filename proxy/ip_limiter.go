package proxy

import (
	"net"
	"strings"
	"sync"
	"time"
)

// ipLimitWindow is the rolling window over which distinct client IPs are counted
// against a key's IPLimit. An IP that has not been seen within this window is
// forgotten, so a user whose address changes (mobile networks, DHCP) is not locked
// out forever — stale IPs free up slots automatically.
const ipLimitWindow = 24 * time.Hour

// ipLimiter enforces a per-key cap on the number of DISTINCT client IPs allowed to
// use a key within ipLimitWindow. It does NOT reject already-seen IPs; only a
// not-yet-seen IP that would exceed the cap is blocked. State is in-memory only,
// keyed by API key ID, and self-prunes stale IPs so it needs no background cleanup.
type ipLimiter struct {
	mu   sync.Mutex
	keys map[string]map[string]time.Time // keyID -> (ip -> lastSeen)
}

func newIPLimiter() *ipLimiter {
	return &ipLimiter{keys: make(map[string]map[string]time.Time)}
}

// allow records a request from ip against keyID and reports whether it may proceed.
// A limit of 0 (or negative) means unlimited and always allows. An empty ip is
// treated as unlimited (we cannot attribute it, so we do not block on it).
//
// Semantics: IPs already active within ipLimitWindow always pass and have their
// lastSeen refreshed. A new IP passes only if the count of active IPs is below
// limit; otherwise it is rejected without being recorded, so it does not consume
// a slot and can succeed later once an existing IP goes stale.
func (l *ipLimiter) allow(keyID, ip string, limit int) bool {
	if limit <= 0 || keyID == "" || ip == "" {
		return true
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	set := l.keys[keyID]
	if set == nil {
		set = make(map[string]time.Time)
		l.keys[keyID] = set
	}

	// Fast path: an IP already tracked within the window just refreshes its
	// timestamp. No need to scan/prune the whole set on every request.
	if _, ok := set[ip]; ok {
		set[ip] = now
		return true
	}

	// New IP. Only when the set looks full do we prune stale entries to try to
	// free a slot before rejecting — this keeps the common case O(1) instead of
	// O(n) under the shared lock.
	if len(set) >= limit {
		for existing, seen := range set {
			if now.Sub(seen) > ipLimitWindow {
				delete(set, existing)
			}
		}
		if len(set) >= limit {
			return false
		}
	}
	set[ip] = now
	return true
}

// forget drops all tracked IPs for a key. Called when a key's usage is reset so an
// operator can clear the IP allow-set alongside the counters.
func (l *ipLimiter) forget(keyID string) {
	if keyID == "" {
		return
	}
	l.mu.Lock()
	delete(l.keys, keyID)
	l.mu.Unlock()
}

// ipSeenEntry is one entry in the snapshot returned by snapshot().
type ipSeenEntry struct {
	IP       string `json:"ip"`
	LastSeen int64  `json:"lastSeen"` // Unix seconds
}

// snapshot returns the IPs active within ipLimitWindow for a key, sorted by
// most-recently-seen descending. Used by the anomaly detector to expose the
// per-key distinct-IP count without having to recount on every request.
func (l *ipLimiter) snapshot(keyID string) []ipSeenEntry {
	if keyID == "" {
		return nil
	}
	now := time.Now()
	l.mu.Lock()
	set := l.keys[keyID]
	if set == nil {
		l.mu.Unlock()
		return nil
	}
	out := make([]ipSeenEntry, 0, len(set))
	for ip, seen := range set {
		if now.Sub(seen) <= ipLimitWindow {
			out = append(out, ipSeenEntry{IP: ip, LastSeen: seen.Unix()})
		}
	}
	l.mu.Unlock()
	// Sort most-recently-seen first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].LastSeen > out[i].LastSeen {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ipMatchesAllowlist reports whether ip matches any entry in allowlist. Each entry
// is either a single IP ("203.0.113.7", "2001:db8::1") or a CIDR block
// ("203.0.113.0/24"). An empty allowlist returns false (callers treat empty as
// "no allowlist configured" before calling this). An unparseable ip returns false.
// Malformed entries are skipped rather than causing an error, so one bad entry does
// not disable the whole allowlist.
func ipMatchesAllowlist(ip string, allowlist []string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, raw := range allowlist {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, network, err := net.ParseCIDR(entry)
			if err != nil {
				continue
			}
			if network.Contains(parsed) {
				return true
			}
			continue
		}
		if candidate := net.ParseIP(entry); candidate != nil && candidate.Equal(parsed) {
			return true
		}
	}
	return false
}

// sanitizeIPAllowlist trims, drops empties, and keeps only entries that parse as a
// valid IP or CIDR. It returns the cleaned list so invalid input from the admin UI is
// rejected at write time rather than silently ignored at request time.
func sanitizeIPAllowlist(entries []string) []string {
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		valid := false
		if strings.Contains(entry, "/") {
			if _, _, err := net.ParseCIDR(entry); err == nil {
				valid = true
			}
		} else if net.ParseIP(entry) != nil {
			valid = true
		}
		if !valid {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out
}
