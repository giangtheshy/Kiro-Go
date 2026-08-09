package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// autobuy.go holds the configuration for unattended purchasing of Kiro keys from
// the kiro-market API (https://api.91kiro.com). The feature exists so the pool
// refills itself while nobody is watching, which makes every guard in this file
// load-bearing: the operator is asleep, so a bad decision here spends real money
// unsupervised and nobody notices until morning.
//
// Two counters in here are deliberately persisted rather than kept in memory.
// A restart must not hand the worker a fresh spending allowance — a crash loop
// at 3am would otherwise buy on every boot.

// Zone identifiers accepted by the market API. Anything else is rejected with
// 400 bad_zone upstream, so validation refuses it locally instead.
const (
	AutoBuyZoneUS = "us"
	AutoBuyZoneEU = "eu"
)

// DefaultMarketBaseURL is the kiro-market API root.
const DefaultMarketBaseURL = "https://api.91kiro.com"

// Bounds mandated by the market API. count is 1..200 per order.
const (
	AutoBuyMinCount = 1
	AutoBuyMaxCount = 200
)

// AutoBuyMinPollInterval is the floor for the polling fallback. The market API
// rate-limits per account and its own docs say not to poll every second, so a
// smaller value is clamped up rather than honoured.
const AutoBuyMinPollInterval = 30

// DefaultAutoBuyPollInterval matches the interval the market docs recommend for
// the polling fallback.
const DefaultAutoBuyPollInterval = 60

// AutoBuyLogKept caps the retained purchase history. Entries live in config.json,
// which is rewritten whole on every save, so this bounds file size as much as
// retention.
const AutoBuyLogKept = 200

// autoBuySeenEventsKept caps the webhook dedupe set. Same storage argument as
// AutoBuyLogKept: the map is serialised into config.json.
const autoBuySeenEventsKept = 500

// DefaultTelegramApiBase is the official Telegram Bot API root.
const DefaultTelegramApiBase = "https://api.telegram.org"

// Bounds for the pool-exhaustion alert repeat count. The ceiling exists because
// each repeat is a real message: a typo of 300 would get the bot rate-limited
// exactly when the operator needs to hear from it.
const (
	DefaultPoolAlertRepeat = 3
	MaxPoolAlertRepeat     = 10
)

// AutoBuyZoneRule is the per-zone purchasing policy.
//
// Price ceilings are per-zone rather than global because the two zones are priced
// independently — the market docs' own example has us at 25 credits and eu at 10
// at the same moment. A single shared ceiling would either lock out the cheap
// zone or wave through the expensive one; there is no value that does both.
type AutoBuyZoneRule struct {
	Enabled bool `json:"enabled"`

	// MaxUnitPrice is the highest per-key price to accept, in market credits.
	// 0 means "buy at any price", which is deliberately spelled as a distinct
	// value rather than a very large number so the intent is readable.
	MaxUnitPrice int `json:"maxUnitPrice,omitempty"`

	// BuyCount is how many keys to request per order, before any trimming by the
	// daily credit ceiling. Clamped to [AutoBuyMinCount, AutoBuyMaxCount].
	BuyCount int `json:"buyCount,omitempty"`

	// MaxKeysPerDay caps keys bought in this zone per local day. 0 = unlimited.
	MaxKeysPerDay int `json:"maxKeysPerDay,omitempty"`

	// BoughtToday is reset by rollDayLocked when DayStamp changes. Persisted for
	// the same reason as SpentToday: a restart must not clear the day's tally.
	BoughtToday int `json:"boughtToday,omitempty"`
}

