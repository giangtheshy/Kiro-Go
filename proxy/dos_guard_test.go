package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testGuard(cfg dosGuardConfig) *dosGuard {
	// Disable the janitor goroutine in tests by zeroing IPRPM only where unwanted;
	// callers that need IP limiting pass IPRPM>0 and accept the background ticker
	// (it fires every 5 minutes, harmless within a test run).
	return newDosGuard(cfg)
}

func TestAllowIPDisabledWhenZero(t *testing.T) {
	g := testGuard(dosGuardConfig{IPRPM: 0})
	for i := 0; i < 1000; i++ {
		if !g.allowIP("1.2.3.4") {
			t.Fatalf("IPRPM=0 must never reject (request %d)", i)
		}
	}
}

func TestAllowIPBurstThenReject(t *testing.T) {
	g := testGuard(dosGuardConfig{IPRPM: 5})
	ip := "9.9.9.9"
	// Bucket starts full (capacity 5): first 5 immediate.
	for i := 0; i < 5; i++ {
		if !g.allowIP(ip) {
			t.Fatalf("burst request %d should be allowed", i)
		}
	}
	// 6th in the same instant has no tokens left → reject.
	if g.allowIP(ip) {
		t.Fatalf("request beyond burst should be rejected")
	}
}

func TestAllowIPIndependentPerIP(t *testing.T) {
	g := testGuard(dosGuardConfig{IPRPM: 1})
	g.allowIP("a") // drains A's single token
	if g.allowIP("a") {
		t.Fatalf("A should be rejected after draining its token")
	}
	if !g.allowIP("b") {
		t.Fatalf("B is independent and should be allowed")
	}
}

func TestAcquireGlobalCap(t *testing.T) {
	g := testGuard(dosGuardConfig{MaxConcurrent: 2})
	r1, ok1 := g.acquireGlobal()
	r2, ok2 := g.acquireGlobal()
	if !ok1 || !ok2 {
		t.Fatalf("first two slots should be granted")
	}
	if _, ok3 := g.acquireGlobal(); ok3 {
		t.Fatalf("third slot should be refused at capacity 2")
	}
	// Releasing one frees a slot.
	r1()
	r3, ok := g.acquireGlobal()
	if !ok {
		t.Fatalf("slot should be available after release")
	}
	// release is idempotent: calling r1 again must not free an extra slot.
	r1()
	r2()
	r3()
}

func TestAcquireGlobalDisabled(t *testing.T) {
	g := testGuard(dosGuardConfig{MaxConcurrent: 0})
	for i := 0; i < 1000; i++ {
		if _, ok := g.acquireGlobal(); !ok {
			t.Fatalf("MaxConcurrent=0 must always admit (request %d)", i)
		}
	}
}

func TestEnterKeyWaitCap(t *testing.T) {
	g := testGuard(dosGuardConfig{KeyMaxWaiters: 2})
	if !g.enterKeyWait("k") || !g.enterKeyWait("k") {
		t.Fatalf("first two waiters should be admitted")
	}
	if g.enterKeyWait("k") {
		t.Fatalf("third concurrent waiter should be rejected at cap 2")
	}
	g.leaveKeyWait("k") // frees one slot
	if !g.enterKeyWait("k") {
		t.Fatalf("a slot should be free after leaveKeyWait")
	}
	// Different key is independent.
	if !g.enterKeyWait("other") {
		t.Fatalf("independent key should not be capped by k")
	}
}

func TestEnterKeyWaitDisabled(t *testing.T) {
	g := testGuard(dosGuardConfig{KeyMaxWaiters: 0})
	for i := 0; i < 100; i++ {
		if !g.enterKeyWait("k") {
			t.Fatalf("KeyMaxWaiters=0 must never cap (request %d)", i)
		}
	}
}

func TestLeaveKeyWaitCleansUpMap(t *testing.T) {
	g := testGuard(dosGuardConfig{KeyMaxWaiters: 4})
	g.enterKeyWait("k")
	g.leaveKeyWait("k")
	g.keyMu.Lock()
	_, present := g.keyWaiters["k"]
	g.keyMu.Unlock()
	if present {
		t.Fatalf("key entry should be deleted once its waiter count hits zero")
	}
}

func TestClientIPUntrustedIgnoresHeaders(t *testing.T) {
	g := testGuard(dosGuardConfig{TrustProxy: false})
	r := &http.Request{
		RemoteAddr: "203.0.113.7:54321",
		Header:     http.Header{},
	}
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	r.Header.Set("X-Real-IP", "2.2.2.2")
	if got := g.clientIP(r); got != "203.0.113.7" {
		t.Fatalf("untrusted mode must use RemoteAddr host, got %q", got)
	}
}

