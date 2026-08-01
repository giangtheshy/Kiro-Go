package proxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// helpers.go — shared small utilities for the proxy package.

// writeJSONOK encodes v as JSON to w with status 200 and returns any encoding error.
func writeJSONOK(w http.ResponseWriter, v interface{}) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(v)
}

// sseHeadersSet configures w for an SSE response and returns true when the
// writer supports flushing. It writes a 500 and returns false otherwise.
func sseHeadersSet(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "streaming unsupported"})
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	return flusher, true
}

// timeNowUnix returns the current wall-clock second. A thin wrapper so
// callers in this package do not import "time" just for this one call.
func timeNowUnix() int64 {
	return time.Now().Unix()
}

// sseWritePing writes a keepalive comment frame to an SSE stream.
func sseWritePing(w http.ResponseWriter, flusher http.Flusher) error {
	_, err := w.Write([]byte(": ping\n\n"))
	if err == nil {
		flusher.Flush()
	}
	return err
}

// sseWriteEvent marshals e as JSON and writes a `data: …\n\n` SSE frame.
func sseWriteEvent(w http.ResponseWriter, flusher http.Flusher, e interface{}) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
	if err == nil {
		flusher.Flush()
	}
	return err
}
