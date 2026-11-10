# Cinderline runner

The Cinderline runner is a native Go program on a developer's machine that takes To do issues from a Cinderline instance, runs an agent team on each in its own Docker container, pushes a branch and opens a pull request.

Status: milestones M0 to M5, M7 (Codex, Gemini CLI, `cinderline login`, client detection, errors reported to Cinderline) and M8 (network allowlist) are built. M6 (per-project and existing environments) is decided (decision 2026-11-08-m6-environments) but not built: every project runs in the default environment. See [docs/runner-rewrite-plan.md](docs/runner-rewrite-plan.md).

## Supported platforms

`cinderline` is one binary for x86-64 and ARM64. It needs Docker and nothing else on the machine: git runs in a container. Agent containers are Linux on every system; Windows containers are not supported.

| System | Versions | Docker |
|---|---|---|
| macOS | The current and two previous major releases, as Docker Desktop supports them; Apple silicon and Intel | Docker Desktop |
| Linux | Any distribution that runs Docker Engine 20.10 or later; `cinderline service` needs systemd | Docker Engine 20.10 or later, or Docker Desktop |
| Windows | Windows 10 22H2 (build 19045) or Windows 11 23H2 or later, 64-bit; ARM64 only as far as Docker Desktop's ARM build (early access) runs | Docker Desktop with the WSL2 backend |

- On a Windows build below 19045 every command except `cinderline version` stops with an error naming the build it found. Windows Server is not supported: Docker Desktop does not run on it.
- CI runs the checks on macOS and Windows on GitHub and on Linux on Forgejo; [docs/ci/platforms.yml](docs/ci/platforms.yml) widens that to the oldest and newest images per system and architecture.

## Install

Install the latest `cinderline` binary with one line — no sudo or elevation required. Each script downloads the archive for your OS and architecture, verifies its SHA-256 checksum against the release's `checksums.txt` (refusing on mismatch), and installs the binary.

**macOS / Linux:**

```
curl -fsSL https://raw.githubusercontent.com/rodo-software/cinderline-runner/main/scripts/install.sh | sh
```

Installs to `~/.local/bin/cinderline`. If that directory is not on your `PATH`, the script prints a line to add to your shell profile.

**Windows (PowerShell 5.1+):**

```
irm https://raw.githubusercontent.com/rodo-software/cinderline-runner/main/scripts/install.ps1 | iex
```

Installs to `%LocalAppData%\Programs\cinderline\cinderline.exe`. If that directory is not on your user `PATH`, the script prints a command to add it.

To pin a specific version, set `CINDERLINE_DOWNLOAD_URL` before running (e.g.
`https://github.com/rodo-software/cinderline-runner/releases/download/v0.1.0`).

## Add a project

`cinderline add` asks for the Cinderline instance's URL, the project's runner token, the repository (it offers the one the project names in Cinderline and refuses another) and a git token. It checks Docker first (installed, daemon running, at least 5 GB free), checks the runner token against the instance as it asks, stores both tokens in the OS keychain (or a 0600 file in the state folder where there is none) and the rest in `config.json`, and warns when CI can read secrets from the runner's branches (see "CI secrets" below). When other projects are configured, it offers to copy their model credentials. A running runner picks the project up at once. `cinderline remove [-url URL] KEY` removes a project and its secrets.

`cinderline add` does not ask for model keys (Anthropic API key or `claude setup-token` token, OpenRouter, OpenAI, Gemini), and no command stores one: a project gets them by copying from another project, or uses a sign-in (below).

Then start it with `cinderline run` (foreground, until Ctrl-C) or as a service (below).

`cinderline run -v` also prints the log on the terminal, one readable line per event, and adds Debug lines (each runner API request) to both the terminal and the log file.

### Sign a client in instead of storing a key

`cinderline login claude --project KEY`, `cinderline login codex --project KEY` or `cinderline login gemini --project KEY` runs the CLI's own sign-in (`claude auth login`, `codex login --device-auth`, Gemini CLI's own Google sign-in) in a container. Open the link it prints and paste the code back. The credentials stay on that project's state volume; the runner never reads them. When the keychain holds no Anthropic, OpenAI or Gemini key for the project, runs then use this login; a stored key still wins. OpenCode uses the project's OpenRouter key only. After `login gemini`, type `/quit` once it says "Authentication succeeded": the interactive Gemini CLI stays open after signing in.

The daemon reports each client's state (ready, missing the binary, missing a credential, or the provider refused the key) in its runner report, so the Cinderline instance can show what a project is set up for. Logins on the state volume count as configured but are not tested, so an expired login only fails on the first run.

## Run as a service

`cinderline service install` registers a user-level service (a launchd LaunchAgent on macOS, a systemd user unit on Linux, a logon scheduled task on Windows) that runs `cinderline run` at login and starts it now; `cinderline status`, `cinderline logs` and `cinderline stop` talk to it; `cinderline service uninstall` stops and removes it. On Linux it needs systemd. `cinderline stop` stops it until the next login. Linux and Windows restart it after a crash; macOS does not, so check `cinderline status` there.