func TestClientIPTrustedUsesRightmostXFF(t *testing.T) {
	// One trusted proxy in front (default hops=1). The attacker spoofs a left-most
	// entry; our proxy APPENDS the real connecting IP on the right. clientIP must
	// return the appended (rightmost) IP, never the spoofed left-most one.
	g := testGuard(dosGuardConfig{TrustProxy: true})
	r := &http.Request{
		RemoteAddr: "10.0.0.1:443",
		Header:     http.Header{},
	}
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 198.51.100.2")
	if got := g.clientIP(r); got != "198.51.100.2" {
		t.Fatalf("trusted mode must use right-most (proxy-appended) XFF, got %q", got)
	}
}

func TestClientIPTrustedIgnoresSpoofedLeftmost(t *testing.T) {
	// A lone spoofed entry with a single trusted hop resolves to that entry (idx 0),
	// but adding the proxy-appended real IP must shift resolution to the real IP.
	g := testGuard(dosGuardConfig{TrustProxy: true})
	r := &http.Request{RemoteAddr: "10.0.0.1:443", Header: http.Header{}}
	r.Header.Set("X-Forwarded-For", "attacker-spoof, 203.0.113.9")
	if got := g.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("spoofed left-most must be ignored, got %q", got)
	}
}

func TestClientIPTrustedMultiHop(t *testing.T) {
	// Two trusted proxies in front: the real client is 2 entries from the right.
	g := testGuard(dosGuardConfig{TrustProxy: true, TrustedHops: 2})
	r := &http.Request{RemoteAddr: "10.0.0.1:443", Header: http.Header{}}
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.5, 10.0.0.2")
	if got := g.clientIP(r); got != "203.0.113.5" {
		t.Fatalf("2-hop chain must use the entry 2 from the right, got %q", got)
	}
}

func TestClientIPTrustedFallsBackToRealIP(t *testing.T) {
	g := testGuard(dosGuardConfig{TrustProxy: true})
	r := &http.Request{
		RemoteAddr: "10.0.0.1:443",
		Header:     http.Header{},
	}
	r.Header.Set("X-Real-IP", "2.2.2.2")
	if got := g.clientIP(r); got != "2.2.2.2" {
		t.Fatalf("trusted mode should fall back to X-Real-IP, got %q", got)
	}
}

func TestIsGuardedAPIPath(t *testing.T) {
	guarded := []string{"/v1/messages", "/messages", "/v1/chat/completions", "/v1/responses", "/v1/stats"}
	for _, p := range guarded {
		if !isGuardedAPIPath(p) {
			t.Fatalf("%s should be guarded", p)
		}
	}
	notGuarded := []string{"/admin", "/admin/api/accounts", "/health", "/", "/v1/models"}
	for _, p := range notGuarded {
		if isGuardedAPIPath(p) {
			t.Fatalf("%s should NOT be guarded", p)
		}
	}
}

