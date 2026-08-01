package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net"
	"net/http"
	"sync"
	"time"
)

// failWindow is how long a client's failed-attempt counter persists without a
// new miss. A slow drip of guesses spread wider than this never trips auto-ban.
const failWindow = 15 * time.Minute

// failCounter tracks in-memory missed attempts for one IP.
type failCounter struct {
	count int
	last  time.Time
}

// ipBanGate enforces the IP ban list for every request.
//
// Differences from kiro-cli-pool-proxy:
//   - Applied to EVERY path except /health and /status (not just login surfaces),
//     since API-key spammers should also be blocked.
//   - Uses dosGuard.clientIP (real IP through XFF) which is already resolved and
//     stored on the request context before this gate runs.
//   - Loopback addresses are counted but never promoted to a persistent ban.
type ipBanGate struct {
	mu    sync.Mutex
	fails map[string]*failCounter
	stop  chan struct{}
}

// newIPBanGate returns a gate with a background prune goroutine.
func newIPBanGate() *ipBanGate {
	g := &ipBanGate{
		fails: make(map[string]*failCounter),
		stop:  make(chan struct{}),
	}
	go g.janitor()
	return g
}

// close stops the background goroutine.
func (g *ipBanGate) close() {
	select {
	case <-g.stop:
	default:
		close(g.stop)
	}
}

func (g *ipBanGate) janitor() {
	ticker := time.NewTicker(failWindow)
	defer ticker.Stop()
	for {
		select {
		case <-g.stop:
			return
		case now := <-ticker.C:
			g.mu.Lock()
			for ip, fc := range g.fails {
				if now.Sub(fc.last) > failWindow {
					delete(g.fails, ip)
				}
			}
			g.mu.Unlock()
		}
	}
}

// isBanned returns true if ip is on the persistent ban list and the gate is active.
func (g *ipBanGate) isBanned(ip string) bool {
	return config.IsBanned(ip)
}

// reject writes a 403 JSON response to a banned client.
func (g *ipBanGate) reject(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{
		"error": "your address is blocked after repeated failed authentication; contact the administrator",
	})
}

// registerFail increments the failure counter for ip.
//
// When the count reaches GetBanThreshold() and the gate is enabled, the IP is
// automatically promoted to the persistent ban list (persisted to config.json).
// Loopback addresses are tracked but never promoted.
func (g *ipBanGate) registerFail(ip string) {
	if ip == "" {
		return
	}
	threshold := config.GetBanThreshold()
	if threshold <= 0 {
		threshold = config.DefaultBanThreshold
	}

	g.mu.Lock()
	now := time.Now()
	fc := g.fails[ip]
	if fc == nil {
		fc = &failCounter{}
		g.fails[ip] = fc
	}
	if now.Sub(fc.last) > failWindow {
		fc.count = 0
	}
	fc.count++
	fc.last = now
	shouldBan := fc.count >= threshold
	if shouldBan {
		fc.count = 0 // reset so a later re-enable doesn't immediately re-ban
	}
	g.mu.Unlock()

	if !shouldBan || !config.BanEnabled() {
		return
	}
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		return
	}
	if _, err := config.BanIP(ip, config.BanReasonAutoSpam, "", threshold); err != nil {
		logger.Warnf("[IPBan] auto-ban of %s failed: %v", ip, err)
	} else {
		logger.Infof("[IPBan] auto-banned %s after %d failed attempts", ip, threshold)
	}
}

// clearFails resets the in-memory failure counter for ip (on successful auth).
func (g *ipBanGate) clearFails(ip string) {
	if ip == "" {
		return
	}
	g.mu.Lock()
	delete(g.fails, ip)
	g.mu.Unlock()
}

// failView is a single entry in the near-threshold snapshot.
type failView struct {
	IP        string `json:"ip"`
	Count     int    `json:"count"`
	Threshold int    `json:"threshold"`
	LastSeen  int64  `json:"lastSeen"`
}

// nearThresholdSnapshot returns IPs whose in-memory failure count is at least
// half of the threshold, sorted descending by count. Used by the admin anomaly
// view so operators can manually ban before auto-ban fires.
func (g *ipBanGate) nearThresholdSnapshot() []failView {
	threshold := config.GetBanThreshold()
	if threshold <= 0 {
		threshold = config.DefaultBanThreshold
	}
	half := threshold / 2
	if half < 1 {
		half = 1
	}

	now := time.Now()
	g.mu.Lock()
	var out []failView
	for ip, fc := range g.fails {
		if now.Sub(fc.last) > failWindow {
			continue
		}
		if fc.count >= half {
			out = append(out, failView{
				IP:        ip,
				Count:     fc.count,
				Threshold: threshold,
				LastSeen:  fc.last.Unix(),
			})
		}
	}
	g.mu.Unlock()

	// sort descending by count
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Count > out[i].Count {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