// AutoBuyLogEntry is one purchase attempt, successful or not. Kept so an operator
// can reconstruct overnight activity from the admin panel alone.
type AutoBuyLogEntry struct {
	TimeUnix int64  `json:"timeUnix"`
	Zone     string `json:"zone,omitempty"`
	Trigger  string `json:"trigger,omitempty"` // "webhook" | "poll" | "manual"

	Requested int `json:"requested,omitempty"`
	Purchased int `json:"purchased,omitempty"`

	// UnitPrice is indicative only. A single order can span rounds at different
	// prices, so Credits (the upstream's total_credits) is the authoritative
	// figure and the one reconciliation must use.
	UnitPrice int `json:"unitPrice,omitempty"`
	Credits   int `json:"credits,omitempty"`

	Imported int    `json:"imported,omitempty"`
	Skipped  int    `json:"skipped,omitempty"`
	OrderID  string `json:"orderId,omitempty"`

	// Code carries the upstream error code (see the market docs' error table) so
	// the UI can distinguish a retryable no_stock from a terminal
	// purchase_cap_reached without parsing prose.
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// AutoBuyConfig is the whole feature's persisted state.
type AutoBuyConfig struct {
	Enabled bool `json:"enabled"`

	// DryRun evaluates every guard and logs the decision without spending. The
	// only way to verify an overnight policy before trusting it with money.
	DryRun bool `json:"dryRun,omitempty"`

	MarketApiKey  string `json:"marketApiKey,omitempty"`
	MarketBaseURL string `json:"marketBaseUrl,omitempty"`

	// WebhookSecret verifies the X-KM-Signature HMAC on inbound market webhooks.
	// Empty means unverified: any caller who finds the URL can trigger a purchase,
	// so the admin API refuses to enable the feature without one.
	WebhookSecret string `json:"webhookSecret,omitempty"`

	// NotifyWebhook receives a POST when a purchase succeeds or hits a terminal
	// error. One of the two channels that reach a sleeping operator.
	NotifyWebhook string `json:"notifyWebhook,omitempty"`

	// TelegramBotToken and TelegramChatID drive the Telegram channel. The token is
	// a secret with full control of the bot, so it is masked on read and treated
	// like MarketApiKey: blank on save means "keep the stored one".
	//
	// Both are required together. A token without a chat id (or the reverse) would
	// silently deliver nothing, and the moment that matters is the moment nobody
	// is watching — so validation refuses the half-configured state rather than
	// accepting it and staying quiet.
	TelegramBotToken string `json:"telegramBotToken,omitempty"`
	TelegramChatID   string `json:"telegramChatId,omitempty"`

	// TelegramApiBase overrides the Telegram API root for networks where
	// api.telegram.org is blocked and traffic goes through a relay. Empty means
	// the official host.
	TelegramApiBase string `json:"telegramApiBase,omitempty"`

	// NotifyPoolExhausted alerts when no pool account can serve a request at all.
	// Distinct from the purchase notices: this fires on the proxy going dark,
	// which can happen with auto-buy switched off entirely.
	NotifyPoolExhausted bool `json:"notifyPoolExhausted,omitempty"`

	// PoolAlertRepeat is how many times the exhaustion alert is sent. Telegram
	// drops a message occasionally and this is the one alert that must land, so
	// the default repeats. 0 → DefaultPoolAlertRepeat.
	PoolAlertRepeat int `json:"poolAlertRepeat,omitempty"`

	// Zones is keyed by AutoBuyZone* constants.
	Zones map[string]*AutoBuyZoneRule `json:"zones,omitempty"`

	// MinHealthyAccounts triggers buying when the count of genuinely usable pool
	// accounts drops below it. "Usable" excludes quota-exhausted and cooled-down
	// accounts, not merely disabled ones — see pool.HealthyCount. 0 disables the
	// trigger, leaving webhooks as the only buy path.
	MinHealthyAccounts int `json:"minHealthyAccounts,omitempty"`

	// MaxPoolAccounts stops buying once the pool holds this many accounts in
	// total. A backstop against unbounded growth. 0 = no cap.
	MaxPoolAccounts int `json:"maxPoolAccounts,omitempty"`

	// MinBalance keeps a floor of market credits in reserve. 0 = spend freely.
	MinBalance int `json:"minBalance,omitempty"`

	ScheduleEnabled bool `json:"scheduleEnabled,omitempty"`

	// WindowStart/WindowEnd are "HH:MM" in the server's local timezone. A start
	// after the end wraps midnight ("22:00"–"06:00").
	WindowStart string `json:"windowStart,omitempty"`
	WindowEnd   string `json:"windowEnd,omitempty"`

	// Weekdays uses time.Weekday numbering (0=Sunday). Empty = every day.
	Weekdays []int `json:"weekdays,omitempty"`

	// MaxCreditsPerDay is the hard spending ceiling per local day, and the guard
	// that actually bounds financial exposure. 0 = unlimited.
	MaxCreditsPerDay int `json:"maxCreditsPerDay,omitempty"`

	// SpentToday and DayStamp implement the daily reset. DayStamp is a local
	// "2006-01-02" date; rollDayLocked zeroes the counters when it no longer
	// matches today. Both persist across restarts on purpose.
	SpentToday int    `json:"spentToday,omitempty"`
	DayStamp   string `json:"dayStamp,omitempty"`

	PollIntervalSec int    `json:"pollIntervalSec,omitempty"`
	DefaultRegion   string `json:"defaultRegion,omitempty"`

	BuyLog []AutoBuyLogEntry `json:"buyLog,omitempty"`

	// SeenEvents maps a webhook event_id to the unix time it was first handled.
	// The market retries a delivery up to three times with the same event_id, so
	// without this a single restock could be bought three times.
	SeenEvents map[string]int64 `json:"seenEvents,omitempty"`
}

// Audit actions for auto-buy. Stable strings: the admin UI filters on them.
const (
	AuditAutoBuySettings = "autobuy.settings"
	AuditAutoBuyPurchase = "autobuy.purchase"
)

// defaultAutoBuyConfig is the shape handed to a caller when nothing is persisted
// yet. Disabled, with both zones present so the UI has rows to render.
func defaultAutoBuyConfig() *AutoBuyConfig {
	return &AutoBuyConfig{
		Enabled:         false,
		MarketBaseURL:   DefaultMarketBaseURL,
		PollIntervalSec: DefaultAutoBuyPollInterval,
		DefaultRegion:   "us-east-1",
		Zones: map[string]*AutoBuyZoneRule{
			AutoBuyZoneUS: {BuyCount: 1},
			AutoBuyZoneEU: {BuyCount: 1},
		},
	}
}

// clone deep-copies so callers cannot mutate live config through a returned
// pointer. The maps and slices matter here: a shallow copy would share Zones.
func (a *AutoBuyConfig) clone() *AutoBuyConfig {
	if a == nil {
		return nil
	}
	out := *a
	if a.Zones != nil {
		out.Zones = make(map[string]*AutoBuyZoneRule, len(a.Zones))
		for k, v := range a.Zones {
			if v == nil {
				continue
			}
			z := *v
			out.Zones[k] = &z
		}
	}
	if a.Weekdays != nil {
		out.Weekdays = append([]int(nil), a.Weekdays...)
	}
	if a.BuyLog != nil {
		out.BuyLog = append([]AutoBuyLogEntry(nil), a.BuyLog...)
	}
	if a.SeenEvents != nil {
		out.SeenEvents = make(map[string]int64, len(a.SeenEvents))
		for k, v := range a.SeenEvents {
			out.SeenEvents[k] = v
		}
	}
	return &out
}

// Zone returns the rule for a zone, or nil. Never returns a zero-valued rule for
// a missing zone: "no rule" and "a rule that happens to be off" lead to the same
// decision today but differ in the UI, and conflating them would silently
// materialise config the operator never wrote.
func (a *AutoBuyConfig) Zone(zone string) *AutoBuyZoneRule {
	if a == nil || a.Zones == nil {
		return nil
	}
	return a.Zones[normalizeZone(zone)]
}

// EffectiveBuyCount clamps a zone's configured count into the API's 1..200 range.
func (z *AutoBuyZoneRule) EffectiveBuyCount() int {
	if z == nil {
		return 0
	}
	if z.BuyCount < AutoBuyMinCount {
		return AutoBuyMinCount
	}
	if z.BuyCount > AutoBuyMaxCount {
		return AutoBuyMaxCount
	}
	return z.BuyCount
}

// EffectivePollInterval clamps the poll interval up to the documented floor.
func (a *AutoBuyConfig) EffectivePollInterval() time.Duration {
	sec := DefaultAutoBuyPollInterval
	if a != nil && a.PollIntervalSec > 0 {
		sec = a.PollIntervalSec
	}
	if sec < AutoBuyMinPollInterval {
		sec = AutoBuyMinPollInterval
	}
	return time.Duration(sec) * time.Second
}

// EffectiveBaseURL returns the market API root with any trailing slash removed,
// so callers can join paths without producing a double slash.
func (a *AutoBuyConfig) EffectiveBaseURL() string {
	if a == nil || strings.TrimSpace(a.MarketBaseURL) == "" {
		return DefaultMarketBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(a.MarketBaseURL), "/")
}

// EffectiveRegion is the AWS region stamped onto imported accounts.
func (a *AutoBuyConfig) EffectiveRegion() string {
	if a == nil || strings.TrimSpace(a.DefaultRegion) == "" {
		return "us-east-1"
	}
	return strings.TrimSpace(a.DefaultRegion)
}

// EffectiveTelegramApiBase returns the Telegram API root with any trailing slash
// removed, so callers can join paths without a double slash.
func (a *AutoBuyConfig) EffectiveTelegramApiBase() string {
	if a == nil || strings.TrimSpace(a.TelegramApiBase) == "" {
		return DefaultTelegramApiBase
	}
	return strings.TrimRight(strings.TrimSpace(a.TelegramApiBase), "/")
}

// TelegramConfigured reports whether both halves of the Telegram channel are
// present. Either one alone delivers nothing, so callers check the pair.
func (a *AutoBuyConfig) TelegramConfigured() bool {
	if a == nil {
		return false
	}
	return strings.TrimSpace(a.TelegramBotToken) != "" && strings.TrimSpace(a.TelegramChatID) != ""
}

// EffectivePoolAlertRepeat clamps the repeat count into [1, MaxPoolAlertRepeat].
func (a *AutoBuyConfig) EffectivePoolAlertRepeat() int {
	if a == nil || a.PoolAlertRepeat <= 0 {
		return DefaultPoolAlertRepeat
	}
	if a.PoolAlertRepeat > MaxPoolAlertRepeat {
		return MaxPoolAlertRepeat
	}
	return a.PoolAlertRepeat
}

func normalizeZone(zone string) string {
	return strings.ToLower(strings.TrimSpace(zone))
}

// IsValidZone reports whether zone is one the market API accepts.
func IsValidZone(zone string) bool {
	switch normalizeZone(zone) {
	case AutoBuyZoneUS, AutoBuyZoneEU:
		return true
	}
	return false
}

// parseClock parses "HH:MM" into minutes since midnight.
func parseClock(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	return h*60 + m, nil
}

// WindowAllows reports whether now falls inside the configured buying window.
//
// Times are interpreted in now's own location, which is the server's local time.
// Both bounds are inclusive of the start and exclusive of the end. A start after
// the end wraps midnight, so "22:00"–"06:00" is one contiguous overnight window
// rather than an empty set.
//
// The weekday check uses the day the window STARTED where it wraps: an overnight
// Friday window still applies at 02:00 on Saturday, because an operator who ticks
// "Friday" and writes 22:00–06:00 means one Friday night, not two fragments.
func (a *AutoBuyConfig) WindowAllows(now time.Time) bool {
	if a == nil || !a.ScheduleEnabled {
		return true
	}

	start, errStart := parseClock(a.WindowStart)
	end, errEnd := parseClock(a.WindowEnd)
	// An unparseable window is treated as "always open" rather than "always shut":
	// the admin API validates these fields on write, so a bad value here can only
	// come from a hand-edited file, and silently refusing to ever buy would look
	// exactly like the feature being broken.
	if errStart != nil || errEnd != nil {
		return true
	}

	cur := now.Hour()*60 + now.Minute()
	wraps := start > end

	var inWindow bool
	// day is the weekday the current window opened on.
	day := int(now.Weekday())
	switch {
	case start == end:
		// Degenerate: a zero-length window would never open, which is almost
		// certainly not what someone who set both fields the same intended.
		inWindow = true
	case wraps:
		inWindow = cur >= start || cur < end
		if cur < end {
			// Still inside last night's window: attribute it to yesterday.
			day = int(now.AddDate(0, 0, -1).Weekday())
		}
	default:
		inWindow = cur >= start && cur < end
	}
	if !inWindow {
		return false
	}
	return a.weekdayAllows(day)
}

func (a *AutoBuyConfig) weekdayAllows(day int) bool {
	if len(a.Weekdays) == 0 {
		return true
	}
	for _, d := range a.Weekdays {
		if d == day {
			return true
		}
	}
	return false
}

// dayStamp formats t as the local calendar day used by the daily counters.
func dayStamp(t time.Time) string {
	return t.Format("2006-01-02")
}

// rollDayLocked zeroes the daily counters when the local day has changed.
// Caller MUST hold cfgLock.
//
// This is called at the start of every decision rather than on a timer, so the
// reset cannot be missed by a process that was asleep or stopped at midnight.
func rollDayLocked(a *AutoBuyConfig, now time.Time) bool {
	if a == nil {
		return false
	}
	today := dayStamp(now)
	if a.DayStamp == today {
		return false
	}
	a.DayStamp = today
	a.SpentToday = 0
	for _, z := range a.Zones {
		if z != nil {
			z.BoughtToday = 0
		}
	}
	return true
}

// RemainingCreditsToday returns how many credits may still be spent today, and
// whether a ceiling applies at all. An unlimited ceiling reports (0, false) so
// callers cannot mistake "no limit" for "nothing left".
func (a *AutoBuyConfig) RemainingCreditsToday() (int, bool) {
	if a == nil || a.MaxCreditsPerDay <= 0 {
		return 0, false
	}
	left := a.MaxCreditsPerDay - a.SpentToday
	if left < 0 {
		left = 0
	}
	return left, true
}

// GetAutoBuyConfig returns a deep copy of the auto-buy configuration, or defaults
// when nothing is persisted. The returned value is safe to mutate.
//
// It also rolls the day counters, so a caller that reads the config right after
// midnight sees a cleared tally rather than yesterday's.
func GetAutoBuyConfig() *AutoBuyConfig {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return defaultAutoBuyConfig()
	}
	if cfg.AutoBuy == nil {
		return defaultAutoBuyConfig()
	}
	if rollDayLocked(cfg.AutoBuy, time.Now()) {
		markDirtyLocked()
	}
	return cfg.AutoBuy.clone()
}

