package config

import (
	"path/filepath"
	"testing"
)

func initBanTestConfig(t *testing.T) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
}

func TestBanIPUnbanClear(t *testing.T) {
	initBanTestConfig(t)

	added, err := BanIP("1.2.3.4", BanReasonManual, "spammer", 3)
	if err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !added {
		t.Fatalf("expected first ban to report a new entry")
	}
	if !IsBanned("1.2.3.4") {
		t.Fatalf("expected 1.2.3.4 to be banned")
	}
	if IsBanned("5.6.7.8") {
		t.Fatalf("expected unrelated IP to be unbanned")
	}

	removed, err := UnbanIP("1.2.3.4")
	if err != nil {
		t.Fatalf("unban: %v", err)
	}
	if !removed {
		t.Fatalf("expected unban to report a removal")
	}
	if IsBanned("1.2.3.4") {
		t.Fatalf("expected IP to be lifted after unban")
	}

	removed, err = UnbanIP("1.2.3.4")
	if err != nil {
		t.Fatalf("second unban: %v", err)
	}
	if removed {
		t.Fatalf("expected second unban to be a no-op")
	}

	if _, err := BanIP("10.0.0.1", BanReasonAutoSpam, "", 5); err != nil {
		t.Fatalf("ban 10.0.0.1: %v", err)
	}
	if _, err := BanIP("10.0.0.2", BanReasonAutoAdmin, "", 5); err != nil {
		t.Fatalf("ban 10.0.0.2: %v", err)
	}
	n, err := ClearBans()
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 cleared entries, got %d", n)
	}
	if got := len(BannedIPsSnapshot()); got != 0 {
		t.Fatalf("expected empty ban list after clear, got %d", got)
	}
	if n, err := ClearBans(); err != nil || n != 0 {
		t.Fatalf("expected clearing an empty list to be a no-op, got n=%d err=%v", n, err)
	}
}

func TestBanIPRejectsInvalidAddress(t *testing.T) {
	initBanTestConfig(t)

	for _, bad := range []string{"", "   ", "not-an-ip", "999.999.999.999"} {
		if _, err := BanIP(bad, BanReasonManual, "", 0); err == nil {
			t.Fatalf("expected BanIP(%q) to fail", bad)
		}
	}
	if got := len(BannedIPsSnapshot()); got != 0 {
		t.Fatalf("expected no entries after rejected bans, got %d", got)
	}
}

// A disabled gate must report every address as unbanned so an operator can lift a
// bad lockout with one toggle, without first clearing the list.
func TestBanIsBannedFalseWhenGateDisabled(t *testing.T) {
	initBanTestConfig(t)

	if _, err := BanIP("1.2.3.4", BanReasonManual, "", 0); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !BanEnabled() {
		t.Fatalf("expected the gate to default to enabled")
	}
	if !IsBanned("1.2.3.4") {
		t.Fatalf("expected IP to be banned while the gate is on")
	}

	if err := SetBanEnabled(false); err != nil {
		t.Fatalf("disable gate: %v", err)
	}
	if BanEnabled() {
		t.Fatalf("expected the gate to be reported as disabled")
	}
	if IsBanned("1.2.3.4") {
		t.Fatalf("expected IsBanned to return false while the gate is disabled")
	}
	// The entry itself must survive the toggle.
	if got := len(BannedIPsSnapshot()); got != 1 {
		t.Fatalf("expected the ban list to be preserved, got %d entries", got)
	}

	if err := SetBanEnabled(true); err != nil {
		t.Fatalf("re-enable gate: %v", err)
	}
	if !IsBanned("1.2.3.4") {
		t.Fatalf("expected the ban to apply again after re-enabling")
	}
}

