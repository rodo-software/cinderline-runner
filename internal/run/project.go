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
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"slices"
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
	Config config.Project
	// Providers are the runner's model providers, the same for every project
	// (decision 2026-11-17-local-model-providers).
	Providers []config.Provider
	Secrets   *secrets.Secrets
	State     string                  // the host state folder (config.Paths.State)
	Env       environment.Environment // where the project's turns run
	Log       *slog.Logger
	// Models are the providers' models, fetched once for the runner; nil: none.
	Models *Models

	Clients   map[string]client.Client // by agent client name
	Workspace workspace.Workspace
	Forge     func(kind string) (forge.Forge, error)

	// Others are all the projects added on this runner, for the code of linked projects.
	Others []config.Project

	// The local limits on kept sessions, from config's keep_sessions_for
	// and min_free_disk; 0 turns each off.
	KeepFor time.Duration
	MinFree int64

	// StrictHosts is config's strict_hosts: a step's allow_hosts are ignored.
	StrictHosts bool

	// Budget is the runner's memory budget, which every project's runs
	// share (decision 2026-11-24-agent-memory-limit); nil: none.
	Budget *Budget

	// Set only by NewProject; nil in tests.
	deleteSession func(ctx context.Context, ref, session string) error                                      // a session's files on the state volume
	prepare       func(ctx context.Context, proj cinderline.Project, base string) (memory int64, err error) // the environment and the tools mount; memory as environment.Docker.AgentImage
	fetchExtras   func(ctx context.Context, path string, repos []workspace.Extra) map[string]error
	detect        func(ctx context.Context) (clients []cinderline.ReportClient, openRouter bool)
	image         string // the agent image the tools mount was filled for; only runs touch it, one at a time
	// reviewChecks are, by Review issue ref, the failed checks of its pull
	// request's head, read once per head; only upkeep passes touch it, one at a time.
	reviewChecks map[string]headChecks

	detectMu sync.Mutex    // one detection at a time
	detected chan struct{} // closed by the first detection, so the loop polls at once; nil in tests
	prepMu   sync.Mutex    // one run prepares at a time: the team's and the manager's share the environment, the bare repository and the extra volumes

	mu       sync.Mutex
	clients  []cinderline.ReportClient // the last detection; nil before the first
	refused  int                       // connections the egress proxy refused since start
	hosts    map[string][]string       // by run's issue ref: the host:port pairs it refused since takeRefused, once each
	status   localapi.ProjectStatus
	stopRuns map[string]context.CancelCauseFunc // by run's issue ref: ends its step in flight
	// memory is a run's share of the budget as the last run's prepare
	// measured it; 0: none measured yet, so the project's memory setting.
	memory int64

	openRouter bool // the last detection found OpenCode's OpenRouter key usable; guarded by mu
}

