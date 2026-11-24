# Cinderline runner 

The Cinderline runner is a native Go program on a developer's machine that runs agent teams on a Cinderline instance's issues, each turn in its own Docker container, pushes a branch and opens a pull request.

Status: milestones M0 to M5, M7 (Codex, Gemini CLI, `cinderline login`, client detection, errors reported to Cinderline), M8 (network allowlist) and M9 (the app decides each step) are built. Of M6, the per-project environment (`docker`) is built. See [docs/runner-rewrite-plan.md](docs/runner-rewrite-plan.md).

Cinderline decides, the runner does the mechanics and reports facts (decision 2026-11-16-runner-protocol-v2). The runner sends three requests: every `poll_after` seconds a poll, with its report (client states, errors) and what it holds, which gets the project's settings, the issue to run when it is idle, the runs to stop, what to do with kept sessions and the Review branches to keep current; at every step of a run `POST /runs/{ref}/next`, which reports the step just done and gets the next one from a closed set (run an agent, resolve a merge conflict, run the project's check command, publish, stop, done); and the download of an attachment a step names. Which issue runs, which agent works on which issue or sub-issue, the prompts, the budget, every comment and every status move are Cinderline's; the runner keeps the containers, git, the forge, the agent CLIs, sessions and its own safety checks, and writes nothing on an issue.

## Supported platforms

`cinderline` is one binary for x86-64 and ARM64. It needs Docker and nothing else on the machine: git runs in a container. Agent containers are Linux on every system; Windows containers are not supported.

| System | Versions | Docker |
|---|---|---|
| macOS | The current and two previous major releases, as Docker Desktop supports them; Apple silicon and Intel | Docker Desktop 4.29 or later |
| Linux | Any distribution that runs a supported Docker Engine; `cinderline service` needs systemd | Docker Engine 26.0 or later, or Docker Desktop 4.29 or later |
| Windows | Windows 10 22H2 (build 19045) or Windows 11 23H2 or later, 64-bit; ARM64 only as far as Docker Desktop's ARM build (early access) runs | Docker Desktop 4.29 or later with the WSL2 backend |

- An older Docker Engine is refused: an agent's container can then be given only its own issue's clone (`volume-subpath`), and older engines also forward DNS lookups from the agents' internal network, a way out past the network allowlist (CVE-2024-29018). Docker Desktop 4.29 ships Engine 26.0.
- On a Windows build below 19045 every command except `cinderline version` stops with an error naming the build it found. Windows Server is not supported: Docker Desktop does not run on it.
- CI runs the checks on macOS on GitHub and on Linux on Forgejo (Windows is off for now, decision 2026-11-12-ci-speedups); [docs/ci/platforms.yml](docs/ci/platforms.yml) widens that to the oldest and newest images per system and architecture.

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

`cinderline add` asks for the Cinderline instance's URL (`https`; plain `http` only on `localhost` or a loopback address, for local development), the project's runner token, the repository as an HTTPS URL without a user name or password (it offers the one the project names in Cinderline and refuses another) and a git token. It checks Docker first (installed, daemon running, at least 5 GB free), checks the runner token against the instance as it asks, stores both tokens in the OS keychain (or a 0600 file in the state folder where there is none) and the rest in `config.json`, and reminds you to check that CI cannot read secrets from the runner's branches (see "CI secrets" below). When other projects are configured, it offers to copy their model credentials. A running runner picks the project up at once. `cinderline remove [-url URL] [-workspace SLUG] KEY` removes a project, its secrets, its Docker volumes (the clones and the clients' sign-ins) and its paused notes; a running runner drops it at once.

`cinderline key list --project KEY` shows which of a project's secrets are set and where, never their values. `cinderline key set NAME --project KEY` replaces one later, read from stdin (not echoed when typed on a terminal), and `cinderline key delete NAME --project KEY` deletes one. NAME is `runner-token`, `git-token`, or `claude`, `opencode`, `codex` or `gemini` for that client's model key. A new runner token is checked against the instance and must belong to the project; the git token and model keys are stored without a check, as `add` does. A running runner reloads after each change.

