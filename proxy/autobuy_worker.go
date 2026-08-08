package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// autobuy_worker.go decides whether to buy Kiro keys and performs the purchase.
//
// The whole point of the feature is that it runs while nobody is watching, so the
// guards below are ordered cheapest-first and every one of them can veto: local
// checks run before any network call, and the market API is only touched once the
// local policy already says yes. A guard that reads config is free; one that
// spends money is not.

// Trigger labels recorded in the purchase log so an operator can tell an
// event-driven buy from a scheduled sweep or a button press.
const (
	autoBuyTriggerWebhook = "webhook"
	autoBuyTriggerPoll    = "poll"
	autoBuyTriggerManual  = "manual"
)

// autoBuyStartupDelay staggers the first check slightly after boot so a restart
// storm does not have every instance calling the market API at once, while still
// checking far sooner than the poll interval — a process that came up at 3am
// should not stay blind until the first tick.
const autoBuyStartupDelay = 20 * time.Second

// autoBuyRetryAttempts bounds retries for retry_same_order, which the market docs
// say to retry using the same idempotency key.
const autoBuyRetryAttempts = 3

// autoBuyRetryDelay spaces those retries. Short: the contention it recovers from
// is other buyers racing for the same stock, which resolves in moments.
const autoBuyRetryDelay = 2 * time.Second

// autoBuyAPITimeout bounds a single market call.
const autoBuyAPITimeout = 30 * time.Second

// autoBuySkip explains why no purchase was attempted. Only reported through logs
// and the status endpoint; a skip is normal operation, not an error.
type autoBuySkip struct {
	Reason string
	// Quiet marks routine skips (out of window, pool already healthy) that would
	// otherwise fill the log with one line per tick forever.
	Quiet bool
}

func skip(format string, args ...any) *autoBuySkip {
	return &autoBuySkip{Reason: fmt.Sprintf(format, args...)}
}

func quietSkip(format string, args ...any) *autoBuySkip {
	return &autoBuySkip{Reason: fmt.Sprintf(format, args...), Quiet: true}
}

// backgroundAutoBuy is the polling fallback for the market webhook.
//
// The webhook is the primary path; this loop exists because the docs acknowledge
// deliveries can fail, and an unattended pool that only refills when a
// notification arrives would stay empty for as long as delivery is broken.
func (h *Handler) backgroundAutoBuy() {
	// Check shortly after boot rather than after a full interval.
	select {
	case <-time.After(autoBuyStartupDelay):
	case <-h.stopAutoBuy:
		return
	}
	h.runAutoBuySweep()

	// The interval is re-read each tick so an operator changing it does not have
	// to restart the process for it to take effect.
	for {
		interval := config.GetAutoBuyConfig().EffectivePollInterval()
		select {
		case <-time.After(interval):
			h.runAutoBuySweep()
		case <-h.stopAutoBuy:
			return
		}
	}
}

// runAutoBuySweep evaluates every zone once.
func (h *Handler) runAutoBuySweep() {
	cfg := config.GetAutoBuyConfig()
	if !cfg.Enabled {
		return
	}
	for _, zone := range []string{config.AutoBuyZoneUS, config.AutoBuyZoneEU} {
		if rule := cfg.Zone(zone); rule == nil || !rule.Enabled {
			continue
		}
		h.attemptAutoBuy(cfg, zone, "", autoBuyTriggerPoll)
	}
}

// localGuards runs every check that needs no network call.
//
// Returning early here is the difference between a quiet night and a night spent
// making requests that were always going to be refused.
func (h *Handler) localGuards(cfg *config.AutoBuyConfig, zone, trigger string, now time.Time) *autoBuySkip {
	if cfg == nil || !cfg.Enabled {
		return quietSkip("auto-buy is disabled")
	}

	// The schedule gates the webhook too. If it did not, the window would be
	// decorative: webhooks are the main buying path, so an out-of-hours restock
	// would spend money during the hours the operator explicitly excluded.
	if !cfg.WindowAllows(now) {
		return quietSkip("outside the configured buying window (%s–%s local)", cfg.WindowStart, cfg.WindowEnd)
	}

	if left, capped := cfg.RemainingCreditsToday(); capped && left <= 0 {
		return skip("daily credit ceiling reached (%d/%d spent today)", cfg.SpentToday, cfg.MaxCreditsPerDay)
	}

	rule := cfg.Zone(zone)
	if rule == nil {
		return skip("zone %s has no configured rule", zone)
	}
	if !rule.Enabled {
		return quietSkip("zone %s is disabled", zone)
	}
	if rule.MaxKeysPerDay > 0 && rule.BoughtToday >= rule.MaxKeysPerDay {
		return skip("zone %s hit its daily key limit (%d/%d)", zone, rule.BoughtToday, rule.MaxKeysPerDay)
	}

	// The need check. HealthyCount excludes quota-exhausted and cooled-down
	// accounts, which is why it and not AvailableCount is the trigger: a pool of
	// twenty accounts that have all burned their quota needs topping up exactly
	// as much as an empty one.
	if cfg.MinHealthyAccounts > 0 {
		healthy := h.pool.HealthyCount()
		if healthy >= cfg.MinHealthyAccounts {
			return quietSkip("pool still has %d usable accounts (threshold %d)", healthy, cfg.MinHealthyAccounts)
		}
	}

	if cfg.MaxPoolAccounts > 0 {
		total := h.pool.Count()
		if total >= cfg.MaxPoolAccounts {
			return skip("pool already holds %d accounts (max %d)", total, cfg.MaxPoolAccounts)
		}
	}
	return nil
}

