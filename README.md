# Cinderline runner

The Cinderline runner is a native Go program on a developer's machine that takes To do issues from a Cinderline instance, runs an agent team on each in its own Docker container, pushes a branch and opens a pull request.

Status: milestones M0 to M5, M7 (Codex, Gemini CLI, `cinderline login`, client detection, errors reported to Cinderline) and M8 (network allowlist) are built. Of M6, the per-project environment (`docker`) is built; the existing setup (`existing`) is decided (decision 2026-11-08-m6-environments) but not built. See [docs/runner-rewrite-plan.md](docs/runner-rewrite-plan.md).

## Supported platforms

`cinderline` is one binary for x86-64 and ARM64. It needs Docker and nothing else on the machine: git runs in a container. Agent containers are Linux on every system; Windows containers are not supported.

| System | Versions | Docker |
|---|---|---|
| macOS | The current and two previous major releases, as Docker Desktop supports them; Apple silicon and Intel | Docker Desktop |
| Linux | Any distribution that runs a supported Docker Engine; `cinderline service` needs systemd | Docker Engine 25.0.5 or later (or 23.0.11 or later of 23.0), or Docker Desktop |
| Windows | Windows 10 22H2 (build 19045) or Windows 11 23H2 or later, 64-bit; ARM64 only as far as Docker Desktop's ARM build (early access) runs | Docker Desktop with the WSL2 backend |

- An older Docker Engine is refused: it forwards DNS lookups from the agents' internal network, a way out past the network allowlist (CVE-2024-29018).
- On a Windows build below 19045 every command except `cinderline version` stops with an error naming the build it found. Windows Server is not supported: Docker Desktop does not run on it.
- CI runs the checks on macOS and Windows on GitHub and on Linux on Forgejo; [docs/ci/platforms.yml](docs/ci/platforms.yml) widens that to the oldest and newest images per system and architecture.

## Install

Install the latest `cinderline` binary with one line — no sudo or elevation required. Each script downloads the archive for your OS and architecture, verifies its SHA-256 checksum against the release's `checksums.txt` (refusing on mismatch), and installs the binary.

**macOS / Linux:**

```
curl -fsSL https://raw.githubusercontent.com/rodo-software/cinderline-runner/main/scripts/install.sh | sh
```

Installs to `~/.local/bin/cinderline`. If that directory is not on your `PATH`, the script appends `export PATH="$HOME/.local/bin:$PATH"` to your shell's profile (`~/.zshrc` for zsh, `~/.bashrc` for bash or `~/.bash_profile` on macOS, `~/.profile` otherwise), once, and tells you to open a new terminal. For fish and other shells with a different syntax, or with `CINDERLINE_NO_MODIFY_PATH=1` set (`curl … | CINDERLINE_NO_MODIFY_PATH=1 sh`), it leaves the profile alone and prints the line to add yourself.

**Windows (PowerShell 5.1+):**

```
irm https://raw.githubusercontent.com/rodo-software/cinderline-runner/main/scripts/install.ps1 | iex
```

Installs to `%LocalAppData%\Programs\cinderline\cinderline.exe`. If that directory is not on your user `PATH`, the script adds it there (the user `PATH`, not the machine one) and tells you to open a new terminal. With `$env:CINDERLINE_NO_MODIFY_PATH = '1'` set first, it prints the command to add it instead.

To pin a specific version, set `CINDERLINE_DOWNLOAD_URL` before running (e.g.
`https://github.com/rodo-software/cinderline-runner/releases/download/v0.1.0`).

## Add a project

`cinderline add` asks for the Cinderline instance's URL, the project's runner token, the repository (it offers the one the project names in Cinderline and refuses another) and a git token. It checks Docker first (installed, daemon running, at least 5 GB free), checks the runner token against the instance as it asks, stores both tokens in the OS keychain (or a 0600 file in the state folder where there is none) and the rest in `config.json`, and warns when CI can read secrets from the runner's branches (see "CI secrets" below). When other projects are configured, it offers to copy their model credentials. A running runner picks the project up at once. `cinderline remove [-url URL] KEY` removes a project, its secrets, its Docker volumes (the clones and the clients' sign-ins) and its paused notes; a running runner drops it at once.