A project is its Cinderline URL, its workspace and its key: project keys are unique per workspace, so one account in two workspaces on the same URL can have a project `APP` in each, and both run side by side. `add` stores the workspace the instance names (`default` for a Cinderline without workspaces) and refuses a project that is already added, naming the one it is. Docker volumes, containers, networks and labels are named `cinderline-<workspace>-<KEY>`, or by the key alone in the `default` workspace, so two projects with one key and workspace on different instances would share them: `add` refuses the second, and one written into `config.json` by hand does not run. Commands that take a project key accept `-url` and `-workspace` to choose between projects that share it.

Each project entry has an `id`, set at `add` as that same name, which names everything the runner keeps for it on this machine: Docker volumes, keychain entries, the outbox and session notes. A key renamed in Cinderline changes nothing here by itself: the runner keeps working under the old key and `id`. To follow the rename, run `cinderline rename --project OLD`. It asks the instance, with the stored runner token, for the project's key now, and gives the entry that key and the `id` a newly added project would get. The entry's settings stay, and its tokens and keys move to the new name. Docker cannot rename volumes, so the old ones are removed: the clones are fetched again with the next run, kept sessions are gone, and a client signed in with `cinderline login` must sign in again. `rename` refuses while the project runs an issue, has a paused run, or has reports the instance has not received yet, because the volumes may then hold work that is not on the forge. An entry that an older runner wrote or renamed keeps the name it had until `rename` is run on it.

### The git token

`add` stores the git token without trying it. The first run is the first place a missing permission shows. The runner uses the token in two places. A throwaway git helper container uses it to clone, fetch and push `cinderline/<REF>` branches over HTTPS; pushes are never forced. The runner process uses it to call the forge's API. It never enters an agent container. Give it access to this one repository only, with the permissions below. Scope names were checked against each forge's documentation on 2026-11-10; those marked *(not checked)* could not be confirmed there.

**GitHub and GitHub Enterprise:** a fine-grained personal access token, limited to the repository:

| Permission | Used for | Without it |
|---|---|---|
| Metadata: read (always granted) | finding the default branch when the project names no base branch | no run starts |
| Contents: read and write | clone, fetch, push | no run starts, or its branch is not pushed |
| Pull requests: read and write | opening and finding the pull request, auto-merge (squash, GraphQL `enablePullRequestAutoMerge`) for trusted teams *(not checked: the docs name no permission for the mutation)* | the branch is pushed but no pull request is opened or merged |
| Checks: read | merge status: failed check runs on the pull request's head, and their names, summaries and links for Cinderline *(not checked: GitHub's docs disagree on whether fine-grained tokens can call this endpoint on github.com)* | the issue's merge status is not updated, and failed CI is not sent back to the agent |
| Actions: read | the end of the log of a failed GitHub Actions job, sent to Cinderline with the failed check | the failed check is sent without its log |
| Workflows: read and write (optional) | pushing commits that change `.github/workflows/` | GitHub refuses that push |

A classic token needs `repo`, plus `workflow` if agents may change workflows. It reaches every repository you can, so prefer the fine-grained one. Auto-merge also needs "Allow auto-merge" turned on in the repository's settings.

**GitLab (gitlab.com and self-managed):** a project access token, or a personal access token, with the scope `api`. No narrower scope works: `write_repository` covers git but not the API, so it cannot open merge requests. `api` covers both. The role matters too:

- Developer: clone, fetch and push `cinderline/` branches, open merge requests, read their pipeline status and the failed jobs' logs.
- The role the default branch's protection allows to merge, Maintainer by default: auto-merge (squash, remove the source branch once the pipeline succeeds). With a lower role, GitLab refuses the merge request.

**Forgejo and Gitea:** an access token (Settings → Applications) with:

| Scope | Used for | Without it |
|---|---|---|
| `write:repository` | clone, fetch, push, opening and finding pull requests, merge when checks succeed (squash, delete the branch), merge status and failed checks from the commit status, and the end of a failed Forgejo Actions job's log *(not checked: whether this scope reads the Actions API)*. The log needs the Actions jobs API, which Forgejo 17 has and Forgejo 15 does not; without it the failed check is sent without its log | no run starts, or its pull request is not opened or merged |

