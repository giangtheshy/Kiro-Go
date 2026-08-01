package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// apiKeyView is the response payload for listing/inspecting API keys. The Key field
// is masked so admins can identify entries without exposing the secret.
type apiKeyView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	KeyMasked     string  `json:"keyMasked"`
	Enabled       bool    `json:"enabled"`
	Migrated      bool    `json:"migrated,omitempty"`
	CreatedAt     int64   `json:"createdAt"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
	ExpiresAt     int64   `json:"expiresAt,omitempty"`
	TokenLimit    int64   `json:"tokenLimit,omitempty"`
	CreditLimit   float64 `json:"creditLimit,omitempty"`
	TokensUsed    int64   `json:"tokensUsed"`
	CreditsUsed   float64 `json:"creditsUsed"`
	RequestsCount int64   `json:"requestsCount"`
	RPMLimit      int      `json:"rpmLimit,omitempty"`
	IPLimit       int      `json:"ipLimit,omitempty"`
	IPAllowlist   []string `json:"ipAllowlist,omitempty"`
	TPMLimit      int      `json:"tpmLimit,omitempty"`

	// Customer attribution labels, surfaced so the admin panel can answer
	// "whose key is this?" without querying the sales system.
	OwnerRef string `json:"ownerRef,omitempty"`
	OrderRef string `json:"orderRef,omitempty"`
	Note     string `json:"note,omitempty"`

	// BoundAccountIDs restricts routing to a fixed set of accounts (empty = shared pool).
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`

	// Models is the per-key model allowlist (empty = use client's model). A client model
	// in the list passes through; one not in the list is remapped to the first entry.
	Models []string `json:"models,omitempty"`

	// Lifetime totals — never cleared by "Reset Usage", only by "Reset All".
	LifetimeTokens   int64   `json:"lifetimeTokens"`
	LifetimeCredits  float64 `json:"lifetimeCredits"`
	LifetimeRequests int64   `json:"lifetimeRequests"`
}

func toApiKeyView(e config.ApiKeyEntry) apiKeyView {
	return apiKeyView{
		ID:            e.ID,
		Name:          e.Name,
		KeyMasked:     config.MaskApiKey(e.Key),
		Enabled:       e.Enabled,
		Migrated:      e.Migrated,
		CreatedAt:     e.CreatedAt,
		LastUsedAt:    e.LastUsedAt,
		ExpiresAt:     e.ExpiresAt,
		TokenLimit:    e.TokenLimit,
		CreditLimit:   e.CreditLimit,
		TokensUsed:    e.TokensUsed,
		CreditsUsed:   e.CreditsUsed,
		RequestsCount: e.RequestsCount,
		RPMLimit:      e.RPMLimit,
		IPLimit:       e.IPLimit,
		IPAllowlist:   e.IPAllowlist,
		TPMLimit:      e.TPMLimit,

		OwnerRef: e.OwnerRef,
		OrderRef: e.OrderRef,
		Note:     e.Note,

		BoundAccountIDs: e.BoundAccountIDs,
		Models:          e.Models,

		LifetimeTokens:   e.LifetimeTokens,
		LifetimeCredits:  e.LifetimeCredits,
		LifetimeRequests: e.LifetimeRequests,
	}
}

func (h *Handler) apiListApiKeys(w http.ResponseWriter, r *http.Request) {
	entries := config.ListApiKeys()
	out := make([]apiKeyView, len(entries))
	for i, e := range entries {
		out[i] = toApiKeyView(e)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"apiKeys": out})
}

func (h *Handler) apiGetApiKey(w http.ResponseWriter, r *http.Request, id string) {
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}
	json.NewEncoder(w).Encode(toApiKeyView(*entry))
}

