// Package run is one project's work: the loop that polls the app (loop.go),
// the runs that follow the app's steps (run.go), the upkeep of Review
// branches (review.go) and the tidying of kept sessions and clones
// (tidy.go). It keeps no workflow state (decision 0091): session notes and
// the outbox are caches it may lose.
package run

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

var keys = secrets.ClientKeys

// PausedDir is the folder of a project's session notes, one <REF>.json per
// issue, in the host state folder. The URL is part of the name because two
// instances may have projects with the same ID.
func PausedDir(state string, p config.Project) string {
	return filepath.Join(state, "paused", url.QueryEscape(p.URL+" "+p.ID()))
}

// OutboxFile is the file of a project's outbox (cinderline.Client.Outbox), named as PausedDir.
func OutboxFile(state string, p config.Project) string {
	return filepath.Join(state, "outbox", url.QueryEscape(p.URL+" "+p.ID())+".json")
}

// Project is one configured project: what its runs need, and what its loop
// knows now.
type Project struct {
	Config  config.Project
	Secrets *secrets.Secrets
	State   string                  // the host state folder (config.Paths.State)
	Env     environment.Environment // where the project's turns run
	Log     *slog.Logger

	Clients   map[string]client.Client // by agent client name
	Workspace workspace.Workspace
	Forge     func(kind string) (forge.Forge, error)

	// Others are all the projects added on this runner, for the code of linked projects.
	Others []config.Project

	// The local limits on kept sessions, from config's keep_sessions_for
	// and min_free_disk; 0 turns each off.
	KeepFor time.Duration
	MinFree int64

	// Set only by NewProject; nil in tests.
	deleteSession func(ctx context.Context, ref, session string) error                  // a session's files on the state volume
	prepare       func(ctx context.Context, proj cinderline.Project, base string) error // the environment and the tools mount
	fetchExtras   func(ctx context.Context, path string, repos []workspace.Extra) map[string]error
	detect        func(ctx context.Context) []cinderline.ReportClient
	image         string // the agent image the tools mount was filled for; only runs touch it, one at a time

	detectMu sync.Mutex // one detection at a time

	mu      sync.Mutex
	clients []cinderline.ReportClient // the last detection; nil before the first
	refused int                       // connections the egress proxy refused since start
	status  localapi.ProjectStatus
	stopRun context.CancelCauseFunc // ends the agent turn in flight; nil while none runs
	stopRef string                  // the run whose turn stopRun ends
}

// NewProject builds a project's dependencies.
func NewProject(cfg config.Project, sec *secrets.Secrets, state string, env *environment.Docker, log *slog.Logger) (*Project, error) {
	gitToken, _, err := sec.Get(cfg, secrets.GitToken)
	if err != nil {
		return nil, err
	}
	ws := &workspace.Docker{Project: cfg.ID(), Repository: cfg.Repository, Token: gitToken}
	p := &Project{
		Config:    cfg,
		Secrets:   sec,
		State:     state,
		Env:       env,
		Log:       log,
		Clients:   map[string]client.Client{},
		Workspace: ws,
	}
	mcpURL := strings.TrimRight(cfg.URL, "/") + "/mcp"
	var clis []*client.CLI // the real clients, which get the tools mount
	for name, kind := range keys {
		cred, _, err := sec.Get(cfg, kind)
		// Without a keychain key, Claude Code, Codex and Gemini CLI use the
		// login that `login` stored on the state volume.
		if err != nil && !(errors.Is(err, secrets.ErrNotFound) && (name == "claude" || name == "codex" || name == "gemini")) {
			continue
		}
		cl := &client.CLI{Name: name, Project: cfg.ID(), Env: env, Credential: cred, BaseURL: cfg.OpenAIBaseURL, Vertex: cfg.GeminiVertex, MCPURL: mcpURL}
		p.Clients[name], clis = cl, append(clis, cl)
	}
	p.Forge = func(kind string) (forge.Forge, error) {
		return forge.New(kind, cfg.Repository, gitToken)
	}
	p.prepare = func(ctx context.Context, proj cinderline.Project, base string) error {
		var compose *environment.Compose
		// existing is not built yet and keeps the default environment.
		if proj.Environment == "docker" {
			// The file is read from the repo volume's base branch: fetch it first.
			if err := ws.Fetch(ctx); err != nil {
				return err
			}
			compose = &environment.Compose{
				Base:    base,
				File:    cmp.Or(proj.ComposeFile, environment.DefaultComposeFile),
				Service: cmp.Or(proj.AgentService, environment.DefaultAgentService),
			}
		}
		env.SetCompose(compose)
		image, err := env.AgentImage(ctx)
		if err != nil {
			return err
		}
		if image == p.image {
			return nil
		}
		mount, err := (&tools.Tools{State: state}).Mount(ctx, image)
		if err != nil {
			return err
		}
		// Every agent container gets the tools mount's flags; the extra and
		// linked repositories ride along. Links can change between runs, so
		// the linked volume is always mounted; every run fills it first.
		if len(cfg.ExtraRepositories) > 0 {
			mount.Args = append(mount.Args, workspace.ExtraMount(cfg.ID(), workspace.ExtraPath)...)
		}
		mount.Args = append(mount.Args, workspace.ExtraMount(cfg.ID(), workspace.LinkedPath)...)
		// The attachments folder is a bind: the runner downloads to the host.
		mount.Args = append(mount.Args, "-v", attachmentsDir(state, cfg)+":"+workspace.AttachmentsPath+":ro")
		for _, c := range clis {
			c.Tools = mount
		}
		p.image = image
		return nil
	}
	exec := env.Exec
	if exec == nil {
		exec = workspace.RunCommand
	}
	p.fetchExtras = func(ctx context.Context, path string, repos []workspace.Extra) map[string]error {
		return workspace.FetchExtras(ctx, exec, cfg.ID(), path, repos)
	}
	p.deleteSession = func(ctx context.Context, ref, session string) error {
		return client.DeleteSession(ctx, exec, cfg.ID(), ref, session)
	}
	p.detect = func(ctx context.Context) []cinderline.ReportClient {
		image, err := env.Image(ctx)
		if err != nil {
			p.Log.Warn("detection: no image", "project", cfg.Key, "err", err)
			return allMissingBinary()
		}
		mount, err := (&tools.Tools{State: state, Exec: exec}).Mount(ctx, image)
		if err != nil {
			p.Log.Warn("detection: no tools mount", "project", cfg.Key, "err", err)
			return allMissingBinary()
		}
		return detectClients(ctx, exec, cfg, sec, image, mount, p.Log)
	}
	return p, nil
}

// Detect reruns client detection and keeps the result for the report.
func (p *Project) Detect(ctx context.Context) {
	if p.detect == nil {
		return
	}
	p.detectMu.Lock()
	defer p.detectMu.Unlock()
	got := p.detect(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients = got
}

// AddRefused counts one connection the egress proxy refused, for the report.
func (p *Project) AddRefused() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refused++
}

// Status is what the CLI shows of the project.
func (p *Project) Status() localapi.ProjectStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status
	st.Project = p.Config.ID()
	return st
}

func (p *Project) setStatus(change func(*localapi.ProjectStatus)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(&p.status)
}

// stripURL removes user info from a URL string. scp-form git@host:o/r stays
// as it is. A string that is not a valid URL is returned unchanged.
func stripURL(s string) string {
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			u.User = nil
			return u.String()
		}
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptr(s string) *string { return &s }