// autoBuyPlan is a purchase that passed every guard.
type autoBuyPlan struct {
	Zone      string
	Count     int
	UnitPrice int
	OrderID   string
	Balance   int
}

// planPurchase applies the guards that need live market data and works out how
// many keys to ask for.
func (h *Handler) planPurchase(ctx context.Context, m *marketClient, cfg *config.AutoBuyConfig,
	zone, clientOrderID string) (*autoBuyPlan, *autoBuySkip, error) {

	profile, err := m.Profile(ctx)
	if err != nil {
		return nil, nil, err
	}
	if cfg.MinBalance > 0 && profile.Balance < cfg.MinBalance {
		return nil, skip("market balance %d is below the configured floor %d", profile.Balance, cfg.MinBalance), nil
	}

	stock, err := m.Stock(ctx)
	if err != nil {
		return nil, nil, err
	}
	zs := stock.ZoneStock(zone)
	if zs == nil {
		return nil, quietSkip("zone %s is not listed in market stock", zone), nil
	}
	if zs.Available <= 0 || stock.Max <= 0 {
		return nil, quietSkip("no stock in zone %s", zone), nil
	}

	rule := cfg.Zone(zone)
	// The per-zone ceiling is the reason zones carry their own price limit: the
	// two are priced independently, so one shared number would either lock out
	// the cheap zone or wave through the expensive one.
	if rule.MaxUnitPrice > 0 && zs.UnitPrice > rule.MaxUnitPrice {
		return nil, quietSkip("zone %s unit price %d exceeds the ceiling %d", zone, zs.UnitPrice, rule.MaxUnitPrice), nil
	}

	count := rule.EffectiveBuyCount()
	if count > zs.Available {
		count = zs.Available
	}
	if stock.Max > 0 && count > stock.Max {
		count = stock.Max
	}
	if stock.MaxPerOrder > 0 && count > stock.MaxPerOrder {
		count = stock.MaxPerOrder
	}

	// Trim to what the daily ceiling still affords rather than abandoning the
	// buy: with a 100-credit remainder and a 30-credit price, buying three is
	// better than buying none.
	if left, capped := cfg.RemainingCreditsToday(); capped && zs.UnitPrice > 0 {
		affordable := left / zs.UnitPrice
		if affordable <= 0 {
			return nil, skip("daily credit ceiling leaves %d, below the unit price %d", left, zs.UnitPrice), nil
		}
		if count > affordable {
			count = affordable
		}
	}

	if rule.MaxKeysPerDay > 0 {
		room := rule.MaxKeysPerDay - rule.BoughtToday
		if room <= 0 {
			return nil, skip("zone %s hit its daily key limit", zone), nil
		}
		if count > room {
			count = room
		}
	}

	// Respect the market-side holding cap locally. Ordering past it would fail
	// with purchase_cap_reached, which the docs say is not worth retrying.
	if room, capped := profile.HoldRoom(); capped {
		if room <= 0 {
			return nil, skip("market holding cap reached (%d/%d keys held)", profile.KeysHeld, profile.HoldCapEffective), nil
		}
		if count > room {
			count = room
		}
	}

	if count < config.AutoBuyMinCount {
		return nil, quietSkip("nothing left to buy in zone %s after applying limits", zone), nil
	}

	orderID := clientOrderID
	if orderID == "" {
		generated, err := newClientOrderID()
		if err != nil {
			return nil, nil, err
		}
		orderID = generated
	}
	if !isValidClientOrderID(orderID) {
		return nil, nil, fmt.Errorf("autobuy: client order id %q is not 32 hex characters", orderID)
	}

	return &autoBuyPlan{
		Zone:      zone,
		Count:     count,
		UnitPrice: zs.UnitPrice,
		OrderID:   orderID,
		Balance:   profile.Balance,
	}, nil, nil
}