// TestBug2GlobalSlotHeldDuringThrottleSleep is the Bug 2 exploration test.
//
// Property 3 (Bug Condition): the global concurrency slot must be acquired only AFTER
// authentication and the RPM throttle wait complete — a request that is merely being
// RPM-delayed must NOT pin one of the KIRO_MAX_CONCURRENT global slots for the whole
// throttle sleep, because that lets a handful of throttled keys starve unrelated clients
// with 503 overloaded_error (a self-inflicted DoS).
//
// isBugCondition_2(req): authenticated(req) AND rpmThrottleWait(req) > 0 on a guarded path.
//
// On the UNFIXED code `ServeHTTP` (~452) calls acquireGlobal() BEFORE routing to the
// handler, which only then reaches the throttle sleep in authenticate() (proxy/auth.go
// ~127-131). So each throttled request holds its global slot while it sleeps. This test
// saturates a deliberately small global pool with throttled-and-sleeping requests, then
// fires ONE unrelated, non-throttled request and asserts it is NOT rejected with
// 503 overloaded_error.
//
// EXPECTED OUTCOME ON UNFIXED CODE: this test FAILS — the unrelated request receives
// 503 overloaded_error while the other keys are merely throttled. That failure confirms
// the bug. After the Bug 2 fix (task 14.1) this same test must PASS.
//
// Validates: Requirements 2.2
func TestBug2GlobalSlotHeldDuringThrottleSleep(t *testing.T) {
	mustInitConfig(t)

	const maxConcurrent = 4

	// maxConcurrent healthy keys, each with RPMLimit=1. Priming the bucket once (below)
	// leaves it empty, so the request that actually goes through ServeHTTP reserves a
	// negative balance and must SLEEP (wait > 0) inside authenticate.
	keyVals := make([]string, 0, maxConcurrent)
	keyIDs := make([]string, 0, maxConcurrent)
	for i := 0; i < maxConcurrent; i++ {
		val := fmt.Sprintf("sk-throttled-%d", i)
		created, err := config.AddApiKey(config.ApiKeyEntry{
			Name: val, Key: val, Enabled: true, RPMLimit: 1,
		})
		if err != nil {
			t.Fatalf("seed throttled key %d: %v", i, err)
		}
		keyVals = append(keyVals, val)
		keyIDs = append(keyIDs, created.ID)
	}

	// One unrelated, non-throttled key (RPMLimit=0). It is made over-limit so that once
	// it is admitted past the guard it fails fast at auth (429, quota) WITHOUT touching
	// upstream — the only thing under test is that it is NOT 503'd while others sleep.
	unrelated, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "unrelated", Key: "sk-unrelated", Enabled: true, TokenLimit: 1,
	})
	if err != nil {
		t.Fatalf("seed unrelated key: %v", err)
	}
	if err := config.RecordApiKeyUsage(unrelated.ID, 1, 0, "", false); err != nil {
		t.Fatalf("record over-limit usage: %v", err)
	}
	if err := config.SetLimitNoticeMessage("quota exhausted"); err != nil {
		t.Fatalf("set notice message: %v", err)
	}

	requireAuth(t)

	// A pool + fast-failing mock upstream so the throttled goroutines terminate quickly
	// once we release them at the end of the test (they only ever SLEEP during the test).
	// Pin a single preferred endpoint with fallback disabled so getSortedEndpoints uses
	// exactly the one mock endpoint below (avoids indexing the default 3-endpoint list).
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unused", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: upstream.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
		rpmThrottle: newRPMThrottle(),
		guard:       newDosGuard(dosGuardConfig{MaxConcurrent: maxConcurrent, KeyMaxWaiters: 8, IPRPM: 0}),
	}

	// Prime each throttled key so the next reserve (inside authenticate) returns wait > 0.
	for _, id := range keyIDs {
		_ = h.rpmThrottle.reserve(id, 1)
	}

	// Fire maxConcurrent throttled requests. After the Bug 2 fix each request sleeps in the
	// RPM throttle inside authenticate WITHOUT holding a global slot (acquireGlobal now runs
	// after authentication + the throttle wait).
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	var wg sync.WaitGroup
	cancels := make([]context.CancelFunc, 0, maxConcurrent)
	for i := 0; i < maxConcurrent; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+keyVals[i])
		wg.Add(1)
		go func(r *http.Request) {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), r)
		}(req)
	}
	// Guarantee the sleeping requests are released and reaped even if an assertion fails.
	defer func() {
		for _, c := range cancels {
			c()
		}
		wg.Wait()
	}()

	// Wait until all maxConcurrent requests are actually sleeping in the RPM throttle
	// (each has called enterKeyWait inside authenticate). After the Bug 2 fix (task 14.1)
	// acquireGlobal runs AFTER authentication + the throttle wait, so at this point every
	// request is merely throttled and holds NO global slot.
	totalWaiters := func() int {
		h.guard.keyMu.Lock()
		defer h.guard.keyMu.Unlock()
		n := 0
		for _, c := range h.guard.keyWaiters {
			n += c
		}
		return n
	}
	deadline := time.Now().Add(3 * time.Second)
	for totalWaiters() < maxConcurrent {
		if time.Now().After(deadline) {
			t.Fatalf("throttled requests never reached the RPM sleep: waiters=%d slots=%d",
				totalWaiters(), len(h.guard.globalSlots))
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Fixed-behavior precondition: the throttled requests are all sleeping in the RPM
	// throttle, yet the global concurrency pool is EMPTY — the fix (task 14.1) moved
	// acquireGlobal to AFTER authentication + the throttle wait, so a merely RPM-delayed
	// request no longer pins one of the KIRO_MAX_CONCURRENT slots during its sleep. (On
	// the unfixed code this value was maxConcurrent, documenting the bug; after the fix
	// it is 0.)
	if got := len(h.guard.globalSlots); got != 0 {
		t.Fatalf("expected NO global slots held during throttle sleep after the fix, got %d", got)
	}

	// The unrelated, non-throttled client sends one request. Expected (fixed) behavior:
	// it is admitted past the global guard and is NOT rejected with 503 overloaded_error
	// merely because other keys are sleeping in the RPM throttle.
	rec := httptest.NewRecorder()
	ureq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	ureq.Header.Set("Authorization", "Bearer sk-unrelated")
	h.ServeHTTP(rec, ureq)

	if rec.Code == http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "overloaded_error") {
		t.Fatalf("BUG 2 CONFIRMED: unrelated non-throttled request rejected with 503 overloaded_error "+
			"while %d keys were merely RPM-throttled (global slots pinned during the throttle sleep). "+
			"code=%d body=%s", maxConcurrent, rec.Code, rec.Body.String())
	}
}

