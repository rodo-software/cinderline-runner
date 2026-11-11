package run

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

	// The limits on sessions kept after a finished run (CSR-145), from
	// config's keep_sessions_for and min_free_disk; 0 turns each off.
	KeepFor time.Duration
	MinFree int64

	// deleteSession removes an issue's client session files from the state
	// volume, set only by NewProject. Nil in tests.
	deleteSession func(ctx context.Context, ref, session string) error

	// run is the state shared by the hooks of one slot, set by Hooks().
	run *runState

	// prepare fills the tools mount on the first agent run, set only by
	// NewProject as a closure over the Docker environment. Nil in tests.
	prepare func(ctx context.Context) error

	// fetchExtras fetches the extra repositories, set only by NewProject.
	// Nil in tests.
	fetchExtras func(ctx context.Context, repos []workspace.Extra) map[string]error

	// detect is overridden by tests; nil means detectClients is called.
	detect func(ctx context.Context) []cinderline.ReportClient

	// clients caches the last detection result; nil before the first run.
	clients *clientCache
}

// runState is the per-slot state shared by the seven hooks.
type runState struct {
	mu      sync.Mutex
	handoff *runHandoff
	ready   bool // the clients have their tools mount
	// noPR is when a Review issue was last found without a pull request, by
	// ref: a cache CheckMerges may lose.
	noPR map[string]time.Time
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
	MaxTurns         int
	Forge            string
	Unread           bool
	// Solo team fields: the orchestrator's session and its scoped token,
	// for the merge attempt before the push (decision 2026-11-07-solo-team-resolves-merge-conflicts).
	Session, Token string
}