// NewProject builds a project's dependencies; providers are the runner's
// model providers, which every project's OpenCode agents can use.
func NewProject(cfg config.Project, providers []config.Provider, sec *secrets.Secrets, state string, env *environment.Docker, log *slog.Logger) (*Project, error) {
	gitToken, _, err := sec.Get(cfg, secrets.GitToken)
	if err != nil {
		return nil, err
	}
	// One Exec for all the project's docker and git commands, set here.
	exec := workspace.OnceVolumes(env.Exec)
	ws := &workspace.Docker{Project: cfg.ID(), Repository: cfg.Repository, Token: gitToken, Exec: exec}
	p := &Project{
		detected:  make(chan struct{}),
		Config:    cfg,
		Providers: providers,
		Secrets:   sec,
		State:     state,
		Env:       env,
		Log:       log,
		Clients:   map[string]client.Client{},
		Workspace: ws,
	}
	mcpURL := strings.TrimRight(cfg.URL, "/") + "/mcp"
	clientProviders, err := modelProviders(providers, sec)
	if err != nil {
		return nil, err
	}
	var clis []*client.CLI // the real clients, which get the tools mount
	for name, kind := range keys {
		cred, _, err := sec.Get(cfg, kind)
		// Without a keychain key, Claude Code, Codex and Gemini CLI use the
		// login that `login` stored on the state volume, and OpenCode the
		// runner's providers.
		if err != nil && !(errors.Is(err, secrets.ErrNotFound) && (name == "claude" || name == "codex" || name == "gemini" || name == "opencode" && len(providers) > 0)) {
			continue
		}
		cl := &client.CLI{Name: name, Project: cfg.ID(), Env: env, Exec: exec, Credential: cred, BaseURL: cfg.OpenAIBaseURL, Vertex: cfg.GeminiVertex, MCPURL: mcpURL}
		if name == "opencode" {
			cl.Providers = clientProviders
		}
		p.Clients[name], clis = cl, append(clis, cl)
	}
	p.Forge = func(kind string) (forge.Forge, error) {
		return forge.New(kind, cfg.Repository, gitToken)
	}
	p.prepare = func(ctx context.Context, proj cinderline.Project, base string) (int64, error) {
		var compose *environment.Compose
		// Any environment other than docker is the default one.
		if proj.Environment == "docker" {
			// The file is read from the repo volume's base branch: fetch it first.
			if err := ws.Fetch(ctx); err != nil {
				return 0, err
			}
			compose = &environment.Compose{
				Base:    base,
				File:    cmp.Or(proj.ComposeFile, environment.DefaultComposeFile),
				Service: cmp.Or(proj.AgentService, environment.DefaultAgentService),
			}
		}
		env.SetCompose(compose)
		image, memory, err := env.AgentImage(ctx)
		if err != nil {
			return 0, err
		}
		if image == p.image {
			return memory, nil
		}
		mount, err := (&tools.Tools{State: state, Exec: exec}).Mount(ctx, image)
		if err != nil {
			return 0, err
		}
		// Every agent container gets the tools mount's flags; the extra and
		// linked repositories ride along. Links can change between runs, so
		// the linked volume is always mounted; every run fills it first.
		if len(cfg.ExtraRepositories) > 0 {
			mount.Args = append(mount.Args, workspace.ExtraMount(cfg.ID(), workspace.ExtraPath)...)
		}
		mount.Args = append(mount.Args, workspace.ExtraMount(cfg.ID(), workspace.LinkedPath)...)
		// Attachments are a bind per run, added by the client when a turn has
		// them: the runner downloads to the host.
		for _, c := range clis {
			c.Tools = mount
		}
		p.image = image
		return memory, nil
	}
	p.fetchExtras = func(ctx context.Context, path string, repos []workspace.Extra) map[string]error {
		return workspace.FetchExtras(ctx, exec, cfg.ID(), path, repos)
	}
	p.deleteSession = func(ctx context.Context, ref, session string) error {
		return client.DeleteSession(ctx, exec, cfg.ID(), ref, session)
	}
	p.detect = func(ctx context.Context) ([]cinderline.ReportClient, bool) {
		image, err := env.Image(ctx)
		if err != nil {
			p.Log.Warn("detection: no image", "project", cfg.Key, "err", err)
			return allMissingBinary(), false
		}
		mount, err := (&tools.Tools{State: state, Exec: exec}).Mount(ctx, image)
		if err != nil {
			p.Log.Warn("detection: no tools mount", "project", cfg.Key, "err", err)
			return allMissingBinary(), false
		}
		return detectClients(ctx, exec, cfg, len(providers) > 0, sec, image, mount, p.Log)
	}
	return p, nil
}

// modelProviders are the runner's providers with their optional runner-wide
// keys (decision 2026-11-17-local-model-providers).
func modelProviders(providers []config.Provider, sec *secrets.Secrets) (map[string]client.Provider, error) {
	m := map[string]client.Provider{}
	for _, p := range providers {
		key, _, err := sec.Get(secrets.Runner, secrets.ProviderKey(p.Name))
		if err != nil && !errors.Is(err, secrets.ErrNotFound) {
			return nil, err
		}
		m[p.Name] = client.Provider{BaseURL: p.BaseURL, Key: key}
	}
	return m, nil
}