`cinderline manage` opens an interactive screen in the terminal: pick a project with the arrow keys to see which tokens and model keys it holds and where (keychain or fallback file), never their values, then replace or delete one; or add a project, with the same questions and checks as `add` but with tokens hidden as you type. A replaced runner token is checked against the instance and must belong to the project; an extra repository's token is checked with `git ls-remote`; the git token and model keys are stored without a check, as `add` does. A running runner reloads after each change. Ctrl-C goes back one screen.

A project key can be added once: Docker volumes are named by the key, so a second project with the same key on another instance is refused by `add`, and one written into `config.json` by hand does not run.

### The git token

`add` stores the git token without trying it. The first run or the CI secrets check is the first place a missing permission shows. The runner uses the token in two places. A throwaway git helper container uses it to clone, fetch and push `cinderline/<REF>` branches over HTTPS; pushes are never forced. The runner process uses it to call the forge's API. It never enters an agent container. Give it access to this one repository only, with the permissions below. Rows marked *CI secrets check* are optional: without them, `add` prints "could not check whether CI on the runner's branches can read secrets" and still adds the project. Scope names were checked against each forge's documentation on 2026-11-10; those marked *(not checked)* could not be confirmed there.

**GitHub and GitHub Enterprise:** a fine-grained personal access token, limited to the repository:

| Permission | Used for | Without it |
|---|---|---|
| Metadata: read (always granted) | finding the default branch when the project names no base branch | no run starts |
| Contents: read and write | clone, fetch, push | no run starts, or its branch is not pushed |
| Pull requests: read and write | opening and finding the pull request, auto-merge (squash, GraphQL `enablePullRequestAutoMerge`) for trusted teams *(not checked: the docs name no permission for the mutation)* | the branch is pushed but no pull request is opened or merged |
| Checks: read | merge status: failed check runs on the pull request's head *(not checked: GitHub's docs disagree on whether fine-grained tokens can call this endpoint on github.com)* | the issue's merge status is not updated |
| Workflows: read and write (optional) | pushing commits that change `.github/workflows/` | GitHub refuses that push |
| Secrets: read | CI secrets check: repository and organisation secrets | the check fails |
| Actions: read | CI secrets check: listing environments | the check fails |
| Environments: read | CI secrets check: secrets of environments any branch can deploy to | the check fails |

A classic token needs `repo`, plus `workflow` if agents may change workflows. It reaches every repository you can, so prefer the fine-grained one. Auto-merge also needs "Allow auto-merge" turned on in the repository's settings.

**GitLab (gitlab.com and self-managed):** a project access token, or a personal access token, with the scope `api`. No narrower scope works: `write_repository` covers git but not the API, so it cannot open merge requests. `api` covers both. The role matters too:

- Developer: clone, fetch and push `cinderline/` branches, open merge requests, read their pipeline status.
- The role the default branch's protection allows to merge, Maintainer by default: auto-merge (squash, remove the source branch once the pipeline succeeds). With a lower role, GitLab refuses the merge request.
- CI secrets check: Maintainer to list the project's variables *(not checked: the API docs name no role)*, and Owner of the group to list the group's variables (GitLab's docs name Owner). A project access token cannot be a group's Owner, so for a project in a group the check fails unless you use the personal access token of a group Owner.

**Forgejo and Gitea:** an access token (Settings → Applications) with:

| Scope | Used for | Without it |
|---|---|---|
| `write:repository` | clone, fetch, push, opening and finding pull requests, merge when checks succeed (squash, delete the branch), merge status from the commit status | no run starts, or its pull request is not opened or merged |
| `read:organization` | CI secrets check: the organisation's Actions secrets, when the repository belongs to an organisation | the check fails |

For the CI secrets check, the token's user must also own the repository to read its secrets, and be an owner of the organisation to read the organisation's secrets. Gitea 1.25 requires this *(not checked for Forgejo)*. Where Forgejo offers tokens limited to chosen repositories, they only allow repository and issue scopes. Such a token can run issues, but the CI secrets check fails for a repository in an organisation.

**Bitbucket Cloud:** a repository access token (Repository settings → Access tokens). A project or workspace access token also works but reaches more. The runner sends the token as a Bearer token and as the password of the git user `x-token-auth`, so app passwords and user API tokens do not work.

| Permission | Used for | Without it |
|---|---|---|
| Pull requests: Write (`pullrequest:write`, includes `repository:write` and `repository`) | clone, fetch, push, default branch, opening and finding pull requests, merge status from the head commit's build statuses | no run starts, or its pull request is not opened |
| Pipelines: Read (`pipeline`) | CI secrets check: repository variables, deployment environments and their variables | the check fails |

The runner never merges on Bitbucket: its API cannot merge when builds pass, so you merge yourself. The check also reads the workspace's variables. A repository access token may be refused there *(not checked)*. In that case the check fails while the runs still work.

Last, `add` asks for the project's model keys, each optional (press Enter to skip): an Anthropic API key or `claude setup-token` token for Claude Code, an OpenRouter key for OpenCode, an OpenAI API key for Codex and a Gemini API key (or a Vertex AI express-mode key, see `gemini_vertex` below) for Gemini CLI. Prefer a spend-limited API key to a subscription. OpenCode runs only with an OpenRouter key. Like the tokens, keys are typed in plain text and stored in the keychain; none is printed back.

`cinderline key set CLIENT --project KEY` (CLIENT is `claude`, `opencode`, `codex` or `gemini`) sets or replaces one key later, read from stdin, e.g. `cinderline key set opencode --project WEB`, then paste the key and press Enter. Add `-url URL` when projects on several instances share the key.

Then start it with `cinderline run` (foreground, until Ctrl-C) or as a service (below).

`cinderline run -v` also prints the log on the terminal, one readable line per event, and adds Debug lines (each runner API request) to both the terminal and the log file.

### Extra repositories

A run can read other repositories of the project, read-only. `cinderline add-repo --project KEY` asks for a repository URL (`https://host/path`, `ssh://git@host/path` or `git@host:path`, without credentials), an optional branch or tag (empty for the default branch) and a git token, and checks with `git ls-remote` that the token can read it. Only HTTPS works in practice: the token is given to git as an HTTPS password, so an SSH URL fails that check. Each token is stored for its URL alone. `cinderline list-repos --project KEY` lists them and `cinderline remove-repo --project KEY NAME` removes one (NAME is the folder name `list-repos` shows, or the URL).

Before each run the runner fetches them in a git container that alone gets their tokens and mounts them read-only at `/extra/NAME` in the agent's container; the prompt lists them. One that fails to fetch is left out and the run says so in a progress update. Cinderline knows nothing about them (app decision 2026-11-11-extra-repositories-in-runner).

### Linked projects

A project can link to another one in Cinderline: "A may reach B", one way, never chained (decision 2026-11-11-project-links). Links are managed in Cinderline; the runner only reads them. A run of A can then read and edit B's issues over MCP, and, when B is also added on this runner (same Cinderline URL and key), read all of B's repositories: B's own repository at its base branch and B's extra repositories, mounted read-only at `/linked/KEY/NAME`. They are fetched before every run, resume and follow-up, each with the token stored for its URL, and the prompt lists them. All changes still go into A's own repository; the runner never pushes to B's. When B is not added on this runner, the run goes on without its code and says so in a progress update; `cinderline add` it to give runs its code. A repository that fails to fetch is left out and reported, as above.

### Sign a client in instead of storing a key

`cinderline login claude --project KEY`, `cinderline login codex --project KEY` or `cinderline login gemini --project KEY` runs the CLI's own sign-in (`claude auth login`, `codex login --device-auth`, Gemini CLI's own Google sign-in) in a container. Open the link it prints and paste the code back. The credentials stay on that project's state volume of that client, which only that client's runs mount; the runner never reads them. When the keychain holds no Anthropic, OpenAI or Gemini key for the project, runs then use this login; a stored key still wins. OpenCode uses the project's OpenRouter key only. After `login gemini`, type `/quit` once it says "Authentication succeeded": the interactive Gemini CLI stays open after signing in.

The daemon reports each client's state (ready, missing the binary, missing a credential, or the provider refused the key) in its runner report, so the Cinderline instance can show what a project is set up for. Logins on the state volume count as configured but are not tested, so an expired login only fails on the first run.

## Run as a service

