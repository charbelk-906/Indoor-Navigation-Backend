package main

// Live web demo for presentations.
//
//   GET  /demo         the web page (works on iPhone and Android, no install)
//   GET  /live/state   what the presenter's phone currently sees (read by the page every second)
//   POST /live/update  the presenter's phone sends what it sees (needs header X-Demo-Key)
//
// The presenter key is set in the run configuration as the environment variable DEMO_KEY.
// Without DEMO_KEY, /live/update is switched off (the page still works in Explore mode).

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

//go:embed demo.html
var demoPage []byte

type liveSnapshot struct {
	mu   sync.RWMutex
	data json.RawMessage
	at   time.Time
}

var liveState liveSnapshot

func registerLiveDemo(mux *http.ServeMux) {
	mux.HandleFunc("/demo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(demoPage)
	})

	mux.HandleFunc("/live/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		liveState.mu.RLock()
		data, at := liveState.data, liveState.at
		liveState.mu.RUnlock()

		out := map[string]any{"online": false, "age_ms": nil, "data": nil}
		if data != nil {
			age := time.Since(at)
			out["online"] = age < 8*time.Second
			out["age_ms"] = age.Milliseconds()
			out["data"] = data
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("/live/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key := os.Getenv("DEMO_KEY")
		if key == "" {
			http.Error(w, "Live demo is switched off: set DEMO_KEY on the server", http.StatusServiceUnavailable)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Demo-Key")), []byte(key)) != 1 {
			http.Error(w, "Wrong presenter key", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
		if err != nil || !json.Valid(body) || len(body) == 0 || body[0] != '{' {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		liveState.mu.Lock()
		liveState.data = json.RawMessage(body)
		liveState.at = time.Now()
		liveState.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}