Where Forgejo offers tokens limited to chosen repositories, they allow repository and issue scopes, which is enough to run issues.

**Bitbucket Cloud:** a repository access token (Repository settings → Access tokens). A project or workspace access token also works but reaches more. The runner sends the token as a Bearer token and as the password of the git user `x-token-auth`, so app passwords and user API tokens do not work.

| Permission | Used for | Without it |
|---|---|---|
| Pull requests: Write (`pullrequest:write`, includes `repository:write` and `repository`) | clone, fetch, push, default branch, opening and finding pull requests, merge status and failed checks from the head commit's build statuses | no run starts, or its pull request is not opened |

The runner does not read Bitbucket Pipelines logs: a failed check goes to Cinderline without its log.

The runner never merges on Bitbucket: its API cannot merge when builds pass, so you merge yourself.

Last, `add` asks for the project's model keys, each optional (press Enter to skip): an Anthropic API key or `claude setup-token` token for Claude Code, an OpenRouter key for OpenCode, an OpenAI API key for Codex and a Gemini API key (or a Vertex AI express-mode key, see `gemini_vertex` below) for Gemini CLI. Prefer a spend-limited API key to a subscription. OpenCode runs with an OpenRouter key, a model provider of the runner (see "Local models"), or both. Like the tokens, keys are typed in plain text and stored in the keychain; none is printed back.

`cinderline key set CLIENT --project KEY` (CLIENT is `claude`, `opencode`, `codex` or `gemini`) sets or replaces one key later, read from stdin, e.g. `cinderline key set opencode --project WEB`, then paste the key and press Enter; `cinderline key delete CLIENT --project KEY` deletes it. Add `-url URL` when projects on several instances share the key.

Then start it with `cinderline run` (foreground, until Ctrl-C) or as a service (below).

`cinderline run -v` also prints the log on the terminal, one readable line per event, and adds Debug lines (each runner API request) to both the terminal and the log file.

### Extra repositories

A run can read other repositories of the project, read-only. `cinderline add-repo --project KEY` asks for a repository URL (`https://host/path`, without a user name or password), an optional branch or tag (empty for the default branch) and a git token, and checks with `git ls-remote` that the token can read it. The token is given to git as an HTTPS password, so the runner takes HTTPS URLs only, for the project's own repository too; an SSH URL is refused. Each token is stored for its URL alone. `cinderline list-repos --project KEY` lists them and `cinderline remove-repo --project KEY NAME` removes one (NAME is the folder name `list-repos` shows, or the URL).

Before each run the runner fetches them in a git container that alone gets their tokens and mounts them read-only at `/extra/NAME` in the agent's container; it tells Cinderline which it fetched, and Cinderline's prompt lists them. One that fails to fetch is left out, and the runner tells Cinderline, which says so in a progress update. Cinderline knows nothing about them (app decision 2026-11-11-extra-repositories-in-runner).

### Local models

An OpenCode agent can run on a model you host, on any server with an OpenAI-compatible API (llama.cpp, Ollama, LM Studio, vLLM and so on). Add the server to the runner once as a named provider; every project on the runner can then use it, simply by giving an agent in Cinderline the model `<provider>/<model id>`, the id as the server knows it; `openrouter/...` keeps using OpenRouter. The runner gives OpenCode the provider's URL and key for that turn only, and the turn's proxy allows exactly that host and port. A turn whose provider this runner does not have is not started. The runner tells Cinderline the names of its providers, never their URLs or keys, and the model ids each serves: it asks each provider's `GET <URL>/models` (with its key) at start and every 10 minutes, so Cinderline can offer them. A provider that does not answer within 5 seconds is reported without models, and its model is typed by hand.

`cinderline add-provider NAME URL` adds one: NAME is lower-case letters, digits and dashes (not `openrouter`), URL the server's base URL with its port and path, such as `http://studio.local:8080/v1`. It asks for an API key; press Enter when the server needs none. The key is one secret for the whole runner, kept with the other secrets. `cinderline key set provider:NAME` changes it and `cinderline key delete provider:NAME` removes it, both without `--project`. `cinderline list-providers` lists the providers, whether each has a key and which are free, and `cinderline remove-provider NAME` removes one with its key. Providers are in the top-level `providers` list of `config.json`, not in a project.

