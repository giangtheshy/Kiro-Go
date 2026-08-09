package proxy

import (
	"fmt"
	"testing"

	"kiro-go/config"
)

// overageErr is the real shape of the upstream rejection: HTTP 400 carrying a
// ServiceQuotaExceededException whose reason names the overage limit. Keying the
// test off a fabricated "402" string would pass while the production matcher
// silently failed on the real payload.
const overageErr = `HTTP 400 from Kiro IDE: {"__type":"com.amazon.aws.q#ServiceQuotaExceededException",` +
	`"message":"You have reached the limit for overages.","reason":"OVERAGE_REQUEST_LIMIT_EXCEEDED"}`

func accountByIDForTest(t *testing.T, id string) config.Account {
	t.Helper()
	for _, acc := range config.GetAccounts() {
		if acc.ID == id {
			return acc
		}
	}
	t.Fatalf("account %q not found", id)
	return config.Account{}
}

// An overage rejection must take the account out of service permanently, not
// cool it down: the allowance holds until the billing period rolls over, so a
// cooldown would just re-offer a guaranteed 402 every hour.
func TestOverageErrorDisablesAccount(t *testing.T) {
	h := newAutoBuyHandler(t)
	addAccount(t, h, "spent", true)

	acc := accountByIDForTest(t, "spent")
	h.handleAccountFailure(&acc, fmt.Errorf("%s", overageErr))

	got := accountByIDForTest(t, "spent")
	if got.Enabled {
		t.Fatal("expected the account to be disabled after an overage rejection")
	}
	// SUSPENDED, not BANNED: the credentials are fine, the allowance is not. The
	// operator's next action is to top up or wait, not to re-authenticate.
	if got.BanStatus != "SUSPENDED" {
		t.Fatalf("expected banStatus SUSPENDED, got %q", got.BanStatus)
	}
	if got.BanReason == "" {
		t.Fatal("expected a ban reason explaining the overage limit")
	}
	if got.BanTime == 0 {
		t.Fatal("expected banTime to be stamped")
	}
}

// FetchOverageStatus talks to the upstream and fails in tests. The disable must
// not depend on it: the upstream already delivered its verdict in the 402, and
// the snapshot is only there to explain the reason in the panel.
func TestOverageDisableSurvivesSnapshotFetchFailure(t *testing.T) {
	h := newAutoBuyHandler(t)
	addAccount(t, h, "nosnap", true)

	acc := accountByIDForTest(t, "nosnap")
	h.disableAccountOverage(&acc)

	if accountByIDForTest(t, "nosnap").Enabled {
		t.Fatal("expected the account to be disabled even when the snapshot fetch fails")
	}
}

// The disabled account must leave the pool immediately, so the very next request
// rotates instead of spending a retry attempt on it.
func TestOverageDisabledAccountLeavesThePool(t *testing.T) {
	h := newAutoBuyHandler(t)
	addAccount(t, h, "spent", true)
	addAccount(t, h, "healthy", true)

	acc := accountByIDForTest(t, "spent")
	h.handleAccountFailure(&acc, fmt.Errorf("%s", overageErr))

	for i := 0; i < 5; i++ {
		picked := h.pool.GetNextForModelExcluding("claude-sonnet-4.5", nil)
		if picked == nil {
			t.Fatal("expected the healthy account to remain selectable")
		}
		if picked.ID == "spent" {
			t.Fatal("expected the overage-disabled account to be out of the pool")
		}
	}
}

// A quota (429) rejection is a different failure: it clears on its own, so it
// must still cool down rather than disable. Regression guard for the two
// matchers drifting into each other — isOverageErrorMessage is checked first
// precisely because the overage payload also contains the word "quota".
func TestPlainQuotaErrorDoesNotDisableAccount(t *testing.T) {
	h := newAutoBuyHandler(t)
	addAccount(t, h, "throttled", true)

	acc := accountByIDForTest(t, "throttled")
	h.handleAccountFailure(&acc, fmt.Errorf("HTTP 429 from Kiro IDE: quota exhausted, retry after 30"))

	if !accountByIDForTest(t, "throttled").Enabled {
		t.Fatal("expected a plain quota error to cool down rather than disable")
	}
}
