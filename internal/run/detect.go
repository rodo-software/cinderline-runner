package run

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

var clientNames = []string{"claude", "codex", "gemini", "opencode"}

var loginFiles = map[string]string{
	"claude": "/state/claude/.credentials.json",
	"codex":  "/state/codex/auth.json",
	"gemini": "/state/gemini/.gemini/oauth_creds.json",
}

var versionPat = regexp.MustCompile(`[0-9]+\.[0-9]+(?:\.[0-9]+)+`)

// clientCache is a thread-safe cache of the last detection result.
type clientCache struct {
	mu  sync.Mutex
	got []cinderline.ReportClient
}

func allMissingBinary() []cinderline.ReportClient {
	var r []cinderline.ReportClient
	for _, n := range clientNames {
		r = append(r, cinderline.ReportClient{Name: n, State: "missing_binary"})
	}
	return r
}

func detectClients(ctx context.Context, exec workspace.Exec, cfg config.Project, sec *secrets.Secrets, image string, mount tools.Mount, log *slog.Logger) []cinderline.ReportClient {
	log = log.With("project", cfg.Key)
	var res []cinderline.ReportClient
	for _, name := range clientNames {
		v, ok := checkBinary(ctx, exec, mount, image, name)
		if !ok {
			res = append(res, cinderline.ReportClient{Name: name, State: "missing_binary"})
			continue
		}
		kind, okk := keys[name]
		if !okk {
			continue
		}
		cred, _, err := sec.Get(cfg, kind)
		if err == nil {
			state := checkKey(ctx, exec, mount, image, name, cred, cfg, log)
			res = append(res, cinderline.ReportClient{Name: name, State: state, Version: v})
			continue
		}
		if name != "opencode" && loginExists(ctx, exec, cfg.Key, name) {
			res = append(res, cinderline.ReportClient{Name: name, State: "ready", Version: v})
			continue
		}
		res = append(res, cinderline.ReportClient{Name: name, State: "missing_credential", Version: v})
	}
	return res
}

// checkBinary runs --version and returns the version string and whether the
// binary exists.
func checkBinary(ctx context.Context, exec workspace.Exec, mount tools.Mount, image, name string) (*string, bool) {
	argv := binaryArgv(mount, image, name)
	if argv == nil {
		return nil, false
	}
	out, err := exec(ctx, nil, argv...)
	if err != nil {
		return nil, false
	}
	s := strings.TrimSpace(out)
	if m := versionPat.FindString(s); m != "" {
		return &m, true
	}
	return nil, true
}

// binaryArgv returns the docker run argv for the client's --version command.
func binaryArgv(mount tools.Mount, image, name string) []string {
	argv := []string{"docker", "run", "--rm"}
	argv = append(argv, mount.Args...)
	for _, e := range mount.Env {
		argv = append(argv, "-e", e)
	}
	argv = append(argv, "--user", workspace.AgentUser)

	switch name {
	case "claude":
		return append(argv, image, tools.Path+"/bin/claude", "--version")
	case "codex":
		return append(argv, image, tools.Path+"/bin/codex", "--version")
	case "gemini":
		for _, e := range mount.OpenCodeEnv {
			argv = append(argv, "-e", e)
		}
		return append(argv, image, tools.Path+"/bin/node", tools.Path+"/gemini/bundle/gemini.js", "--version")
	case "opencode":
		return append(argv, image, tools.Path+"/bin/opencode", "--version")
	}
	return nil
}

// loginExists checks whether the login file exists on the state volume.
func loginExists(ctx context.Context, exec workspace.Exec, project, name string) bool {
	p := loginFiles[name]
	if p == "" {
		return false
	}
	argv := []string{"docker", "run", "--rm", "--network", "none",
		"-v", workspace.Volume(project, "state") + ":/state",
		"--entrypoint", "test", workspace.GitImage, "-f", p}
	_, err := exec(ctx, nil, argv...)
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}
	return err == nil
}

// checkKey checks whether the key works via a free HTTP GET.
func checkKey(ctx context.Context, exec workspace.Exec, mount tools.Mount, image, name, cred string, cfg config.Project, log *slog.Logger) string {
	switch name {
	case "claude":
		if strings.HasPrefix(cred, "sk-ant-oat") {
			// shortcut: setup-token keys are not checked, add a check once
			// a free endpoint accepts OAuth tokens
			return "ready"
		}
		return checkHTTP(ctx, exec, mount, image, "KEY", cred,
			`fetch(process.env.URL,{headers:{'x-api-key':process.env.KEY,'anthropic-version':'2023-06-01'}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`, "https://api.anthropic.com/v1/models", name, log)
	case "codex":
		base := strings.TrimRight(cfg.OpenAIBaseURL, "/")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		url := base + "/models"
		return checkHTTP(ctx, exec, mount, image, "KEY", cred,
			`fetch(process.env.URL,{headers:{'Authorization':'Bearer '+process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`, url, name, log)
	case "opencode":
		return checkHTTP(ctx, exec, mount, image, "KEY", cred,
			`fetch(process.env.URL,{headers:{'Authorization':'Bearer '+process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`, "https://openrouter.ai/api/v1/key", name, log)
	case "gemini":
		if cfg.GeminiVertex {
			// shortcut: Vertex keys are not checked, add a check once
			// a free endpoint accepts Vertex tokens
			return "ready"
		}
		return checkHTTP(ctx, exec, mount, image, "KEY", cred,
			`fetch(process.env.URL,{headers:{'x-goog-api-key':process.env.KEY}}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`, "https://generativelanguage.googleapis.com/v1beta/models", name, log)
	}
	return "ready"
}

// checkHTTP runs a node container that makes an HTTP GET. The key is passed
// through the exec's env, never in argv.
func checkHTTP(ctx context.Context, exec workspace.Exec, mount tools.Mount, image, envName, cred, script, url, name string, log *slog.Logger) string {
	argv := []string{"docker", "run", "--rm"}
	argv = append(argv, mount.Args...)
	for _, e := range mount.Env {
		argv = append(argv, "-e", e)
	}
	for _, e := range mount.OpenCodeEnv {
		argv = append(argv, "-e", e)
	}
	argv = append(argv, "--user", workspace.AgentUser, "-e", envName, "-e", "URL")
	argv = append(argv, image, tools.Path+"/bin/node", "-e", script)

	env := []string{envName + "=" + cred, "URL=" + url}

	_, err := exec(ctx, env, argv...)
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return "invalid_credential"
	}
	if err != nil {
		log.Warn("key check failed", "client", name, "err", err)
	}
	return "ready"
}

// Detect reruns client detection and caches the result. Thread-safe.
func (p *Project) Detect(ctx context.Context) []cinderline.ReportClient {
	if p.clients == nil {
		p.clients = &clientCache{}
	}
	p.clients.mu.Lock()
	defer p.clients.mu.Unlock()
	if p.detect != nil {
		p.clients.got = p.detect(ctx)
	}
	return p.clients.got
}

// detected returns the cached detection result, running detection once if
// needed. Thread-safe.
func (p *Project) detected(ctx context.Context) []cinderline.ReportClient {
	if p.clients == nil {
		p.clients = &clientCache{}
	}
	p.clients.mu.Lock()
	defer p.clients.mu.Unlock()
	if p.clients.got != nil {
		return p.clients.got
	}
	if p.detect != nil {
		p.clients.got = p.detect(ctx)
	}
	return p.clients.got
}
