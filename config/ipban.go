package config

import (
	"errors"
	"net"
	"strings"
	"time"
)

// Ban reasons recorded on a BannedIP. These are stable strings persisted in
// config.json and shown in the admin panel, so do not rename them casually.
const (
	// BanReasonManual is a ban an operator added from the admin panel.
	BanReasonManual = "manual"
	// BanReasonAutoSpam is an automatic ban after too many invalid API keys were
	// presented from one address within the fail window.
	BanReasonAutoSpam = "auto-spam"
	// BanReasonAutoAdmin is an automatic ban after too many wrong admin passwords.
	BanReasonAutoAdmin = "auto-admin-login"
)

// DefaultBanThreshold is the failed-attempt count that trips an automatic ban when
// the operator has not configured BanThreshold.
const DefaultBanThreshold = 5

// BannedIP is a client address blocked from every endpoint except /health and
// /status. Bans do not expire on their own; an operator lifts them explicitly.
type BannedIP struct {
	IP         string `json:"ip"`
	Reason     string `json:"reason"`
	Fails      int    `json:"fails,omitempty"`
	BannedUnix int64  `json:"bannedUnix"`
	Note       string `json:"note,omitempty"`
}

// BanEnabled reports whether the IP ban gate is active.
//
// The persisted field is BanDisabled — stored inverted on purpose. JSON's zero
// value for a bool is false, so a config.json written before this feature existed
// (and any future one an operator never touches) keeps the gate ON. A "banEnabled"
// field would instead silently disable protection for every deployment that
// upgrades without editing its config.
func BanEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return !cfg.BanDisabled
}

// SetBanEnabled turns the ban gate on or off and persists the change.
func SetBanEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	cfg.BanDisabled = !enabled
	return saveLocked()
}

// GetBanThreshold returns the configured auto-ban threshold, or DefaultBanThreshold
// when unset. The value is always at least 1.
func GetBanThreshold() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.BanThreshold <= 0 {
		return DefaultBanThreshold
	}
	return cfg.BanThreshold
}

// SetBanThreshold stores the auto-ban threshold, clamped to at least 1 so a zero
// can never arm a one-strike ban.
func SetBanThreshold(n int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if n < 1 {
		n = 1
	}
	cfg.BanThreshold = n
	return saveLocked()
}

// IsBanned reports whether ip is on the ban list. It returns false when the gate
// is disabled, so an operator can lift a bad lockout with a single toggle without
// having to clear the list first.
func IsBanned(ip string) bool {
	if ip == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.BanDisabled {
		return false
	}
	for i := range cfg.BannedIPs {
		if cfg.BannedIPs[i].IP == ip {
			return true
		}
	}
	return false
}

// BannedIPsSnapshot returns a copy of the ban list, newest ban first.
func BannedIPsSnapshot() []BannedIP {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]BannedIP, len(cfg.BannedIPs))
	copy(out, cfg.BannedIPs)
	// Newest first. Sorted here rather than at insert so the stored order stays
	// append-only and diff-friendly.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// BanIP adds ip to the ban list, or refreshes an existing entry rather than
// duplicating it. It reports whether the address was newly banned (false = the
// entry already existed and was refreshed). The write is persisted immediately:
// a ban that survives only until the next restart is not a ban.
func BanIP(ip, reason, note string, fails int) (bool, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" || net.ParseIP(ip) == nil {
		return false, errors.New("not a valid IP address")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return false, errors.New("config not initialized")
	}
	if reason == "" {
		reason = BanReasonManual
	}
	now := time.Now().Unix()
	for i := range cfg.BannedIPs {
		if cfg.BannedIPs[i].IP != ip {
			continue
		}
		cfg.BannedIPs[i].Reason = reason
		cfg.BannedIPs[i].BannedUnix = now
		if fails > 0 {
			cfg.BannedIPs[i].Fails = fails
		}
		if note != "" {
			cfg.BannedIPs[i].Note = note
		}
		return false, saveLocked()
	}
	entry := BannedIP{IP: ip, Reason: reason, Fails: fails, BannedUnix: now, Note: note}
	cfg.BannedIPs = append(cfg.BannedIPs, entry)
	if err := saveLocked(); err != nil {
		cfg.BannedIPs = cfg.BannedIPs[:len(cfg.BannedIPs)-1]
		return false, err
	}
	return true, nil
}

// UnbanIP removes ip from the ban list. It reports whether an entry was removed.
func UnbanIP(ip string) (bool, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false, nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return false, errors.New("config not initialized")
	}
	original := append([]BannedIP(nil), cfg.BannedIPs...)
	kept := cfg.BannedIPs[:0]
	removed := false
	for _, b := range cfg.BannedIPs {
		if b.IP == ip {
			removed = true
			continue
		}
		kept = append(kept, b)
	}
	cfg.BannedIPs = kept
	if !removed {
		return false, nil
	}
	if err := saveLocked(); err != nil {
		cfg.BannedIPs = original
		return false, err
	}
	return true, nil
}

// ClearBans empties the ban list and returns how many entries were removed.
func ClearBans() (int, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, errors.New("config not initialized")
	}
	n := len(cfg.BannedIPs)
	if n == 0 {
		return 0, nil
	}
	original := cfg.BannedIPs
	cfg.BannedIPs = nil
	if err := saveLocked(); err != nil {
		cfg.BannedIPs = original
		return 0, err
	}
	return n, nil
}