func TestBanIsBannedFalseForEmptyIP(t *testing.T) {
	initBanTestConfig(t)

	if IsBanned("") {
		t.Fatalf("expected IsBanned(\"\") to be false")
	}
	if _, err := BanIP("1.2.3.4", BanReasonManual, "", 0); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if IsBanned("") {
		t.Fatalf("expected IsBanned(\"\") to stay false with a non-empty ban list")
	}
}

func TestBanIPSameAddressUpdatesInsteadOfDuplicating(t *testing.T) {
	initBanTestConfig(t)

	if added, err := BanIP("1.2.3.4", BanReasonAutoSpam, "first", 2); err != nil || !added {
		t.Fatalf("first ban: added=%v err=%v", added, err)
	}
	added, err := BanIP("1.2.3.4", BanReasonManual, "second", 9)
	if err != nil {
		t.Fatalf("second ban: %v", err)
	}
	if added {
		t.Fatalf("expected re-banning a known IP to report added=false")
	}

	list := BannedIPsSnapshot()
	if len(list) != 1 {
		t.Fatalf("expected exactly one entry, got %d", len(list))
	}
	got := list[0]
	if got.Reason != BanReasonManual {
		t.Fatalf("expected reason to be refreshed, got %q", got.Reason)
	}
	if got.Fails != 9 {
		t.Fatalf("expected fails to be refreshed, got %d", got.Fails)
	}
	if got.Note != "second" {
		t.Fatalf("expected note to be refreshed, got %q", got.Note)
	}

	// Zero fails and an empty note must not wipe existing values.
	if _, err := BanIP("1.2.3.4", "", "", 0); err != nil {
		t.Fatalf("third ban: %v", err)
	}
	list = BannedIPsSnapshot()
	if len(list) != 1 {
		t.Fatalf("expected still one entry, got %d", len(list))
	}
	if list[0].Fails != 9 || list[0].Note != "second" {
		t.Fatalf("expected fails/note to be preserved, got fails=%d note=%q", list[0].Fails, list[0].Note)
	}
	if list[0].Reason != BanReasonManual {
		t.Fatalf("expected empty reason to default to manual, got %q", list[0].Reason)
	}
}

func TestBannedIPsSnapshotIsNewestFirst(t *testing.T) {
	initBanTestConfig(t)

	order := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	for _, ip := range order {
		if _, err := BanIP(ip, BanReasonManual, "", 0); err != nil {
			t.Fatalf("ban %s: %v", ip, err)
		}
	}

	list := BannedIPsSnapshot()
	if len(list) != len(order) {
		t.Fatalf("expected %d entries, got %d", len(order), len(list))
	}
	for i, ip := range []string{"10.0.0.3", "10.0.0.2", "10.0.0.1"} {
		if list[i].IP != ip {
			t.Fatalf("snapshot[%d] = %q, want %q (newest first)", i, list[i].IP, ip)
		}
	}

	// The snapshot must be a copy: mutating it cannot corrupt stored state.
	list[0].IP = "mutated"
	if BannedIPsSnapshot()[0].IP != "10.0.0.3" {
		t.Fatalf("expected BannedIPsSnapshot to return an independent copy")
	}
}

func TestBanThresholdClampsToAtLeastOne(t *testing.T) {
	initBanTestConfig(t)

	if got := GetBanThreshold(); got != DefaultBanThreshold {
		t.Fatalf("expected unset threshold to fall back to %d, got %d", DefaultBanThreshold, got)
	}

	for _, n := range []int{0, -1, -100} {
		if err := SetBanThreshold(n); err != nil {
			t.Fatalf("SetBanThreshold(%d): %v", n, err)
		}
		if got := GetBanThreshold(); got != 1 {
			t.Fatalf("SetBanThreshold(%d) then GetBanThreshold() = %d, want 1", n, got)
		}
	}

	if err := SetBanThreshold(12); err != nil {
		t.Fatalf("SetBanThreshold(12): %v", err)
	}
	if got := GetBanThreshold(); got != 12 {
		t.Fatalf("expected threshold 12, got %d", got)
	}
}
