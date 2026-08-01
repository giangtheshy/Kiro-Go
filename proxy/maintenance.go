package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
)

// maintenance.go implements the GET /status public endpoint and the maintenance
// mode toggle under POST /admin/api/maintenance.

// handlePublicStatus serves GET /status — no auth required, intentionally
// light-weight so external monitors and bots can poll it safely.
func (h *Handler) handlePublicStatus(w http.ResponseWriter, r *http.Request) {
	setWebSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	status := "ok"
	if config.GetMaintenanceMode() {
		status = "maintenance"
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        status,
		"message":       config.GetMaintenanceMessage(),
		"version":       config.Version,
		"poolAvailable": h.pool.AvailableCount(),
		"uptimeSec":     h.uptimeSeconds(),
	})
}

// uptimeSeconds returns the number of seconds since the proxy started.
func (h *Handler) uptimeSeconds() int64 {
	return timeNowUnix() - h.startTime
}

// isMaintenanceMode checks config and, if on, writes the maintenance notice
// to w in the format matching the endpoint type, and returns true.
//
// The function respects the existing limit-notice infrastructure so the client
// receives a well-formed, readable error rather than a cryptic HTTP code:
//   - Claude clients get the standard {"error":…} body
//   - OpenAI clients get the standard {"error":{"message":…}} wrapper
//   - Raw health/status/admin paths bypass this entirely
func (h *Handler) isMaintenanceMode(w http.ResponseWriter, r *http.Request, path string) bool {
	if !config.GetMaintenanceMode() {
		return false
	}
	msg := config.GetMaintenanceMessage()
	if isOpenAIStylePath(path) {
		h.sendOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
	} else {
		h.sendClaudeError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
	}
	return true
}

// apiSetMaintenanceMode handles POST /admin/api/maintenance.
func (h *Handler) apiSetMaintenanceMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if err := config.SetMaintenanceMode(body.Enabled, body.Message); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	ip := h.resolveClientIP(r)
	detail := "disabled"
	if body.Enabled {
		detail = "enabled: " + body.Message
	}
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditMaintenance,
		Actor:  config.AuditActorAdmin,
		Detail: detail,
		IP:     ip,
	})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"enabled": body.Enabled,
		"message": body.Message,
	})
}