Cinderline does not know what a provider's models cost, so it shows them as "price unknown". When they cost nothing per token, such as a model on your own machine, add the provider with `--free` (`cinderline add-provider --free NAME URL`): the runner then tells Cinderline it is free, and Cinderline prices its models at $0 in the orchestrator's prompt, the team editor and the usage. Without `--free` a provider is not free, which is right for a paid endpoint with a key. `cinderline set-provider NAME --free=true` or `--free=false` changes it later and keeps the key.

A server on another machine, say a Mac Studio at `studio.local` running llama.cpp's `llama-server --host 0.0.0.0 --port 8080`:

```sh
cinderline add-provider cerebellum http://studio.local:8080/v1
```

and an agent with the model `cerebellum/qwen3-coder-30b`. The runner's machine must reach that address; the agent reaches it only through the proxy.

A server on the runner's own machine, say Ollama on its default port:

```sh
cinderline add-provider --free ollama http://localhost:11434/v1
```

and an agent with the model `ollama/qwen3-coder:30b`. A URL with `localhost`, `127.0.0.1`, `[::1]` or `host.docker.internal` is reached as `host.docker.internal` on exactly its port: only the run's proxy maps that name to the host, so the agent itself still has no route to the host and no other host port. On Docker Desktop (macOS, Windows) a server listening on `127.0.0.1` works. On Docker Engine on Linux the name maps to the Docker bridge address, so the server must listen on that address or on all addresses (`OLLAMA_HOST=0.0.0.0`, `llama-server --host 0.0.0.0`); one that listens only on `127.0.0.1` is not reachable. An `https` server on the same machine must have a certificate for `host.docker.internal`. A provider is the only way to reach the runner's own machine: a project's `allow_hosts` refuses `localhost`, loopback addresses and `host.docker.internal`.

### Linked projects

A project can link to another one in Cinderline: "A may reach B", one way, never chained (decision 2026-11-11-project-links). Links are managed in Cinderline; the runner only reads them. A run of A can then read and edit B's issues over MCP, and, when B is also added on this runner (same Cinderline URL and key), read all of B's repositories: B's own repository at its base branch and B's extra repositories, mounted read-only at `/linked/KEY/NAME`. They are fetched before every run, resume and follow-up, each with the token stored for its URL, and Cinderline's prompt lists those the runner fetched. All changes still go into A's own repository; the runner never pushes to B's. When B is not added on this runner, the run goes on without its code and Cinderline says so in a progress update; `cinderline add` it to give runs its code. A repository that fails to fetch is left out and reported, as above.

### Attachments

Files attached to an issue in Cinderline (at most 5, 10 MB in total) reach its run read-only at `/attachments/NAME`. Cinderline names the files each agent turn gets, such as a sub-issue's and its parent's, and its prompt lists them. The runner downloads them with its runner token into the project's folder `attachments/KEY` in its state folder, emptied first, and bind-mounts that folder. It refuses a name that is not one safe file name (a path separator, a character a file system refuses, or one already taken ignoring case) or a file outside the run's issue and its sub-issues: that turn does not start, and Cinderline hears why.

### Sign a client in instead of storing a key

`cinderline login claude --project KEY`, `cinderline login codex --project KEY` or `cinderline login gemini --project KEY` runs the CLI's own sign-in (`claude auth login`, `codex login --device-auth`, Gemini CLI's own Google sign-in) in a container. Open the link it prints and paste the code back. The credentials stay on that project's state volume of that client, which only that client's runs mount; the runner never reads them. A stored Anthropic, OpenAI or Gemini key wins over a login, so a successful `login` deletes that client's stored key for the project and says so; a later `cinderline key set` for that client wins over the login again. OpenCode uses the project's OpenRouter key or the runner's model providers. After `login gemini`, type `/quit` once it says "Authentication succeeded": the interactive Gemini CLI stays open after signing in.

