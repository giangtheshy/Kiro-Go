package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedExportKeys inserts four entries covering: unlimited, under-limit,
// over-token-limit, and expired. Returns their IDs in insertion order.
func seedExportKeys(t *testing.T) (unlimited, under, overTok, expired string) {
	t.Helper()

	e1, err := config.AddApiKey(config.ApiKeyEntry{Name: "unlimited", Key: "sk-unlimited-secret", Enabled: true})
	if err != nil {
		t.Fatalf("seed unlimited: %v", err)
	}
	e2, err := config.AddApiKey(config.ApiKeyEntry{Name: "under", Key: "sk-under-secret", Enabled: true, TokenLimit: 1000, CreditLimit: 10})
	if err != nil {
		t.Fatalf("seed under: %v", err)
	}
	e3, err := config.AddApiKey(config.ApiKeyEntry{Name: "overtok", Key: "sk-overtok-secret", Enabled: true, TokenLimit: 100})
	if err != nil {
		t.Fatalf("seed overtok: %v", err)
	}
	e4, err := config.AddApiKey(config.ApiKeyEntry{Name: "expired", Key: "sk-expired-secret", Enabled: true, ExpiresAt: time.Now().Unix() - 3600})
	if err != nil {
		t.Fatalf("seed expired: %v", err)
	}

	// Drive usage counters. under: 500/1000 tokens, 5/10 credits. overtok: 200/100 tokens.
	if err := config.RecordApiKeyUsage(e2.ID, 500, 5, "claude-sonnet", false); err != nil {
		t.Fatalf("usage under: %v", err)
	}
	if err := config.RecordApiKeyUsage(e3.ID, 200, 0, "claude-sonnet", false); err != nil {
		t.Fatalf("usage overtok: %v", err)
	}
	return e1.ID, e2.ID, e3.ID, e4.ID
}

func decodeExport(t *testing.T, body string, ids []string) map[string]apiKeyExportView {
	t.Helper()
	reqBody := "{}"
	if ids != nil {
		b, _ := json.Marshal(map[string][]string{"ids": ids})
		reqBody = string(b)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/export", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	h := &Handler{}
	h.apiExportApiKeys(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Raw secret must never appear.
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("raw key value leaked in output: %s", w.Body.String())
	}

	var out struct {
		Version    string             `json:"version"`
		ExportedAt int64              `json:"exportedAt"`
		ApiKeys    []apiKeyExportView `json:"apiKeys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ExportedAt == 0 {
		t.Fatalf("expected exportedAt to be set")
	}
	byID := make(map[string]apiKeyExportView, len(out.ApiKeys))
	for _, v := range out.ApiKeys {
		byID[v.ID] = v
	}
	return byID
}

func TestExportApiKeysAllMaskedAndDerived(t *testing.T) {
	mustInitConfig(t)
	_, under, overTok, expired := seedExportKeys(t)

	got := decodeExport(t, "", nil)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}

	u := got[under]
	if !strings.HasPrefix(u.KeyMasked, "sk-") || !strings.Contains(u.KeyMasked, "***") {
		t.Fatalf("under key not masked: %q", u.KeyMasked)
	}
	if u.TokenPercentUsed != 50 {
		t.Fatalf("under tokenPercentUsed: want 50, got %v", u.TokenPercentUsed)
	}
	if u.CreditPercentUsed != 50 {
		t.Fatalf("under creditPercentUsed: want 50, got %v", u.CreditPercentUsed)
	}
	if u.OverToken || u.OverCredit || u.Expired {
		t.Fatalf("under should not be over/expired: %+v", u)
	}

	o := got[overTok]
	if !o.OverToken {
		t.Fatalf("overtok should be OverToken: %+v", o)
	}
	if o.TokenPercentUsed != 200 {
		t.Fatalf("overtok tokenPercentUsed: want 200, got %v", o.TokenPercentUsed)
	}

	e := got[expired]
	if !e.Expired {
		t.Fatalf("expired key should have Expired=true: %+v", e)
	}
	// Unlimited: no limits => percent 0.
	for _, v := range got {
		if v.Name == "unlimited" && (v.TokenPercentUsed != 0 || v.CreditPercentUsed != 0) {
			t.Fatalf("unlimited should have 0 percents: %+v", v)
		}
	}
}

func TestExportApiKeysFilterByIDs(t *testing.T) {
	mustInitConfig(t)
	unlimited, under, _, _ := seedExportKeys(t)

	got := decodeExport(t, "", []string{unlimited, under})
	if len(got) != 2 {
		t.Fatalf("expected 2 filtered entries, got %d", len(got))
	}
	if _, ok := got[unlimited]; !ok {
		t.Fatalf("expected unlimited in filtered result")
	}
	if _, ok := got[under]; !ok {
		t.Fatalf("expected under in filtered result")
	}
}