// ValidateAutoBuyConfig checks an operator-supplied configuration, including that
// an enabled feature actually has the credentials it needs.
//
// The rules that reject rather than clamp are the ones where guessing an intent
// could spend money: an enabled feature with no market key cannot work, and an
// enabled feature with no webhook secret would let any caller who discovers the
// endpoint trigger purchases.
func ValidateAutoBuyConfig(a *AutoBuyConfig) error {
	if err := validateAutoBuyShape(a); err != nil {
		return err
	}
	if a.Enabled {
		if strings.TrimSpace(a.MarketApiKey) == "" {
			return errors.New("marketApiKey is required when autoBuy is enabled")
		}
		if strings.TrimSpace(a.WebhookSecret) == "" {
			return errors.New("webhookSecret is required when autoBuy is enabled: without it any caller who finds the webhook URL can trigger a purchase")
		}
	}

	// The Telegram pair is checked here rather than in validateAutoBuyShape for
	// the same reason as the credentials above: the panel receives a masked token,
	// so a legitimate save carries a blank token alongside a filled chat id. Only
	// the merged object knows whether a token actually exists.
	hasToken := strings.TrimSpace(a.TelegramBotToken) != ""
	hasChat := strings.TrimSpace(a.TelegramChatID) != ""
	if hasToken != hasChat {
		if hasToken {
			return errors.New("telegramChatId is required alongside telegramBotToken: a bot with no chat to post to delivers nothing")
		}
		return errors.New("telegramBotToken is required alongside telegramChatId")
	}
	return nil
}