// attemptAutoBuy runs the full decision and, if it passes, buys and imports.
//
// clientOrderID comes from a webhook's purchase_order_id when present. Reusing it
// is what makes a redelivered notification safe: the market returns the original
// result instead of charging twice.
func (h *Handler) attemptAutoBuy(cfg *config.AutoBuyConfig, zone, clientOrderID, trigger string) (*config.AutoBuyLogEntry, *autoBuySkip, error) {
	now := time.Now()
	zone = strings.ToLower(strings.TrimSpace(zone))

	if s := h.localGuards(cfg, zone, trigger, now); s != nil {
		logAutoBuySkip(trigger, zone, s)
		return nil, s, nil
	}

	m, err := newMarketClient(cfg)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), autoBuyAPITimeout)
	defer cancel()

	plan, s, err := h.planPurchase(ctx, m, cfg, zone, clientOrderID)
	if err != nil {
		h.recordAutoBuyFailure(trigger, zone, err)
		return nil, nil, err
	}
	if s != nil {
		logAutoBuySkip(trigger, zone, s)
		return nil, s, nil
	}

	// A dry run stops here, after every guard has run and priced the order, so
	// the log line shows exactly what would have been bought without spending.
	if cfg.DryRun {
		entry := config.AutoBuyLogEntry{
			Zone:      plan.Zone,
			Trigger:   trigger,
			Requested: plan.Count,
			UnitPrice: plan.UnitPrice,
			OrderID:   plan.OrderID,
			DryRun:    true,
		}
		logger.Infof("[AutoBuy] DRY RUN: would buy %d key(s) in zone %s at %d credits each (trigger=%s)",
			plan.Count, plan.Zone, plan.UnitPrice, trigger)
		if err := config.RecordAutoBuyPurchase(entry); err != nil {
			logger.Warnf("[AutoBuy] could not record dry-run entry: %v", err)
		}
		return &entry, nil, nil
	}

	result, err := h.purchaseWithRetry(ctx, m, plan)
	if err != nil {
		h.recordAutoBuyFailure(trigger, zone, err)
		return nil, nil, err
	}

	entry := config.AutoBuyLogEntry{
		Zone:      plan.Zone,
		Trigger:   trigger,
		Requested: plan.Count,
		Purchased: result.Purchased,
		UnitPrice: result.UnitPrice,
		Credits:   result.TotalCredits,
		OrderID:   result.OrderID,
	}

	// Purchased can legitimately be below Requested: stock is contended and a
	// partial fill is documented as normal. Charge and count what arrived.
	if result.Purchased == 0 {
		entry.Error = "order returned no keys"
		if err := config.RecordAutoBuyPurchase(entry); err != nil {
			logger.Warnf("[AutoBuy] could not record empty order: %v", err)
		}
		logger.Warnf("[AutoBuy] order %s in zone %s returned no keys", result.OrderID, plan.Zone)
		return &entry, nil, nil
	}

	imported, skipped := h.importMarketKeys(cfg, result)
	entry.Imported = imported
	entry.Skipped = skipped

	if err := config.RecordAutoBuyPurchase(entry); err != nil {
		// The keys are already bought and imported at this point, so a failed
		// write is worth shouting about but must not undo the purchase.
		logger.Errorf("[AutoBuy] bought %d key(s) but could not persist the log entry: %v", result.Purchased, err)
	}

	logger.Infof("[AutoBuy] bought %d key(s) in zone %s for %d credits (order=%s, imported=%d, skipped=%d, trigger=%s)",
		result.Purchased, plan.Zone, result.TotalCredits, result.OrderID, imported, skipped, trigger)

	h.notifyAutoBuy(cfg, autoBuyNotice{
		Event:     "purchase",
		Zone:      plan.Zone,
		Trigger:   trigger,
		Purchased: result.Purchased,
		Credits:   result.TotalCredits,
		UnitPrice: result.UnitPrice,
		OrderID:   result.OrderID,
		Imported:  imported,
		Skipped:   skipped,
		Balance:   result.Remaining,
	})
	return &entry, nil, nil
}

// purchaseWithRetry performs the order, retrying only where the upstream says a
// retry can help.
func (h *Handler) purchaseWithRetry(ctx context.Context, m *marketClient, plan *autoBuyPlan) (*marketPurchase, error) {
	var lastErr error
	for attempt := 1; attempt <= autoBuyRetryAttempts; attempt++ {
		// The same order id on every attempt is mandatory, not an optimisation:
		// it is what stops a retry from becoming a second charge.
		result, err := m.Purchase(ctx, plan.Zone, plan.Count, plan.OrderID)
		if err == nil {
			return result, nil
		}
		lastErr = err

		me, ok := asMarketError(err)
		if !ok || !me.Retryable() {
			return nil, err
		}

		// no_stock means the shelf emptied between the stock read and the order.
		// Retrying immediately will not refill it; wait for the next trigger.
		if me.Code == marketCodeNoStock {
			return nil, err
		}

		delay := autoBuyRetryDelay
		if me.RetryAfter > 0 {
			// Honour the server's own interval on a 429 instead of guessing.
			delay = me.RetryAfter
		}
		logger.Warnf("[AutoBuy] purchase attempt %d/%d failed (%s); retrying in %s with the same order id",
			attempt, autoBuyRetryAttempts, err, delay)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.stopAutoBuy:
			return nil, fmt.Errorf("autobuy: shutting down")
		}
	}
	return nil, lastErr
}

