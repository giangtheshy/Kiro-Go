package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// autobuy_handler.go serves the admin endpoints for the market key buyer plus the
// inbound market webhook.
//
// The webhook is NOT under /admin/api. Everything under that prefix is gated on
// the admin password, and the market server does not have it — it authenticates
// by signing the body with a shared secret. Putting the receiver behind the
// password gate would reject every legitimate delivery with 401.

// autoBuyWebhookPath is the public receiver for market notifications. Its only
// authentication is the HMAC signature, which is why the admin API refuses to
// enable auto-buy until a secret is configured.
const autoBuyWebhookPath = "/autobuy/webhook"

// autoBuyWebhookMaxBody caps the inbound payload. The market's own limit is 1 MiB
// and these events are a few hundred bytes.
const autoBuyWebhookMaxBody = 1 << 20

// autoBuyWebhookSkew is the tolerance for the signed timestamp. The market docs
// recommend rejecting anything more than five minutes out, which bounds how long
// a captured request stays replayable.
const autoBuyWebhookSkew = 5 * time.Minute

// Market webhook event names.
const (
	marketEventNewKeys        = "new_keys_available"
	marketEventReservedKeys   = "reserved_keys_delivered"
	marketEventAllKeysDead    = "all_keys_dead"
	marketEventWarrantyRefund = "warranty_refund"
	marketEventTest           = "webhook_test"
)

// autoBuyConfigView is the wire shape for the admin panel. Secrets are reported
// as booleans rather than values, so the panel can show "configured" without ever
// holding the credential.
type autoBuyConfigView struct {
	*config.AutoBuyConfig
	MarketApiKey     string `json:"marketApiKey"`
	WebhookSecret    string `json:"webhookSecret"`
	HasMarketApiKey  bool   `json:"hasMarketApiKey"`
	HasWebhookSecret bool   `json:"hasWebhookSecret"`
	// TelegramBotToken is masked like the two secrets above. The chat id is not a
	// secret and is returned in full, so the panel can show which chat is targeted.
	TelegramBotToken    string `json:"telegramBotToken"`
	HasTelegramBotToken bool   `json:"hasTelegramBotToken"`
	WebhookPath         string `json:"webhookPath"`
	// BuyLog and SeenEvents are served by their own endpoint; repeating them in
	// every config read would send the whole history on each poll.
	BuyLog     []config.AutoBuyLogEntry `json:"buyLog,omitempty"`
	SeenEvents map[string]int64         `json:"seenEvents,omitempty"`
}

func toAutoBuyConfigView(c *config.AutoBuyConfig) autoBuyConfigView {
	v := autoBuyConfigView{
		AutoBuyConfig:       c,
		HasMarketApiKey:     strings.TrimSpace(c.MarketApiKey) != "",
		HasWebhookSecret:    strings.TrimSpace(c.WebhookSecret) != "",
		HasTelegramBotToken: strings.TrimSpace(c.TelegramBotToken) != "",
		WebhookPath:         autoBuyWebhookPath,
	}
	// Blank the embedded copies so the raw values never reach the browser.
	v.AutoBuyConfig.MarketApiKey = ""
	v.AutoBuyConfig.WebhookSecret = ""
	v.AutoBuyConfig.TelegramBotToken = ""
	v.AutoBuyConfig.BuyLog = nil
	v.AutoBuyConfig.SeenEvents = nil
	return v
}

func writeAutoBuyError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// apiGetAutoBuyConfig handles GET /admin/api/autobuy/config.
func (h *Handler) apiGetAutoBuyConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetAutoBuyConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{"config": toAutoBuyConfigView(cfg)})
}

// apiSetAutoBuyConfig handles PUT /admin/api/autobuy/config.
func (h *Handler) apiSetAutoBuyConfig(w http.ResponseWriter, r *http.Request) {
	var body config.AutoBuyConfig
	if err := json.NewDecoder(io.LimitReader(r.Body, autoBuyWebhookMaxBody)).Decode(&body); err != nil {
		writeAutoBuyError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if err := config.SetAutoBuyConfig(&body); err != nil {
		writeAutoBuyError(w, http.StatusBadRequest, err.Error())
		return
	}

	saved := config.GetAutoBuyConfig()
	config.RecordAudit(config.AuditEntry{
		Action: config.AuditAutoBuySettings,
		Actor:  config.AuditActorAdmin,
		Detail: describeAutoBuySettings(saved),
		IP:     h.resolveClientIP(r),
	})

	logger.Infof("[AutoBuy] settings updated: %s", describeAutoBuySettings(saved))
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "config": toAutoBuyConfigView(saved)})
}