// validateAutoBuyShape checks everything except credential presence.
//
// The split exists because the admin panel never receives the stored secrets — it
// gets a masked placeholder — so a saved form legitimately arrives with both
// fields blank, meaning "unchanged". Demanding them on the incoming object would
// reject every edit made through the UI. Presence is therefore checked only after
// the stored values have been carried over, on the object that will actually be
// persisted.
func validateAutoBuyShape(a *AutoBuyConfig) error {
	if a == nil {
		return errors.New("autoBuy config is required")
	}
	if base := strings.TrimSpace(a.MarketBaseURL); base != "" {
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return errors.New("marketBaseUrl must be an absolute http(s) URL")
		}
	}
	if u := strings.TrimSpace(a.NotifyWebhook); u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return errors.New("notifyWebhook must be an absolute http(s) URL")
		}
	}
	if u := strings.TrimSpace(a.TelegramApiBase); u != "" {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return errors.New("telegramApiBase must be an absolute http(s) URL")
		}
	}
	if a.PoolAlertRepeat < 0 {
		return errors.New("poolAlertRepeat cannot be negative")
	}
	for zone, rule := range a.Zones {
		if !IsValidZone(zone) {
			return fmt.Errorf("unknown zone %q: only %q and %q exist", zone, AutoBuyZoneUS, AutoBuyZoneEU)
		}
		if rule == nil {
			continue
		}
		if rule.BuyCount != 0 && (rule.BuyCount < AutoBuyMinCount || rule.BuyCount > AutoBuyMaxCount) {
			return fmt.Errorf("zone %s: buyCount must be between %d and %d", zone, AutoBuyMinCount, AutoBuyMaxCount)
		}
		if rule.MaxUnitPrice < 0 {
			return fmt.Errorf("zone %s: maxUnitPrice cannot be negative", zone)
		}
		if rule.MaxKeysPerDay < 0 {
			return fmt.Errorf("zone %s: maxKeysPerDay cannot be negative", zone)
		}
	}
	if a.ScheduleEnabled {
		if _, err := parseClock(a.WindowStart); err != nil {
			return fmt.Errorf("windowStart: %w", err)
		}
		if _, err := parseClock(a.WindowEnd); err != nil {
			return fmt.Errorf("windowEnd: %w", err)
		}
	}
	for _, d := range a.Weekdays {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekday %d out of range: 0 (Sunday) to 6 (Saturday)", d)
		}
	}
	if a.MinHealthyAccounts < 0 || a.MaxPoolAccounts < 0 || a.MinBalance < 0 || a.MaxCreditsPerDay < 0 {
		return errors.New("thresholds cannot be negative")
	}
	if a.PollIntervalSec != 0 && a.PollIntervalSec < AutoBuyMinPollInterval {
		return fmt.Errorf("pollIntervalSec must be at least %d seconds", AutoBuyMinPollInterval)
	}
	return nil
}