The daemon reports each client's state (ready, missing the binary, missing a credential, or the provider refused the key) in its runner report, so the Cinderline instance can show what a project is set up for. Logins on the state volume count as configured but are not tested, so an expired login only fails on the first run.

Claude Code subscription limits are reported as `usage_limit` when its result has `api_error: usage_limit_reached` or its last `rate_limit_event` is `rejected`. The reset time, when reported as `resetsAt`, is sent in UTC. The session is kept so a person can continue it after the limit resets; the runner does not wait or retry automatically. Codex, Gemini CLI and OpenCode currently keep reporting `failed`: their supported output formats have no verified subscription-limit signal distinct from ordinary rate limiting. A generic HTTP 429 remains `failed` for every client. Deploy the app's CL-269 support before releasing a runner that sends `usage_limit`.

## Run as a service

`cinderline service install` registers a user-level service (a launchd LaunchAgent on macOS, a systemd user unit on Linux, a logon scheduled task on Windows) that runs `cinderline run` at login and starts it now; `cinderline status`, `cinderline logs` and `cinderline stop` talk to it; `cinderline service uninstall` stops and removes it. On Linux it needs systemd. `cinderline stop` stops it until the next login. Linux and Windows restart it after a crash; macOS does not, so check `cinderline status` there.

`cinderline uninstall` asks for confirmation (`-yes` skips it), then removes the service, the runner's containers, networks, volumes and images (the default image, built or pulled, the proxy's and those built for a project's agent service), the config and state folders and every project's secrets. It lists each project's Cinderline URL and key: revoke that runner token in Cinderline yourself. It leaves the binary and prints its path for you to delete.

## Configuration

`config.json` lives in the `cinderline` folder of the OS's user config folder (`~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows), with the log and other state in its `state` subfolder. It holds no secrets. Besides what `add` writes, you can set:

- `keep_sessions_for`: how long a finished run's agent session and worktree are kept after its last run, so that moving the issue back to To do continues the same session with its full context; days such as `"7d"` or hours such as `"36h"`, `"7d"` by default, `"off"` for no age limit. Sessions of Done, Cancelled or archived issues are always deleted.
- `min_free_disk`: when free space on Docker's disk (where the worktree and session volumes are) is below this, kept sessions are deleted oldest first; such as `"10GB"`, in `MB`, `GB` or `TB`, `"10GB"` by default, `"off"` to never delete them for space. The runner applies both limits, per project, to the sessions Cinderline says it may expire (those of issues no run needs now), at start, after each run and every ten minutes, leaving out the issues being worked on; a worktree with unpushed commits is never deleted. A follow-up whose session was deleted starts fresh, and Cinderline says so on the issue.
- `providers`: the runner's model providers for OpenCode, for every project, which `add-provider` writes (see "Local models").
- `strict_hosts`: `true` keeps Cinderline from opening a host for any project's runs, which `cinderline strict-hosts on|off` sets (see "Network"); off by default.
- `allow_hosts`: extra `host:port` entries every current and future project's agents may reach (see "Network"), which `allow-host` without `--project` writes, never `localhost`, a loopback address or `host.docker.internal`.
- Per project: `allow_hosts`, extra `host:port` entries that project's agents may reach (see "Network"), which `allow-host --project KEY` writes, never `localhost`, a loopback address or `host.docker.internal`; `memory`, the memory limit of its agent containers (see "Environments"), which `memory set|clear --project KEY` writes; `max_parallel_runs`, the most of its team runs at once, 1 to 10, `1` by default: with more, Cinderline offers one issue per lane and each runs in its own slot, worktree and branch, each slot taking its own share of the memory budget, while the Project Manager keeps its own slot next to them (needs a Cinderline with lanes; an older one still gets one run at a time); `openai_base_url`, an OpenAI-compatible base URL for Codex; `gemini_vertex`, `true` when the Gemini key is a Vertex AI express-mode key; `extra_repositories`, which `add-repo` writes (see "Extra repositories").

