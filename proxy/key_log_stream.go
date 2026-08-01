package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"sync"
	"time"
)

// keyLogStreamCap is the ring buffer size per key. A larger value lets a newly
// connected portal subscriber see more history but uses more memory.
const keyLogStreamCap = 200

// keyLogEntry is one request record sent on the per-key SSE stream.
// ClientIP is populated only for admin-facing views; the customer-facing stream
// always receives an entry with ClientIP cleared.
type keyLogEntry struct {
	Time       int64   `json:"time"`
	Status     string  `json:"status"` // "ok" | "error"
	Model      string  `json:"model,omitempty"`
	InTokens   int     `json:"inputTokens"`
	OutTokens  int     `json:"outputTokens"`
	Credits    float64 `json:"credits"`
	DurationMs int64   `json:"durationMs"`
	Error      string  `json:"error,omitempty"`
	ClientIP   string  `json:"clientIp,omitempty"`
}

// keyLogStream is the per-key ring + subscriber set.
type keyLogStream struct {
	mu          sync.Mutex
	ring        []keyLogEntry
	next        int
	full        bool
	subscribers map[chan keyLogEntry]struct{}
}

func newKeyLogStream() *keyLogStream {
	return &keyLogStream{
		ring:        make([]keyLogEntry, keyLogStreamCap),
		subscribers: make(map[chan keyLogEntry]struct{}),
	}
}

// publish appends to the ring and fans out to subscribers non-blocking.
// Slow subscribers drop the entry rather than stalling the calling goroutine.
func (s *keyLogStream) publish(e keyLogEntry) {
	s.mu.Lock()
	if e.Time == 0 {
		e.Time = time.Now().Unix()
	}
	s.ring[s.next] = e
	s.next = (s.next + 1) % keyLogStreamCap
	if !s.full && s.next == 0 {
		s.full = true
	}
	subs := make([]chan keyLogEntry, 0, len(s.subscribers))
	for ch := range s.subscribers {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			// Slow subscriber — drop rather than block.
		}
	}
}

// subscribe returns a buffered channel that receives new entries and a cancel
// function. The cancel must be called when the subscriber disconnects.
func (s *keyLogStream) subscribe() (<-chan keyLogEntry, func()) {
	ch := make(chan keyLogEntry, 64)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.subscribers[ch]; ok {
				delete(s.subscribers, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
	return ch, cancel
}

// history returns up to keyLogStreamCap entries, newest first.
func (s *keyLogStream) history() []keyLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []keyLogEntry
	if s.full {
		// Entries from s.next (oldest) to end, then 0 to s.next-1.
		for i := s.next; i < keyLogStreamCap; i++ {
			if s.ring[i].Time != 0 {
				out = append(out, s.ring[i])
			}
		}
		for i := 0; i < s.next; i++ {
			if s.ring[i].Time != 0 {
				out = append(out, s.ring[i])
			}
		}
	} else {
		for i := 0; i < s.next; i++ {
			out = append(out, s.ring[i])
		}
	}
	// Reverse to newest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// keyLogHub is the collection of per-key streams, keyed by API key ID.
type keyLogHub struct {
	mu      sync.Mutex
	streams map[string]*keyLogStream
}

func newKeyLogHub() *keyLogHub {
	return &keyLogHub{streams: make(map[string]*keyLogStream)}
}

func (h *keyLogHub) streamFor(keyID string) *keyLogStream {
	if keyID == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.streams[keyID]
	if s == nil {
		s = newKeyLogStream()
		h.streams[keyID] = s
	}
	return s
}

// publish delivers an entry to the stream for keyID, creating it on demand.
func (h *keyLogHub) publish(keyID string, e keyLogEntry) {
	if s := h.streamFor(keyID); s != nil {
		s.publish(e)
	}
}

// subscribe returns a channel and cancel for the stream of keyID.
func (h *keyLogHub) subscribe(keyID string) (<-chan keyLogEntry, func()) {
	s := h.streamFor(keyID)
	if s == nil {
		ch := make(chan keyLogEntry)
		close(ch)
		return ch, func() {}
	}
	return s.subscribe()
}

// history returns the ring contents for keyID, newest first.
func (h *keyLogHub) history(keyID string) []keyLogEntry {
	h.mu.Lock()
	s := h.streams[keyID]
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.history()
}

// forget drops the stream for keyID (called when a key is deleted).
func (h *keyLogHub) forget(keyID string) {
	if keyID == "" {
		return
	}
	h.mu.Lock()
	delete(h.streams, keyID)
	h.mu.Unlock()
}

// --- stream ticket store ---

const streamTicketTTL = 30 * time.Second

type streamTicket struct {
	keyID   string
	expires time.Time
}

// streamTicketStore issues and validates short-lived one-use tokens so a
// client can open an EventSource (which cannot set headers) for the per-key
// SSE stream without embedding the API key in the URL (which would appear in
// access logs and browser history).
type streamTicketStore struct {
	mu      sync.Mutex
	tickets map[string]streamTicket
}

func newStreamTicketStore() *streamTicketStore {
	s := &streamTicketStore{tickets: make(map[string]streamTicket)}
	go s.janitor()
	return s
}

func (s *streamTicketStore) issue(keyID string) string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.tickets[token] = streamTicket{keyID: keyID, expires: time.Now().Add(streamTicketTTL)}
	s.mu.Unlock()
	return token
}

// consume validates and deletes the ticket, returning the keyID. Returns ""
// when the ticket is unknown or expired.
func (s *streamTicketStore) consume(token string) string {
	if token == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[token]
	if !ok {
		return ""
	}
	delete(s.tickets, token)
	if time.Now().After(t.expires) {
		return ""
	}
	return t.keyID
}

func (s *streamTicketStore) janitor() {
	ticker := time.NewTicker(streamTicketTTL)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for tok, t := range s.tickets {
			if now.After(t.expires) {
				delete(s.tickets, tok)
			}
		}
		s.mu.Unlock()
	}
}

