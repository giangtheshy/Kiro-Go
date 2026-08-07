package config

import (
	"sort"
	"time"
)

// AuditLogKept caps the retained audit trail by entry count. Entries live in
// config.json, which is rewritten whole on every save, so this is a size bound as
// much as a retention policy.
const AuditLogKept = 2000

// Audit actor labels — who performed the action.
const (
	AuditActorAdmin  = "admin-panel"
	AuditActorSales  = "sales-api"
	AuditActorSystem = "system"
)

// Audit action labels. Stable strings: the admin UI filters on them.
const (
	AuditKeyCreate     = "key.create"
	AuditKeyUpdate     = "key.update"
	AuditKeyDelete     = "key.delete"
	AuditKeyTopUp      = "key.topup"
	AuditKeyExtend     = "key.extend"
	AuditKeyEnable     = "key.enable"
	AuditKeyDisable    = "key.disable"
	AuditKeyResetUsage = "key.reset-usage"
	AuditKeyBulkGrant  = "key.bulk-grant"
	AuditIPBan         = "ip.ban"
	AuditIPUnban       = "ip.unban"
	AuditIPBanClear    = "ip.ban-clear"
	AuditIPBanSettings = "ip.ban-settings"
	AuditMaintenance   = "maintenance.set"

	AuditProviderCreate = "provider.create"
	AuditProviderUpdate = "provider.update"
	AuditProviderDelete = "provider.delete"
)

// AuditEntry is one recorded administrative action. Detail is free-form text for
// an operator to read; it must never contain a plaintext API key or password.
type AuditEntry struct {
	TimeUnix int64  `json:"timeUnix"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Target   string `json:"target,omitempty"`
	Detail   string `json:"detail,omitempty"`
	IP       string `json:"ip,omitempty"`
}

// RecordAudit appends an entry to the audit trail.
//
// This runs on admin/sales requests rather than the inference hot path, but it is
// still only marked dirty rather than saved inline: a mutation that matters has
// already persisted itself via its own Save(), and forcing a second full-config
// write per audit line would double the disk cost of every admin action.
func RecordAudit(e AuditEntry) {
	if e.Action == "" {
		return
	}
	if e.TimeUnix == 0 {
		e.TimeUnix = time.Now().Unix()
	}
	if e.Actor == "" {
		e.Actor = AuditActorSystem
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return
	}
	cfg.AuditLog = append(cfg.AuditLog, e)
	if len(cfg.AuditLog) > AuditLogKept {
		cfg.AuditLog = append([]AuditEntry(nil), cfg.AuditLog[len(cfg.AuditLog)-AuditLogKept:]...)
	}
	markDirtyLocked()
}

// AuditLogSnapshot returns audit entries newest first. An empty action returns
// every action; a limit <= 0 means no limit.
func AuditLogSnapshot(limit int, action string) []AuditEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]AuditEntry, 0, len(cfg.AuditLog))
	for _, e := range cfg.AuditLog {
		if action != "" && e.Action != action {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeUnix > out[j].TimeUnix })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ClearAuditLog empties the audit trail and returns how many entries were removed.
func ClearAuditLog() (int, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, nil
	}
	n := len(cfg.AuditLog)
	if n == 0 {
		return 0, nil
	}
	original := cfg.AuditLog
	cfg.AuditLog = nil
	if err := saveLocked(); err != nil {
		cfg.AuditLog = original
		return 0, err
	}
	return n, nil
}
