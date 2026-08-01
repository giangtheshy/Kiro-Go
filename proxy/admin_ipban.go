package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net"
	"net/http"
	"strings"
)

// admin_ipban.go implements the /admin/api/ip-bans routes.
// All handlers require admin authorization (enforced by the handleAdminAPI gate).

// apiGetIPBans returns the ban list, current settings, and near-threshold IPs.
func (h *Handler) apiGetIPBans(w http.ResponseWriter, r *http.Request) {
	threshold := config.GetBanThreshold()
	enabled := config.BanEnabled()
	bans := config.BannedIPsSnapshot()

	var nearThreshold []failView
	if h.ipBanGate != nil {
		nearThreshold = h.ipBanGate.nearThresholdSnapshot()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"bans":          bans,
		"threshold":     threshold,
		"enabled":       enabled,
		"nearThreshold": nearThreshold,
	})
}

// apiAddIPBan handles POST /admin/api/ip-bans.
func (h *Handler) apiAddIPBan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP   string `json:"ip"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	ip := strings.TrimSpace(body.IP)
	if net.ParseIP(ip) == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "not a valid IP address"})
		return
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "refusing to ban loopback address"})
		return
	}
	if adminIP := h.resolveClientIP(r); adminIP == ip {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "refusing to ban your own address"})
		return
	}

	added, err := config.BanIP(ip, config.BanReasonManual, body.Note, 0)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	config.RecordAudit(config.AuditEntry{
		Action: config.AuditIPBan,
		Actor:  config.AuditActorAdmin,
		Target: ip,
		Detail: body.Note,
		IP:     h.resolveClientIP(r),
	})

	if !added {
		// Refreshed an existing ban.
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "refreshed": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// apiDeleteIPBan handles DELETE /admin/api/ip-bans/:ip (exact IP).
func (h *Handler) apiDeleteIPBan(w http.ResponseWriter, r *http.Request, ip string) {
	ip = strings.TrimSpace(ip)
	removed, err := config.UnbanIP(ip)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if !removed {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "IP not found in ban list"})
		return
	}
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditIPUnban,
		Actor:  config.AuditActorAdmin,
		Target: ip,
		IP:     h.resolveClientIP(r),
	})
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// apiClearIPBans handles DELETE /admin/api/ip-bans (clear all).
func (h *Handler) apiClearIPBans(w http.ResponseWriter, r *http.Request) {
	n, err := config.ClearBans()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditIPBanClear,
		Actor:  config.AuditActorAdmin,
		IP:     h.resolveClientIP(r),
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "removed": n})
}

// apiUpdateIPBanSettings handles PATCH /admin/api/ip-bans/settings.
// Uses pointer fields so a missing JSON key leaves the setting unchanged.
func (h *Handler) apiUpdateIPBanSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled   *bool `json:"enabled"`
		Threshold *int  `json:"threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if body.Enabled == nil && body.Threshold == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "nothing to update"})
		return
	}
	if body.Enabled != nil {
		if err := config.SetBanEnabled(*body.Enabled); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if body.Threshold != nil {
		if *body.Threshold < 1 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "threshold must be at least 1"})
			return
		}
		if err := config.SetBanThreshold(*body.Threshold); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditIPBanSettings,
		Actor:  config.AuditActorAdmin,
		IP:     h.resolveClientIP(r),
	})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled":   config.BanEnabled(),
		"threshold": config.GetBanThreshold(),
	})
}

// apiGetAuditLog handles GET /admin/api/audit-log.
func (h *Handler) apiGetAuditLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	action := r.URL.Query().Get("action")
	entries := config.AuditLogSnapshot(limit, action)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
	})
}