// --- HTTP handlers ---

// keyLogPingInterval keeps the SSE connection alive through proxies that close
// idle upstreams. It must stay comfortably below the typical 60s idle timeout.
const keyLogPingInterval = 25 * time.Second

// apiKeyStreamTicket handles POST /v1/key/stream-ticket.
//
// An EventSource cannot set request headers, so the customer's API key cannot
// authenticate the stream itself. The client exchanges its key (sent in a normal
// header here) for a single-use, 30-second ticket and puts THAT in the stream
// URL — a query string that leaks into access logs and browser history is then
// worthless to anyone who reads it later.
func (h *Handler) apiKeyStreamTicket(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	provided := extractProvidedKey(r)
	if provided == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}
	entry := config.FindApiKeyByValue(provided)
	if entry == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
		return
	}
	if h.streamTickets == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "streaming unavailable"})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ticket":    h.streamTickets.issue(entry.ID),
		"expiresIn": int(streamTicketTTL / time.Second),
	})
}

// apiKeyLogStream handles GET /v1/key/logs/stream?ticket=… — a live SSE feed of
// the caller's own request log.
func (h *Handler) apiKeyLogStream(w http.ResponseWriter, r *http.Request) {
	if h.streamTickets == nil || h.keyLogHub == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "streaming unavailable"})
		return
	}

	keyID := h.streamTickets.consume(r.URL.Query().Get("ticket"))
	if keyID == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid or expired ticket"})
		return
	}

	flusher, ok := sseHeadersSet(w)
	if !ok {
		return
	}
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	flusher.Flush()

	// Subscribe BEFORE replaying history so an entry recorded between the two
	// is delivered by the live channel rather than dropped.
	ch, cancel := h.keyLogHub.subscribe(keyID)
	defer cancel()

	ping := time.NewTicker(keyLogPingInterval)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, open := <-ch:
			if !open {
				return
			}
			// The customer-facing stream never exposes client IPs: a shared key
			// would otherwise let one holder watch the others' addresses.
			e.ClientIP = ""
			if err := sseWriteEvent(w, flusher, map[string]interface{}{
				"type": "keylog",
				"data": e,
			}); err != nil {
				return
			}
		case <-ping.C:
			if err := sseWritePing(w, flusher); err != nil {
				return
			}
		}
	}
}
