package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
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
				"keep_sessions_for":   config.DefaultKeepSessionsFor,
				"min_free_disk":       config.DefaultMinFreeDisk,
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
	writeRoute(mux, "PUT /api/settings/memory", h, func(_ *http.Request, b struct {
		manage.Project
		Value string `json:"value"`
	}) error {
		return manage.SetMemory(h.Paths, b.Project, b.Value)
	})
	writeRoute(mux, "PUT /api/settings/parallel", h, func(_ *http.Request, b struct {
		manage.Project
		Value int `json:"value"`
	}) error {
		return manage.SetParallel(h.Paths, b.Project, b.Value)
	})
	writeRoute(mux, "PUT /api/settings/transcript-days", h, func(_ *http.Request, b struct {
		manage.Project
		Value *int `json:"value"`
	}) error {
		return manage.SetTranscriptDays(h.Paths, b.Project, b.Value)
	})
	writeRoute(mux, "PUT /api/settings/transcript-max-size", h, func(_ *http.Request, b struct {
		Value string `json:"value"`
	}) error {
		return manage.SetTranscriptMaxSize(h.Paths, b.Value)
	})
	mux.HandleFunc("PUT /api/settings/web", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			On   *bool `json:"on"`
			Port *int  `json:"port"`
		}
		if err := readBody(w, r, &b); err != nil {
			writeError(w, err)
			return
		}
		writes.Lock()
		defer writes.Unlock()
		if err := manage.SetWeb(h.Paths, b.On, b.Port); err != nil {
			writeError(w, err)
			return
		}
		cfg, err := manage.Load(h.Paths)
		if err != nil {
			writeError(w, err)
			return
		}
		// The reload closes this connection when the page moves or goes off,
		// so the whole answer goes out first, with its length, and the
		// connection is not kept for another request; a reload that fails
		// after it is the daemon's to log.
		body, _ := json.Marshal(map[string]any{"web_on": cfg.WebOn(), "web_addr": cfg.WebAddr()})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
		http.NewResponseController(w).Flush()
		h.Reload()
	})
	// Adds a project as `add` does; no answer holds its tokens.
	writeRoute(mux, "POST /api/projects", h, func(r *http.Request, b manage.NewProject) error {
		_, _, err := manage.AddProject(r.Context(), h.Paths, h.Secrets, h.Version, b)
		return err
	})
	// Removing and renaming take the project's key in {key}, not its ID, and
	// url and workspace for a key that several share, as the CLI's -url and
	// -workspace; neither can be undone, so the server, not only the page,
	// checks that the key was typed (decisions 2026-11-26-local-web-page and
	// 2026-11-28-web-remove-project).
	type confirmBody struct {
		Confirm   string `json:"confirm"`
		URL       string `json:"url"`
		Workspace string `json:"workspace"`
	}
	confirmed := func(r *http.Request, b confirmBody) (manage.Project, error) {
		if b.Confirm != r.PathValue("key") {
			return manage.Project{}, manage.Refused{Err: errors.New("type the project's key to confirm")}
		}
		return manage.Project{Key: r.PathValue("key"), URL: b.URL, Workspace: b.Workspace}, nil
	}
	writeRouteThen(mux, "DELETE /api/projects/{key}", h, func(r *http.Request, b confirmBody) (func(), error) {
		which, err := confirmed(r, b)
		if err != nil {
			return nil, err
		}
		p, err := manage.RemoveProject(h.Paths, h.Secrets, which, h.warn(which.Key))
		if err != nil {
			return nil, err
		}
		return func() { h.dropWhenEnded(p) }, nil
	})
	writeRouteThen(mux, "POST /api/projects/{key}/rename", h, func(r *http.Request, b struct {
		confirmBody
		NewKey string `json:"new_key"`
	}) (func(), error) {
		which, err := confirmed(r, b.confirmBody)
		if err != nil {
			return nil, err
		}
		status := func() (localapi.Status, error) { return h.Status(), nil }
		old, p, err := manage.RenameProject(r.Context(), h.Paths, h.Secrets, h.Exec, h.Version, status, which, b.NewKey, h.warn(which.Key))
		if err != nil {
			return nil, err
		}
		if old.ID() == p.ID() {
			return nil, errUnchanged
		}
		return func() { h.dropWhenEnded(old) }, nil
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
	// project is the project's ID, or "" for runner-wide allow_hosts.
	type hostBody struct {
		Project string `json:"project"`
		Host    string `json:"host"`
	}
	writeRoute(mux, "POST /api/hosts", h, func(_ *http.Request, b hostBody) error {
		return manage.AddHost(h.Paths, b.Project, b.Host)
	})
	writeRoute(mux, "DELETE /api/hosts", h, func(_ *http.Request, b hostBody) error {
		return manage.RemoveHost(h.Paths, b.Project, b.Host)
	})
	// project is the project's ID, or "" for runner-wide allow_skills.
	type skillBody struct {
		Project string `json:"project"`
		Name    string `json:"name"`
	}
	writeRoute(mux, "POST /api/skills", h, func(_ *http.Request, b struct {
		skillBody
		URL string `json:"url"`
	}) error {
		return manage.AddSkill(h.Paths, b.Project, b.Name, b.URL)
	})
	writeRoute(mux, "DELETE /api/skills", h, func(_ *http.Request, b skillBody) error {
		return manage.RemoveSkill(h.Paths, b.Project, b.Name)
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

// ID shadows the config's id: it is the project's ID even where the
// config sets none, what the routes' {key} takes; Label is how status
// names it (config.Project.ID and Label).
type projectJSON struct {
	config.Project
	ID                string     `json:"id"`
	Label             string     `json:"label"`
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
		out.Projects[i].Project, out.Projects[i].ID, out.Projects[i].Label = p, p.ID(), p.Label()
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
	writeRouteThen(mux, pattern, h, func(r *http.Request, b B) (func(), error) { return nil, change(r, b) })
}

// writeRouteThen is writeRoute for a change that also returns what to do
// once the daemon has reloaded, or nil.
func writeRouteThen[B any](mux *http.ServeMux, pattern string, h *Handlers, change func(*http.Request, B) (func(), error)) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var b B
		if err := readBody(w, r, &b); err != nil {
			writeError(w, err)
			return
		}
		writes.Lock()
		defer writes.Unlock()
		then, err := change(r, b)
		if errors.Is(err, errUnchanged) {
			err = nil
		} else if err == nil {
			err = h.Reload()
			if err == nil && then != nil {
				then()
			}
		}
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// warn logs what fails once a change to project is saved.
func (h *Handlers) warn(project string) func(error) {
	return func(err error) { h.Log.Warn("not all removed", "project", project, "err", err) }
}

// dropWhenEnded removes p's Docker volumes, session notes and outbox
// (manage.DropLocal) once the daemon's loop of p has ended, as `remove`
// does after its wait, in the background: the request does not wait for
// runs that may take hours (decision 2026-11-28-web-remove-project). When
// the daemon stops first they stay; the log line says how to find them.
func (h *Handlers) dropWhenEnded(p config.Project) {
	h.Log.Info("removing the project's Docker volumes, session notes and outbox once its runs have ended; if the runner stops first, list its volumes with docker volume ls --filter",
		"project", p.Key, "filter", "label="+workspace.ProjectLabel(p.ID()))
	go func() {
		for slices.ContainsFunc(h.Status().Projects, func(s localapi.ProjectStatus) bool { return s.Project == p.Label() && s.Draining }) {
			time.Sleep(h.Tick)
		}
		writes.Lock() // so no write adds a project with p's ID meanwhile
		defer writes.Unlock()
		manage.DropLocal(context.Background(), h.Paths, h.Exec, p, h.warn(p.Key))
		h.Log.Info("removed the project's Docker volumes, session notes and outbox", "project", p.Key)
	}()
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

// writeError answers err as {"error": "..."}: 400 for a refused value, 404
// for a project the config does not have, 500 for anything else. A
// refusal wins: a route that names its project in the body answers 400.
func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, new(manage.Refused)):
		code = http.StatusBadRequest
	case errors.As(err, new(manage.NotFound)):
		code = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