// describeAutoBuySettings renders an audit-safe summary. It must never include the
// market key or the webhook secret.
func describeAutoBuySettings(c *config.AutoBuyConfig) string {
	var b strings.Builder
	b.WriteString("enabled=")
	b.WriteString(strconv.FormatBool(c.Enabled))
	if c.DryRun {
		b.WriteString(" dryRun=true")
	}
	for _, zone := range []string{config.AutoBuyZoneUS, config.AutoBuyZoneEU} {
		z := c.Zone(zone)
		if z == nil || !z.Enabled {
			continue
		}
		b.WriteString(" " + zone + "=[")
		b.WriteString("count=" + strconv.Itoa(z.EffectiveBuyCount()))
		if z.MaxUnitPrice > 0 {
			b.WriteString(" maxPrice=" + strconv.Itoa(z.MaxUnitPrice))
		}
		if z.MaxKeysPerDay > 0 {
			b.WriteString(" maxKeys/day=" + strconv.Itoa(z.MaxKeysPerDay))
		}
		b.WriteString("]")
	}
	if c.MaxCreditsPerDay > 0 {
		b.WriteString(" maxCredits/day=" + strconv.Itoa(c.MaxCreditsPerDay))
	}
	if c.MinHealthyAccounts > 0 {
		b.WriteString(" minHealthy=" + strconv.Itoa(c.MinHealthyAccounts))
	}
	if c.ScheduleEnabled {
		b.WriteString(" window=" + c.WindowStart + "-" + c.WindowEnd)
	}
	return b.String()
}

// apiGetAutoBuyStatus handles GET /admin/api/autobuy/status.
//
// Local state is always reported; the live market figures are best-effort so the
// panel still renders when the market API is unreachable or the key is unset.
func (h *Handler) apiGetAutoBuyStatus(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetAutoBuyConfig()
	now := time.Now()

	status := map[string]interface{}{
		"enabled":         cfg.Enabled,
		"dryRun":          cfg.DryRun,
		"withinWindow":    cfg.WindowAllows(now),
		"healthyAccounts": h.pool.HealthyCount(),
		"totalAccounts":   h.pool.Count(),
		"spentToday":      cfg.SpentToday,
		"dayStamp":        cfg.DayStamp,
		"pollIntervalSec": int(cfg.EffectivePollInterval().Seconds()),
		"webhookPath":     autoBuyWebhookPath,
	}
	if left, capped := cfg.RemainingCreditsToday(); capped {
		status["remainingCreditsToday"] = left
		status["maxCreditsPerDay"] = cfg.MaxCreditsPerDay
	}

	zones := map[string]interface{}{}
	for _, zone := range []string{config.AutoBuyZoneUS, config.AutoBuyZoneEU} {
		if z := cfg.Zone(zone); z != nil {
			zones[zone] = map[string]interface{}{
				"enabled":       z.Enabled,
				"boughtToday":   z.BoughtToday,
				"maxKeysPerDay": z.MaxKeysPerDay,
				"maxUnitPrice":  z.MaxUnitPrice,
				"buyCount":      z.EffectiveBuyCount(),
			}
		}
	}
	status["zones"] = zones

	if m, err := newMarketClient(cfg); err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()

		if profile, err := m.Profile(ctx); err == nil {
			status["market"] = map[string]interface{}{
				"username":         profile.Username,
				"balance":          profile.Balance,
				"keysHeld":         profile.KeysHeld,
				"holdCapEffective": profile.HoldCapEffective,
			}
		} else {
			status["marketError"] = err.Error()
		}

		if stock, err := m.Stock(ctx); err == nil {
			status["stock"] = map[string]interface{}{
				"publicAvailable": stock.Stock.PublicAvailable,
				"max":             stock.Max,
				"warrantyMinutes": stock.WarrantyMinutes,
				"zones":           stock.Zones,
			}
		} else if _, exists := status["marketError"]; !exists {
			status["marketError"] = err.Error()
		}
	} else {
		status["marketError"] = err.Error()
	}

	json.NewEncoder(w).Encode(status)
}

// apiGetAutoBuyLogs handles GET /admin/api/autobuy/logs.
func (h *Handler) apiGetAutoBuyLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries := config.GetAutoBuyLog(limit)
	json.NewEncoder(w).Encode(map[string]interface{}{"logs": entries, "total": len(entries)})
}