`cinderline service install` registers a user-level service (a launchd LaunchAgent on macOS, a systemd user unit on Linux, a logon scheduled task on Windows) that runs `cinderline run` at login and starts it now; `cinderline status`, `cinderline logs` and `cinderline stop` talk to it; `cinderline service uninstall` stops and removes it. On Linux it needs systemd. `cinderline stop` stops it until the next login. Linux and Windows restart it after a crash; macOS does not, so check `cinderline status` there.

`cinderline uninstall` asks for confirmation (`-yes` skips it), then removes the service, the runner's containers, networks, volumes and its own image, the config and state folders and every project's secrets. It lists each project's Cinderline URL and key: revoke that runner token in Cinderline yourself. It leaves the binary and prints its path for you to delete.

## Configuration

`config.json` lives in the `cinderline` folder of the OS's user config folder (`~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows), with the log and other state in its `state` subfolder. It holds no secrets. Besides what `add` writes, you can set:

- `merge_poll_seconds`: how often the merge status of Review issues is checked; 60 by default.
- `build_image`: `true` builds the default image locally instead of pulling it.
- `keep_sessions_for`: how long a finished run's agent session and worktree are kept after its last run, so that moving the issue back to To do continues the same session with its full context; days such as `"7d"` or hours such as `"36h"`, `"7d"` by default, `"off"` for no age limit. Sessions of Done, Cancelled or archived issues are always deleted.
- `min_free_disk`: when free space on Docker's disk (where the worktree and session volumes are) is below this, kept sessions are deleted oldest first; such as `"10GB"`, in `MB`, `GB` or `TB`, `"10GB"` by default, `"off"` to never delete them for space. The runner checks both limits at start and before each run, per project; a paused run (waiting for an answer, or between an orchestrator's rounds) and a worktree with unpushed commits are never deleted. A follow-up whose session was deleted starts fresh and says so in a comment.
- Per project: `allow_hosts`, extra `host:port` entries its agents may reach (see "Network"); `openai_base_url`, an OpenAI-compatible base URL for Codex; `gemini_vertex`, `true` when the Gemini key is a Vertex AI express-mode key; `extra_repositories`, which `add-repo` writes (see "Extra repositories").

Unknown fields are refused. `add`, `manage`, `key set`, `login`, `add-repo` and `remove-repo` make a running runner reload the file, which aborts its runs in flight; they are recovered right after. `remove` reloads it too. After a hand edit, restart the runner.

## Network

Each issue runs on its own internal Docker network with a fresh MySQL 8 and an egress proxy, its only way out (decision 2026-11-09-network-allowlist). The proxy allows exact `host:port` pairs, no wildcards:

- the model provider of the run's client only (`api.anthropic.com`, `openrouter.ai`, `api.openai.com` or the `openai_base_url` host, `generativelanguage.googleapis.com` or `aiplatform.googleapis.com`), plus that provider's sign-in hosts when the run uses a sign-in;
- the project's Cinderline host, for MCP;
- the package registries: `repo.packagist.org`, `packagist.org`, `codeload.github.com`, `api.github.com`, `registry.npmjs.org`, `proxy.golang.org`, `sum.golang.org`, on port 443;
- the project's `allow_hosts`.

No forge (git fetch and push run in a separate helper container) and never `host.docker.internal`. The run's log names each refused host, and the runner report counts them.

## Environments

A project runs in the default environment above unless an admin sets its environment to `docker` in Cinderline. Then each turn runs in the project's own compose file, read from the base branch only (`.cinderline/compose.yml` by default, path and agent service set in Cinderline), so a pull request cannot change what runs for its own issue. The agent runs in the agent service's image (`agent` by default), pulled or built, with that service's environment, hardened as in the default environment; the other services start beside it with `docker compose up --wait` on the run's internal network, behind the same egress proxy, instead of MySQL, and everything is removed after the turn. The agent service's own command, entrypoint, user and mounts are not used. This needs `docker compose`; it runs with only the variables the docker CLI needs, so the file cannot read the runner's environment.

A file is refused, with the reason in a progress line on the issue, when a service has a bind mount, `privileged`, `network_mode`, another network, `pid`, `ipc`, `uts`, `cgroup` or `userns_mode` set to the host or another container, `cap_add`, devices, published ports or `volumes_from`; when a volume or network is external; when a secret or config comes from the environment; or when a build context, `env_file`, secret or config file lies outside the repository. Symlinks in the repository are left out of the copy compose reads. A service may not be named `proxy`.

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