type apiKeyCreateRequest struct {
	Name        string  `json:"name,omitempty"`
	Key         string  `json:"key,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
	RPMLimit    int      `json:"rpmLimit,omitempty"`
	IPLimit     int      `json:"ipLimit,omitempty"`
	IPAllowlist []string `json:"ipAllowlist,omitempty"`
	TPMLimit    int      `json:"tpmLimit,omitempty"`

	OwnerRef string `json:"ownerRef,omitempty"`
	OrderRef string `json:"orderRef,omitempty"`
	Note     string `json:"note,omitempty"`

	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	// Models is the per-key model allowlist. Model is a legacy single-value alias folded
	// into Models when Models is empty.
	Models []string `json:"models,omitempty"`
	Model  string   `json:"model,omitempty"`
}

func (h *Handler) apiCreateApiKey(w http.ResponseWriter, r *http.Request) {
	var req apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	entry, err := createApiKeyFromRequest(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Return the cleartext key exactly once on creation so the operator can copy it.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      entry.ID,
		"key":     entry.Key,
		"apiKey":  toApiKeyView(entry),
	})
}

func createApiKeyFromRequest(req apiKeyCreateRequest) (config.ApiKeyEntry, error) {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	keyValue := req.Key
	if keyValue == "" {
		keyValue = config.GenerateApiKeyValue()
	}

	return config.AddApiKey(config.ApiKeyEntry{
		Name:            req.Name,
		Key:             keyValue,
		Enabled:         enabled,
		TokenLimit:      req.TokenLimit,
		CreditLimit:     req.CreditLimit,
		ExpiresAt:       req.ExpiresAt,
		RPMLimit:        req.RPMLimit,
		IPLimit:         req.IPLimit,
		IPAllowlist:     sanitizeIPAllowlist(req.IPAllowlist),
		TPMLimit:        req.TPMLimit,
		OwnerRef:        strings.TrimSpace(req.OwnerRef),
		OrderRef:        strings.TrimSpace(req.OrderRef),
		Note:            strings.TrimSpace(req.Note),
		BoundAccountIDs: req.BoundAccountIDs,
		Models:          mergeModelList(req.Models, req.Model),
	})
}

// mergeModelList folds a legacy single-value model into the allowlist: the list wins
// when non-empty, otherwise a non-empty legacy value becomes a one-element list.
func mergeModelList(list []string, legacy string) []string {
	if len(list) > 0 {
		return list
	}
	if strings.TrimSpace(legacy) != "" {
		return []string{legacy}
	}
	return nil
}

type apiKeyBulkCreateRequest struct {
	Count       int     `json:"count"`
	NamePrefix  string  `json:"namePrefix,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
	RPMLimit    int      `json:"rpmLimit,omitempty"`
	IPLimit     int      `json:"ipLimit,omitempty"`
	IPAllowlist []string `json:"ipAllowlist,omitempty"`
	TPMLimit    int      `json:"tpmLimit,omitempty"`

	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	// Models is the per-key model allowlist; Model is a legacy single-value alias.
	Models []string `json:"models,omitempty"`
	Model  string   `json:"model,omitempty"`
}

func (h *Handler) apiBulkCreateApiKeys(w http.ResponseWriter, r *http.Request) {
	var req apiKeyBulkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Count <= 0 || req.Count > 100 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "count must be between 1 and 100"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	prefix := req.NamePrefix
	if prefix == "" {
		prefix = "API Key"
	}

	ipAllowlist := sanitizeIPAllowlist(req.IPAllowlist)
	models := mergeModelList(req.Models, req.Model)
	entries := make([]config.ApiKeyEntry, req.Count)
	for i := range entries {
		entries[i] = config.ApiKeyEntry{
			Name:            prefix + " " + strconv.Itoa(i+1),
			Key:             config.GenerateApiKeyValue(),
			Enabled:         enabled,
			TokenLimit:      req.TokenLimit,
			CreditLimit:     req.CreditLimit,
			ExpiresAt:       req.ExpiresAt,
			RPMLimit:        req.RPMLimit,
			IPLimit:         req.IPLimit,
			IPAllowlist:     ipAllowlist,
			TPMLimit:        req.TPMLimit,
			BoundAccountIDs: req.BoundAccountIDs,
			Models:          models,
		}
	}
	created, err := config.AddApiKeys(entries)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	keys := make([]string, len(created))
	views := make([]apiKeyView, len(created))
	for i, entry := range created {
		keys[i] = entry.Key
		views[i] = toApiKeyView(entry)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(created),
		"keys":    keys,
		"apiKeys": views,
	})
}

