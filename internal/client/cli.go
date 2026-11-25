package client

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLI is the Client of every agent CLI: one container per turn through the
// Environment (decision 2026-11-06-container-per-turn), the binary from the
// tools volume. Name picks what differs per CLI, from kinds.
type CLI struct {
	Name       string                  // claude, codex, opencode or gemini
	Project    string                  // project key, for its volumes
	Env        environment.Environment // where the turn runs
	Exec       workspace.Exec          // prepares the state volume
	Credential string                  // the project's key for the CLI's provider (or a `claude setup-token` token); empty: the stored sign-in
	BaseURL    string                  // Codex: an OpenAI-compatible base URL; empty: OpenAI's API
	Vertex     bool                    // Gemini: Credential is a Vertex AI express-mode key; false: the Gemini API
	MCPURL     string                  // Cinderline's MCP endpoint, as the agent container reaches it
	// Tools is from tools.Tools.Mount for the Environment's image. Set it
	// here before the CLI is shared, later only with SetTools: a run's
	// prepare changes it while another lane's turn runs (CLR-228).
	Tools   tools.Mount
	toolsMu sync.Mutex // guards Tools
	// Providers are the runner's model providers by name, for OpenCode
	// (decision 2026-11-17-local-model-providers).
	Providers map[string]Provider
}

var _ Client = (*CLI)(nil)

// Provider is a model provider of the project's: an OpenAI-compatible base
// URL as configured, and its key; empty: none.
type Provider struct{ BaseURL, Key string }

// ErrNotStarted is a turn the CLI did not start, as its model's provider is
// missing: no container ran.
var ErrNotStarted = errors.New("turn not started")

// kind is what differs between the agent CLIs.
type kind struct {
	env   func(c *CLI, t Turn) []string                 // the CLI's own environment
	cmd   func(c *CLI, t Turn, session string) []string // the argv in the container
	parse func(out string) Result
	// hosts are the provider host:port pairs a turn needs, for its network
	// allowlist. Without a key the turn uses the stored sign-in (CSR-58),
	// which also needs the provider's sign-in hosts: decision
	// 2026-11-09-network-allowlist point 5, unchecked against the CLIs; a
	// missing one shows up as a refused host. The CLI's own background
	// traffic is switched off where it can be, and only otherwise added
	// here, per client (decision 2026-11-25-client-own-traffic).
	hosts      func(c *CLI, t Turn) []string
	check      func(c *CLI, t Turn) error // nil: every turn can start
	ownSession bool                       // the runner picks a fresh turn's session ID
	libs       bool                       // needs Tools.OpenCodeEnv: on musl, the libraries from the tools volume
}

var kinds = map[string]kind{"claude": claude, "codex": codex, "opencode": opencode, "gemini": gemini}

// SetTools replaces the tools mount for the turns that start after it.
func (c *CLI) SetTools(m tools.Mount) {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	c.Tools = m
}

// mount is the current tools mount. A turn reads it once, so it uses one
// whole mount; nobody writes a Mount's slices after SetTools.
func (c *CLI) mount() tools.Mount {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	return c.Tools
}

// Run runs one turn. The turn limit is an error with HitTurnLimit set. A
// turn that printed no session keeps the one it resumed, never the ID the
// runner picked for a fresh one: a turn that failed before the CLI printed
// anything made no session to resume.
func (c *CLI) Run(ctx context.Context, t Turn) (Result, error) {
	k, ok := kinds[c.Name]
	if !ok {
		return Result{}, fmt.Errorf("unknown client %q", c.Name)
	}
	if k.check != nil {
		if err := k.check(c, t); err != nil {
			return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
		}
	}
	mount := c.mount()
	mounts, err := agentMounts(c.Project, c.Name, t.Dir, mount)
	if err != nil {
		return Result{}, err
	}
	if err = PrepareState(ctx, c.Exec, c.Project, c.Name); err != nil {
		return Result{}, err
	}
	session := t.SessionID
	if session == "" && k.ownSession {
		session = newSessionID()
	}
	var libs []string
	if k.libs {
		libs = mount.OpenCodeEnv
	}
	if t.Attachments != "" {
		mounts = append(mounts, "-v", t.Attachments+":"+workspace.AttachmentsPath+":ro")
	}
	out, err := c.Env.Run(ctx, environment.Spec{
		Issue:    path.Base(t.Dir), // the clone is RunsPath/<REF>
		Dir:      t.Dir,
		Mounts:   mounts,
		Env:      slices.Concat(mount.Env, libs, k.env(c, t), workspace.Identity(t.Author)),
		Cmd:      k.cmd(c, t, session),
		Stdin:    t.Prompt,
		Timeout:  time.Duration(t.MaxMinutes) * time.Minute,
		Idle:     t.Idle,
		Allow:    k.hosts(c, t),
		AppAllow: t.Allow,
	})
	res := k.parse(out)
	res.SessionID = cmp.Or(res.SessionID, t.SessionID)
	res.Tail = environment.Tail(out)
	if err == nil && res.HitUsageLimit {
		err = fmt.Errorf("%s: usage limit reached", c.Name)
	}
	if err == nil && res.HitTurnLimit {
		err = fmt.Errorf("%s: stopped at the turn limit", c.Name)
	}
	return res, err
}