// apiAutoBuyManual handles POST /admin/api/autobuy/buy.
//
// The manual path deliberately runs the same guards as the unattended one, so the
// button verifies the live policy instead of bypassing it. dryRun in the body
// forces a dry run for this call only; it cannot turn a configured dry run off,
// because that would let one click spend real money against a config the operator
// believes is still in test mode.
func (h *Handler) apiAutoBuyManual(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Zone   string `json:"zone"`
		DryRun bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && err != io.EOF {
		writeAutoBuyError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	zone := strings.ToLower(strings.TrimSpace(body.Zone))
	if zone == "" {
		zone = config.AutoBuyZoneUS
	}
	if !config.IsValidZone(zone) {
		writeAutoBuyError(w, http.StatusBadRequest, "zone must be \"us\" or \"eu\"")
		return
	}

	cfg := config.GetAutoBuyConfig()
	if strings.TrimSpace(cfg.MarketApiKey) == "" {
		writeAutoBuyError(w, http.StatusBadRequest, "marketApiKey is not configured")
		return
	}
	// A manual buy works even when the master switch is off: an operator pressing
	// the button is present and deciding, which is the thing Enabled gates.
	cfg.Enabled = true
	if body.DryRun {
		cfg.DryRun = true
	}

	entry, skipped, err := h.attemptAutoBuy(cfg, zone, "", autoBuyTriggerManual)
	if err != nil {
		writeAutoBuyError(w, http.StatusBadGateway, err.Error())
		return
	}
	if skipped != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      false,
			"skipped": true,
			"reason":  skipped.Reason,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "entry": entry})
}