// SetAutoBuyConfig validates and persists an operator-supplied configuration.
//
// Runtime state (counters, log, dedupe set) is carried over from the stored
// config rather than taken from the caller: the admin UI round-trips this object,
// and letting a form submit reset SpentToday would turn "save settings" into a
// way to bypass the daily ceiling.
func SetAutoBuyConfig(in *AutoBuyConfig) error {
	// Shape only at this point. Credential presence is checked further down, on
	// the merged object, because blank secrets here mean "keep the stored ones".
	if err := validateAutoBuyShape(in); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}

	next := in.clone()
	if next.Zones == nil {
		next.Zones = map[string]*AutoBuyZoneRule{}
	}
	// Keep both zones present so the UI always has both rows to render.
	for _, z := range []string{AutoBuyZoneUS, AutoBuyZoneEU} {
		if next.Zones[z] == nil {
			next.Zones[z] = &AutoBuyZoneRule{BuyCount: AutoBuyMinCount}
		}
	}
	sort.Ints(next.Weekdays)

	if prev := cfg.AutoBuy; prev != nil {
		next.SpentToday = prev.SpentToday
		next.DayStamp = prev.DayStamp
		next.BuyLog = append([]AutoBuyLogEntry(nil), prev.BuyLog...)
		if prev.SeenEvents != nil {
			next.SeenEvents = make(map[string]int64, len(prev.SeenEvents))
			for k, v := range prev.SeenEvents {
				next.SeenEvents[k] = v
			}
		}
		for zone, rule := range next.Zones {
			if p := prev.Zones[zone]; p != nil && rule != nil {
				rule.BoughtToday = p.BoughtToday
			}
		}
		// An empty key or secret from the UI means "unchanged": the admin API
		// masks both on read, so echoing the mask back must not overwrite them.
		if strings.TrimSpace(next.MarketApiKey) == "" {
			next.MarketApiKey = prev.MarketApiKey
		}
		if strings.TrimSpace(next.WebhookSecret) == "" {
			next.WebhookSecret = prev.WebhookSecret
		}
		// The Telegram token follows the same "blank means unchanged" rule, with one
		// exception: blanking BOTH fields is how the channel gets turned off. Without
		// that carve-out the stored token would come back every time, and clearing
		// the chat id alone would then fail the paired check — leaving no way to
		// disable Telegram short of hand-editing config.json.
		if strings.TrimSpace(next.TelegramBotToken) == "" && strings.TrimSpace(next.TelegramChatID) != "" {
			next.TelegramBotToken = prev.TelegramBotToken
		}
	}

	// Re-validate after carrying secrets over: a submit that enables the feature
	// while relying on the stored key must still end up with one present.
	if err := ValidateAutoBuyConfig(next); err != nil {
		return err
	}

	rollDayLocked(next, time.Now())
	cfg.AutoBuy = next
	return saveLocked()
}

