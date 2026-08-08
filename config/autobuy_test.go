package config

import (
	"path/filepath"
	"testing"
	"time"
)

// newAutoBuyTestConfig points the package at a throwaway config file so each test
// starts from an empty auto-buy block.
func newAutoBuyTestConfig(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
}

// enabledAutoBuy is a minimally valid configuration. Both secrets are present
// because ValidateAutoBuyConfig refuses to enable the feature without them.
func enabledAutoBuy() *AutoBuyConfig {
	return &AutoBuyConfig{
		Enabled:       true,
		MarketApiKey:  "usr-test",
		WebhookSecret: "s3cret",
		Zones: map[string]*AutoBuyZoneRule{
			AutoBuyZoneUS: {Enabled: true, BuyCount: 5, MaxUnitPrice: 25},
			AutoBuyZoneEU: {Enabled: true, BuyCount: 3, MaxUnitPrice: 10},
		},
	}
}

func mustSetAutoBuy(t *testing.T, c *AutoBuyConfig) {
	t.Helper()
	if err := SetAutoBuyConfig(c); err != nil {
		t.Fatalf("SetAutoBuyConfig: %v", err)
	}
}

// A price ceiling per zone is the whole reason zones carry their own rule: the
// market prices them independently, so one shared ceiling cannot serve both.
func TestZoneRulesKeepIndependentPriceCeilings(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	got := GetAutoBuyConfig()
	if us := got.Zone(AutoBuyZoneUS); us == nil || us.MaxUnitPrice != 25 {
		t.Fatalf("us ceiling: want 25, got %+v", us)
	}
	if eu := got.Zone(AutoBuyZoneEU); eu == nil || eu.MaxUnitPrice != 10 {
		t.Fatalf("eu ceiling: want 10, got %+v", eu)
	}
}

func TestZoneReturnsNilForUnknownZone(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	// A missing zone must not materialise a zero-valued rule: "no rule" and "a
	// rule that happens to be off" are different states in the UI.
	if got := GetAutoBuyConfig().Zone("apac"); got != nil {
		t.Fatalf("unknown zone should have no rule, got %+v", got)
	}
}

func TestSetAutoBuyConfigAlwaysMaterialisesBothZones(t *testing.T) {
	newAutoBuyTestConfig(t)
	in := enabledAutoBuy()
	in.Zones = map[string]*AutoBuyZoneRule{AutoBuyZoneUS: {Enabled: true, BuyCount: 2}}
	mustSetAutoBuy(t, in)

	got := GetAutoBuyConfig()
	if got.Zone(AutoBuyZoneEU) == nil {
		t.Fatal("eu rule should be created so the panel has a row to render")
	}
}

