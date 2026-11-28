// Package web is the daemon's local web page (decision
// 2026-11-26-local-web-page): the embedded files of static/, index.html at /,
// and its API under /api/.
package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
)

// Handlers are what the daemon gives the page.
type Handlers struct {
	Status func() localapi.Status
	Tick   time.Duration // how often a stream checks for a change; 0: every second
}

// pingTicks is how many ticks pass between two pings of a stream: 20 s at
// one a second, so a dead connection is noticed.
const pingTicks = 20

// Handler serves the page's files and API on 127.0.0.1:port, to the browser
// that holds the token of stateDir.
func Handler(stateDir string, port int, h Handlers) http.Handler {
	if h.Tick == 0 {
		h.Tick = time.Second
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Status())
	})
	mux.HandleFunc("GET /api/status/stream", func(w http.ResponseWriter, r *http.Request) {
		s := newSSE(w)
		last, _ := json.Marshal(h.Status())
		if s.send(last) != nil {
			return
		}
		t := time.NewTicker(h.Tick)
		defer t.Stop()
		for n := 1; ; n++ {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
			}
			var err error
			if b, _ := json.Marshal(h.Status()); !bytes.Equal(b, last) {
				last = b
				err = s.send(b)
			} else if n%pingTicks == 0 {
				err = s.ping()
			}
			if err != nil {
				return
			}
		}
	})
	return guard(stateDir, port, mux)
}