// withToken ends a CLI's environment: the scoped token for agents with
// Cinderline access, or else without, which is what makes the CLI skip the
// project's own instruction files (app repository decision
// 2026-11-05-workers-skip-project-files).
func withToken(env []string, t Turn, without ...string) []string {
	if t.Token != "" {
		return append(env, tokenVar+"="+t.Token)
	}
	return append(env, without...)
}

// jsonLines are the lines of out that are a JSON object: stdout and stderr
// are mixed, so the rest is noise.
func jsonLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "{") && json.Valid([]byte(line)) {
			lines = append(lines, line)
		}
	}
	return lines
}

// agentMounts mounts the issue's clone dir, the repo volume that holds the
// objects the clone's alternates point at, the client's state volume and the
// tools.
func agentMounts(project, client, dir string, t tools.Mount) ([]string, error) {
	clone, err := workspace.CloneMounts(project, dir)
	if err != nil {
		return nil, err
	}
	return slices.Concat(clone, []string{"-v", StateVolume(project, client) + ":" + StatePath}, t.Args), nil
}

// StateVolume is the name of the volume with one client's sessions, config
// dir and sign-in for a project. One per client, so a run can read only its
// own client's sign-in (decision 2026-11-11-audit-fixes).
func StateVolume(project, client string) string {
	return workspace.Volume(project, "state-"+client)
}

// PrepareState makes the client's state volume for the project and hands its
// root to the agent user: a new volume belongs to root, and the CLI writes
// its config dir and sessions in it. Every turn, as it costs a second and
// repairs itself.
func PrepareState(ctx context.Context, x workspace.Exec, project, client string) error {
	vol := StateVolume(project, client)
	if _, err := x(ctx, nil, append(append([]string{"docker", "volume", "create"}, workspace.Labels(project, "")...), vol)...); err != nil {
		return fmt.Errorf("state volume: %w", err)
	}
	if _, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", vol+":/s", "--entrypoint", "sh", workspace.GitImage, "-c", "chown "+workspace.AgentUser+" /s"); err != nil {
		return fmt.Errorf("state volume: %w", err)
	}
	return nil
}

// sessionName is what a session ID or issue ref may hold before it goes
// into a find pattern: no glob or path characters.
var sessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// DeleteSession removes a finished issue's client files from the project's
// state volumes: every file or folder whose name holds session, and Claude
// Code's folder for the issue's clone /runs/<ref>, which also holds its
// workers' sessions. Every client's volume is searched, as an issue's agents
// run on several clients, but only those that exist: mounting a missing
// volume would create it.
//
// shortcut: by name, so Gemini CLI's chat files (named by an ID prefix) and
// OpenCode's message parts or database rows stay; delete per client when
// those grow large.
func DeleteSession(ctx context.Context, x workspace.Exec, project, ref, session string) error {
	if !sessionName.MatchString(ref) || session != "" && (len(session) < 8 || !sessionName.MatchString(session)) {
		return fmt.Errorf("delete session: %q / %q is not an issue ref and session ID", ref, session)
	}
	out, err := x(ctx, nil, "docker", "volume", "ls", "--quiet", "--filter", "name="+StateVolume(project, ""))
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	existing := strings.Fields(out)
	argv := []string{"docker", "run", "--rm", "--network", "none"}
	for _, client := range slices.Sorted(maps.Keys(kinds)) {
		if vol := StateVolume(project, client); slices.Contains(existing, vol) {
			argv = append(argv, "-v", vol+":/s/"+client)
		}
	}
	if len(argv) == 5 {
		return nil
	}
	match := `-name "-runs-$1"`
	if session != "" {
		match = `\( ` + match + ` -o -name "*$2*" \)`
	}
	// -mindepth 2: never a volume's mount point itself.
	_, err = x(ctx, nil, append(argv, "--entrypoint", "sh", workspace.GitImage, "-c", `find /s -mindepth 2 `+match+` -prune -exec rm -rf -- {} +`, "sh", ref, session)...)
	return err
}

// StatePath is where the agent container mounts its client's state volume,
// which holds the client's sessions and config dir (decision
// 2026-11-05-runner-state-location, 0116).
const StatePath = "/state"

// tokenVar holds the run's scoped token in the agent container. The MCP
// config names it, so the token never appears in an argv (Claude Code
// expands ${VAR} in --mcp-config, checked with 2.1.316).
var tokenVar = strings.ToUpper(name.Name) + "_TOKEN"
