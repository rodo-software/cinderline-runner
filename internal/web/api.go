package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
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
		c, err := withTokens(cfg, h.Secrets)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]any{
			"config":   c,
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
	writeRoute(mux, "PUT /api/settings/strict-hosts", h, func(_ *http.Request, b struct {
		On *bool `json:"on"`
	}) error {
		if b.On == nil {
			return manage.Refused{Err: errors.New("on must be true or false")}
		}
		return manage.SetStrictHosts(h.Paths, *b.On)
	})
	// {key} is the project's ID, the config's id: unique where a key is not.
	writeRoute(mux, "POST /api/projects/{key}/repos", h, func(r *http.Request, b struct {
		URL   string `json:"url"`
		Ref   string `json:"ref"`
		Token string `json:"token"`
	}) error {
		_, err := manage.AddRepo(r.Context(), h.Log, h.Paths, h.Secrets, h.Exec, r.PathValue("key"), config.ExtraRepository{URL: b.URL, Ref: b.Ref}, b.Token)
		return err
	})
	writeRoute(mux, "DELETE /api/projects/{key}/repos", h, func(r *http.Request, b struct {
		Repo string `json:"repo"`
	}) error {
		_, err := manage.RemoveRepo(h.Paths, h.Secrets, r.PathValue("key"), b.Repo)
		return err
	})
}

// configJSON is the config as the page gets it: each extra repository says
// whether its token is stored, never the token.
type configJSON struct {
	*config.Config
	Projects []projectJSON `json:"projects"`
}

type projectJSON struct {
	config.Project
	ExtraRepositories []repoJSON `json:"extra_repositories,omitempty"`
}

type repoJSON struct {
	config.ExtraRepository
	HasToken bool `json:"has_token"`
}

// withTokens is cfg as configJSON, with what sec holds.
func withTokens(cfg *config.Config, sec *secrets.Secrets) (configJSON, error) {
	out := configJSON{Config: cfg, Projects: make([]projectJSON, len(cfg.Projects))}
	for i, p := range cfg.Projects {
		out.Projects[i].Project = p
		for _, r := range p.ExtraRepositories {
			_, _, err := sec.Get(p, secrets.ExtraGitToken(r.URL))
			if err != nil && !errors.Is(err, secrets.ErrNotFound) {
				return configJSON{}, err
			}
			out.Projects[i].ExtraRepositories = append(out.Projects[i].ExtraRepositories, repoJSON{r, err == nil})
		}
	}
	return out, nil
}

// writeRoute adds a write route to mux: it reads a body of type B, calls
// change with the request and it under the write lock, then reloads the daemon, and
// answers 204, or the first error.
func writeRoute[B any](mux *http.ServeMux, pattern string, h *Handlers, change func(*http.Request, B) error) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var b B
		if err := readBody(w, r, &b); err != nil {
			writeError(w, err)
			return
		}
		writes.Lock()
		defer writes.Unlock()
		err := change(r, b)
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