Unknown fields are refused, except those an older runner wrote (`names_by_key`, `allow_existing`, `merge_poll_seconds`, `build_image`): they are ignored with a warning and left out the next time the file is written. `add`, `rename`, `key set`, `key delete` (also of a provider's key), `login`, `add-repo`, `remove-repo`, `allow-host add|remove`, `memory set|clear`, `add-provider`, `set-provider`, `remove-provider` and `strict-hosts on|off` make a running runner reload the file; `remove` reloads it too. A reload cuts no run: each project takes the change once its own runs have finished, taking no new work meanwhile, and the command returns at once, naming the issues that still run on the old config; `cinderline status` shows those projects until they have. Only `remove` and `rename` wait, for their own project's runs, before they remove its Docker volumes. Press `s` while they wait, and confirm with `y`, to stop the runner at once instead, after which the service starts again: at the next poll Cinderline moves the issues of the runs it cut to Blocked with a note naming the branch, and a person moves them back to To do to run them again. Ctrl-C ends the wait and keeps the project's volumes. After a hand edit, restart the runner.

## Network

Each issue runs on its own internal Docker network with an egress proxy, its only way out (decision 2026-11-09-network-allowlist). The proxy allows exact `host:port` pairs, no wildcards:

- the model provider of the run's client only (`api.anthropic.com`, `openrouter.ai`, `api.openai.com` or the `openai_base_url` host, `generativelanguage.googleapis.com` or `aiplatform.googleapis.com`, or for OpenCode the host and port of the runner's provider the agent's model names), plus that provider's sign-in hosts when the run uses a sign-in;
- the project's Cinderline host, for MCP;
- the package registries: `repo.packagist.org`, `packagist.org`, `codeload.github.com`, `api.github.com`, `registry.npmjs.org`, `proxy.golang.org`, `sum.golang.org`, on port 443;
- the runner's and project's `allow_hosts`, which refuse `localhost`, loopback addresses and `host.docker.internal`;
- the hosts a person allowed for the issue in Cinderline, unless strict host blocking is on (below).

No forge (git fetch and push run in a separate helper container), and `host.docker.internal` only on the port of a provider on the runner's own machine, for a turn that uses it (see "Local models"). The run's log names each refused host, and the runner report counts them.

After each turn and each check, the runner tells Cinderline which hosts the proxy refused during it (decision 2026-11-17-hosts-from-cinderline). Cinderline names them on the issue and asks a person whether to open them: *Yes, this run* opens a host for that issue's next runs only, checked as an `allow_hosts` entry is, so never the runner's own machine. To open a host for every run, add it to the project's `allow_hosts` in `config.json` with `cinderline allow-host add HOST:PORT --project KEY`, or omit `--project KEY` (with an extra confirmation prompt) to allow it for all current and future projects; nothing in Cinderline does that. `cinderline allow-host list [--project KEY]` lists the entries and `cinderline allow-host remove HOST:PORT [--project KEY]` removes one. The change applies to every run started after the command, not to one already going.

With strict host blocking on (`cinderline strict-hosts on`, for every project of the runner), the runner ignores every host Cinderline allows and tells Cinderline so, which then does not ask: the agent goes on without the host. `cinderline strict-hosts` shows whether it is on.

## Environments

A project runs in the default environment unless an admin sets its environment to `docker` in Cinderline: the default image (PHP 8.4, Composer, Node 22, git) on the network above, with no other service and no database. With `docker`, each turn runs in the project's own compose file, read from the base branch only (`.cinderline/compose.yml` by default, path and agent service set in Cinderline), so a pull request cannot change what runs for its own issue. The agent runs in the agent service's image (`agent` by default), pulled or built, with that service's environment, hardened as in the default environment; the other services start beside it with `docker compose up --wait` on the run's internal network, behind the same egress proxy, and everything is removed after the turn. The agent service's own command, entrypoint, user and mounts are not used. This needs `docker compose`; it runs with only the variables the docker CLI needs, so the file cannot read the runner's environment.

Each agent turn and each check runs in a container limited to 4 GiB of memory, without swap, so a turn that needs more is killed rather than taking memory from other runs or the machine. `cinderline memory set 8g --project KEY` sets another limit for one project (`m` or `g`, 1024-based as in `docker run --memory`, at least `256m`), `cinderline memory show --project KEY` shows it and `cinderline memory clear --project KEY` returns to the default. A `mem_limit` on the agent service of a project's compose file wins over both. The other services of the file keep their own `mem_limit`, or none. The egress proxy is limited to 64 MiB.

Runs start only while their memory fits. The runner shares one budget among the runs of all its projects: Docker's total memory (`docker info`: the VM's size on Docker Desktop, the machine's on Linux) less 1 GiB for the proxies, the git helpers and Docker itself. A run counts its agent container's limit plus each other service of the compose file: its `mem_limit`, or 512 MiB without one. A slot whose run does not fit in what is left is not offered to Cinderline until other runs end; the log says so once, and `cinderline status` shows the budget, what is reserved and which projects wait. A run larger than the whole budget runs only when nothing else does. Docker Desktop's memory is set under Settings, Resources; on macOS the runner sees only that VM, so other apps that fill the Mac's own memory still make macOS swap.

A project that needs a database brings it in that file. For a Laravel project with MySQL 8, as `.cinderline/compose.yml`:

```yaml
services:
  agent:
    image: ghcr.io/rodo-software/cinderline-runner-default:v0.1.1
    environment:
      DB_CONNECTION: mysql
      DB_HOST: mysql
      DB_PORT: "3306"
      DB_DATABASE: laravel_testing
      DB_USERNAME: laravel
      DB_PASSWORD: laravel
  mysql:
    image: mysql:8.0
    environment:
      MYSQL_DATABASE: laravel_testing
      MYSQL_USER: laravel
      MYSQL_PASSWORD: laravel
      MYSQL_ROOT_PASSWORD: root
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "--host=127.0.0.1", "--user=laravel", "--password=laravel"]
      interval: 2s
      retries: 30
```

The agent's image here is the default image. For another toolchain, see "A project's own toolchain" below.

A file is refused, with the reason in a progress line on the issue, when:

- a service sets a key outside `build`, `cap_drop`, `command`, `configs`, `cpus`, `depends_on`, `entrypoint`, `env_file`, `environment`, `expose`, `healthcheck`, `image`, `init`, `mem_limit`, `networks`, `pids_limit`, `platform`, `pull_policy`, `read_only`, `restart`, `secrets`, `shm_size`, `stdin_open`, `stop_grace_period`, `stop_signal`, `tmpfs`, `tty`, `ulimits`, `user`, `volumes` and `working_dir`. So no `privileged`, `cap_add`, `security_opt`, `runtime`, `devices`, `ports`, `network_mode`, `pid`, `volumes_from`, `extra_hosts`, `labels` and so on;
- a build sets a key outside `context`, `dockerfile`, `dockerfile_inline`, `args`, `target` and `additional_contexts`, or its service also sets `image`;
- a service has a bind mount, joins another network than the default one or sets options such as aliases on it, or is named `proxy`;
- a volume is external or sets anything (`driver`, `driver_opts`, `name`, `labels`), or a network is external;
- a secret or config comes from the environment;
- a build context, Dockerfile, `env_file`, secret or config file lies outside the repository. Symlinks in the repository are left out of the copy compose reads.

### A project's own toolchain

A project that needs a language or tool the default image lacks brings its own agent image (decision 2026-11-16-project-toolchains):

1. Put a Dockerfile and a `.cinderline/compose.yml` in the repository and merge them into the base branch. The runner reads only that branch. The agent service gets `build: .` to build the Dockerfile beside the file, or `image:` to pull a published image, but not both.
2. An admin sets the project's environment to `docker` in Cinderline.

The runner builds or pulls the image on its own machine before each turn. What the image needs:

- `/bin/sh` and a glibc or musl loader: the agent CLIs come from the runner's tools volume, so the image needs none of them. It needs no git token either, because the runner fetches and pushes outside it. Install git anyway: the agents use it on their work.
- Everything writable for uid 1000:1000, which the runner runs it as: `HOME` and the caches of the toolchain. Either give uid 1000 a user with a home, or set `HOME` and the caches to `/tmp`.
- The toolchain at build time. During a turn the agent reaches only the hosts under "Network", and PyPI is not among them: add `pypi.org:443` and `files.pythonhosted.org:443` to the project's `allow_hosts` (`cinderline allow-host add pypi.org:443 --project KEY`) if its agents install Python packages.

The agent service's `command`, `entrypoint`, `user` and mounts are ignored; its `environment` is used. Other services, such as a database, start beside it as in the MySQL example above.

Examples, each a Dockerfile and a `compose.yml` to copy into `.cinderline/`. Each passes the check above:

- Go: this repository's own [`.cinderline/`](.cinderline/), with staticcheck and govulncheck, `HOME` in `/tmp`.
- Python: [`docs/toolchains/python/`](docs/toolchains/python/), with pytest and ruff in a virtual environment uid 1000 owns.
- Node: [`docs/toolchains/node/`](docs/toolchains/node/), whose `node` user is uid 1000 already.

Each `compose.yml` is the same three lines:

```yaml
services:
  agent:
    build: .
```

## Update

`cinderline update` runs the install script above over the binary you run it from: it downloads the latest release, checks it against `checksums.txt` and replaces the binary where it is, leaving your `PATH` alone. The new binary then refreshes the tools volume and default image, and the running runner finishes its runs before it stops (press `s` and confirm with `y` to stop it at once, cutting them); `cinderline stop --drain` does only the last part. If the service is installed, `cinderline update` starts it again with the new binary.

When Cinderline needs a newer runner, it answers `426` with the minimum version. The runner then takes no new work for that project, logs one error and shows it in `cinderline status` ("cinderline needs runner ≥ X; run `cinderline update` and then `cinderline run`"), and asks again only once an hour.

Agent containers run the default image, `ghcr.io/rodo-software/cinderline-runner-default`, pulled by a pinned digest for linux/amd64 and linux/arm64. When the pull fails, the run waits for the next poll; the runner builds the image locally from `images/default/Dockerfile` only while that file has no published pin.

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

The runner's agents push to `cinderline/<REF>` branches. Any CI secret that is available to every branch can be read by code an agent wrote and committed. The runner does not check this for you: `cinderline add` only reminds you. Check it yourself, per forge:

### GitHub

Move secrets from repository or organisation secrets to an environment (Settings → Environments) whose "Deployment branches and tags" is set to protected branches or selected branches, or has required reviewers enabled. Reference the environment with `environment:` in the workflow.

### GitLab

Mark CI/CD variables as "Protected" (Settings → CI/CD → Variables), for both project and group variables, so that only protected branches and tags receive them. Ensure `cinderline/*` is not a protected branch pattern.

### Forgejo / Gitea

Actions secrets have no branch protection: every workflow on every branch can read the repository's and its organisation's secrets, whatever its trigger. Keep deploy secrets out of both, for example by deploying from a separate repository the runner does not push to.

### Bitbucket Cloud

Repository and workspace variables reach every branch. Move them to deployment environments and restrict which branches or users can deploy (deployment permissions, Premium tier).

## A Cinderline on the same machine

The runner uses the project's Cinderline URL both from the host and, through the run's egress proxy, from agent containers (for MCP), so that URL must work in both places. Agent containers no longer get `host.docker.internal` (decision 2026-11-09-network-allowlist); the proxy reaches the URL's host because it is the project's Cinderline host.

- **Docker Desktop (macOS, Windows):** use `http://host.docker.internal:<port>`. Docker Desktop resolves that name on the host and in containers, and forwards it to the host's loopback.
- **Docker Engine on Linux:** `host.docker.internal` does not exist there. Use the Docker bridge address, usually `http://172.17.0.1:<port>`, and let the backend listen on it or on all addresses (for example `php artisan serve --host=0.0.0.0`): a backend that listens only on `127.0.0.1` is not reachable from the proxy.

## Licence

The runner is source available under the [Functional Source License, Version 1.1, ALv2 Future License](LICENSE) (FSL-1.1-ALv2): you may use, change and share it for any purpose except a competing commercial product or service, and each version becomes Apache 2.0 two years after its release. See also [NOTICE](NOTICE).
