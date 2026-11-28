// Package web is the daemon's local web page (decision
// 2026-11-26-local-web-page): the embedded files of static/, index.html at /,
// and its API under /api/.
package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/transcript"
)

// Handlers are what the daemon gives the page.
type Handlers struct {
	Status func() localapi.Status
	Follow func(n int) (tail []byte, lines <-chan []byte, cancel func(), err error) // as localapi.Handlers.Follow
	Tick   time.Duration                                                            // how often a stream checks for a change; 0: every second

	Transcripts transcript.Store
	Config      func() *config.Config // the config now, safe from any goroutine
}

// maxTail is the most lines GET /api/log sends before following: what the
// page keeps.
const maxTail = 5000

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
	mux.HandleFunc("GET /api/log", func(w http.ResponseWriter, r *http.Request) {
		n := 200
		if v := r.URL.Query().Get("tail"); v != "" {
			var err error
			if n, err = strconv.Atoi(v); err != nil || n < 0 || n > maxTail {
				http.Error(w, "tail must be a number of lines from 0 to "+strconv.Itoa(maxTail), http.StatusBadRequest)
				return
			}
		}
		tail, lines, cancel, err := h.Follow(n)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer cancel()
		s := newSSE(w)
		for line := range bytes.Lines(tail) {
			if s.send(bytes.TrimSuffix(line, []byte("\n"))) != nil {
				return
			}
		}
		if s.ping() != nil { // sends the headers when the tail is empty
			return
		}
		t := time.NewTicker(h.Tick * pingTicks)
		defer t.Stop()
		for {
			var err error
			select {
			case <-r.Context().Done():
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				err = s.send(bytes.TrimSuffix(line, []byte("\n")))
			case <-t.C:
				err = s.ping()
			}
			if err != nil {
				return
			}
		}
	})
	runRoutes(mux, &h)
	return guard(stateDir, port, mux)
}