// TestBug2PreservationGuardInvariantsProperty is the Bug 2 PRESERVATION property test.
//
// Property 4 (Preservation): Guard order, routing, and limits unchanged.
// Validates: Requirements 3.2
//
// The Bug 2 fix (task 14.1) only moves WHERE acquireGlobal() is called inside
// ServeHTTP (after auth + the RPM throttle wait). It does NOT change the dosGuard
// primitives themselves. This property therefore anchors the invariants of those
// primitives — the global concurrency cap, the per-key in-flight cap, the
// 429 + RPM-refund reject path, and exactly-once slot release — that MUST hold
// identically before and after the fix.
//
// It drives many randomized, interleaved lifecycles that mix NON-THROTTLED requests
// (which acquire a global slot immediately — they are already past the throttle wait,
// wait == 0) and THROTTLED requests (which sit in the per-key in-flight cap while
// "sleeping" in the RPM throttle and only acquire a global slot AFTER the wait
// completes). Against a reference model it asserts, at every step:
//
//   - global-slot occupancy never exceeds the number of requests past the throttle
//     wait (a request merely sleeping in the throttle holds a per-key waiter slot but
//     NO global slot — so it never inflates occupancy);
//   - occupancy never exceeds KIRO_MAX_CONCURRENT and acquireGlobal admits iff a slot
//     is free (a genuinely full pool is refused — the 503 overloaded_error path);
//   - a request rejected by the per-key in-flight cap does NOT hold a global slot and
//     its reserved RPM token is refunded (the 429 rate_limit_error path);
//   - the per-key waiter count in the guard exactly matches the model;
//   - release() frees exactly one slot and is idempotent (exactly-once), so no slot
//     leaks once every request completes.
//
// Because it phrases every assertion in terms of PRESERVED (non-throttled / primitive)
// behavior, it PASSES on both the UNFIXED and the FIXED code. It is NOT an exploration
// test — it must never fail on unfixed code (that is task 2's job).
func TestBug2PreservationGuardInvariantsProperty(t *testing.T) {
	const trials = 300
	rng := rand.New(rand.NewSource(0xB2B2B2B2))
	keys := []string{"kA", "kB", "kC"}

	type acquisition struct {
		release  func()
		released bool
	}
	type waitState struct {
		key string
	}

	for trial := 0; trial < trials; trial++ {
		maxConcurrent := 1 + rng.Intn(6) // 1..6
		keyMax := 1 + rng.Intn(4)        // 1..4
		g := newDosGuard(dosGuardConfig{MaxConcurrent: maxConcurrent, KeyMaxWaiters: keyMax, IPRPM: 0})
		rpm := newRPMThrottle()

		held := 0 // reference count of global slots acquired-and-not-released
		modelWaiters := map[string]int{}
		var active []*acquisition // requests past the wait, holding a global slot
		var sleeping []waitState  // throttled requests sleeping in the RPM throttle

		// checkInvariants asserts the model matches the real guard state.
		checkInvariants := func(where string) {
			if got := len(g.globalSlots); got != held {
				t.Fatalf("[trial %d %s] global occupancy %d != model held %d", trial, where, got, held)
			}
			if held > maxConcurrent {
				t.Fatalf("[trial %d %s] occupancy %d exceeds MaxConcurrent %d", trial, where, held, maxConcurrent)
			}
			// Occupancy must never exceed the number of requests past the throttle
			// wait: sleeping (throttled) requests hold no global slot.
			if len(g.globalSlots) > held {
				t.Fatalf("[trial %d %s] a sleeping throttled request inflated occupancy", trial, where)
			}
			// Per-key waiter counts in the guard must match the model exactly.
			g.keyMu.Lock()
			for k, want := range modelWaiters {
				if got := g.keyWaiters[k]; got != want {
					g.keyMu.Unlock()
					t.Fatalf("[trial %d %s] key %q waiters got %d want %d", trial, where, k, got, want)
				}
			}
			for k, got := range g.keyWaiters {
				if modelWaiters[k] != got {
					g.keyMu.Unlock()
					t.Fatalf("[trial %d %s] guard has stray waiter for key %q: %d", trial, where, k, got)
				}
			}
			g.keyMu.Unlock()
		}

		ops := 40 + rng.Intn(60)
		for op := 0; op < ops; op++ {
			switch rng.Intn(4) {
			case 0:
				// A NON-THROTTLED request arrives (wait == 0): it acquires a global
				// slot immediately, i.e. already past the throttle wait.
				r, ok := g.acquireGlobal()
				wantOK := held < maxConcurrent
				if ok != wantOK {
					t.Fatalf("[trial %d] acquireGlobal ok=%v want=%v (held=%d max=%d)", trial, ok, wantOK, held, maxConcurrent)
				}
				if ok {
					held++
					active = append(active, &acquisition{release: r})
				}
				checkInvariants("non-throttled acquire")

			case 1:
				// A THROTTLED request arrives: it reserves an RPM token and enters the
				// per-key in-flight cap BEFORE sleeping. It holds NO global slot while
				// sleeping.
				key := keys[rng.Intn(len(keys))]
				_ = rpm.reserve(key, 60)
				gotEnter := g.enterKeyWait(key)
				wantEnter := modelWaiters[key] < keyMax
				if gotEnter != wantEnter {
					t.Fatalf("[trial %d] enterKeyWait(%q) got=%v want=%v (waiters=%d cap=%d)",
						trial, key, gotEnter, wantEnter, modelWaiters[key], keyMax)
				}
				if !gotEnter {
					// Preserved 429 rate_limit_error path: refund the reserved token so
					// the effective RPM does not drift below the configured limit, and
					// confirm the rejected request holds NO global slot.
					rpm.refund(key, 60)
					checkInvariants("per-key cap reject")
					continue
				}
				modelWaiters[key]++
				sleeping = append(sleeping, waitState{key: key})
				// Sleeping request must not have changed global occupancy.
				checkInvariants("throttled enter wait")

			case 2:
				// A sleeping throttled request finishes its wait: it leaves the per-key
				// cap and only THEN acquires a global slot (post-throttle admission).
				if len(sleeping) == 0 {
					continue
				}
				i := rng.Intn(len(sleeping))
				ws := sleeping[i]
				sleeping = append(sleeping[:i], sleeping[i+1:]...)
				g.leaveKeyWait(ws.key)
				modelWaiters[ws.key]--
				if modelWaiters[ws.key] == 0 {
					delete(modelWaiters, ws.key)
				}
				r, ok := g.acquireGlobal()
				wantOK := held < maxConcurrent
				if ok != wantOK {
					t.Fatalf("[trial %d] post-throttle acquireGlobal ok=%v want=%v", trial, ok, wantOK)
				}
				if ok {
					held++
					active = append(active, &acquisition{release: r})
				}
				checkInvariants("post-throttle acquire")

			case 3:
				// An in-flight request completes: release its slot exactly once. The
				// release closure is guarded by sync.Once, so a stray double-release
				// (exercised randomly) must not free an extra slot.
				if len(active) == 0 {
					continue
				}
				i := rng.Intn(len(active))
				a := active[i]
				active = append(active[:i], active[i+1:]...)
				a.release()
				if !a.released {
					held--
					a.released = true
				}
				if rng.Intn(2) == 0 {
					a.release() // idempotent: must not free an extra slot
				}
				checkInvariants("release")
			}
		}

		// Drain everything and confirm exactly-once release leaves no leaked slots.
		for _, ws := range sleeping {
			g.leaveKeyWait(ws.key)
			modelWaiters[ws.key]--
			if modelWaiters[ws.key] == 0 {
				delete(modelWaiters, ws.key)
			}
		}
		for _, a := range active {
			if !a.released {
				a.release()
				held--
				a.released = true
			}
		}
		if len(g.globalSlots) != 0 {
			t.Fatalf("[trial %d] slot leak: %d slots still held after draining", trial, len(g.globalSlots))
		}
		if held != 0 {
			t.Fatalf("[trial %d] model held=%d after draining, want 0", trial, held)
		}
		g.keyMu.Lock()
		if n := len(g.keyWaiters); n != 0 {
			g.keyMu.Unlock()
			t.Fatalf("[trial %d] %d stray per-key waiters after draining", trial, n)
		}
		g.keyMu.Unlock()
	}
}
