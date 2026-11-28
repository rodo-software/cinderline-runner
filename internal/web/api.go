package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
)

// maxBody is the largest JSON body a write route reads.
const maxBody = 64 << 10

// writes is held around every write route, so two requests never edit
// config.json at once; package-wide, as a moved page gets a new Handler.
var writes sync.Mutex

// configRoutes adds the config's read route and its write routes to mux.
func configRoutes(mux *http.ServeMux, h *Handlers) {
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := manage.Load(h.Paths)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"config":   cfg,
			"web_on":   cfg.WebOn(),
			"web_addr": cfg.WebAddr(),
			"defaults": map[string]any{ // what applies when the config sets none
				"memory":              config.DefaultMemory,
				"max_parallel_runs":   config.Project{}.Parallel(0),
				"transcript_days":     config.DefaultTranscriptDays,
				"transcript_max_size": config.DefaultTranscriptSize,
			},
		})
	})
	writeRoute(mux, "PUT /api/settings/strict-hosts", h, func(b struct {
		On *bool `json:"on"`
	}) error {
		if b.On == nil {
			return manage.Refused{Err: errors.New("on must be true or false")}
		}
		return manage.SetStrictHosts(h.Paths, *b.On)
	})
}

// writeRoute adds a write route to mux: it reads a body of type B, calls
// change with it under the write lock, then reloads the daemon, and
// answers 204, or the first error.
func writeRoute[B any](mux *http.ServeMux, pattern string, h *Handlers, change func(B) error) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var b B
		if err := readBody(w, r, &b); err != nil {
			writeError(w, err)
			return
		}
		writes.Lock()
		defer writes.Unlock()
		err := change(b)
		if err == nil {
			err = h.Reload()
		}
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// readBody decodes the request's one JSON value, of at most maxBody bytes
// and with no field v lacks, into v; anything else is refused.
func readBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if err = dec.Decode(new(json.RawMessage)); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("the body holds more than one JSON value")
		}
	}
	if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
		err = errors.New("the body is larger than 64 KiB")
	}
	if err != nil {
		return manage.Refused{Err: err}
	}
	return nil
}

// writeError answers err as {"error": "..."}: 400 for a refused value, 500
// for anything else.
func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.As(err, new(manage.Refused)) {
		code = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
