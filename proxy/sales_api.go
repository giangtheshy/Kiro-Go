package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sales_api.go implements the /api/sales/v1/* endpoints consumed by the
// Telegram sales bot.
//
// Auth: same mechanism as the admin panel — either the HttpOnly admin_session
// cookie (set at POST /admin/api/login) or the X-Admin-Password header.
// The bot holds the admin password and re-uses the login flow.
//
// Security properties:
//   - GET /keys does NOT return plaintext keys; the bot already stored them at
//     create time and reads them from its own DB.
//   - Audit entries are written for every mutating call.
//   - PlainText key is returned ONCE — in the POST /keys create response only.
//   - Error codes for the top-up endpoint are a stable external contract;
//     do not rename them without updating the bot's TERMINAL_TOPUP_CODES set.

func (h *Handler) handleSalesAPI(w http.ResponseWriter, r *http.Request) {
	// Sales API inherits the admin auth gate so the admin password / session
	// cookie already verified by handleAdminAPI before we get here.
	// But we're reached from ServeHTTP directly, so we need to auth here too.
	if !h.salesAuthorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/sales/v1")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	// --- Pool health ---
	case path == "/overview" && r.Method == http.MethodGet:
		h.salesOverview(w, r)

	// --- Key management ---
	case path == "/keys" && r.Method == http.MethodGet:
		h.salesListKeys(w, r)
	case path == "/keys" && r.Method == http.MethodPost:
		h.salesCreateKey(w, r)

	// Suffix routes MUST come before prefix-only routes.
	case strings.HasSuffix(path, "/credits") && r.Method == http.MethodPost:
		id := strings.TrimPrefix(path, "/keys/")
		id = strings.TrimSuffix(id, "/credits")
		h.salesAddCredits(w, r, id)
	case strings.HasSuffix(path, "/extend") && r.Method == http.MethodPost:
		id := strings.TrimPrefix(path, "/keys/")
		id = strings.TrimSuffix(id, "/extend")
		h.salesExtendKey(w, r, id)
	case strings.HasSuffix(path, "/enabled") && r.Method == http.MethodPatch:
		id := strings.TrimPrefix(path, "/keys/")
		id = strings.TrimSuffix(id, "/enabled")
		h.salesSetKeyEnabled(w, r, id)
	case strings.HasSuffix(path, "/reclaim") && r.Method == http.MethodPost:
		id := strings.TrimPrefix(path, "/keys/")
		id = strings.TrimSuffix(id, "/reclaim")
		h.salesReclaimCredits(w, r, id)

	// bulk-grant BEFORE the generic /:id routes so "bulk-grant" isn't treated as a key ID.
	case path == "/keys/bulk-grant" && r.Method == http.MethodPost:
		h.salesBulkGrant(w, r)

	case strings.HasPrefix(path, "/keys/") && r.Method == http.MethodGet:
		h.salesGetKey(w, r, strings.TrimPrefix(path, "/keys/"))
	case strings.HasPrefix(path, "/keys/") && r.Method == http.MethodDelete:
		h.salesDeleteKey(w, r, strings.TrimPrefix(path, "/keys/"))

	// --- Reconciliation & ledger ---
	case path == "/reconcile" && r.Method == http.MethodGet:
		h.salesReconcile(w, r)
	case path == "/topups" && r.Method == http.MethodGet:
		h.salesTopups(w, r)

	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

// salesAuthorized checks admin authorization for the sales API.
func (h *Handler) salesAuthorized(r *http.Request) bool {
	ok, _ := h.adminAuthorized(r)
	return ok
}

// --- salesKeyView is the safe view returned by list/get (no plaintext key) ---
type salesKeyView struct {
	ID          string              `json:"id"`
	Name        string              `json:"name,omitempty"`
	KeyMasked   string              `json:"keyMasked"`
	Enabled     bool                `json:"enabled"`
	CreatedAt   int64               `json:"createdAt"`
	LastUsedAt  int64               `json:"lastUsedAt,omitempty"`
	ExpiresAt   int64               `json:"expiresAt,omitempty"`
	CreditLimit float64             `json:"creditLimit"`
	CreditsUsed float64             `json:"creditsUsed"`
	Remaining   float64             `json:"remaining"` // -1 = unlimited
	TokenLimit  int64               `json:"tokenLimit,omitempty"`
	TokensUsed  int64               `json:"tokensUsed,omitempty"`
	RPMLimit    int                 `json:"rpmLimit,omitempty"`
	OwnerRef    string              `json:"ownerRef,omitempty"`
	OrderRef    string              `json:"orderRef,omitempty"`
	Note        string              `json:"note,omitempty"`
	Daily       []config.DailyUsage `json:"daily,omitempty"`
	ByModel     []config.ModelTally `json:"byModel,omitempty"`
}

func toSalesKeyView(e config.ApiKeyEntry) salesKeyView {
	v := salesKeyView{
		ID:          e.ID,
		Name:        e.Name,
		KeyMasked:   config.MaskApiKey(e.Key),
		Enabled:     e.Enabled,
		CreatedAt:   e.CreatedAt,
		LastUsedAt:  e.LastUsedAt,
		ExpiresAt:   e.ExpiresAt,
		CreditLimit: e.CreditLimit,
		CreditsUsed: e.CreditsUsed,
		Remaining:   config.ApiKeyRemaining(e),
		TokenLimit:  e.TokenLimit,
		TokensUsed:  e.TokensUsed,
		RPMLimit:    e.RPMLimit,
		OwnerRef:    e.OwnerRef,
		OrderRef:    e.OrderRef,
		Note:        e.Note,
		Daily:       e.Daily,
		ByModel:     e.ByModel,
	}
	return v
}

// salesOverview returns pool health metrics.
func (h *Handler) salesOverview(w http.ResponseWriter, r *http.Request) {
	keys := config.ListApiKeys()
	var totalCredits, totalLimit float64
	active := 0
	for _, k := range keys {
		if k.Enabled && !config.ApiKeyExpired(k) {
			active++
		}
		totalCredits += k.CreditsUsed
		if k.CreditLimit > 0 {
			totalLimit += k.CreditLimit
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":    h.pool.Count(),
		"enabled":     h.pool.AvailableCount(),
		"available":   h.pool.AvailableCount(),
		"totalKeys":   len(keys),
		"activeKeys":  active,
		"creditsUsed": totalCredits,
		"maintenance": config.GetMaintenanceMode(),
	})
}

// salesListKeys returns all keys without plaintext values.
func (h *Handler) salesListKeys(w http.ResponseWriter, r *http.Request) {
	keys := config.ListApiKeys()
	out := make([]salesKeyView, len(keys))
	for i, k := range keys {
		out[i] = toSalesKeyView(k)
	}
	json.NewEncoder(w).Encode(out)
}

// salesCreateKey mints a new API key and returns its plaintext value ONCE.
func (h *Handler) salesCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string   `json:"name"`
		CreditLimit float64  `json:"creditLimit"`
		ExpiresAt   int64    `json:"expiresAt"`
		OwnerRef    string   `json:"ownerRef"`
		OrderRef    string   `json:"orderRef"`
		Note        string   `json:"note"`
		Models      []string `json:"models"`
		RPMLimit    int      `json:"rpmLimit"`
		IPLimit     int      `json:"ipLimit"`
		TokenLimit  int64    `json:"tokenLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}

	entry := config.ApiKeyEntry{
		Name:        strings.TrimSpace(body.Name),
		Key:         config.GenerateApiKeyValue(),
		Enabled:     true,
		CreditLimit: body.CreditLimit,
		ExpiresAt:   body.ExpiresAt,
		OwnerRef:    strings.TrimSpace(body.OwnerRef),
		OrderRef:    strings.TrimSpace(body.OrderRef),
		Note:        strings.TrimSpace(body.Note),
		Models:      body.Models,
		RPMLimit:    body.RPMLimit,
		IPLimit:     body.IPLimit,
		TokenLimit:  body.TokenLimit,
	}

	created, err := config.AddApiKey(entry)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	config.RecordAudit(config.AuditEntry{
		Action: config.AuditKeyCreate,
		Actor:  config.AuditActorSales,
		Target: created.ID,
		Detail: "name=" + created.Name + " ownerRef=" + created.OwnerRef + " orderRef=" + created.OrderRef,
		IP:     h.resolveClientIP(r),
	})

	// Return the plaintext key ONCE — after this it is only available masked.
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":          created.ID,
		"key":         created.Key, // plaintext, one-time-only
		"keyMasked":   config.MaskApiKey(created.Key),
		"name":        created.Name,
		"creditLimit": created.CreditLimit,
		"expiresAt":   created.ExpiresAt,
		"ownerRef":    created.OwnerRef,
		"orderRef":    created.OrderRef,
		"createdAt":   created.CreatedAt,
		"enabled":     created.Enabled,
	})
}

// salesGetKey returns a single key (no plaintext).
func (h *Handler) salesGetKey(w http.ResponseWriter, r *http.Request, id string) {
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(salesErrorResp("KEY_NOT_FOUND", "api key not found"))
		return
	}
	json.NewEncoder(w).Encode(toSalesKeyView(*entry))
}

// salesDeleteKey removes a key (404-idempotent).
func (h *Handler) salesDeleteKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(salesErrorResp("INTERNAL", err.Error()))
		return
	}
	if h.keyLogHub != nil {
		h.keyLogHub.forget(id)
	}
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditKeyDelete,
		Actor:  config.AuditActorSales,
		Target: id,
		IP:     h.resolveClientIP(r),
	})
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// salesSetKeyEnabled handles PATCH /keys/:id/enabled.
func (h *Handler) salesSetKeyEnabled(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(salesErrorResp("INVALID_JSON", "invalid JSON"))
		return
	}
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(salesErrorResp("KEY_NOT_FOUND", "api key not found"))
		return
	}
	patch := *entry
	patch.Enabled = body.Enabled
	if err := config.UpdateApiKey(id, patch); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(salesErrorResp("INTERNAL", err.Error()))
		return
	}
	action := config.AuditKeyDisable
	if body.Enabled {
		action = config.AuditKeyEnable
	}
	config.RecordAudit(config.AuditEntry{
		Action: action,
		Actor:  config.AuditActorSales,
		Target: id,
		IP:     h.resolveClientIP(r),
	})
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// --- top-up / extend ---

// salesErrorResp is the error envelope every sales endpoint returns. `code` is a
// stable machine-readable token the bot branches on; `error` is human text that
// may be reworded freely.
func salesErrorResp(code, msg string) map[string]interface{} {
	return map[string]interface{}{"ok": false, "code": code, "error": msg}
}

// salesTopUpResponse embeds AddCreditsResult so its fields are inlined next to
// the `ok` flag rather than nested under a "result" object.
type salesTopUpResponse struct {
	OK bool `json:"ok"`
	config.AddCreditsResult
}

// salesTopUpErrorCode maps an AddAPIKeyCredits error to (httpStatus, code).
//
// This table is an external contract: the bot treats a fixed subset of these
// codes as terminal (do not retry) and everything else as retryable. Renaming a
// code without updating the bot's TERMINAL_TOPUP_CODES set turns a permanent
// rejection into an infinite retry loop against a paid endpoint.
func salesTopUpErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, config.ErrIdempotencyRequired):
		return http.StatusBadRequest, "IDEMPOTENCY_REQUIRED"
	case errors.Is(err, config.ErrIdempotencyInvalid):
		return http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY"
	case errors.Is(err, config.ErrIdempotencyConflict):
		return http.StatusConflict, "IDEMPOTENCY_CONFLICT"
	case errors.Is(err, config.ErrInvalidCredits):
		return http.StatusBadRequest, "INVALID_CREDITS"
	case errors.Is(err, config.ErrInvalidDays):
		return http.StatusBadRequest, "INVALID_DAYS"
	case errors.Is(err, config.ErrNothingToApply):
		return http.StatusBadRequest, "INVALID_CREDITS"
	case errors.Is(err, config.ErrKeyNotFound):
		return http.StatusNotFound, "KEY_NOT_FOUND"
	case errors.Is(err, config.ErrKeyUnlimited):
		return http.StatusConflict, "KEY_UNLIMITED"
	// Reclaim-only. Lives in this shared table so the reclaim endpoint inherits the
	// same envelope and the same SAVE_FAILED / INTERNAL fallbacks as the top-up path.
	case errors.Is(err, config.ErrKeyNotExpired):
		return http.StatusConflict, "KEY_NOT_EXPIRED"
	case strings.Contains(err.Error(), "could not persist"):
		// The increment was rolled back in memory but the disk write failed;
		// the caller may safely retry with the SAME idempotency key.
		return http.StatusInternalServerError, "SAVE_FAILED"
	default:
		return http.StatusInternalServerError, "INTERNAL"
	}
}

// writeSalesTopUpError renders a failed top-up with its mapped status + code.
func writeSalesTopUpError(w http.ResponseWriter, err error) {
	status, code := salesTopUpErrorCode(err)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(salesErrorResp(code, err.Error()))
}

// topUpDetail formats an audit line for a top-up / extension.
func topUpDetail(res config.AddCreditsResult, idem string) string {
	var b strings.Builder
	b.WriteString("credits+")
	b.WriteString(strconv.FormatFloat(res.AddedCredits, 'f', -1, 64))
	b.WriteString(" days+")
	b.WriteString(strconv.Itoa(res.AddedDays))
	b.WriteString(" limit=")
	b.WriteString(strconv.FormatFloat(res.CreditLimit, 'f', -1, 64))
	b.WriteString(" idem=")
	b.WriteString(idem)
	if res.IdempotentReplay {
		b.WriteString(" (replay)")
	}
	return b.String()
}

// salesAddCredits handles POST /keys/:id/credits — additive top-up of a key's
// credit limit, optionally extending its expiry at the same time.
func (h *Handler) salesAddCredits(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		AddCredits     float64 `json:"addCredits"`
		AddDays        int     `json:"addDays"`
		IdempotencyKey string  `json:"idempotencyKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(salesErrorResp("INVALID_JSON", "invalid JSON"))
		return
	}

	idem := strings.TrimSpace(body.IdempotencyKey)
	res, err := config.AddAPIKeyCredits(id, body.AddCredits, body.AddDays, idem, config.TopUpSourceSalesAPI)
	if err != nil {
		writeSalesTopUpError(w, err)
		return
	}

	// A replay must not produce a second audit line — the first apply already
	// recorded one, and duplicating it would make the trail overstate revenue.
	if !res.IdempotentReplay {
		config.RecordAudit(config.AuditEntry{
			Action: config.AuditKeyTopUp,
			Actor:  config.AuditActorSales,
			Target: id,
			Detail: topUpDetail(res, idem),
			IP:     h.resolveClientIP(r),
		})
		logger.Infof("[Sales] topped up key %s: +%g credits, +%d days (limit now %g)",
			id, res.AddedCredits, res.AddedDays, res.CreditLimit)
	}

	json.NewEncoder(w).Encode(salesTopUpResponse{OK: true, AddCreditsResult: res})
}

// salesExtendKey handles POST /keys/:id/extend — expiry extension only. It goes
// through the same ledger as a credit top-up so the two operations share one
// idempotency namespace and a mixed-up retry cannot double-apply.
func (h *Handler) salesExtendKey(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		AddDays        int    `json:"addDays"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(salesErrorResp("INVALID_JSON", "invalid JSON"))
		return
	}

	idem := strings.TrimSpace(body.IdempotencyKey)
	res, err := config.AddAPIKeyCredits(id, 0, body.AddDays, idem, config.TopUpSourceSalesAPI)
	if err != nil {
		writeSalesTopUpError(w, err)
		return
	}

	if !res.IdempotentReplay {
		config.RecordAudit(config.AuditEntry{
			Action: config.AuditKeyExtend,
			Actor:  config.AuditActorSales,
			Target: id,
			Detail: topUpDetail(res, idem),
			IP:     h.resolveClientIP(r),
		})
		logger.Infof("[Sales] extended key %s by %d days (expires %d)", id, res.AddedDays, res.ExpiresAt)
	}

	json.NewEncoder(w).Encode(salesTopUpResponse{OK: true, AddCreditsResult: res})
}

// salesReclaimResponse inlines ReclaimResult next to the `ok` flag, matching the
// shape of salesTopUpResponse.
type salesReclaimResponse struct {
	OK bool `json:"ok"`
	config.ReclaimResult
}

// salesReclaimCredits handles POST /keys/:id/reclaim — drop an EXPIRED key's unspent
// credits so the seller can put them back on the shelf, WITHOUT deleting the key.
//
// The key keeps its ID, its plaintext value, and its eligibility for a paid top-up
// that revives it. This is the difference from DELETE /keys/:id, and the reason the
// endpoint exists: the pool only ever returns a key's plaintext once, at creation, so
// deleting is irreversible for the customer while reclaiming is not.
//
// minExpiredSeconds lets the caller demand the key has been expired for a while
// already (the bot sends its 3-hour grace). Absent or 0 means "expired at all is
// enough". The floor is enforced in config.ReclaimAPIKeyCredits, not here, so a
// second caller cannot bypass it by omitting the field.
//
// No idempotency key: this converges on a computed target rather than applying an
// increment, so a repeat call is a no-op that reports alreadyReclaimed=true. A caller
// that lost the response may simply retry.
func (h *Handler) salesReclaimCredits(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		MinExpiredSeconds int64 `json:"minExpiredSeconds"`
	}
	// An empty body is valid — it means "no extra age requirement". Only malformed
	// JSON is rejected.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(salesErrorResp("INVALID_JSON", "invalid JSON"))
			return
		}
	}

	res, err := config.ReclaimAPIKeyCredits(id, body.MinExpiredSeconds)
	if err != nil {
		writeSalesTopUpError(w, err)
		return
	}

	// A no-op reclaim writes no audit line: the trail would otherwise fill with
	// identical entries every time the caller's sweep revisits the same key.
	if !res.AlreadyReclaimed {
		config.RecordAudit(config.AuditEntry{
			Action: config.AuditKeyReclaim,
			Actor:  config.AuditActorSales,
			Target: id,
			Detail: "reclaimed=" + strconv.FormatFloat(res.Reclaimed, 'f', -1, 64) +
				" limit=" + strconv.FormatFloat(res.PreviousCreditLimit, 'f', -1, 64) +
				"→" + strconv.FormatFloat(res.CreditLimit, 'f', -1, 64) +
				" used=" + strconv.FormatFloat(res.CreditsUsed, 'f', -1, 64),
			IP: h.resolveClientIP(r),
		})
		logger.Infof("[Sales] reclaimed %g unspent credits from expired key %s (limit %g→%g)",
			res.Reclaimed, id, res.PreviousCreditLimit, res.CreditLimit)
	}

	json.NewEncoder(w).Encode(salesReclaimResponse{OK: true, ReclaimResult: res})
}

// --- bulk grant ---

// salesBulkGrantConfirmThreshold is how many matched keys a bulk grant may touch
// before the caller must explicitly pass confirm=true. A filter typo that widens
// the match set from three keys to the whole pool is a costly mistake to undo.
const salesBulkGrantConfirmThreshold = 10

type salesBulkGrantResult struct {
	KeyID       string  `json:"keyId"`
	OK          bool    `json:"ok"`
	Replayed    bool    `json:"replayed,omitempty"`
	Code        string  `json:"code,omitempty"`
	Error       string  `json:"error,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
}

// salesBulkGrant handles POST /keys/bulk-grant — apply the same increment to
// every key matching a filter.
//
// Each key gets its own derived idempotency key ("<base>:<keyID>") so a partial
// failure can be retried with the identical request body: keys that already
// succeeded replay, and only the ones that failed are applied.
func (h *Handler) salesBulkGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filter struct {
			OwnerRef           string `json:"ownerRef"`
			ActiveOnly         bool   `json:"activeOnly"`
			ExpiringBeforeDays int    `json:"expiringBeforeDays"`
		} `json:"filter"`
		AddCredits     float64 `json:"addCredits"`
		AddDays        int     `json:"addDays"`
		IdempotencyKey string  `json:"idempotencyKey"`
		Confirm        bool    `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(salesErrorResp("INVALID_JSON", "invalid JSON"))
		return
	}

	base := strings.TrimSpace(body.IdempotencyKey)
	if base == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(salesErrorResp("IDEMPOTENCY_REQUIRED", config.ErrIdempotencyRequired.Error()))
		return
	}
	if body.AddCredits == 0 && body.AddDays == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(salesErrorResp("INVALID_CREDITS", config.ErrNothingToApply.Error()))
		return
	}

	ownerRef := strings.TrimSpace(body.Filter.OwnerRef)
	now := time.Now().Unix()
	var expiryCutoff int64
	if body.Filter.ExpiringBeforeDays > 0 {
		expiryCutoff = now + int64(body.Filter.ExpiringBeforeDays)*86400
	}

	var targets []config.ApiKeyEntry
	for _, k := range config.ListApiKeys() {
		if ownerRef != "" && k.OwnerRef != ownerRef {
			continue
		}
		if body.Filter.ActiveOnly && (!k.Enabled || config.ApiKeyExpired(k)) {
			continue
		}
		if expiryCutoff > 0 {
			// A key that never expires has nothing to renew, so it is not part of
			// an "expiring soon" grant.
			if k.ExpiresAt == 0 || k.ExpiresAt > expiryCutoff {
				continue
			}
		}
		targets = append(targets, k)
	}

	if len(targets) > salesBulkGrantConfirmThreshold && !body.Confirm {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      false,
			"code":    "CONFIRMATION_REQUIRED",
			"error":   "filter matches " + strconv.Itoa(len(targets)) + " keys; resend with confirm=true",
			"matched": len(targets),
		})
		return
	}

	applied, replayed, skipped := 0, 0, 0
	results := make([]salesBulkGrantResult, 0, len(targets))
	for _, k := range targets {
		res, err := config.AddAPIKeyCredits(k.ID, body.AddCredits, body.AddDays, base+":"+k.ID, config.TopUpSourceBulk)
		if err != nil {
			_, code := salesTopUpErrorCode(err)
			skipped++
			results = append(results, salesBulkGrantResult{KeyID: k.ID, Code: code, Error: err.Error()})
			continue
		}
		if res.IdempotentReplay {
			replayed++
		} else {
			applied++
		}
		results = append(results, salesBulkGrantResult{
			KeyID:       k.ID,
			OK:          true,
			Replayed:    res.IdempotentReplay,
			CreditLimit: res.CreditLimit,
			ExpiresAt:   res.ExpiresAt,
		})
	}

	if applied > 0 {
		config.RecordAudit(config.AuditEntry{
			Action: config.AuditKeyBulkGrant,
			Actor:  config.AuditActorSales,
			Detail: "matched=" + strconv.Itoa(len(targets)) +
				" applied=" + strconv.Itoa(applied) +
				" replayed=" + strconv.Itoa(replayed) +
				" skipped=" + strconv.Itoa(skipped) +
				" credits+" + strconv.FormatFloat(body.AddCredits, 'f', -1, 64) +
				" days+" + strconv.Itoa(body.AddDays) +
				" idem=" + base,
			IP: h.resolveClientIP(r),
		})
		logger.Infof("[Sales] bulk grant applied to %d/%d keys (replayed %d, skipped %d)",
			applied, len(targets), replayed, skipped)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       true,
		"matched":  len(targets),
		"applied":  applied,
		"replayed": replayed,
		"skipped":  skipped,
		"results":  results,
	})
}

// --- reconciliation & ledger ---

// salesReconcileRow is the minimal projection the bot diffs its own database
// against. It is deliberately smaller than salesKeyView: a reconcile pass runs
// over every key, so per-key daily/model history would dominate the payload.
type salesReconcileRow struct {
	ID          string  `json:"id"`
	Name        string  `json:"name,omitempty"`
	CreditLimit float64 `json:"creditLimit"`
	CreditsUsed float64 `json:"creditsUsed"`
	Remaining   float64 `json:"remaining"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
	Enabled     bool    `json:"enabled"`
	Expired     bool    `json:"expired"`
	OwnerRef    string  `json:"ownerRef,omitempty"`
	OrderRef    string  `json:"orderRef,omitempty"`
}

// salesReconcile handles GET /reconcile.
func (h *Handler) salesReconcile(w http.ResponseWriter, r *http.Request) {
	keys := config.ListApiKeys()
	rows := make([]salesReconcileRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, salesReconcileRow{
			ID:          k.ID,
			Name:        k.Name,
			CreditLimit: k.CreditLimit,
			CreditsUsed: k.CreditsUsed,
			Remaining:   config.ApiKeyRemaining(k),
			ExpiresAt:   k.ExpiresAt,
			Enabled:     k.Enabled,
			Expired:     config.ApiKeyExpired(k),
			OwnerRef:    k.OwnerRef,
			OrderRef:    k.OrderRef,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"keys":   rows,
		"count":  len(rows),
		"atUnix": time.Now().Unix(),
	})
}

// salesTopupsDefaultLimit / salesTopupsMaxLimit bound the ledger page size so a
// caller cannot ask for the whole 10k-entry history in one response.
const (
	salesTopupsDefaultLimit = 100
	salesTopupsMaxLimit     = 500
)

// salesTopups handles GET /topups?keyId=&limit=.
func (h *Handler) salesTopups(w http.ResponseWriter, r *http.Request) {
	limit := salesTopupsDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > salesTopupsMaxLimit {
		limit = salesTopupsMaxLimit
	}

	topups := config.ListCreditTopUps(strings.TrimSpace(r.URL.Query().Get("keyId")), limit)
	if topups == nil {
		topups = []config.CreditTopUp{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"topups": topups,
		"count":  len(topups),
	})
}

// salesTopupsAdmin exposes the same ledger under GET /admin/api/credit-topups so
// the admin panel does not have to speak the sales API's auth scheme.
func (h *Handler) salesTopupsAdmin(w http.ResponseWriter, r *http.Request) {
	h.salesTopups(w, r)
}
