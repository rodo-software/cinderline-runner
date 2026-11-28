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
	writeRoute(mux, "POST /api/providers", h, func(_ *http.Request, b struct {
		config.Provider
		Key string `json:"key"`
	}) error {
		_, err := manage.AddProvider(h.Paths, h.Secrets, b.Provider, b.Key)
		return err
	})
	writeRoute(mux, "PUT /api/providers/{name}", h, func(r *http.Request, b struct {
		Free      *bool `json:"free"`
		MaxAgents *int  `json:"max_agents"`
		Lookahead *int  `json:"lookahead"`
	}) error {
		changed, err := manage.SetProvider(h.Paths, r.PathValue("name"), b.Free, b.MaxAgents, b.Lookahead)
		if err == nil && !changed {
			return errUnchanged
		}
		return err
	})
	writeRoute(mux, "DELETE /api/providers/{name}", h, func(r *http.Request, _ struct{}) error {
		return manage.RemoveProvider(h.Paths, h.Secrets, r.PathValue("name"))
	})

	// Secrets are write-only: no route answers a value, nor puts one in an
	// error (decision 2026-11-26-local-web-page, Config management).
	mux.HandleFunc("GET /api/projects/{key}/keys", func(w http.ResponseWriter, r *http.Request) {
		p, err := manage.FindProject(h.Paths, r.PathValue("key"))
		var ks []manage.KeyInfo
		if err == nil {
			ks, err = manage.ListKeys(h.Secrets, p)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, ks)
	})
	type keyBody struct {
		Value string `json:"value"`
	}
	writeRoute(mux, "PUT /api/projects/{key}/keys/{kind}", h, func(r *http.Request, b keyBody) error {
		p, err := manage.FindProject(h.Paths, r.PathValue("key"))
		if err != nil {
			return err
		}
		_, err = manage.SetKey(r.Context(), h.Paths, h.Secrets, h.Version, p, r.PathValue("kind"), b.Value)
		return err
	})
	writeRoute(mux, "DELETE /api/projects/{key}/keys/{kind}", h, func(r *http.Request, _ struct{}) error {
		p, err := manage.FindProject(h.Paths, r.PathValue("key"))
		if err != nil {
			return err
		}
		return manage.DeleteKey(h.Paths, h.Secrets, p, r.PathValue("kind"))
	})
	// A provider's key belongs to no project: the zero one.
	writeRoute(mux, "PUT /api/providers/{name}/key", h, func(r *http.Request, b keyBody) error {
		_, err := manage.SetKey(r.Context(), h.Paths, h.Secrets, h.Version, config.Project{}, "provider:"+r.PathValue("name"), b.Value)
		return err
	})
	writeRoute(mux, "DELETE /api/providers/{name}/key", h, func(r *http.Request, _ struct{}) error {
		return manage.DeleteKey(h.Paths, h.Secrets, config.Project{}, "provider:"+r.PathValue("name"))
	})
}

// errUnchanged is a write that changed nothing: answered 204 without a
// reload, which would drain the runs in flight for nothing.
var errUnchanged = errors.New("unchanged")

// configJSON is the config as the page gets it: each extra repository and
// provider says whether its token or key is stored, never the value.
type configJSON struct {
	*config.Config
	Projects  []projectJSON  `json:"projects"`
	Providers []providerJSON `json:"providers"`
}

type providerJSON struct {
	config.Provider
	HasKey bool `json:"has_key"`
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
	out := configJSON{Config: cfg, Projects: make([]projectJSON, len(cfg.Projects)), Providers: make([]providerJSON, len(cfg.Providers))}
	for i, p := range cfg.Providers {
		_, _, err := sec.Get(secrets.Runner, secrets.ProviderKey(p.Name))
		if err != nil && !errors.Is(err, secrets.ErrNotFound) {
			return configJSON{}, err
		}
		out.Providers[i] = providerJSON{p, err == nil}
	}
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
		if errors.Is(err, errUnchanged) {
			err = nil
		} else if err == nil {
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
// and with no field v lacks, into v; anything else is refused. An empty
// body leaves v zero, for a DELETE without one.
func readBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == io.EOF {
		return nil
	}
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
