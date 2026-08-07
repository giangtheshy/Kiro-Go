package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// ParsedProxy is one normalized proxy entry after parsing a raw line.
type ParsedProxy struct {
	Raw      string // Original input line
	Scheme   string // Resolved scheme: socks5h | http | https (empty if undetected)
	Host     string // Host without port
	Port     string // Port
	Username string // Proxy username (never serialized)
	Password string // Proxy password (never serialized)
}

// MaskedURL returns the proxy URL with the password obscured for display/logging.
func (p *ParsedProxy) MaskedURL() string {
	if p.Scheme == "" || p.Host == "" {
		return ""
	}
	auth := ""
	if p.Username != "" {
		pw := p.Password
		if pw != "" {
			pw = "***"
		}
		auth = url.QueryEscape(p.Username) + ":" + pw + "@"
	}
	return fmt.Sprintf("%s://%s%s:%s", p.Scheme, auth, p.Host, p.Port)
}

// fullURL builds the usable proxy URL with real credentials.
func (p *ParsedProxy) fullURL(scheme string) string {
	auth := ""
	if p.Username != "" {
		auth = url.QueryEscape(p.Username) + ":" + url.QueryEscape(p.Password) + "@"
	}
	return fmt.Sprintf("%s://%s%s:%s", scheme, auth, p.Host, p.Port)
}

// parseProxyLine normalizes a single raw proxy line into a ParsedProxy.
// Accepted shapes:
//
//	scheme://[user:pass@]host:port
//	host:port
//	host:port:user:pass
//	host:port|user|pass
//	host:port|user:pass
//	user:pass@host:port
//
// When the input carries an explicit scheme it is preserved; otherwise Scheme
// is left empty for later probing.
func parseProxyLine(line string) (*ParsedProxy, error) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return nil, fmt.Errorf("empty line")
	}

	p := &ParsedProxy{Raw: raw}

	// Case 1: explicit URL with scheme.
	if i := strings.Index(raw, "://"); i > 0 {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid URL: %v", err)
		}
		p.Scheme = strings.ToLower(u.Scheme)
		// Normalize socks5 -> socks5h so DNS resolves remotely.
		if p.Scheme == "socks5" {
			p.Scheme = "socks5h"
		}
		p.Host = u.Hostname()
		p.Port = u.Port()
		if u.User != nil {
			p.Username = u.User.Username()
			p.Password, _ = u.User.Password()
		}
		if p.Host == "" || p.Port == "" {
			return nil, fmt.Errorf("missing host or port")
		}
		return p, nil
	}

	// Strip a possible trailing scheme hint is not supported; work on the body.
	body := raw

	// Case 2: user:pass@host:port (no scheme).
	if at := strings.LastIndex(body, "@"); at >= 0 {
		cred := body[:at]
		hostport := body[at+1:]
		host, port, err := splitHostPort(hostport)
		if err != nil {
			return nil, err
		}
		user, pass := splitCred(cred)
		p.Host, p.Port, p.Username, p.Password = host, port, user, pass
		return p, nil
	}

	// Case 3: pipe-separated. host:port | user | pass  OR  host:port | user:pass
	if strings.Contains(body, "|") {
		parts := splitAndTrim(body, "|")
		if len(parts) == 0 {
			return nil, fmt.Errorf("invalid pipe format")
		}
		host, port, err := splitHostPort(parts[0])
		if err != nil {
			return nil, err
		}
		p.Host, p.Port = host, port
		switch len(parts) {
		case 1:
			// host:port only
		case 2:
			p.Username, p.Password = splitCred(parts[1])
		default:
			p.Username = parts[1]
			p.Password = parts[2]
		}
		return p, nil
	}

	// Case 4: colon-separated. host:port  OR  host:port:user:pass
	colonParts := strings.Split(body, ":")
	switch len(colonParts) {
	case 2:
		p.Host, p.Port = colonParts[0], colonParts[1]
	case 4:
		p.Host, p.Port = colonParts[0], colonParts[1]
		p.Username, p.Password = colonParts[2], colonParts[3]
	default:
		return nil, fmt.Errorf("unrecognized format (expected host:port[:user:pass])")
	}
	if p.Host == "" || p.Port == "" {
		return nil, fmt.Errorf("missing host or port")
	}
	return p, nil
}