`cinderline uninstall` asks for confirmation (`-yes` skips it), then removes the service, the runner's containers, networks, volumes and its own image, the config and state folders and every project's secrets. It lists each project's Cinderline URL and key: revoke that runner token in Cinderline yourself. It leaves the binary and prints its path for you to delete.

## Configuration

`config.json` lives in the `cinderline` folder of the OS's user config folder (`~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows), with the log and other state in its `state` subfolder. It holds no secrets. Besides what `add` writes, you can set:

- `merge_poll_seconds`: how often the merge status of Review issues is checked; 60 by default.
- `build_image`: `true` builds the default image locally instead of pulling it.
- Per project: `allow_hosts`, extra `host:port` entries its agents may reach (see "Network"); `openai_base_url`, an OpenAI-compatible base URL for Codex; `gemini_vertex`, `true` when the Gemini key is a Vertex AI express-mode key.

Unknown fields are refused. `add` and `login` make a running runner reload the file, which aborts its runs in flight; they are recovered right after. After `remove` or a hand edit, restart the runner.

## Network

Each issue runs on its own internal Docker network with a fresh MySQL 8 and an egress proxy, its only way out (decision 2026-11-09-network-allowlist). The proxy allows exact `host:port` pairs, no wildcards:

- the model provider of the run's client only (`api.anthropic.com`, `openrouter.ai`, `api.openai.com` or the `openai_base_url` host, `generativelanguage.googleapis.com` or `aiplatform.googleapis.com`), plus that provider's sign-in hosts when the run uses a sign-in;
- the project's Cinderline host, for MCP;
- the package registries: `repo.packagist.org`, `packagist.org`, `codeload.github.com`, `api.github.com`, `registry.npmjs.org`, `proxy.golang.org`, `sum.golang.org`, on port 443;
- the project's `allow_hosts`.

No forge (git fetch and push run in a separate helper container) and never `host.docker.internal`. The run's log names each refused host, and the runner report counts them.

## Update

`cinderline update` downloads and verifies the latest release, refreshes the tools volume and default image, replaces the binary, and lets the running runner finish its runs before it stops; `cinderline stop --drain` does only the last part. If the service is installed, `cinderline update` starts it again with the new binary.

Agent containers run the default image, `ghcr.io/rodo-software/cinderline-runner-default`, pulled by a pinned digest. Offline, or with `"build_image": true` in `config.json`, the runner builds it locally from `images/default/Dockerfile` instead.

## Build

```
go build ./cmd/cinderline
./cinderline version
```

## Checks

```
gofmt -l .
go vet ./...
staticcheck ./...
go test -race ./...
govulncheck ./...
```

See [AGENTS.md](AGENTS.md) and [docs/runner-rewrite-plan.md](docs/runner-rewrite-plan.md).

## CI secrets

The runner's agents push to `cinderline/<REF>` branches. Any CI secret that is available to every branch can be read by code an agent wrote and committed. `cinderline add` warns when it detects such secrets.

### GitHub

Move secrets from repository or organisation secrets to an environment (Settings → Environments) whose "Deployment branches and tags" is set to protected branches or selected branches, or has required reviewers enabled. Reference the environment with `environment:` in the workflow.

### GitLab

Mark CI/CD variables as "Protected" (Settings → CI/CD → Variables), for both project and group variables, so that only protected branches and tags receive them. Ensure `cinderline/*` is not a protected branch pattern.

### Forgejo / Gitea

Actions secrets have no branch protection: every workflow on every branch can read the repository's and its organisation's secrets, whatever its trigger. Keep deploy secrets out of both, for example by deploying from a separate repository the runner does not push to.

### Bitbucket Cloud

Repository and workspace variables reach every branch. Move them to deployment environments and restrict which branches or users can deploy (deployment permissions, Premium tier). The check cannot read those permissions via the API, so it warns when deployment variables exist.

## A Cinderline on the same machine

The runner uses the project's Cinderline URL both from the host and, through the run's egress proxy, from agent containers (for MCP), so that URL must work in both places. Agent containers no longer get `host.docker.internal` (decision 2026-11-09-network-allowlist); the proxy reaches the URL's host because it is the project's Cinderline host.

- **Docker Desktop (macOS, Windows):** use `http://host.docker.internal:<port>`. Docker Desktop resolves that name on the host and in containers, and forwards it to the host's loopback.
- **Docker Engine on Linux:** `host.docker.internal` does not exist there. Use the Docker bridge address, usually `http://172.17.0.1:<port>`, and let the backend listen on it or on all addresses (for example `php artisan serve --host=0.0.0.0`): a backend that listens only on `127.0.0.1` is not reachable from the proxy.

## Licence

The runner is fair source, not open source: it is licensed under the [Functional Source License, Version 1.1, MIT Future License](https://fsl.software) (FSL-1.1-MIT); see [LICENSE](LICENSE) and [NOTICE](NOTICE). You may read, modify and run it, also against your own backend for internal use, but not ship or host it as part of a commercial product or service that competes with Cinderline. Each version becomes available under the MIT licence two years after its release. Commercial licences are available from Rodo Software.