// NewProject builds a project's dependencies.
func NewProject(cfg config.Project, sec *secrets.Secrets, state string, env *environment.Docker, log *slog.Logger) (*Project, error) {
	gitToken, _, err := sec.Get(cfg, secrets.GitToken)
	if err != nil {
		return nil, err
	}
	p := &Project{
		Config:    cfg,
		Secrets:   sec,
		State:     state,
		Env:       env,
		Log:       log,
		Clients:   map[string]client.Client{},
		clients:   &clientCache{},
		Workspace: &workspace.Docker{Project: cfg.Key, Repository: cfg.Repository, Token: gitToken},
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
			p.Clients["claude"] = &client.Claude{Project: cfg.Key, Env: env, Credential: cred, MCPURL: mcpURL}
		case "codex":
			p.Clients["codex"] = &client.Codex{Project: cfg.Key, Env: env, Key: cred, BaseURL: cfg.OpenAIBaseURL, MCPURL: mcpURL}
		case "opencode":
			p.Clients["opencode"] = &client.OpenCode{Project: cfg.Key, Env: env, Key: cred, MCPURL: mcpURL}
		case "gemini":
			p.Clients["gemini"] = &client.Gemini{Project: cfg.Key, Env: env, Key: cred, Vertex: cfg.GeminiVertex, MCPURL: mcpURL}
		}
	}
	p.Forge = func(kind string) (forge.Forge, error) {
		return forge.New(kind, cfg.Repository, gitToken)
	}
	p.prepare = func(ctx context.Context) error {
		image, err := env.Image(ctx)
		if err != nil {
			return err
		}
		mount, err := (&tools.Tools{State: state}).Mount(ctx, image)
		if err != nil {
			return err
		}
		if len(cfg.ExtraRepositories) > 0 {
			// Every agent container gets the tools mount's flags; the extra repositories ride along.
			mount.Args = append(mount.Args, workspace.ExtraMount(cfg.Key)...)
		}
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
		return nil
	}
	exec := env.Exec
	if exec == nil {
		exec = workspace.RunCommand
	}
	p.fetchExtras = func(ctx context.Context, repos []workspace.Extra) map[string]error {
		return workspace.FetchExtras(ctx, exec, cfg.Key, repos)
	}
	p.deleteSession = func(ctx context.Context, ref, session string) error {
		return client.DeleteSession(ctx, exec, cfg.Key, ref, session)
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
	log := p.Log.With("project", p.Config.Key, "issue", issue.Ref)
	ref := issue.Ref

	if p.prepare != nil {
		p.run.mu.Lock()
		if !p.run.ready {
			if err := p.prepare(ctx); err != nil {
				p.run.mu.Unlock()
				return notStarted(err)
			}
			p.run.ready = true
		}
		p.run.mu.Unlock()
	}

	// A run may need the space; only a stop keeps it from starting.
	if err := p.Prune(ctx, c); err != nil {
		return notStarted(err)
	}

	team, err := c.GetTeam(ctx, issue.Team.ID)
	if err != nil {
		return notStarted(err)
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
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return notStarted(err)
	}
	base, err := BaseBranch(ctx, c, fg)
	if err != nil {
		return notStarted(err)
	}
	branch := name.Name + "/" + ref

	maxTurns := team.MaxTurns
	if maxTurns == 0 {
		maxTurns = 50 // decision 2026-11-07-team-max-turns
	}

	title := full.Title
	body := deref(full.Body)
	prompt := CodingPrompt(team, ref, title, body, branch, p.Config.Key, p.extraRepositories(ctx, c, ref))

	if len(team.Agents) > 1 {
		// Team with workers.
		out, err := p.Rounds(ctx, c, p.Clients, p.Workspace, TeamJob{
			Ref: ref, Branch: branch, Base: base, MaxTurns: maxTurns, Team: team, Prompt: prompt,
		})
		if err != nil {
			return err
		}
		if out.Paused {
			return nil // Publish must skip it.
		}
		p.setHandoff(ref, title, body, out.Dir, base, team, maxTurns, proj.Forge, out.Unread, "", "")
		return nil
	}

	// Solo team (one agent).
	agent := &team.Agents[0]
	cl := p.Clients[agent.Client]
	if cl == nil {
		return notStarted(fmt.Errorf("agent %s runs on %s, which this runner does not run", agent.Name, agent.Client))
	}

	dir := ""
	session := ""
	note, resumed := p.TakePaused(ctx, c, p.Workspace, ref, branch, agent.Client)
	if resumed {
		dir = note.Dir
		session = note.Session
	} else {
		var err error
		dir, err = p.Workspace.Prepare(ctx, branch, base)
		if err != nil {
			return notStarted(err)
		}
	}

	tok, err := c.CreateAgentToken(ctx, ref, cinderline.KindOrchestrator, agent.Name)
	if err != nil {
		return notStarted(err)
	}
	token := tok.Token

	p.CatchUp(ctx, c, p.Clients, p.Workspace, Upkeep{
		Ref: ref, Branch: branch, Base: base, Dir: dir, MaxTurns: maxTurns, Team: team,
		Session: session, Token: token,
	}, title)

	turn := client.Turn{
		Dir: dir, Prompt: prompt, MaxTurns: maxTurns,
		MaxMinutes: team.MaxMinutes, Model: deref(agent.Model),
		Token: token, Ponytail: team.Ponytail,
	}
	out, err := p.Attempts(ctx, c, cl, Job{Ref: ref, Client: agent.Client, Turn: turn, Resume: session, FollowUp: resumed && !note.Kept.IsZero()})
	if ctx.Err() == nil && !out.Paused {
		// Kept for a follow-up even when the run failed: the next run can continue it.
		p.keep(ref, Paused{Dir: dir, Session: cmp.Or(out.SessionID, session), Client: agent.Client})
	}
	if err != nil {
		if ctx.Err() == nil {
			if derr := c.DeleteAgentTokens(ctx, ref); derr != nil {
				log.Warn("agent tokens not deleted", "err", derr)
			}
		}
		return err
	}
	if out.Paused {
		if ctx.Err() == nil {
			if derr := c.DeleteAgentTokens(ctx, ref); derr != nil {
				log.Warn("agent tokens not deleted", "err", derr)
			}
		}
		return nil // Publish must skip it.
	}
	// Run hands off to Publish; Publish deletes the token.
	p.setHandoff(ref, title, body, dir, base, team, maxTurns, proj.Forge, out.Unread, out.SessionID, token)
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
	maps.Copy(failed, p.fetchExtras(ctx, repos))
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

func (p *Project) setHandoff(ref, title, body, dir, base string, team cinderline.Team, maxTurns int, forgeKind string, unread bool, session, token string) {
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	p.run.handoff = &runHandoff{
		Ref: ref, Title: title, Body: body, Dir: dir, Base: base,
		Team: team, MaxTurns: maxTurns, Forge: forgeKind, Unread: unread,
		Session: session, Token: token,
	}
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
	if h.Ref != issue.Ref {
		return fmt.Errorf("handoff for %s, but the scheduled issue is %s", h.Ref, issue.Ref)
	}
	proj, err := c.GetProject(ctx)
	if err != nil {
		return err
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return err
	}
	defer func() {
		if ctx.Err() != nil {
			return
		}
		if h.Token != "" {
			if derr := c.DeleteAgentTokens(ctx, issue.Ref); derr != nil {
				p.Log.Warn("agent tokens not deleted", "err", derr)
			}
		}
	}()
	return p.Publish(ctx, c, p.Clients, p.Workspace, fg, Pub{
		Ref: h.Ref, Title: h.Title, Body: h.Body, Base: h.Base,
		Dir: h.Dir, MaxTurns: h.MaxTurns, Team: h.Team, Forge: h.Forge, Unread: h.Unread,
		Session: h.Session, Token: h.Token,
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
