package client

import (
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
			"-e", "CLAUDE_CONFIG_DIR="+StatePath+"/claude",
			image, tools.Path+"/bin/claude", "auth", "login")
		return argv, nil

	case "codex":
		argv := append(prefix,
			"-e", "CODEX_HOME="+StatePath+"/codex",
			image, "sh", "-c", `mkdir -p "$CODEX_HOME" && exec "$0" "$@"`,
			tools.Path+"/bin/codex", "login", "--device-auth")
		return argv, nil

	case "gemini":
		// headless Gemini CLI refuses a manual sign-in, so the login starts the
		// interactive CLI with the Google login preselected.
		for _, kv := range t.OpenCodeEnv {
			prefix = append(prefix, "-e", kv)
		}
		argv := append(prefix,
			"-e", "GEMINI_CLI_HOME="+StatePath+"/gemini",
			image, "sh", "-c", `mkdir -p "$GEMINI_CLI_HOME/.gemini" && printf '%s' '{"security":{"auth":{"selectedType":"oauth-personal"}}}' > "$GEMINI_CLI_HOME/.gemini/settings.json" && exec "$0" "$@"`,
			tools.Path+"/bin/node", tools.Path+"/gemini/bundle/gemini.js")
		return argv, nil

	case "opencode":
		return nil, fmt.Errorf("OpenCode has no sign-in of its own here; its runs use the project's OpenRouter key, set with `add`")

	default:
		return nil, fmt.Errorf("%s has no sign-in of its own here; supported clients: claude, codex, gemini", client)
	}
}
