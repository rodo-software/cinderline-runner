package run

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/scheduler"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

var keys = secrets.ClientKeys

// Project is what the steps of one configured project need.
type Project struct {
	Config  config.Project
	Secrets *secrets.Secrets
	State   string                  // the host state folder (config.Paths.State)
	Env     environment.Environment // where the project's turns run
	Log     *slog.Logger

	Clients   map[string]client.Client // by agent client name
	Workspace workspace.Workspace
	Forge     func(kind string) (forge.Forge, error)

	// Others are all the projects added on this runner, for the code of
	// linked projects (CSR-161).
	Others []config.Project

	// The limits on sessions kept after a finished run (CSR-145), from
	// config's keep_sessions_for and min_free_disk; 0 turns each off.
	KeepFor time.Duration
	MinFree int64

	// deleteSession removes an issue's client session files from the state
	// volume, set only by NewProject. Nil in tests.
	deleteSession func(ctx context.Context, ref, session string) error

	// run is the state shared by the hooks of one slot, set by Hooks().
	run *runState

	// prepare points the Docker environment at the project's environment
	// in Cinderline and fills the tools mount for its agent image, set only
	// by NewProject. Nil in tests.
	prepare func(ctx context.Context, proj cinderline.Project, base string) error

	// fetchExtras fetches repositories into the volume of path (ExtraPath
	// or LinkedPath), set only by NewProject. Nil in tests.
	fetchExtras func(ctx context.Context, path string, repos []workspace.Extra) map[string]error

	// detect is overridden by tests; nil means detectClients is called.
	detect func(ctx context.Context) []cinderline.ReportClient

	// clients caches the last detection result; nil before the first run.
	clients *clientCache
}

// runState is the per-slot state shared by the seven hooks.
type runState struct {
	mu      sync.Mutex
	handoff *runHandoff
	image   string        // the agent image the clients' tools mount was filled for
	idle    time.Duration // the project's idle limit, from its last GET project (CSR-159)
	// noPR is when a Review issue was last found without a pull request, by
	// ref: a cache CheckMerges may lose.
	noPR map[string]time.Time
	// usage is the usage entries of the running run, by its top-level
	// issue, until reportUsage sends them at the run's end.
	usage map[string][]cinderline.UsageEntry
	// asked is the conflict of each Review issue's branch last reported to
	// next, by ref: a cache ReviewBranches may lose.
	asked map[string]string
}

// askedAbout is the conflict last reported to next for ref; "": none. A nil
// runState remembers nothing.
func (r *runState) askedAbout(ref string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked[ref]
}

// setAsked records the conflict reported to next for ref.
func (r *runState) setAsked(ref, conflict string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.asked == nil {
		r.asked = map[string]string{}
	}
	r.asked[ref] = conflict
}

// noPullRequestFor is how long CheckMerges leaves an issue it found without
// a pull request alone: finding none walks the forge's whole list, and a run
// that changed no files never gets one (decision 2026-11-11-audit-fixes).
const noPullRequestFor = 10 * time.Minute

// withoutPullRequest reports whether ref was found without a pull request
// less than noPullRequestFor ago. A nil runState remembers nothing.
func (r *runState) withoutPullRequest(ref string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.noPR[ref]) < noPullRequestFor
}

// setPullRequest records whether ref has a pull request.
func (r *runState) setPullRequest(ref string, has bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if has {
		delete(r.noPR, ref)
		return
	}
	if r.noPR == nil {
		r.noPR = map[string]time.Time{}
	}
	r.noPR[ref] = time.Now()
}