// Detect reruns client detection and keeps the result for the report.
func (p *Project) Detect(ctx context.Context) {
	if p.detect == nil {
		return
	}
	p.detectMu.Lock()
	defer p.detectMu.Unlock()
	got, openRouter := p.detect(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil && p.detected != nil {
		close(p.detected)
	}
	p.clients = append([]cinderline.ReportClient{}, got...) // never nil: detection has a result
	p.openRouter = openRouter
}

// hasClients reports whether client detection has had a result.
func (p *Project) hasClients() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clients != nil
}

// AddRefused counts one connection the egress proxy refused, for the
// report, and keeps its host:port for the step's facts of run ref: once, in
// lower case as the allowlist writes it, and only a well-formed one. Two
// runs may refuse at once, so the hosts are per run (CLR-194).
func (p *Project) AddRefused(ref, hostPort string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refused++
	hp := strings.ToLower(hostPort)
	if !config.ValidHostPort(hp) || slices.Contains(p.hosts[ref], hp) {
		return
	}
	if p.hosts == nil {
		p.hosts = map[string][]string{}
	}
	p.hosts[ref] = append(p.hosts[ref], hp)
}

// takeRefused returns the hosts run ref refused since its last call and
// forgets them.
func (p *Project) takeRefused(ref string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	hosts := p.hosts[ref]
	if p.hosts != nil {
		delete(p.hosts, ref)
	}
	return hosts
}

// allowHosts are the hosts a person allowed in Cinderline for issue's runs,
// for a step's allowlist: none on a strict runner, and only those that pass
// as an allow_hosts entry and do not name a private network address.
func (p *Project) allowHosts(issue string, hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	log := p.Log.With("project", p.Config.Key, "issue", issue)
	if p.StrictHosts {
		log.Info("hosts allowed in Cinderline ignored: strict host blocking is on", "hosts", hosts)
		return nil
	}
	var out []string
	for _, hp := range hosts {
		host, _, _ := net.SplitHostPort(hp)
		ip := net.ParseIP(host)
		// Local config keeps private access; app approvals do not (decision 2026-11-19-app-host-addresses).
		if !config.AllowedHost(hp) || ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
			log.Warn("host allowed in Cinderline dropped: not host:port, or a private network address or the runner's own machine", "host", hp)
			continue
		}
		out = append(out, strings.ToLower(hp))
	}
	if out != nil {
		log.Info("hosts allowed in Cinderline opened for this step", "hosts", out)
	}
	return out
}

// Status is what the CLI shows of the project.
func (p *Project) Status() localapi.ProjectStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status
	st.Project = p.Config.Label()
	return st
}

// slot names the project's team slot, or its Project Manager slot, in the budget.
func (p *Project) slot(manager bool) string {
	if manager {
		return p.Config.ID() + " manager"
	}
	return p.Config.ID()
}

// runMemory is a run's share of the budget: as the last run measured it,
// else the project's memory setting, which a project without a compose file
// takes, until a run has read the compose file.
func (p *Project) runMemory() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cmp.Or(p.memory, p.Config.MemoryLimit())
}

// errNoMemory: the run, measured, no longer fits in the budget.
var errNoMemory = errors.New("not enough memory")

// fit makes memory, as the run's prepare measured it from the base's
// compose file, its slot's reservation and the project's run size. It
// fails with errNoMemory when that no longer fits: the base's compose file
// has grown since the last run measured it.
func (p *Project) fit(ctx context.Context, manager bool, memory int64) error {
	p.mu.Lock()
	p.memory = memory
	p.status.RunMemory = memory
	p.mu.Unlock()
	if p.Budget == nil {
		return nil
	}
	if ok, _ := p.Budget.reserve(ctx, p.slot(manager), memory); !ok {
		return fmt.Errorf("the run needs %s, more than the memory budget has left: %w", config.GiB(memory), errNoMemory)
	}
	return nil
}

func (p *Project) setStatus(change func(*localapi.ProjectStatus)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(&p.status)
}

// stripURL removes user info from a URL string, as one the app sends may
// hold it. A string that is not a valid URL is returned unchanged.
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