func splitHostPort(s string) (string, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return "", "", fmt.Errorf("invalid host:port %q: %v", s, err)
	}
	return host, port, nil
}

// splitCred splits "user:pass" into its parts. A missing colon yields user only.
func splitCred(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func splitAndTrim(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		t := strings.TrimSpace(part)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// probeProxyScheme finds which scheme actually works for a parsed proxy by
// making a real request through it. It tries candidate schemes in order and
// returns the first that reaches the probe endpoint.
//
// Browsers cannot open raw sockets to test SOCKS vs HTTP, so this detection
// must happen server-side.
func probeProxyScheme(p *ParsedProxy) (string, error) {
	const probeURL = "https://checkip.amazonaws.com"

	// If the scheme is already explicit, only verify that one.
	var candidates []string
	if p.Scheme != "" {
		candidates = []string{p.Scheme}
	} else {
		candidates = []string{"socks5h", "http", "https"}
	}

	var lastErr error
	for _, scheme := range candidates {
		client := &http.Client{
			Timeout:   8 * time.Second,
			Transport: buildKiroTransport(p.fullURL(scheme)),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", probeURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		cancel()
		// Any HTTP response means the tunnel works.
		return scheme, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no working scheme")
}

// ImportProxiesResult is the per-proxy outcome returned to the caller.
type ImportProxiesResult struct {
	Raw          string `json:"raw"`
	MaskedURL    string `json:"maskedUrl,omitempty"`
	Scheme       string `json:"scheme,omitempty"`
	AssignedID   string `json:"assignedId,omitempty"`
	AssignedEmail string `json:"assignedEmail,omitempty"`
	Reachable    bool   `json:"reachable"`
	Assigned     bool   `json:"assigned"`
	Tested       bool   `json:"tested"`
	TestPassed   bool   `json:"testPassed"`
	AddedToPool  bool   `json:"addedToPool,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ImportAndAssignProxies parses raw proxy lines, probes each one's working
// scheme, assigns them round-robin to the given accounts, and optionally tests
// each assigned account end-to-end through its new proxy.
//
// dryRun parses and probes but does not persist or assign.
func (h *Handler) ImportAndAssignProxies(rawText string, targetIDs []string, autoTest, dryRun bool) []ImportProxiesResult {
	lines := strings.Split(rawText, "\n")

	// Resolve target accounts in a stable order.
	all := config.GetAccounts()
	byID := make(map[string]config.Account, len(all))
	for _, a := range all {
		byID[a.ID] = a
	}
	var targets []config.Account
	if len(targetIDs) > 0 {
		for _, id := range targetIDs {
			if a, ok := byID[id]; ok {
				targets = append(targets, a)
			}
		}
	} else {
		// Default: all enabled accounts, in config order.
		for _, a := range all {
			if a.Enabled {
				targets = append(targets, a)
			}
		}
	}

	var results []ImportProxiesResult
	assignIdx := 0

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		res := ImportProxiesResult{Raw: strings.TrimSpace(line)}

		p, err := parseProxyLine(line)
		if err != nil {
			res.Error = "parse: " + err.Error()
			results = append(results, res)
			continue
		}

		scheme, perr := probeProxyScheme(p)
		if perr != nil {
			res.Error = "unreachable: " + perr.Error()
			res.MaskedURL = p.MaskedURL()
			results = append(results, res)
			continue
		}
		p.Scheme = scheme
		res.Scheme = scheme
		res.Reachable = true
		res.MaskedURL = p.MaskedURL()

		if dryRun || len(targets) == 0 {
			results = append(results, res)
			continue
		}

		// Round-robin assignment.
		target := targets[assignIdx%len(targets)]
		assignIdx++
		res.AssignedID = target.ID
		res.AssignedEmail = target.Email

		fullURL := p.fullURL(scheme)
		updated := target
		updated.ProxyURL = fullURL
		if err := config.UpdateAccount(target.ID, updated); err != nil {
			res.Error = "save: " + err.Error()
			results = append(results, res)
			continue
		}
		res.Assigned = true
		h.pool.Reload()

		if autoTest {
			res.Tested = true
			if err := h.testAccountThroughProxy(&updated); err != nil {
				res.Error = "test: " + err.Error()
			} else {
				res.TestPassed = true
			}
		}

		results = append(results, res)
	}

	logger.Infof("[ProxyImport] processed %d lines, assigned %d (dryRun=%v autoTest=%v)",
		len(results), assignIdx, dryRun, autoTest)
	return results
}

// ImportProxiesToPool parses raw proxy lines, probes each one's working scheme,
// and adds reachable proxies to the shared proxy pool instead of assigning them
// to accounts. Deduplication and persistence are handled by config.AddProxyToPool.
//
// dryRun parses and probes but does not persist to the pool.
func (h *Handler) ImportProxiesToPool(rawText string, dryRun bool) []ImportProxiesResult {
	lines := strings.Split(rawText, "\n")

	var results []ImportProxiesResult
	added := 0

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		res := ImportProxiesResult{Raw: strings.TrimSpace(line)}

		p, err := parseProxyLine(line)
		if err != nil {
			res.Error = "parse: " + err.Error()
			results = append(results, res)
			continue
		}

		scheme, perr := probeProxyScheme(p)
		if perr != nil {
			res.Error = "unreachable: " + perr.Error()
			res.MaskedURL = p.MaskedURL()
			results = append(results, res)
			continue
		}
		p.Scheme = scheme
		res.Scheme = scheme
		res.Reachable = true
		res.MaskedURL = p.MaskedURL()

		if dryRun {
			results = append(results, res)
			continue
		}

		// 加入共享代理池（AddProxyToPool 内部去重并持久化）。
		if err := config.AddProxyToPool(p.fullURL(scheme)); err != nil {
			res.Error = "pool: " + err.Error()
			results = append(results, res)
			continue
		}
		res.AddedToPool = true
		added++

		results = append(results, res)
	}

	logger.Infof("[ProxyImport] processed %d lines, addedToPool %d (dryRun=%v)",
		len(results), added, dryRun)
	return results
}

// testAccountThroughProxy sends a minimal real request through the account's
// configured proxy, mirroring apiTestAccount but without HTTP plumbing.
func (h *Handler) testAccountThroughProxy(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	openaiReq := &OpenAIRequest{
		Model:     "claude-sonnet-4",
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, false)
	callback := &KiroStreamCallback{
		OnText:         func(string, bool) {},
		OnToolUse:      func(KiroToolUse) {},
		OnComplete:     func(int, int) {},
		OnCredits:      func(float64) {},
		OnContextUsage: func(float64) {},
	}
	return CallKiroAPI(account, kiroPayload, callback)
}

// apiImportProxies handles POST /admin/api/proxy/import.
//
// Body:
//
//	{
//	  "proxies": "raw text, one proxy per line",
//	  "accountIds": ["id1", "id2"],   // optional; defaults to all enabled accounts
//	  "autoTest": true,                // optional; run an end-to-end test per assigned account
//	  "dryRun": false                  // optional; parse + probe only, do not assign/persist
//	}
//
// Each proxy line is parsed, its working scheme is probed server-side
// (socks5h/http/https), then proxies are assigned round-robin to the target
// accounts. The response is a per-line report.
func (h *Handler) apiImportProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxies    string   `json:"proxies"`
		AccountIDs []string `json:"accountIds"`
		AutoTest   bool     `json:"autoTest"`
		DryRun     bool     `json:"dryRun"`
		Target     string   `json:"target"` // "accounts" (default) | "pool"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Proxies) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxies is required"})
		return
	}

	var results []ImportProxiesResult
	if req.Target == "pool" {
		results = h.ImportProxiesToPool(req.Proxies, req.DryRun)
	} else {
		results = h.ImportAndAssignProxies(req.Proxies, req.AccountIDs, req.AutoTest, req.DryRun)
	}

	assigned, reachable, testPassed, addedToPool := 0, 0, 0, 0
	for _, r := range results {
		if r.Reachable {
			reachable++
		}
		if r.Assigned {
			assigned++
		}
		if r.TestPassed {
			testPassed++
		}
		if r.AddedToPool {
			addedToPool++
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"total":       len(results),
		"reachable":   reachable,
		"assigned":    assigned,
		"testPassed":  testPassed,
		"addedToPool": addedToPool,
		"results":     results,
	})
}