// runHandoff is what Run passes to Publish for the same issue.
type runHandoff struct {
	Ref, Title, Body string
	Dir, Base        string
	Team             cinderline.Team
	Forge            string
	Foot             string // below the finish report: the app's report_foot
	Verdict          string // above it: the app's report_head
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
		clients:   &clientCache{},
		Workspace: ws,
	}
	mcpURL := strings.TrimRight(cfg.URL, "/") + "/mcp"
	for name, kind := range keys {
		cred, _, err := sec.Get(cfg, kind)
		// Without a keychain key, Claude Code, Codex and Gemini CLI use the
		// login that `login` stored on the state volume (CSR-58).
		if err != nil && !(errors.Is(err, secrets.ErrNotFound) && (name == "claude" || name == "codex" || name == "gemini")) {
			continue
		}
		switch name {
		case "claude":
			p.Clients["claude"] = &client.Claude{Project: cfg.ID(), Env: env, Credential: cred, MCPURL: mcpURL}
		case "codex":
			p.Clients["codex"] = &client.Codex{Project: cfg.ID(), Env: env, Key: cred, BaseURL: cfg.OpenAIBaseURL, MCPURL: mcpURL}
		case "opencode":
			p.Clients["opencode"] = &client.OpenCode{Project: cfg.ID(), Env: env, Key: cred, MCPURL: mcpURL}
		case "gemini":
			p.Clients["gemini"] = &client.Gemini{Project: cfg.ID(), Env: env, Key: cred, Vertex: cfg.GeminiVertex, MCPURL: mcpURL}
		}
	}
	p.Forge = func(kind string) (forge.Forge, error) {
		return forge.New(kind, cfg.Repository, gitToken)
	}
	p.prepare = func(ctx context.Context, proj cinderline.Project, base string) error {
		var compose *environment.Compose
		// existing (EMBER-29) is not built yet and keeps the default environment.
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
		if image == p.run.image {
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
		// The attachments folder is a bind: the runner downloads to the host (CSR-173).
		mount.Args = append(mount.Args, "-v", attachmentsDir(state, cfg)+":"+workspace.AttachmentsPath+":ro")
		for _, cl := range p.Clients {
			switch c := cl.(type) {
			case *client.Claude:
				c.Tools = mount
			case *client.Codex:
				c.Tools = mount
			case *client.OpenCode:
				c.Tools = mount
			case *client.Gemini:
				c.Tools = mount
			}
		}
		p.run.image = image
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

// Hooks returns the project's steps for its scheduler slot.
func (p *Project) Hooks() scheduler.Hooks {
	if p.run == nil {
		p.run = &runState{}
	}
	return scheduler.Hooks{
		Recover:        p.Recover,
		Pin:            p.Pin,
		Check:          p.Check,
		Run:            p.hookRun,
		Publish:        p.hookPublish,
		ReviewBranches: p.hookReviewBranches,
		MergeCheck:     p.hookMergeCheck,
	}
}

// notStarted wraps an error that happened before the first agent turn, so the
// scheduler can tell it apart from a run that started (decision 2026-11-07-publish-resilience).
func notStarted(err error) error { return fmt.Errorf("%w: %w", scheduler.ErrNotStarted, err) }

// hookRun is the Run hook that the scheduler calls.
func (p *Project) hookRun(ctx context.Context, c *cinderline.Client, issue cinderline.Issue) error {
	ref := issue.Ref
	// The run ends here unless Publish takes over: one usage report per run.
	handed := false
	defer func() {
		if !handed {
			p.reportUsage(ctx, c, ref)
		}
	}()

	// A run may need the space; only a stop keeps it from starting.
	if err := p.Prune(ctx, c); err != nil {
		return notStarted(err)
	}

	team, err := c.GetTeam(ctx, issue.Team.ID)
	if err != nil {
		return notStarted(err)
	}
	generic := team.GenericRuns
	if generic && len(team.Agents) > 1 {
		// A generic team never has workers; old data that has both runs the orchestrator alone, as legacy work().
		team.Agents = team.Agents[:1]
	}
	full, err := c.GetIssue(ctx, ref)
	if err != nil {
		return notStarted(err)
	}
	p.Started(ctx, c, ref, team)

	proj, err := c.GetProject(ctx)
	if err != nil {
		return notStarted(err)
	}
	p.run.setIdle(proj)
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return notStarted(err)
	}
	base, err := BaseBranch(ctx, c, fg)
	if err != nil {
		return notStarted(err)
	}
	if p.prepare != nil {
		p.run.mu.Lock()
		err := p.prepare(ctx, proj, base)
		p.run.mu.Unlock()
		if err != nil {
			// A refused compose file is the admin's to fix: say so at once, not after three strikes.
			p.progress(ctx, c, ref, "The run's environment could not be prepared: "+err.Error())
			return notStarted(err)
		}
	}
	branch := name.Name + "/" + ref

	maxTurns := team.MaxTurns
	if maxTurns == 0 {
		maxTurns = 50 // decision 2026-11-07-team-max-turns
	}

	// Fetched on every Run, so a resume and a follow-up read fresh code too (CSR-161).
	links := p.linkedProjects(ctx, c, ref, proj.LinkedProjects)
	check := deref(proj.CheckCommand)
	if generic {
		check = "" // a generic run changes no files
	}

	// Solo, generic and team runs alike: the app decides each step (decision 2026-11-15-server-decides-steps).
	lead := team.Agents[0]
	if p.Clients[lead.Client] == nil {
		return notStarted(fmt.Errorf("agent %s runs on %s, which this runner does not run", lead.Name, lead.Client))
	}
	var extras []config.ExtraRepository
	if !generic {
		extras = p.extraRepositories(ctx, c, ref) // legacy's generic prompts name none
	}
	j := runJob{
		Ref: ref, Title: full.Title, Body: deref(full.Body), Branch: branch, Base: base, Team: team,
		MaxTurns: maxTurns, Generic: generic, Check: check, Forge: proj.Forge,
		Workspace: runWorkspace(branch, base, extras, links),
	}
	// A run's attachments come from its steps; another run's files must not linger.
	if err := p.resetAttachments(); err != nil {
		return notStarted(err)
	}
	take := p.TakePaused
	if len(team.Agents) > 1 {
		// A team's note stays, so a restart during its sub-issues resumes the orchestrator (0122).
		take = p.resumable
	}
	note, resumed := take(ctx, c, p.Workspace, ref, branch, lead.Client, generic)
	if resumed {
		j.Dir, j.Session, j.Origin = note.Dir, note.Session, "paused"
		if !note.Kept.IsZero() {
			j.Origin = "kept"
		}
	} else if err := p.pushBeforePrepare(ctx, ref, branch, generic); err != nil {
		return notStarted(err)
	} else if j.Dir, err = p.Workspace.Prepare(ctx, branch, base); err != nil {
		return notStarted(err)
	}
	// A generic run never publishes, so its branch needs no base (legacy attempts()).
	// Also on a clone fresh from the base, where the merge changes nothing,
	// as Prepare does not say whether it continued the remote branch.
	if !generic {
		j.Conflict = p.CatchUp(ctx, p.Workspace, ref, j.Dir, base, j.Title, team)
	}
	handed, err = p.steps(ctx, c, j)
	return err
}

// pushBeforePrepare pushes the issue's clone if it holds commits no remote
// branch has, as Prepare replaces it; an error keeps the clone (CSR-180). A
// generic run's clone is replaced as before: its commits are never published (CSR-75).
func (p *Project) pushBeforePrepare(ctx context.Context, ref, branch string, generic bool) error {
	if generic {
		return nil
	}
	clones, err := p.Workspace.Stranded(ctx)
	if err != nil {
		return err // unknown, so the clone may not be replaced
	}
	for _, clone := range clones {
		if clone.Branch != branch {
			continue
		}
		if err := p.Workspace.Push(ctx, clone.Dir, branch); err != nil {
			return fmt.Errorf("the clone has unpushed commits, kept: %w", err)
		}
		p.Log.Info("stranded branch pushed", "project", p.Config.Key, "issue", ref, "branch", branch)
	}
	return nil
}

// extraRepositories fetches the project's extra repositories and returns
// those the run can read. One that fails is left out and reported in a
// progress line; the run goes on without it (CSR-160).
func (p *Project) extraRepositories(ctx context.Context, c *cinderline.Client, ref string) []config.ExtraRepository {
	if p.fetchExtras == nil || len(p.Config.ExtraRepositories) == 0 {
		return nil
	}
	failed := map[string]error{}
	var repos []workspace.Extra
	for _, r := range p.Config.ExtraRepositories {
		token, _, err := p.Secrets.Get(p.Config, secrets.ExtraGitToken(r.URL))
		if err != nil {
			failed[r.Name()] = err
			continue
		}
		repos = append(repos, workspace.Extra{Name: r.Name(), URL: r.URL, Ref: r.Ref, Token: token})
	}
	maps.Copy(failed, p.fetchExtras(ctx, workspace.ExtraPath, repos))
	var ok []config.ExtraRepository
	for _, r := range p.Config.ExtraRepositories {
		if err := failed[r.Name()]; err != nil {
			p.Log.Warn("extra repository not fetched", "project", p.Config.Key, "issue", ref, "repository", r.URL, "err", err)
			p.progress(ctx, c, ref, "Extra repository "+r.URL+" not fetched; the run goes on without it")
			continue
		}
		ok = append(ok, r)
	}
	return ok
}

// linkedProjects fetches the repositories of the linked projects that are
// added on this runner: each one's own repository at its base branch and its
// extra repositories, each with the token stored for its URL. A linked
// project not added here, or a repository that fails, is reported in a
// progress line and the run goes on without it (CSR-161). The linked volume
// is filled even without links, so a removed link's code is gone.
func (p *Project) linkedProjects(ctx context.Context, c *cinderline.Client, ref string, linked []cinderline.LinkedProject) []Linked {
	if p.fetchExtras == nil {
		return nil
	}
	links := make([]Linked, len(linked))
	var repos []workspace.Extra
	failed := map[string]error{}
	urls := map[string]string{} // by folder, for the progress lines
	for i, l := range linked {
		links[i] = Linked{Key: l.Key, Name: l.Name}
		j := slices.IndexFunc(p.Others, func(o config.Project) bool {
			// A token is for one workspace, so a link is in this project's (CSR-97).
			return o.Key == l.Key && o.Workspace == p.Config.Workspace && strings.TrimRight(o.URL, "/") == strings.TrimRight(p.Config.URL, "/")
		})
		if j < 0 {
			p.Log.Info("linked project not added on this runner", "project", p.Config.Key, "issue", ref, "linked", l.Key)
			p.progress(ctx, c, ref, "Linked project "+l.Key+" is not added on this runner, so the run goes on without its code; `"+name.Name+" add` it to give runs its code")
			continue
		}
		b := p.Others[j]
		names := map[string]bool{}
		for k, r := range append([]config.ExtraRepository{{URL: b.Repository, Ref: l.BaseBranch}}, b.ExtraRepositories...) {
			folder := l.Key + "/" + r.Name()
			kind := secrets.ExtraGitToken(r.URL)
			if k == 0 {
				kind = secrets.GitToken
			}
			if names[r.Name()] {
				// The one already listed keeps the folder.
				p.progress(ctx, c, ref, "Repository "+r.URL+" of linked project "+l.Key+" not fetched: another of its repositories has the same name")
				continue
			}
			names[r.Name()] = true
			links[i].Paths = append(links[i].Paths, folder)
			urls[folder] = r.URL
			err := r.CheckRefAndName()
			token := ""
			if err == nil {
				token, _, err = p.Secrets.Get(b, kind)
			}
			if err != nil {
				failed[folder] = err
				continue
			}
			repos = append(repos, workspace.Extra{Name: folder, URL: r.URL, Ref: r.Ref, Token: token})
		}
	}
	maps.Copy(failed, p.fetchExtras(ctx, workspace.LinkedPath, repos))
	for i := range links {
		links[i].Paths = slices.DeleteFunc(links[i].Paths, func(folder string) bool {
			err := failed[folder]
			if err != nil {
				p.Log.Warn("linked repository not fetched", "project", p.Config.Key, "issue", ref, "repository", urls[folder], "err", err)
				p.progress(ctx, c, ref, "Repository "+urls[folder]+" of linked project "+links[i].Key+" not fetched; the run goes on without it")
			}
			return err != nil
		})
	}
	return links
}

// Linked is a project this project's agents may reach (decision
// 2026-11-11-project-links), with the folders of its repositories this run
// fetched under workspace.LinkedPath, as KEY/name.
type Linked struct {
	Key, Name string
	Paths     []string
}

// Started is the first progress line of a claimed issue, as legacy
// runner.mjs reports after the claim. The Run hook calls it right after the
// claim.
func (p Project) Started(ctx context.Context, c *cinderline.Client, ref string, team cinderline.Team) {
	p.progress(ctx, c, ref, "Started "+team.Name+" / "+team.Agents[0].Name)
}

func (p *Project) setHandoff(h runHandoff) {
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	p.run.handoff = &h
}

func (p *Project) takeHandoff() *runHandoff {
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	h := p.run.handoff
	p.run.handoff = nil
	return h
}

// hookPublish is the Publish hook that the scheduler calls.
func (p *Project) hookPublish(ctx context.Context, c *cinderline.Client, issue cinderline.Issue) error {
	h := p.takeHandoff()
	if h == nil {
		return nil
	}
	defer p.reportUsage(ctx, c, h.Ref) // the run ends with its publish
	if h.Ref != issue.Ref {
		return fmt.Errorf("handoff for %s, but the scheduled issue is %s", h.Ref, issue.Ref)
	}
	// The run's forge, not a new GET project: the push needs no Cinderline (CSR-159).
	fg, err := p.Forge(h.Forge)
	if err != nil {
		return err
	}
	return p.Publish(ctx, c, p.Workspace, fg, Pub{
		Ref: h.Ref, Title: h.Title, Body: h.Body, Base: h.Base,
		Dir: h.Dir, Team: h.Team, Forge: h.Forge, Foot: h.Foot, Verdict: h.Verdict,
	})
}

// hookReviewBranches is the ReviewBranches hook that the scheduler calls.
func (p *Project) hookReviewBranches(ctx context.Context, c *cinderline.Client) error {
	proj, err := c.GetProject(ctx)
	if err != nil {
		return err
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return err
	}
	base, err := BaseBranch(ctx, c, fg)
	if err != nil {
		return err
	}
	return p.ReviewBranches(ctx, c, p.Workspace, fg, base)
}

// hookMergeCheck is the MergeCheck hook that the scheduler calls.
func (p *Project) hookMergeCheck(ctx context.Context, c *cinderline.Client) error {
	proj, err := c.GetProject(ctx)
	if err != nil {
		return err
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return err
	}
	return p.CheckMerges(ctx, c, fg)
}