// SetAutoBuyEnabled flips the master switch without touching anything else.
func SetAutoBuyEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if cfg.AutoBuy == nil {
		cfg.AutoBuy = defaultAutoBuyConfig()
	}
	if enabled {
		if strings.TrimSpace(cfg.AutoBuy.MarketApiKey) == "" {
			return errors.New("marketApiKey is required before enabling autoBuy")
		}
		if strings.TrimSpace(cfg.AutoBuy.WebhookSecret) == "" {
			return errors.New("webhookSecret is required before enabling autoBuy")
		}
	}
	cfg.AutoBuy.Enabled = enabled
	return saveLocked()
}

// RecordAutoBuyPurchase persists the outcome of one purchase attempt: it appends
// a log entry and, for a real purchase, advances the daily counters.
//
// Counters advance from the entry's own Credits and Purchased figures, which come
// from the upstream response, not from what was requested. Concurrent competition
// for stock means asking for 5 and receiving 3 is a normal outcome, and charging
// the ceiling for the 2 that never arrived would drift the tally.
func RecordAutoBuyPurchase(e AutoBuyLogEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if cfg.AutoBuy == nil {
		cfg.AutoBuy = defaultAutoBuyConfig()
	}
	a := cfg.AutoBuy
	now := time.Now()
	rollDayLocked(a, now)

	if e.TimeUnix == 0 {
		e.TimeUnix = now.Unix()
	}
	e.Zone = normalizeZone(e.Zone)

	// A dry run must leave the counters untouched, otherwise testing a policy
	// would consume the very allowance it is meant to verify.
	if !e.DryRun && e.Purchased > 0 {
		a.SpentToday += e.Credits
		if z := a.Zones[e.Zone]; z != nil {
			z.BoughtToday += e.Purchased
		}
	}

	a.BuyLog = append(a.BuyLog, e)
	if len(a.BuyLog) > AutoBuyLogKept {
		a.BuyLog = a.BuyLog[len(a.BuyLog)-AutoBuyLogKept:]
	}
	return saveLocked()
}

