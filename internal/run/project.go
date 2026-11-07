package run

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
	"github.com/rodo-software/cinderstack-runner/internal/client"
	"github.com/rodo-software/cinderstack-runner/internal/config"
	"github.com/rodo-software/cinderstack-runner/internal/environment"
	"github.com/rodo-software/cinderstack-runner/internal/forge"
	"github.com/rodo-software/cinderstack-runner/internal/name"
	"github.com/rodo-software/cinderstack-runner/internal/scheduler"
	"github.com/rodo-software/cinderstack-runner/internal/secrets"
	"github.com/rodo-software/cinderstack-runner/internal/tools"
	"github.com/rodo-software/cinderstack-runner/internal/workspace"
)

var keys = map[string]secrets.Kind{"claude": secrets.AnthropicCredential, "opencode": secrets.OpenRouterKey}

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

	// run is the state shared by the hooks of one slot, set by Hooks().
	run *runState

	// prepare fills the tools mount on the first agent run, set only by
	// NewProject as a closure over the Docker environment. Nil in tests.
	prepare func(ctx context.Context) error
}

// runState is the per-slot state shared by the seven hooks.
type runState struct {
	mu      sync.Mutex
	handoff *runHandoff
	ready   bool // the clients have their tools mount
}

// runHandoff is what Run passes to Publish for the same issue.
type runHandoff struct {
	Ref, Title, Body string
	Dir, Base        string
	Team             cinderstack.Team
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
		Workspace: &workspace.Docker{Project: cfg.Key, Repository: cfg.Repository, Token: gitToken},
	}
	mcpURL := strings.TrimRight(cfg.URL, "/") + "/mcp"
	for name, kind := range keys {
		cred, _, err := sec.Get(cfg, kind)
		if err != nil {
			continue
		}
		switch name {
		case "claude":
			p.Clients["claude"] = &client.Claude{Project: cfg.Key, Env: env, Credential: cred, MCPURL: mcpURL}
		case "opencode":
			p.Clients["opencode"] = &client.OpenCode{Project: cfg.Key, Env: env, Key: cred, MCPURL: mcpURL}
		}
	}
	p.Forge = func(kind string) (forge.Forge, error) {
		switch kind {
		case "github":
			return forge.NewGitHub(cfg.Repository, gitToken)
		case "gitlab":
			return forge.NewGitLab(cfg.Repository, gitToken)
		case "forgejo":
			return forge.NewForgejo(cfg.Repository, gitToken)
		case "bitbucket":
			return forge.NewBitbucket(cfg.Repository, gitToken)
		default:
			return nil, fmt.Errorf("unsupported forge kind %q", kind)
		}
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
		for _, cl := range p.Clients {
			switch c := cl.(type) {
			case *client.Claude:
				c.Tools = mount
			case *client.OpenCode:
				c.Tools = mount
			}
		}
		return nil
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

// hookRun is the Run hook that the scheduler calls.
func (p *Project) hookRun(ctx context.Context, c *cinderstack.Client, issue cinderstack.Issue) error {
	log := p.Log.With("project", p.Config.Key, "issue", issue.Ref)
	ref := issue.Ref

	if p.prepare != nil {
		p.run.mu.Lock()
		if !p.run.ready {
			if err := p.prepare(ctx); err != nil {
				p.run.mu.Unlock()
				return err
			}
			p.run.ready = true
		}
		p.run.mu.Unlock()
	}

	team, err := c.GetTeam(ctx, issue.Team.ID)
	if err != nil {
		return err
	}
	full, err := c.GetIssue(ctx, ref)
	if err != nil {
		return err
	}
	p.Started(ctx, c, ref, team)

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
	branch := name.Name + "/" + ref

	maxTurns := team.MaxTurns
	if maxTurns == 0 {
		maxTurns = 50 // decision 2026-11-07-team-max-turns
	}

	title := full.Title
	body := deref(full.Body)
	prompt := CodingPrompt(team, ref, title, body, branch, p.Config.Key)

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
		return fmt.Errorf("agent %s runs on %s, which this runner does not run", agent.Name, agent.Client)
	}

	dir := ""
	session := ""
	note, resumed := p.TakePaused(ref, agent.Client)
	if resumed {
		dir = note.Dir
		session = note.Session
	} else {
		var err error
		dir, err = p.Workspace.Prepare(ctx, branch, base)
		if err != nil {
			return err
		}
	}

	tok, err := c.CreateAgentToken(ctx, ref, cinderstack.KindOrchestrator, agent.Name)
	if err != nil {
		return err
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
	out, err := p.Attempts(ctx, c, cl, Job{Ref: ref, Client: agent.Client, Turn: turn, Resume: session})
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

func (p *Project) setHandoff(ref, title, body, dir, base string, team cinderstack.Team, maxTurns int, forgeKind string, unread bool, session, token string) {
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
func (p *Project) hookPublish(ctx context.Context, c *cinderstack.Client, issue cinderstack.Issue) error {
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
func (p *Project) hookReviewBranches(ctx context.Context, c *cinderstack.Client) error {
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
func (p *Project) hookMergeCheck(ctx context.Context, c *cinderstack.Client) error {
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