type apiKeyBulkDeleteRequest struct {
	IDs []string `json:"ids"`
}

func (h *Handler) apiBulkDeleteApiKeys(w http.ResponseWriter, r *http.Request) {
	var req apiKeyBulkDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	deleted, err := config.DeleteApiKeys(req.IDs)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "deleted": deleted})
}

type apiKeyUpdateRequest struct {
	Name        *string   `json:"name,omitempty"`
	Key         *string   `json:"key,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
	TokenLimit  *int64    `json:"tokenLimit,omitempty"`
	CreditLimit *float64  `json:"creditLimit,omitempty"`
	ExpiresAt   *int64    `json:"expiresAt,omitempty"`
	RPMLimit    *int      `json:"rpmLimit,omitempty"`
	IPLimit     *int      `json:"ipLimit,omitempty"`
	IPAllowlist *[]string `json:"ipAllowlist,omitempty"`
	TPMLimit    *int      `json:"tpmLimit,omitempty"`

	OwnerRef *string `json:"ownerRef,omitempty"`
	OrderRef *string `json:"orderRef,omitempty"`
	Note     *string `json:"note,omitempty"`

	BoundAccountIDs *[]string `json:"boundAccountIds,omitempty"`

	// Models is the per-key model allowlist. Model is a legacy single-value alias kept
	// for older API clients; it is folded into a one-element allowlist below.
	Models *[]string `json:"models,omitempty"`
	Model  *string   `json:"model,omitempty"`
}

func (h *Handler) apiUpdateApiKey(w http.ResponseWriter, r *http.Request, id string) {
	existing := config.GetApiKeyEntry(id)
	if existing == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}

	var req apiKeyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	patch := *existing
	if req.Name != nil {
		patch.Name = *req.Name
	}
	if req.Key != nil {
		patch.Key = *req.Key
	}
	if req.Enabled != nil {
		patch.Enabled = *req.Enabled
	}
	if req.TokenLimit != nil {
		patch.TokenLimit = *req.TokenLimit
	}
	if req.CreditLimit != nil {
		patch.CreditLimit = *req.CreditLimit
	}
	if req.ExpiresAt != nil {
		patch.ExpiresAt = *req.ExpiresAt
	}
	if req.RPMLimit != nil {
		patch.RPMLimit = *req.RPMLimit
	}
	if req.IPLimit != nil {
		patch.IPLimit = *req.IPLimit
	}
	if req.IPAllowlist != nil {
		patch.IPAllowlist = sanitizeIPAllowlist(*req.IPAllowlist)
	}
	if req.TPMLimit != nil {
		patch.TPMLimit = *req.TPMLimit
	}
	if req.OwnerRef != nil {
		patch.OwnerRef = strings.TrimSpace(*req.OwnerRef)
	}
	if req.OrderRef != nil {
		patch.OrderRef = strings.TrimSpace(*req.OrderRef)
	}
	if req.Note != nil {
		patch.Note = strings.TrimSpace(*req.Note)
	}
	if req.BoundAccountIDs != nil {
		patch.BoundAccountIDs = *req.BoundAccountIDs
	}
	// Models is authoritative when present; Model is the legacy single-value alias.
	if req.Models != nil {
		patch.Models = *req.Models
	} else if req.Model != nil {
		if m := *req.Model; m != "" {
			patch.Models = []string{m}
		} else {
			patch.Models = nil
		}
	}

	if err := config.UpdateApiKey(id, patch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// If the key value or IP limits changed, drop the tracked IP allow-set so stale
	// slots don't linger.
	if h.ipLimiter != nil && (req.Key != nil || req.IPLimit != nil || req.IPAllowlist != nil) {
		h.ipLimiter.forget(id)
	}

	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to reload entry"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}

// apiKeyExportView is the masked usage-report row. It extends the masked fields
// with server-computed derived columns so the frontend renders both JSON and CSV
// from one source. Never contains the raw key value.
type apiKeyExportView struct {
	ID                string  `json:"id"`
	Name              string  `json:"name,omitempty"`
	KeyMasked         string  `json:"keyMasked"`
	Enabled           bool    `json:"enabled"`
	RequestsCount     int64   `json:"requestsCount"`
	TokensUsed        int64   `json:"tokensUsed"`
	CreditsUsed       float64 `json:"creditsUsed"`
	TokenLimit        int64   `json:"tokenLimit"`
	CreditLimit       float64 `json:"creditLimit"`
	ExpiresAt         int64   `json:"expiresAt"`
	CreatedAt         int64   `json:"createdAt"`
	LastUsedAt        int64   `json:"lastUsedAt"`
	TokenPercentUsed  float64 `json:"tokenPercentUsed"`
	CreditPercentUsed float64 `json:"creditPercentUsed"`
	OverToken         bool    `json:"overToken"`
	OverCredit        bool    `json:"overCredit"`
	Expired           bool    `json:"expired"`
}

func toApiKeyExportView(e config.ApiKeyEntry) apiKeyExportView {
	overToken, overCredit := config.ApiKeyOverLimit(e)
	tokenPct := 0.0
	if e.TokenLimit > 0 {
		tokenPct = float64(e.TokensUsed) / float64(e.TokenLimit) * 100
	}
	creditPct := 0.0
	if e.CreditLimit > 0 {
		creditPct = e.CreditsUsed / e.CreditLimit * 100
	}
	return apiKeyExportView{
		ID:                e.ID,
		Name:              e.Name,
		KeyMasked:         config.MaskApiKey(e.Key),
		Enabled:           e.Enabled,
		RequestsCount:     e.RequestsCount,
		TokensUsed:        e.TokensUsed,
		CreditsUsed:       e.CreditsUsed,
		TokenLimit:        e.TokenLimit,
		CreditLimit:       e.CreditLimit,
		ExpiresAt:         e.ExpiresAt,
		CreatedAt:         e.CreatedAt,
		LastUsedAt:        e.LastUsedAt,
		TokenPercentUsed:  tokenPct,
		CreditPercentUsed: creditPct,
		OverToken:         overToken,
		OverCredit:        overCredit,
		Expired:           config.ApiKeyExpired(e),
	}
}

// apiExportApiKeys handles POST /admin/api/api-keys/export. It returns a masked
// usage report (never re-importable). Body: {"ids": [...]}; empty/missing = all.
func (h *Handler) apiExportApiKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	// Empty/invalid body = export all.
	_ = json.NewDecoder(r.Body).Decode(&req)

	entries := config.ListApiKeys()
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool, len(req.IDs))
		for _, id := range req.IDs {
			idSet[id] = true
		}
		filtered := entries[:0]
		for _, e := range entries {
			if idSet[e.ID] {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	views := make([]apiKeyExportView, len(entries))
	for i, e := range entries {
		views[i] = toApiKeyExportView(e)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":    config.Version,
		"exportedAt": time.Now().Unix(),
		"apiKeys":    views,
	})
}

func (h *Handler) apiDeleteApiKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiResetApiKeyUsage(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.ResetApiKeyUsage(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Clear the tracked IP allow-set so a reset also frees all IP slots.
	if h.ipLimiter != nil {
		h.ipLimiter.forget(id)
	}
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}

// apiResetApiKeyUsageAll wipes BOTH the current-period and lifetime counters, resetting
// the key as if it were new. Unlike apiResetApiKeyUsage (which keeps the lifetime total),
// this is the destructive "Reset All" action.
func (h *Handler) apiResetApiKeyUsageAll(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.ResetApiKeyUsageAll(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Clear the tracked IP allow-set so a full reset also frees all IP slots.
	if h.ipLimiter != nil {
		h.ipLimiter.forget(id)
	}
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}