// GetAutoBuyLog returns the most recent purchase attempts, newest first.
func GetAutoBuyLog(limit int) []AutoBuyLogEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AutoBuy == nil {
		return nil
	}
	src := cfg.AutoBuy.BuyLog
	if limit <= 0 || limit > len(src) {
		limit = len(src)
	}
	out := make([]AutoBuyLogEntry, 0, limit)
	for i := len(src) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, src[i])
	}
	return out
}

// MarkAutoBuyEventSeen records a webhook event_id and reports whether it is new.
//
// The market retries a failed delivery up to three times carrying the same
// event_id. Returning false on a repeat is what stops one restock notification
// from becoming three purchases. The idempotency key sent upstream guards the
// same case server-side; this guard also saves the wasted round trips.
func MarkAutoBuyEventSeen(eventID string) (bool, error) {
	id := strings.TrimSpace(eventID)
	if id == "" {
		// No id to deduplicate on. Treat as new and let the upstream idempotency
		// key be the only protection, rather than dropping the event entirely.
		return true, nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return false, errors.New("config not initialized")
	}
	if cfg.AutoBuy == nil {
		cfg.AutoBuy = defaultAutoBuyConfig()
	}
	a := cfg.AutoBuy
	if a.SeenEvents == nil {
		a.SeenEvents = map[string]int64{}
	}
	if _, dup := a.SeenEvents[id]; dup {
		return false, nil
	}
	a.SeenEvents[id] = time.Now().Unix()
	pruneSeenEventsLocked(a)
	return true, saveLocked()
}

// pruneSeenEventsLocked keeps the dedupe map bounded by dropping the oldest ids.
// Caller MUST hold cfgLock.
func pruneSeenEventsLocked(a *AutoBuyConfig) {
	if len(a.SeenEvents) <= autoBuySeenEventsKept {
		return
	}
	type seen struct {
		id string
		ts int64
	}
	all := make([]seen, 0, len(a.SeenEvents))
	for id, ts := range a.SeenEvents {
		all = append(all, seen{id, ts})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ts < all[j].ts })
	for _, s := range all[:len(all)-autoBuySeenEventsKept] {
		delete(a.SeenEvents, s.id)
	}
}
