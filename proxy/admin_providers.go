package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
)

// admin_providers.go implements the /admin/api/providers routes for external
// OpenAI-/Anthropic-compatible upstreams. Admin authorization is enforced by the
// handleAdminAPI gate before any of these run.

// providerView is the wire shape returned to the admin panel. It mirrors
// config.Provider but never exposes the raw API key.
type providerView struct {
	config.Provider
	APIKey    string `json:"apiKey"`    // masked
	HasAPIKey bool   `json:"hasApiKey"` // whether a key is stored at all
}

func toProviderView(p config.Provider) providerView {
	v := providerView{Provider: p, HasAPIKey: p.APIKey != "", APIKey: config.MaskApiKey(p.APIKey)}
	v.Provider.APIKey = ""
	return v
}

// apiGetProviders handles GET /admin/api/providers.
func (h *Handler) apiGetProviders(w http.ResponseWriter, r *http.Request) {
	all := config.GetProviders()
	views := make([]providerView, 0, len(all))
	for _, p := range all {
		views = append(views, toProviderView(p))
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"providers": views})
}

// apiAddProvider handles POST /admin/api/providers.
func (h *Handler) apiAddProvider(w http.ResponseWriter, r *http.Request) {
	var body config.Provider
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProviderError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.ID = ""

	created, err := config.AddProvider(body)
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, err.Error())
		return
	}

	config.RecordAudit(config.AuditEntry{
		Action: config.AuditProviderCreate,
		Actor:  config.AuditActorAdmin,
		Target: created.Name,
		Detail: fmt.Sprintf("protocol=%s priority=%d models=%d", created.Protocol, created.Priority, len(created.Models)),
		IP:     h.resolveClientIP(r),
	})

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "provider": toProviderView(created)})
}

// apiUpdateProvider handles PUT /admin/api/providers/{id}. An empty apiKey keeps
// the stored credential, so the panel can round-trip the masked value.
func (h *Handler) apiUpdateProvider(w http.ResponseWriter, r *http.Request, id string) {
	var body config.Provider
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProviderError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if err := config.UpdateProvider(id, body); err != nil {
		status := http.StatusBadRequest
		if err.Error() == "provider not found" {
			status = http.StatusNotFound
		}
		writeProviderError(w, status, err.Error())
		return
	}

	config.RecordAudit(config.AuditEntry{
		Action: config.AuditProviderUpdate,
		Actor:  config.AuditActorAdmin,
		Target: body.Name,
		Detail: fmt.Sprintf("protocol=%s priority=%d enabled=%t", body.Protocol, body.Priority, body.Enabled),
		IP:     h.resolveClientIP(r),
	})

	updated := config.GetProvider(id)
	if updated == nil {
		writeProviderError(w, http.StatusNotFound, "provider not found")
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "provider": toProviderView(*updated)})
}

// apiDeleteProvider handles DELETE /admin/api/providers/{id}.
func (h *Handler) apiDeleteProvider(w http.ResponseWriter, r *http.Request, id string) {
	existing := config.GetProvider(id)
	if err := config.DeleteProvider(id); err != nil {
		writeProviderError(w, http.StatusNotFound, err.Error())
		return
	}

	name := id
	if existing != nil {
		name = existing.Name
	}
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditProviderDelete,
		Actor:  config.AuditActorAdmin,
		Target: name,
		IP:     h.resolveClientIP(r),
	})

	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// apiTestProvider handles POST /admin/api/providers/{id}/test. It sends a real
// minimal completion so the operator learns whether the base URL, key, headers,
// proxy and model mapping actually work — before customer traffic finds out.
func (h *Handler) apiTestProvider(w http.ResponseWriter, r *http.Request, id string) {
	p := config.GetProvider(id)
	if p == nil {
		writeProviderError(w, http.StatusNotFound, "provider not found")
		return
	}
	if len(p.Models) == 0 {
		writeProviderError(w, http.StatusBadRequest, "provider has no model mapping")
		return
	}

	var body struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	alias := strings.TrimSpace(body.Model)
	if alias == "" {
		alias = p.Models[0].Alias
	}
	upstreamModel, _, ok := p.ResolveModel(alias)
	if !ok {
		writeProviderError(w, http.StatusBadRequest, "model not mapped by this provider: "+alias)
		return
	}

	endpoint := config.ProviderEndpointChatCompletions
	if p.Protocol == config.ProviderProtocolAnthropic {
		endpoint = config.ProviderEndpointMessages
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"model":      upstreamModel,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})

	proxyURL, _, err := SelectProxyForAccount(&config.Account{ProxyURL: p.ProxyURL})
	if err != nil {
		writeProviderError(w, http.StatusBadGateway, err.Error())
		return
	}

	req, err := http.NewRequest(http.MethodPost, p.BaseURL+endpoint, bytes.NewReader(payload))
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, err.Error())
		return
	}
	applyProviderHeaders(req, p, &passthroughCtx{Header: http.Header{}, Endpoint: endpoint})

	start := time.Now()
	client := GetRestClientForProxy(proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
			"model": upstreamModel,
		})
		return
	}
	defer resp.Body.Close()

	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	out := map[string]interface{}{
		"ok":         success,
		"status":     resp.StatusCode,
		"model":      upstreamModel,
		"durationMs": time.Since(start).Milliseconds(),
	}
	if !success {
		out["error"] = strings.TrimSpace(string(detail))
	}
	json.NewEncoder(w).Encode(out)
}

func writeProviderError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