// apiAutoBuyNotifyTest handles POST /admin/api/autobuy/notify-test.
//
// It sends a real message through every channel the request describes and reports
// each one's outcome separately. Testing before saving is the useful case — an
// operator pasting a fresh token wants to know it works before committing it — so
// credentials supplied in the body take precedence over the stored ones, and a
// blank field falls back to what is stored.
func (h *Handler) apiAutoBuyNotifyTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TelegramBotToken string `json:"telegramBotToken"`
		TelegramChatID   string `json:"telegramChatId"`
		TelegramApiBase  string `json:"telegramApiBase"`
		NotifyWebhook    string `json:"notifyWebhook"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && err != io.EOF {
		writeAutoBuyError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	stored := config.GetAutoBuyConfig()

	token := strings.TrimSpace(body.TelegramBotToken)
	if token == "" {
		token = strings.TrimSpace(stored.TelegramBotToken)
	}
	chatID := strings.TrimSpace(body.TelegramChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(stored.TelegramChatID)
	}
	apiBase := strings.TrimSpace(body.TelegramApiBase)
	if apiBase == "" {
		apiBase = stored.EffectiveTelegramApiBase()
	}
	webhook := strings.TrimSpace(body.NotifyWebhook)
	if webhook == "" {
		webhook = strings.TrimSpace(stored.NotifyWebhook)
	}

	n := notice{
		Kind:  noticeKindTest,
		Title: "✅ Kiro-Go notification test",
		Lines: []string{
			"If you can read this, alerts will reach you here.",
			"Sent from the admin panel at " + time.Now().Format("2006-01-02 15:04:05 -0700") + ".",
		},
		Fields: map[string]any{"test": true},
	}

	results := map[string]any{}
	anyChannel := false

	if webhook != "" {
		anyChannel = true
		// Synchronous, unlike the fire-and-forget production path: the whole point
		// of a test is to report the result back to the caller.
		if err := postNotifyWebhook(webhook, n.webhookPayload()); err != nil {
			results["webhook"] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			results["webhook"] = map[string]any{"ok": true}
		}
	}

	if token != "" && chatID != "" {
		anyChannel = true
		if err := sendTelegram(apiBase, token, chatID, n.telegramText()); err != nil {
			// Telegram's own description is passed through: "chat not found" and
			// "Unauthorized" need different fixes, and a generic failure message
			// would leave the operator guessing which one they are looking at.
			results["telegram"] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			results["telegram"] = map[string]any{"ok": true}
		}
	} else if token != "" || chatID != "" {
		anyChannel = true
		results["telegram"] = map[string]any{
			"ok":    false,
			"error": "both the bot token and the chat id are required",
		}
	}

	if !anyChannel {
		writeAutoBuyError(w, http.StatusBadRequest, "no notification channel is configured")
		return
	}

	allOK := true
	for _, v := range results {
		if m, ok := v.(map[string]any); ok {
			if ok2, _ := m["ok"].(bool); !ok2 {
				allOK = false
			}
		}
	}

	logger.Infof("[Notify] test dispatched from the admin panel (allOK=%v)", allOK)
	json.NewEncoder(w).Encode(map[string]any{"ok": allOK, "results": results})
}

// verifyMarketSignature checks the HMAC over the raw body.
//
// The signed content is timestamp + "." + body, and verification must use the
// bytes as received: parsing and re-serialising JSON reorders keys and rewrites
// whitespace, which changes the digest and fails every time.
func verifyMarketSignature(secret, timestamp, signature string, body []byte) bool {
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	// Constant-time compare: a byte-by-byte comparison leaks how much of a
	// forged signature was correct, which is enough to construct one.
	return hmac.Equal([]byte(want), []byte(strings.TrimSpace(signature)))
}

// timestampWithinSkew reports whether the signed timestamp is recent enough.
func timestampWithinSkew(raw string, now time.Time, skew time.Duration) bool {
	secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return false
	}
	delta := now.Sub(time.Unix(secs, 0))
	if delta < 0 {
		delta = -delta
	}
	return delta <= skew
}

// marketWebhookPayload is the subset of the event body that drives a purchase.
type marketWebhookPayload struct {
	Event   string `json:"event"`
	EventID string `json:"event_id"`
	Zone    string `json:"zone"`
	NewKeys int    `json:"new_keys"`
	// PurchaseOrderID is an idempotency key, NOT an order number. It is passed
	// straight through as client_order_id; calling the order-fetch API with it
	// returns 404 because no order exists yet.
	PurchaseOrderID string `json:"purchase_order_id"`
	// OrderID appears on reserved_keys_delivered, where the keys are already
	// bought and paid for and must be fetched rather than purchased again.
	OrderID   string `json:"order_id"`
	PoolID    string `json:"pool_id"`
	RoundID   string `json:"round_id"`
	Timestamp int64  `json:"timestamp"`
}

// handleAutoBuyWebhook serves POST /autobuy/webhook.
//
// It answers 200 for anything it accepts, including events it takes no action on:
// a non-2xx triggers up to three redeliveries, and asking the market to retry an
// event that was understood and deliberately ignored is noise. Rejections are
// limited to requests that fail authentication.
func (h *Handler) handleAutoBuyWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
		return
	}

	cfg := config.GetAutoBuyConfig()
	secret := strings.TrimSpace(cfg.WebhookSecret)
	if secret == "" {
		// Unauthenticated acceptance would let anyone who guesses the path spend
		// the operator's credits, so refuse rather than trust the caller.
		logger.Warnf("[AutoBuy] webhook rejected: no webhookSecret configured")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "webhook secret is not configured"})
		return
	}

	// Read the raw bytes before any decoding: the signature covers exactly these.
	body, err := io.ReadAll(io.LimitReader(r.Body, autoBuyWebhookMaxBody))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "could not read body"})
		return
	}

	timestamp := r.Header.Get("X-KM-Timestamp")
	signature := r.Header.Get("X-KM-Signature")

	if !timestampWithinSkew(timestamp, time.Now(), autoBuyWebhookSkew) {
		logger.Warnf("[AutoBuy] webhook rejected: timestamp %q outside the %s tolerance", timestamp, autoBuyWebhookSkew)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "stale or invalid timestamp"})
		return
	}
	if !verifyMarketSignature(secret, timestamp, signature, body) {
		logger.Warnf("[AutoBuy] webhook rejected: signature mismatch (ip=%s)", h.resolveClientIP(r))
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "signature mismatch"})
		return
	}

	var payload marketWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// Signed but unparseable: accept so it is not redelivered, and log it.
		logger.Warnf("[AutoBuy] webhook body did not parse: %v", err)
		json.NewEncoder(w).Encode(map[string]string{"status": "ignored", "reason": "unparseable body"})
		return
	}

	event := strings.TrimSpace(payload.Event)
	if event == "" {
		event = strings.TrimSpace(r.Header.Get("X-KM-Event"))
	}

	// Deduplicate before acting. The market retries a delivery up to three times
	// with the same event_id, and the idempotency key only protects the purchase
	// call itself — this also saves the wasted round trips.
	eventID := strings.TrimSpace(payload.EventID)
	if eventID == "" {
		eventID = strings.TrimSpace(r.Header.Get("X-KM-Event-Id"))
	}
	if event != marketEventTest {
		fresh, err := config.MarkAutoBuyEventSeen(eventID)
		if err != nil {
			logger.Warnf("[AutoBuy] could not record event id %s: %v", eventID, err)
		} else if !fresh {
			logger.Infof("[AutoBuy] webhook event %s already handled; ignoring redelivery", eventID)
			json.NewEncoder(w).Encode(map[string]string{"status": "duplicate"})
			return
		}
	}

	switch event {
	case marketEventNewKeys:
		h.handleNewKeysEvent(cfg, payload)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})

	case marketEventReservedKeys:
		// Already bought and charged at an agreed price. Purchasing again would
		// buy a second batch at the public price, so this path only fetches.
		h.handleReservedKeysEvent(cfg, payload)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})

	case marketEventAllKeysDead:
		logger.Warnf("[AutoBuy] market reports every key dead for round %s", payload.RoundID)
		json.NewEncoder(w).Encode(map[string]string{"status": "noted"})

	case marketEventWarrantyRefund:
		logger.Infof("[AutoBuy] market refunded a round under warranty (round=%s)", payload.RoundID)
		json.NewEncoder(w).Encode(map[string]string{"status": "noted"})

	case marketEventTest:
		logger.Infof("[AutoBuy] webhook test received and verified")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		logger.Infof("[AutoBuy] ignoring unhandled webhook event %q", event)
		json.NewEncoder(w).Encode(map[string]string{"status": "ignored"})
	}
}

// handleNewKeysEvent buys against a restock notification.
//
// It runs in the background so the market gets its 200 immediately: a purchase
// plus its account imports can take longer than the delivery timeout, and a
// timeout would be recorded as a failure and redelivered.
func (h *Handler) handleNewKeysEvent(cfg *config.AutoBuyConfig, payload marketWebhookPayload) {
	zone := strings.ToLower(strings.TrimSpace(payload.Zone))
	if zone == "" {
		// The market defaults an unspecified zone to us, and never silently
		// substitutes eu, so matching that default is the safe reading.
		zone = config.AutoBuyZoneUS
	}

	// purchase_order_id is pre-generated by the market for this batch and is what
	// makes a redelivered notification idempotent end to end.
	orderID := strings.TrimSpace(payload.PurchaseOrderID)
	if orderID != "" && !isValidClientOrderID(orderID) {
		logger.Warnf("[AutoBuy] webhook supplied a malformed purchase_order_id %q; generating a fresh one", orderID)
		orderID = ""
	}

	logger.Infof("[AutoBuy] restock notification: %d key(s) in zone %s", payload.NewKeys, zone)
	safeGo(func() {
		if _, _, err := h.attemptAutoBuy(cfg, zone, orderID, autoBuyTriggerWebhook); err != nil {
			logger.Warnf("[AutoBuy] webhook-triggered purchase failed: %v", err)
		}
	})
}

// handleReservedKeysEvent imports keys from a reserved delivery.
//
// Money has already been deducted here, so there is nothing to decide and no
// guard to apply: the keys belong to this account whether or not they are
// imported, and skipping the fetch would strand a batch that was paid for. The
// fetch endpoint is the only way to obtain the full key text — the key list API
// returns prefixes only.
func (h *Handler) handleReservedKeysEvent(cfg *config.AutoBuyConfig, payload marketWebhookPayload) {
	orderID := strings.TrimSpace(payload.OrderID)
	if orderID == "" {
		logger.Warnf("[AutoBuy] reserved-delivery event carried no order_id; the keys cannot be fetched")
		return
	}
	logger.Infof("[AutoBuy] reserved delivery of %d key(s) in zone %s (order=%s)", payload.NewKeys, payload.Zone, orderID)

	safeGo(func() {
		m, err := newMarketClient(cfg)
		if err != nil {
			logger.Errorf("[AutoBuy] cannot fetch reserved order %s: %v", orderID, err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), autoBuyAPITimeout)
		defer cancel()

		result, err := m.OrderKeys(ctx, orderID)
		if err != nil {
			logger.Errorf("[AutoBuy] could not fetch reserved order %s: %v — the keys are paid for and must be collected manually", orderID, err)
			return
		}

		imported, skipped := h.importMarketKeys(cfg, result)
		entry := config.AutoBuyLogEntry{
			Zone:      strings.ToLower(strings.TrimSpace(payload.Zone)),
			Trigger:   autoBuyTriggerWebhook,
			Purchased: len(result.Keys),
			Credits:   result.TotalCredits,
			UnitPrice: result.UnitPrice,
			OrderID:   orderID,
			Imported:  imported,
			Skipped:   skipped,
		}
		if err := config.RecordAutoBuyPurchase(entry); err != nil {
			logger.Warnf("[AutoBuy] could not record reserved delivery: %v", err)
		}
		logger.Infof("[AutoBuy] reserved order %s: imported %d, skipped %d", orderID, imported, skipped)
	})
}
