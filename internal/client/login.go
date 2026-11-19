package client

import (
	"context"
	"fmt"

	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// LoginArgv returns the docker argv that runs the CLI's own sign-in in an
// interactive container with the client's state volume of the project mounted. The credentials land on the volume; the runner never
// reads or copies them.
func LoginArgv(client, project, image string, t tools.Mount) ([]string, error) {
	prefix := []string{
		"docker", "run", "--rm", "--interactive", "--tty",
		"--user", workspace.AgentUser,
		"-v", StateVolume(project, client) + ":" + StatePath,
	}
	prefix = append(prefix, t.Args...)
	for _, kv := range t.Env {
		prefix = append(prefix, "-e", kv)
	}

	switch client {
	case "claude":
		argv := append(prefix,
			"-e", claudeHome,
			image, tools.Path+"/bin/claude", "auth", "login")
		return argv, nil

	case "codex":
		argv := append(prefix,
			"-e", codexHome,
			image, "sh", "-c", codexShell,
			tools.Path+"/bin/codex", "login", "--device-auth")
		return argv, nil

	case "gemini":
		// headless Gemini CLI refuses a manual sign-in, so the login starts the
		// interactive CLI with the Google login preselected.
		for _, kv := range t.OpenCodeEnv {
			prefix = append(prefix, "-e", kv)
		}
		argv := append(prefix,
			"-e", geminiHome,
			image, "sh", "-c", geminiShell(`'{"security":{"auth":{"selectedType":"oauth-personal"}}}'`),
			tools.Path+"/bin/node", tools.Path+"/gemini/bundle/gemini.js")
		return argv, nil

	case "opencode":
		return nil, fmt.Errorf("OpenCode has no sign-in of its own here; its runs use the project's OpenRouter key, set with `add`")

	default:
		return nil, fmt.Errorf("%s has no sign-in of its own here; supported clients: claude, codex, gemini", client)
	}
}

// loginFiles are where each CLI's own sign-in leaves its credentials on its
// state volume, under the config dirs of claudeHome, codexHome and geminiHome.
var loginFiles = map[string]string{
	"claude": StatePath + "/claude/.credentials.json",
	"codex":  StatePath + "/codex/auth.json",
	"gemini": StatePath + "/gemini/.gemini/oauth_creds.json",
}

// LoginExists reports whether the client's sign-in file is on the project's
// state volume of that client; false for a client without a sign-in.
func LoginExists(ctx context.Context, x workspace.Exec, project, client string) (bool, error) {
	file := loginFiles[client]
	if file == "" {
		return false, nil
	}
	_, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none",
		"-v", StateVolume(project, client)+":"+StatePath,
		"--entrypoint", "test", workspace.GitImage, "-f", file)
	return workspace.ExitedZero(err)
}