func TestEffectiveBuyCountClampsToApiBounds(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero becomes the minimum", 0, AutoBuyMinCount},
		{"negative becomes the minimum", -4, AutoBuyMinCount},
		{"in range is preserved", 7, 7},
		{"above the cap is clamped", 5000, AutoBuyMaxCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := &AutoBuyZoneRule{BuyCount: tc.in}
			if got := z.EffectiveBuyCount(); got != tc.want {
				t.Fatalf("EffectiveBuyCount(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The market docs warn against polling too often, so a smaller interval is
// clamped up rather than honoured.
func TestPollIntervalIsClampedToTheDocumentedFloor(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, DefaultAutoBuyPollInterval * time.Second},
		{1, AutoBuyMinPollInterval * time.Second},
		{29, AutoBuyMinPollInterval * time.Second},
		{30, 30 * time.Second},
		{120, 120 * time.Second},
	}
	for _, tc := range cases {
		c := &AutoBuyConfig{PollIntervalSec: tc.in}
		if got := c.EffectivePollInterval(); got != tc.want {
			t.Fatalf("PollIntervalSec=%d gave %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveBaseURLTrimsTrailingSlash(t *testing.T) {
	c := &AutoBuyConfig{MarketBaseURL: "https://api.91kiro.com/"}
	if got := c.EffectiveBaseURL(); got != "https://api.91kiro.com" {
		t.Fatalf("got %q, want the URL without a trailing slash", got)
	}
	empty := &AutoBuyConfig{}
	if got := empty.EffectiveBaseURL(); got != DefaultMarketBaseURL {
		t.Fatalf("empty base URL should fall back to the default, got %q", got)
	}
}

// An overnight window is the interesting case: a naive start<=now<end comparison
// reports false for every hour of it.
func TestWindowAllowsHandlesMidnightWrap(t *testing.T) {
	c := &AutoBuyConfig{ScheduleEnabled: true, WindowStart: "22:00", WindowEnd: "06:00"}
	cases := []struct {
		hour, min int
		want      bool
	}{
		{22, 0, true},   // opens exactly on the boundary
		{23, 30, true},  // before midnight
		{2, 15, true},   // after midnight, same window
		{5, 59, true},   // last minute inside
		{6, 0, false},   // end is exclusive
		{12, 0, false},  // middle of the day
		{21, 59, false}, // one minute early
	}
	for _, tc := range cases {
		now := time.Date(2026, 8, 7, tc.hour, tc.min, 0, 0, time.Local)
		if got := c.WindowAllows(now); got != tc.want {
			t.Fatalf("%02d:%02d inside 22:00-06:00 = %v, want %v", tc.hour, tc.min, got, tc.want)
		}
	}
}

func TestWindowAllowsSameDayRange(t *testing.T) {
	c := &AutoBuyConfig{ScheduleEnabled: true, WindowStart: "09:00", WindowEnd: "17:00"}
	cases := []struct {
		hour int
		want bool
	}{{8, false}, {9, true}, {13, true}, {16, true}, {17, false}, {23, false}}
	for _, tc := range cases {
		now := time.Date(2026, 8, 7, tc.hour, 0, 0, 0, time.Local)
		if got := c.WindowAllows(now); got != tc.want {
			t.Fatalf("%02d:00 inside 09:00-17:00 = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

func TestWindowAllowsEverythingWhenScheduleIsOff(t *testing.T) {
	c := &AutoBuyConfig{ScheduleEnabled: false, WindowStart: "09:00", WindowEnd: "10:00"}
	now := time.Date(2026, 8, 7, 3, 0, 0, 0, time.Local)
	if !c.WindowAllows(now) {
		t.Fatal("a disabled schedule must not gate anything")
	}
}

// An unparseable window can only come from a hand-edited file. Failing open is
// deliberate: silently never buying would look exactly like a broken feature.
func TestWindowAllowsFailsOpenOnUnparseableBounds(t *testing.T) {
	c := &AutoBuyConfig{ScheduleEnabled: true, WindowStart: "not-a-time", WindowEnd: "06:00"}
	if !c.WindowAllows(time.Now()) {
		t.Fatal("a malformed window should fail open, not shut")
	}
}

func TestWindowAllowsFiltersWeekdays(t *testing.T) {
	// 2026-08-07 is a Friday; time.Weekday numbers Friday as 5.
	friday := time.Date(2026, 8, 7, 12, 0, 0, 0, time.Local)
	if friday.Weekday() != time.Friday {
		t.Fatalf("fixture drifted: %s is a %s", friday, friday.Weekday())
	}
	saturday := friday.AddDate(0, 0, 1)

	c := &AutoBuyConfig{
		ScheduleEnabled: true,
		WindowStart:     "09:00",
		WindowEnd:       "17:00",
		Weekdays:        []int{int(time.Friday)},
	}
	if !c.WindowAllows(friday) {
		t.Fatal("Friday is selected and should be allowed")
	}
	if c.WindowAllows(saturday) {
		t.Fatal("Saturday is not selected and should be refused")
	}
}

// An overnight Friday window still applies at 02:00 on Saturday: someone who
// ticks Friday and writes 22:00-06:00 means one Friday night, not two fragments.
func TestOvernightWindowAttributesEarlyHoursToTheOpeningDay(t *testing.T) {
	c := &AutoBuyConfig{
		ScheduleEnabled: true,
		WindowStart:     "22:00",
		WindowEnd:       "06:00",
		Weekdays:        []int{int(time.Friday)},
	}
	saturdayEarly := time.Date(2026, 8, 8, 2, 0, 0, 0, time.Local)
	if saturdayEarly.Weekday() != time.Saturday {
		t.Fatalf("fixture drifted: %s is a %s", saturdayEarly, saturdayEarly.Weekday())
	}
	if !c.WindowAllows(saturdayEarly) {
		t.Fatal("02:00 Saturday belongs to Friday night's window and should be allowed")
	}

	// The evening half of Saturday is a different window and is not selected.
	saturdayLate := time.Date(2026, 8, 8, 23, 0, 0, 0, time.Local)
	if c.WindowAllows(saturdayLate) {
		t.Fatal("23:00 Saturday opens Saturday's own window, which is not selected")
	}
}

func TestRemainingCreditsTodayDistinguishesUnlimited(t *testing.T) {
	unlimited := &AutoBuyConfig{MaxCreditsPerDay: 0, SpentToday: 900}
	if _, capped := unlimited.RemainingCreditsToday(); capped {
		t.Fatal("a zero ceiling means unlimited and must not report as capped")
	}

	capped := &AutoBuyConfig{MaxCreditsPerDay: 100, SpentToday: 30}
	left, isCapped := capped.RemainingCreditsToday()
	if !isCapped || left != 70 {
		t.Fatalf("got (%d, %v), want (70, true)", left, isCapped)
	}

	// Overspend must clamp at zero, never report a negative allowance.
	over := &AutoBuyConfig{MaxCreditsPerDay: 100, SpentToday: 250}
	left, _ = over.RemainingCreditsToday()
	if left != 0 {
		t.Fatalf("overspend should leave 0, got %d", left)
	}
}

// The daily counters are persisted precisely so a restart cannot hand the worker
// a fresh allowance. This pins the reset to the calendar day, not to process
// lifetime.
func TestRollDayResetsCountersOnlyWhenTheDayChanges(t *testing.T) {
	c := &AutoBuyConfig{
		SpentToday: 400,
		DayStamp:   "2026-08-07",
		Zones:      map[string]*AutoBuyZoneRule{AutoBuyZoneUS: {BoughtToday: 9}},
	}

	sameDay := time.Date(2026, 8, 7, 23, 59, 0, 0, time.Local)
	if rolled := rollDayLocked(c, sameDay); rolled {
		t.Fatal("same calendar day must not reset")
	}
	if c.SpentToday != 400 || c.Zones[AutoBuyZoneUS].BoughtToday != 9 {
		t.Fatal("counters changed on the same day")
	}

	nextDay := time.Date(2026, 8, 8, 0, 1, 0, 0, time.Local)
	if rolled := rollDayLocked(c, nextDay); !rolled {
		t.Fatal("a new calendar day must reset")
	}
	if c.SpentToday != 0 {
		t.Fatalf("SpentToday should reset to 0, got %d", c.SpentToday)
	}
	if c.Zones[AutoBuyZoneUS].BoughtToday != 0 {
		t.Fatalf("per-zone counter should reset to 0, got %d", c.Zones[AutoBuyZoneUS].BoughtToday)
	}
	if c.DayStamp != "2026-08-08" {
		t.Fatalf("DayStamp should advance, got %q", c.DayStamp)
	}
}

// Saving settings must not clear the day's tally, or "save" would become a way to
// bypass the ceiling the operator just configured.
func TestSetAutoBuyConfigPreservesRuntimeCounters(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	if err := RecordAutoBuyPurchase(AutoBuyLogEntry{
		Zone: AutoBuyZoneUS, Purchased: 4, Credits: 120,
	}); err != nil {
		t.Fatalf("RecordAutoBuyPurchase: %v", err)
	}

	// Round-trip the config the way the admin panel does, with zeroed counters.
	resubmit := enabledAutoBuy()
	resubmit.SpentToday = 0
	resubmit.Zones[AutoBuyZoneUS].BoughtToday = 0
	mustSetAutoBuy(t, resubmit)

	got := GetAutoBuyConfig()
	if got.SpentToday != 120 {
		t.Fatalf("SpentToday should survive a settings save, got %d", got.SpentToday)
	}
	if us := got.Zone(AutoBuyZoneUS); us == nil || us.BoughtToday != 4 {
		t.Fatalf("per-zone counter should survive a settings save, got %+v", us)
	}
	if len(got.BuyLog) != 1 {
		t.Fatalf("buy log should survive a settings save, got %d entries", len(got.BuyLog))
	}
}

// The admin API masks both secrets on read, so an empty value coming back means
// "unchanged" and must not blank the stored credential.
func TestSetAutoBuyConfigTreatsEmptySecretsAsUnchanged(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	resubmit := enabledAutoBuy()
	resubmit.MarketApiKey = ""
	resubmit.WebhookSecret = ""
	mustSetAutoBuy(t, resubmit)

	cfgLock.RLock()
	stored := cfg.AutoBuy
	key, secret := stored.MarketApiKey, stored.WebhookSecret
	cfgLock.RUnlock()

	if key != "usr-test" {
		t.Fatalf("market key should be retained, got %q", key)
	}
	if secret != "s3cret" {
		t.Fatalf("webhook secret should be retained, got %q", secret)
	}
}

// Counters advance from what the upstream delivered, not from what was asked for:
// partial fills are documented as normal and charging for keys that never
// arrived would drift the tally.
func TestRecordPurchaseCountsDeliveredNotRequested(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	if err := RecordAutoBuyPurchase(AutoBuyLogEntry{
		Zone: AutoBuyZoneUS, Requested: 10, Purchased: 3, Credits: 75,
	}); err != nil {
		t.Fatalf("RecordAutoBuyPurchase: %v", err)
	}

	got := GetAutoBuyConfig()
	if got.SpentToday != 75 {
		t.Fatalf("SpentToday should be the charged total 75, got %d", got.SpentToday)
	}
	if us := got.Zone(AutoBuyZoneUS); us.BoughtToday != 3 {
		t.Fatalf("BoughtToday should be the delivered count 3, got %d", us.BoughtToday)
	}
}

// A dry run exists to verify a policy. Consuming the allowance it is testing
// would make the test itself change the outcome.
func TestDryRunLeavesCountersUntouched(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	if err := RecordAutoBuyPurchase(AutoBuyLogEntry{
		Zone: AutoBuyZoneUS, Requested: 5, Purchased: 5, Credits: 125, DryRun: true,
	}); err != nil {
		t.Fatalf("RecordAutoBuyPurchase: %v", err)
	}

	got := GetAutoBuyConfig()
	if got.SpentToday != 0 {
		t.Fatalf("a dry run must not spend, got SpentToday=%d", got.SpentToday)
	}
	if us := got.Zone(AutoBuyZoneUS); us.BoughtToday != 0 {
		t.Fatalf("a dry run must not count keys, got %d", us.BoughtToday)
	}
	if len(got.BuyLog) != 1 {
		t.Fatal("a dry run should still be logged")
	}
}

// A failed attempt has no delivery, so it must not move the counters even though
// it is recorded.
func TestFailedAttemptIsLoggedWithoutSpending(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	if err := RecordAutoBuyPurchase(AutoBuyLogEntry{
		Zone: AutoBuyZoneUS, Code: "no_stock", Error: "no stock available",
	}); err != nil {
		t.Fatalf("RecordAutoBuyPurchase: %v", err)
	}

	got := GetAutoBuyConfig()
	if got.SpentToday != 0 {
		t.Fatalf("a failure must not spend, got %d", got.SpentToday)
	}
	logs := GetAutoBuyLog(10)
	if len(logs) != 1 || logs[0].Code != "no_stock" {
		t.Fatalf("failure should be logged with its code, got %+v", logs)
	}
}

func TestGetAutoBuyLogReturnsNewestFirstAndRespectsLimit(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	for i := 1; i <= 5; i++ {
		if err := RecordAutoBuyPurchase(AutoBuyLogEntry{
			Zone: AutoBuyZoneUS, Purchased: i, Credits: i,
		}); err != nil {
			t.Fatalf("RecordAutoBuyPurchase: %v", err)
		}
	}

	logs := GetAutoBuyLog(2)
	if len(logs) != 2 {
		t.Fatalf("limit not honoured: got %d entries", len(logs))
	}
	if logs[0].Purchased != 5 || logs[1].Purchased != 4 {
		t.Fatalf("want newest first (5 then 4), got %d then %d", logs[0].Purchased, logs[1].Purchased)
	}
}

func TestBuyLogIsBounded(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	for i := 0; i < AutoBuyLogKept+25; i++ {
		if err := RecordAutoBuyPurchase(AutoBuyLogEntry{Zone: AutoBuyZoneUS, Purchased: 1}); err != nil {
			t.Fatalf("RecordAutoBuyPurchase: %v", err)
		}
	}
	if got := len(GetAutoBuyConfig().BuyLog); got != AutoBuyLogKept {
		t.Fatalf("log should be capped at %d, got %d", AutoBuyLogKept, got)
	}
}

// The market retries a delivery up to three times carrying the same event_id.
// Without this, one restock notification becomes three purchases.
func TestMarkEventSeenRejectsRedeliveries(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	fresh, err := MarkAutoBuyEventSeen("evt-1")
	if err != nil || !fresh {
		t.Fatalf("first delivery should be fresh: fresh=%v err=%v", fresh, err)
	}
	for attempt := 2; attempt <= 3; attempt++ {
		fresh, err = MarkAutoBuyEventSeen("evt-1")
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if fresh {
			t.Fatalf("attempt %d with the same event_id must be seen as a duplicate", attempt)
		}
	}

	if fresh, _ = MarkAutoBuyEventSeen("evt-2"); !fresh {
		t.Fatal("a different event_id must be treated as new")
	}
}

// An event with no id cannot be deduplicated locally. Treat it as new and let the
// upstream idempotency key be the guard, rather than dropping it entirely.
func TestMarkEventSeenAcceptsMissingID(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	for i := 0; i < 2; i++ {
		fresh, err := MarkAutoBuyEventSeen("  ")
		if err != nil {
			t.Fatalf("MarkAutoBuyEventSeen: %v", err)
		}
		if !fresh {
			t.Fatal("a blank event id must not be treated as a duplicate")
		}
	}
}

func TestSeenEventsSetIsBounded(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	for i := 0; i < autoBuySeenEventsKept+40; i++ {
		if _, err := MarkAutoBuyEventSeen("evt-" + time.Duration(i).String() + "-" + string(rune('a'+i%26))); err != nil {
			t.Fatalf("MarkAutoBuyEventSeen: %v", err)
		}
	}
	cfgLock.RLock()
	size := len(cfg.AutoBuy.SeenEvents)
	cfgLock.RUnlock()
	if size > autoBuySeenEventsKept {
		t.Fatalf("dedupe set should be capped at %d, got %d", autoBuySeenEventsKept, size)
	}
}

// Enabling without a market key cannot work, and enabling without a webhook
// secret would let anyone who finds the URL trigger a purchase. Both are refused
// rather than guessed at.
func TestValidateRefusesEnablingWithoutCredentials(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AutoBuyConfig)
	}{
		{"no market key", func(c *AutoBuyConfig) { c.MarketApiKey = "" }},
		{"no webhook secret", func(c *AutoBuyConfig) { c.WebhookSecret = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := enabledAutoBuy()
			tc.mut(c)
			if err := ValidateAutoBuyConfig(c); err == nil {
				t.Fatal("expected validation to refuse")
			}
		})
	}
}

// A disabled feature needs no credentials: an operator should be able to save a
// draft policy before pasting secrets in.
func TestValidateAllowsDisabledWithoutCredentials(t *testing.T) {
	c := &AutoBuyConfig{Enabled: false}
	if err := ValidateAutoBuyConfig(c); err != nil {
		t.Fatalf("a disabled config should validate, got %v", err)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AutoBuyConfig)
	}{
		{"relative base URL", func(c *AutoBuyConfig) { c.MarketBaseURL = "api.91kiro.com" }},
		{"relative notify URL", func(c *AutoBuyConfig) { c.NotifyWebhook = "example.com/hook" }},
		{"unknown zone", func(c *AutoBuyConfig) { c.Zones["apac"] = &AutoBuyZoneRule{} }},
		{"count above the API cap", func(c *AutoBuyConfig) { c.Zones[AutoBuyZoneUS].BuyCount = 500 }},
		{"negative price ceiling", func(c *AutoBuyConfig) { c.Zones[AutoBuyZoneUS].MaxUnitPrice = -1 }},
		{"negative daily key cap", func(c *AutoBuyConfig) { c.Zones[AutoBuyZoneEU].MaxKeysPerDay = -3 }},
		{"weekday out of range", func(c *AutoBuyConfig) { c.Weekdays = []int{7} }},
		{"negative threshold", func(c *AutoBuyConfig) { c.MinHealthyAccounts = -1 }},
		{"poll interval below the floor", func(c *AutoBuyConfig) { c.PollIntervalSec = 5 }},
		{"schedule on with a bad window", func(c *AutoBuyConfig) {
			c.ScheduleEnabled = true
			c.WindowStart = "25:00"
			c.WindowEnd = "06:00"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := enabledAutoBuy()
			tc.mut(c)
			if err := ValidateAutoBuyConfig(c); err == nil {
				t.Fatal("expected validation to refuse")
			}
		})
	}
}

func TestSetAutoBuyEnabledRequiresCredentials(t *testing.T) {
	newAutoBuyTestConfig(t)

	draft := &AutoBuyConfig{Enabled: false}
	mustSetAutoBuy(t, draft)

	if err := SetAutoBuyEnabled(true); err == nil {
		t.Fatal("enabling without credentials should be refused")
	}

	mustSetAutoBuy(t, enabledAutoBuy())
	if err := SetAutoBuyEnabled(false); err != nil {
		t.Fatalf("disabling should always be allowed: %v", err)
	}
	if GetAutoBuyConfig().Enabled {
		t.Fatal("Enabled should now be false")
	}
}

// GetAutoBuyConfig hands out a deep copy, so a caller mutating what it got back
// must not reach into live config.
func TestGetAutoBuyConfigReturnsAnIsolatedCopy(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())

	got := GetAutoBuyConfig()
	got.Enabled = false
	got.SpentToday = 9999
	got.Zone(AutoBuyZoneUS).MaxUnitPrice = 1

	again := GetAutoBuyConfig()
	if !again.Enabled {
		t.Fatal("mutating the copy changed live Enabled")
	}
	if again.SpentToday == 9999 {
		t.Fatal("mutating the copy changed live SpentToday")
	}
	if again.Zone(AutoBuyZoneUS).MaxUnitPrice != 25 {
		t.Fatal("mutating the copy changed the live zone rule")
	}
}

func TestIsValidZone(t *testing.T) {
	for _, ok := range []string{"us", "eu", "US", " eu "} {
		if !IsValidZone(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "apac", "us-east-1"} {
		if IsValidZone(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}

// Reading the config after midnight must show a cleared tally, even if no
// purchase has happened since. The reset is driven by reads rather than a timer
// so a process that was stopped at midnight cannot miss it.
func TestGetAutoBuyConfigRollsTheDayOnRead(t *testing.T) {
	newAutoBuyTestConfig(t)
	mustSetAutoBuy(t, enabledAutoBuy())
	if err := RecordAutoBuyPurchase(AutoBuyLogEntry{Zone: AutoBuyZoneUS, Purchased: 2, Credits: 50}); err != nil {
		t.Fatalf("RecordAutoBuyPurchase: %v", err)
	}

	// Backdate the stamp to simulate the process having last acted yesterday.
	cfgLock.Lock()
	cfg.AutoBuy.DayStamp = "2000-01-01"
	cfgLock.Unlock()

	got := GetAutoBuyConfig()
	if got.SpentToday != 0 {
		t.Fatalf("a stale day stamp should clear SpentToday, got %d", got.SpentToday)
	}
	if got.DayStamp != dayStamp(time.Now()) {
		t.Fatalf("DayStamp should be today, got %q", got.DayStamp)
	}
}
