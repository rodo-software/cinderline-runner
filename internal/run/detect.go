package run

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

var clientNames = []string{"claude", "codex", "gemini", "opencode"}

var versionPat = regexp.MustCompile(`[0-9]+\.[0-9]+(?:\.[0-9]+)+`)

func allMissingBinary() []cinderline.ReportClient {
	var r []cinderline.ReportClient
	for _, n := range clientNames {
		r = append(r, cinderline.ReportClient{Name: n, State: "missing_binary"})
	}
	return r
}

// detectClients is each client's state and whether OpenCode can use
// OpenRouter: its key is set and not known to be invalid. OpenCode is ready
// when its binary is there and OpenRouter or one of the runner's providers
// (providers says it has one) is usable; the app checks each agent's provider by name (decision
// 2026-11-17-local-model-providers). Configured providers are not checked.
func detectClients(ctx context.Context, exec workspace.Exec, cfg config.Project, providers bool, sec *secrets.Secrets, image string, mount tools.Mount, log *slog.Logger) (res []cinderline.ReportClient, openRouter bool) {
	log = log.With("project", cfg.Key)
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
			if name == "opencode" {
				openRouter = state == "ready"
				if providers {
					state = "ready"
				}
			}
			res = append(res, cinderline.ReportClient{Name: name, State: state, Version: v})
			continue
		}
		// By ID, as every run and login names the volume; by key, a project
		// outside the default workspace never found its login (CLR-199).
		login, _ := client.LoginExists(ctx, exec, cfg.ID(), name)
		if name == "opencode" && providers || login {
			res = append(res, cinderline.ReportClient{Name: name, State: "ready", Version: v})
			continue
		}
		res = append(res, cinderline.ReportClient{Name: name, State: "missing_credential", Version: v})
	}
	return res, openRouter
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

// keyScript makes a GET to URL with the key in header HEADER after PREFIX,
// plus an anthropic-version header when VERSION is set. 401 or 403 exits 3;
// any other answer and a network error count as ready.
const keyScript = `const h={[process.env.HEADER]:process.env.PREFIX+process.env.KEY};if(process.env.VERSION)h['anthropic-version']=process.env.VERSION;fetch(process.env.URL,{headers:h}).then(r=>process.exit(r.status==401||r.status==403?3:0)).catch(()=>process.exit(0))`

// checkKey checks whether the key works via a free HTTP GET.
func checkKey(ctx context.Context, exec workspace.Exec, mount tools.Mount, image, name, cred string, cfg config.Project, log *slog.Logger) string {
	var url, header, prefix, version string
	switch name {
	case "claude":
		if strings.HasPrefix(cred, "sk-ant-oat") {
			// shortcut: setup-token keys are not checked, add a check once
			// a free endpoint accepts OAuth tokens
			return "ready"
		}
		url, header, version = "https://api.anthropic.com/v1/models", "x-api-key", "2023-06-01"
	case "codex":
		base := strings.TrimRight(cfg.OpenAIBaseURL, "/")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		url, header, prefix = base+"/models", "Authorization", "Bearer "
	case "opencode":
		url, header, prefix = "https://openrouter.ai/api/v1/key", "Authorization", "Bearer "
	case "gemini":
		if cfg.GeminiVertex {
			// shortcut: Vertex keys are not checked, add a check once
			// a free endpoint accepts Vertex tokens
			return "ready"
		}
		url, header = "https://generativelanguage.googleapis.com/v1beta/models", "x-goog-api-key"
	default:
		return "ready"
	}
	// Every variable is set, even when empty: docker -e NAME takes an unset
	// one from the runner's own environment.
	env := []string{"KEY=" + cred, "URL=" + url, "HEADER=" + header, "PREFIX=" + prefix, "VERSION=" + version}
	return checkHTTP(ctx, exec, mount, image, env, name, log)
}

// checkHTTP runs keyScript in a node container. The variables, the key among
// them, go through the exec's env and only their names into argv.
func checkHTTP(ctx context.Context, exec workspace.Exec, mount tools.Mount, image string, env []string, name string, log *slog.Logger) string {
	argv := []string{"docker", "run", "--rm"}
	argv = append(argv, mount.Args...)
	for _, e := range mount.Env {
		argv = append(argv, "-e", e)
	}
	for _, e := range mount.OpenCodeEnv {
		argv = append(argv, "-e", e)
	}
	argv = append(argv, "--user", workspace.AgentUser)
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		argv = append(argv, "-e", k)
	}
	argv = append(argv, image, tools.Path+"/bin/node", "-e", keyScript)

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