// importMarketKeys turns delivered keys into pool accounts.
//
// Region comes from each key rather than from config: keys are bound to their
// region and the two zones use different hostnames, so stamping a European key
// with us-east-1 produces an account that fails every request with 403 and looks
// like a dud purchase.
func (h *Handler) importMarketKeys(cfg *config.AutoBuyConfig, result *marketPurchase) (imported, skipped int) {
	byRegion := map[string][]string{}
	for _, k := range result.Keys {
		key := strings.TrimSpace(k.Key)
		if key == "" {
			continue
		}
		region := strings.TrimSpace(k.Region)
		if region == "" {
			region = cfg.EffectiveRegion()
		}
		byRegion[region] = append(byRegion[region], key)
	}

	for region, keys := range byRegion {
		results := h.ImportApiKeys(strings.Join(keys, "\n"), region, region, region)
		for _, r := range results {
			switch {
			case r.Imported:
				imported++
			case r.Skipped:
				skipped++
			default:
				logger.Warnf("[AutoBuy] key %s could not be imported: %s", r.MaskedKey, r.Error)
			}
		}
	}
	return imported, skipped
}

// recordAutoBuyFailure logs a failed attempt, persists it, and notifies when the
// failure is one that will not fix itself.
func (h *Handler) recordAutoBuyFailure(trigger, zone string, err error) {
	code := marketErrorCode(err)
	entry := config.AutoBuyLogEntry{
		Zone:    zone,
		Trigger: trigger,
		Code:    code,
		Error:   err.Error(),
	}
	if recErr := config.RecordAutoBuyPurchase(entry); recErr != nil {
		logger.Warnf("[AutoBuy] could not record failure: %v", recErr)
	}

	me, ok := asMarketError(err)
	if ok && me.Terminal() {
		// Terminal means retrying is pointless, so this is the one case where a
		// sleeping operator genuinely needs waking: nothing will improve until
		// they act.
		logger.Errorf("[AutoBuy] zone %s: %v — retrying will not help; auto-buy stays idle until this is resolved", zone, err)
		h.notifyAutoBuy(config.GetAutoBuyConfig(), autoBuyNotice{
			Event:   "error",
			Zone:    zone,
			Trigger: trigger,
			Code:    code,
			Error:   err.Error(),
		})
		return
	}
	logger.Warnf("[AutoBuy] zone %s: %v", zone, err)
}

// logAutoBuySkip emits a skip at a level matching how routine it is.
func logAutoBuySkip(trigger, zone string, s *autoBuySkip) {
	if s == nil {
		return
	}
	if s.Quiet {
		logger.Debugf("[AutoBuy] zone %s skipped (%s): %s", zone, trigger, s.Reason)
		return
	}
	logger.Infof("[AutoBuy] zone %s skipped (%s): %s", zone, trigger, s.Reason)
}

// autoBuyNotice is the payload posted to the operator's notify webhook.
type autoBuyNotice struct {
	Event     string `json:"event"` // "purchase" | "error"
	Zone      string `json:"zone,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
	Purchased int    `json:"purchased,omitempty"`
	Credits   int    `json:"credits,omitempty"`
	UnitPrice int    `json:"unitPrice,omitempty"`
	OrderID   string `json:"orderId,omitempty"`
	Imported  int    `json:"imported,omitempty"`
	Skipped   int    `json:"skipped,omitempty"`
	Balance   int    `json:"balance,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
	TimeUnix  int64  `json:"timeUnix"`
}

// notifyAutoBuy posts a notice to the configured webhook. Fire-and-forget: a
// notification failure must never affect a purchase that already happened.
func (h *Handler) notifyAutoBuy(cfg *config.AutoBuyConfig, notice autoBuyNotice) {
	if cfg == nil {
		return
	}
	url := strings.TrimSpace(cfg.NotifyWebhook)
	if url == "" {
		return
	}
	notice.TimeUnix = time.Now().Unix()

	safeGo(func() {
		body, err := json.Marshal(notice)
		if err != nil {
			logger.Warnf("[AutoBuy] could not marshal notification: %v", err)
			return
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			logger.Warnf("[AutoBuy] could not build notification request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			logger.Warnf("[AutoBuy] notification POST failed: %v", err)
			return
		}
		resp.Body.Close()
	})
}
