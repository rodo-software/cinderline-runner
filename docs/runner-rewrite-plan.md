# CinderStack runner rewrite — Plan

Written on 2026-11-04 for EMBER-20. The direction was decided by Willem on 2026-11-04 in decision 2026-11-04-runner-rewrite-host-program (app repository); the open points were settled on 2026-11-05 in decision 2026-11-05-runner-rewrite-details (here). This plan says what gets built and in which order. Where it says **Open**, Willem decides before that milestone starts. Where it says **Unchecked**, a claim has to be verified before code depends on it.

**Names.** The backend app, called Ember in older documents, decisions and issue keys, is CinderStack. The runner's command, config folder, service, keychain entries, branch prefix, MCP server name and version header are `cinderstack`, held in one constant. Issue keys such as EMBER-20 and the names of existing decisions stay as they are. Section 4 (the app's side) stays in the app repository; everything else lives here.

## 1. Goal

Today's runner (`runner/` in the app repository; a copy in `legacy/` here, git-ignored, is the reference for the port) is a Node script that runs in one Docker container. The agents run in that same container, as the same user. The new runner is:

- **A native Go program on the host** for macOS, Linux and Windows (x86-64 and ARM64). It holds the configuration and secrets of every project on the machine, talks to CinderStack and the forges, and starts containers through the `docker` command.
- **A container per agent run**, built from the project's environment. It gets the agent CLIs, the project's toolchain, the issue's clone, the model credential that one agent needs and, for an agent that may use CinderStack, a short-lived token scoped to the issue. The runner's own token and the git token never enter it.
- **Installed with one line and running as a service**, with `cinderstack status`, `cinderstack logs` and the rest talking to the service over a local API.
- **Built for change:** new agent CLIs, forges, environments and front ends (a tray app or a local web page later) are new implementations of small interfaces, not rewrites.

When the new runner can do everything today's runner does (section 3, milestone M4), the app's project switches to it and `runner/` leaves the app repository. That completes EMBER-20.

## 2. Decisions this plan rests on

Made by Willem on 2026-11-04; recorded in decision 2026-11-04-runner-rewrite-host-program.

| Topic | Choice |
| --- | --- |
| Shape | A native program on the host, a container per agent run |
| Language | Go |
| Repository | `github.com/rodo-software/cinderstack-runner`, public, MIT. Runner issues get their own project in CinderStack |
| Concurrency | One run at a time per project, projects in parallel. A scheduler with one slot per project, so more slots per project is a setting later (0091 allows different issues in parallel) |
| Environments | The default image (PHP and Node, like today), a per-project image (`docker`, EMBER-28) and an existing setup (`existing`, EMBER-29). `host` and `devcontainer.json` are not in this plan |
| Agent CLIs | A read-only **tools volume** of pinned CLI binaries, which the runner downloads on the user's machine and mounts into any image |
| Workspace | Per environment: Docker volumes with git in a helper container for Docker environments; the host's filesystem and git for `existing` |
| Daemon | `cinderstack service install` and a local API that the CLI uses, and later a tray app or local web page |
| Secrets | The OS keychain, with a 0600 file where there is none |
| Extensibility | In-tree Go interfaces (`Client`, `Forge`, `Environment`, `Workspace`), one file per implementation. No plugin system |
| Protocol | The runner talks only to a versioned REST runner API in CinderStack. Agents keep MCP, each run with its own short-lived token scoped to the issue |
| Scope | Everything today's runner does, plus every runner-side item in the launch backlog (section 6) |
| Command name | `cinderstack`, in one constant |
| Who builds it | Teams through today's runner, once Go is added to its image (milestone M0) |

Made by Willem on 2026-11-05; recorded in decision 2026-11-05-runner-rewrite-details, with the reasons.

| Topic | Choice |
| --- | --- |
| Order | M1 (app) before M2; M2 is finished against the real API |
| OpenAPI file | Authored here under `docs/runner-api/`, mirrored into the app repository and tested there |
| Scoped tokens | The agent tools; the runner asks with a `kind`: `orchestrator`, `writer` or `reader` |
| Local backend | Agent containers get `host.docker.internal:host-gateway`; a backend on the same machine is `http://host.docker.internal:<port>` |
| Claude subscriptions | The keychain holds an API key or a `claude setup-token` token per project, as today; `cinderstack login claude` comes in M7 |
| Credentials | Per project; `cinderstack add` offers to copy them from an existing project. Single-user program |
| Service identity | User level: LaunchAgent, systemd user unit, a logon scheduled task on Windows |
| Git isolation | A clone per issue in a runs volume; the token helper fetches from it read-only and pushes from its own clone |
| MySQL | A fresh container per run, removed with it |
| `cinderstack stop` | Stops at once; the drain comes with `update` in M5 |
| Logs | A rotating file in the state folder; `logs` prints the tail and follows |
| Signing | GitHub artifact attestations; installers verify SHA-256 |
| Install location | `~/.local/bin`, `%LocalAppData%\Programs\cinderstack`; no sudo |
| M6 to M8 | Their open items wait for their milestone |

## 3. Milestones

Each milestone ends with Pest, Pint and Larastan passing on the app side, and with `gofmt`, `go vet`, `staticcheck`, `go test -race` and `govulncheck` passing on the runner side. Then work stops for Willem's review. Each milestone becomes issues: app items in EMBER, runner items in the runner project.

### M0. Bootstrap

- **Willem:** creates the GitHub repository `rodo-software/cinderstack-runner` (done: it exists), the runner project in CinderStack with that repository URL, a team for it, and its runner token.
- **App repository:** add a pinned Go toolchain to `runner/Dockerfile` so teams can build and test Go. Agents in today's container have no Docker, so tests that need Docker run in the new repository's CI, not in the agent's run.
- **Second runner:** today's runner serves one project per folder (R5). Copy `runner/` to its own folder with its own `.env` for the runner project; the folder name gives it its own compose project.
- **Repository skeleton:** `LICENSE` (MIT, Rodo Software, decision 2026-11-04-licences-before-publishing), `NOTICE`, `README.md`, `AGENTS.md` (already adapted, in the root), `docs/decisions/` (already started), `go.mod` with the module path above and the latest stable Go, `cmd/cinderstack/main.go` with `cinderstack version`, `internal/name` with the one name constant, and a GitHub Actions CI that runs the checks above on Linux, macOS and Windows plus a GoReleaser snapshot build of every target, and a test that listens and dials on a Unix socket on each platform (for M2's local API).
- **Done when:** a team on the runner project can build and test `cinderstack version` in its run, and CI is green on the three platforms.

### M1. CinderStack: runner API and scoped agent tokens (app repository)

The runner API is a third thin adapter over the same Actions, Policies, Rules and Resources (the app's AGENTS.md); no business rule moves into it. Today's MCP tools stay as they are, so the old runner keeps working until M4. M1 is built and merged before M2 starts.

- **Routes** under `/api/runner/v1`, Sanctum token with a new `runner` ability. The runner token belongs to the runner user of one project (0099), so every route is limited to that project by the existing policies.
- **Endpoints**, covering what today's runner does over MCP:
  - `GET project`: repository URL, forge, base branch, environment.
  - `GET work?wait=25`: long-poll for the first To do issue with a team, answered as soon as there is one or after the wait, with an ETag so an unchanged answer is a 304.
  - `POST issues/{ref}/claim`: move to In progress with `expected_status`, 409 when someone else took it.
  - `GET issues/{ref}`, `GET issues/{ref}/comments`, `GET teams/{id}`.
  - `POST issues/{ref}/comments`, `POST issues/{ref}/progress`, `PATCH issues/{ref}` (status with `expected_status`, merge status).
  - `GET issues?status=…`, for recovery (In progress), keeping review branches current and checking merges (Review).
  - `PUT report`: runner version, clients with version and state, environments. Stored as columns on `personal_access_tokens` (D5, EMBER-41).
  - `POST heartbeat` (EMBER-43), also sent during long runs.
  - `POST runs/{ref}/agent-token` with `kind` and `DELETE runs/{ref}/agent-token`, below.
- **Versions:** the runner sends `CinderStack-Runner-Version`. The app answers `426` with the minimum version when the runner is too old, and states its own API version in every response (EMBER-44).
- **Scoped agent tokens:** for each agent run that may use CinderStack, the runner asks for a token with a `kind`. `orchestrator` and `writer` get the agent tools: `get_issue`, `list_comments`, `add_comment`, `create_issue` for sub-issues, `update_issue` within the issue tree, `ask_person`, `reply_person`. `reader`, for the orchestrator's own sub-issue, gets only `get_issue` and `list_comments`. No kind reaches other issues. The token is named after the team and agent, so the Actor detects the agent from it, as today (0045). It expires after the team's `max_minutes` plus a margin, and the runner deletes it when the run ends. Workers and the Merge Resolver still get no CinderStack access (0122).
- **An OpenAPI description** of the runner API, authored in this repository under `docs/runner-api/openapi.yaml` and mirrored into the app repository, where a test checks it against the routes. It is the contract for anyone who builds a runner (0104).
- **Unchecked:** a long-poll holds one PHP-FPM worker per waiting runner for up to 25 seconds. With one runner per project and one instance per customer (D1) that should be fine, but it depends on the site's pool size (`pm`, `pm.max_children`) and on nginx's `fastcgi_read_timeout`, which must be above the wait. Check both on the CloudPanel host before M1, and size the wait to fit.
- **Done when:** Pest covers every endpoint and each token kind's policy (dataset-driven: every endpoint against runner, the three scoped kinds and person tokens, other projects, archived projects), and the OpenAPI file matches the routes.

### M2. Runner core (Go), no agents yet

- **Packages** (all `internal/`):
  - `name`: one constant for the command, config folder, service, keychain, branch prefix, MCP server name and version header.
  - `config`: the projects on this machine, each with a CinderStack URL (several instances are allowed: an agency may use a customer's instance), its project key, repository, environment allowance (`existing` must be allowed locally, 0098) and local paths. A versioned JSON file in the user's config folder (`os.UserConfigDir()/cinderstack/config.json`); logs and state beside it, the socket in the runtime folder.
  - `secrets`: the keychain with a 0600-file fallback. One entry per project per secret: the runner token, the git token, and the model credentials (an Anthropic API key or a `claude setup-token` token, an OpenRouter key). `cinderstack add` offers to copy the model credentials from a project already configured.
  - `cinderstack`: the REST client from M1, including the long-poll, the version handshake, the heartbeat (every 60 seconds) and the scoped tokens.
  - `scheduler`: one slot per project. Per slot: recovery at start, then claim, run, publish, and the timers for review branches and merge checks. A slot that fails never stops the others.
  - `daemon` and `localapi`: the service process and an HTTP API on a local socket in a 0700 folder, no further auth (status, logs as a stream, stop, reload configuration). The CLI is a client of that API. The daemon writes a rotating log file in the state folder; `cinderstack logs` prints its tail, then follows the stream. `cinderstack stop` stops at once; a run in flight is recovered at the next start, as today. `cinderstack run` refuses to start while the service holds the socket.
  - `cli`: `cinderstack add` (adds a project and checks its token, URL and repository as it asks, EMBER-26), `cinderstack run` (foreground), `cinderstack status`, `cinderstack logs`, `cinderstack stop`, `cinderstack remove`, `cinderstack version`.
- **Interfaces with fakes:** `Client`, `Forge`, `Environment` and `Workspace` are defined here with in-memory fakes, so M3 can be tested without Docker, git or a network.
- **Logging:** `log/slog`, one structured line per event, tagged with project and issue. The local API streams it.
- **Dependencies:** Go's standard library, plus only `github.com/zalando/go-keyring` (keychain on three OSes) and `github.com/kardianos/service` (LaunchAgent and systemd user unit, used in M5; on Windows the runner registers a logon scheduled task through `schtasks` instead, so it runs as the user who owns the keychain and Docker Desktop). Each gets its reason in `docs/decisions/`.
- **Local socket on Windows:** checked on paper on 2026-11-05. Go supports Unix sockets on Windows since issue 26072 was fixed, and Windows has them since build 17063 (Windows 10 1803). Docker Desktop, which the runner needs, requires Windows 10 22H2 (build 19045) or Windows 11 23H2, so every supported machine has them. Go itself still supports Windows Server 2016 (build 14393), which lacks them; the runner doesn't support it. M0's CI runs a socket test on `windows-latest`, so this is checked by running code before M2 depends on it.
- **Done when:** `cinderstack add` and `cinderstack run` connect to a real CinderStack, report and heartbeat, find no work or skip it, and `cinderstack status` shows that, on all three platforms.

### M3. The run itself, against fakes

A port of today's behaviour in `legacy/runner.mjs` and `legacy/lib.mjs`. Every behaviour below is first a Go test ported from the `node --test` suites in `legacy/` (about 3,400 lines), then the code. Nothing in this list changes; it's the parity list.

- **Claim and recovery:** claim with `expected_status`. At start, put the project's team issues that are In progress back to To do, and drop paused notes whose issue is closed (0114). An issue that can't be run (unsupported client, missing key) is skipped before the claim, so it doesn't drain To do. From the claim on, every failure ends in a comment and Review.
- **Repository pin:** refuse a repository from CinderStack that differs from the one configured locally, so the git token never follows a changed URL (0114).
- **Attempts:** a failed run is retried once. A run that hit the turn limit continues its own session with a fresh budget; any other failure starts over. A second failure posts the output's tail (0095).
- **Questions:** after `ask_person` the issue is Blocked or back in To do. The run pauses, its clone and session stay, and it resumes in the same session with the resume prompt (0116, 2026-11-03-reply-person). A paused run whose team changed between writer and coding starts over and names the last commit (2026-11-03-issue-writer-teams).
- **Comments during a run:** a comment cursor by ID. Resume the session to read new comments from a person; retry that once; publish with the unread note if it fails twice (2026-11-03-comments-during-a-run).
- **Writer teams:** keep the original request as a comment once, use the writer prompt, publish nothing, and report written or not written (2026-11-03-issue-writer-teams).
- **Teams with workers:** rounds of the orchestrator; sub-issues in creation order, one at a time in one clone; commit and push after every sub-issue; the orchestrator's own sub-issue in a fresh session with a `reader` token; failed workers marked **Failed**; resume after a restart from the paused note (0122).
- **Branch upkeep:** merge the base into continued work before the first turn, and again before the push. One verified attempt by the team's `Merge Resolver`: a clean worktree, base and old head both ancestors, no conflict markers, no `Blocked` report, otherwise reset. Keep Review branches current with git only; ask for the merge again for trusted teams (2026-11-04-agent-resolves-merge-conflicts, 2026-11-04-runner-updates-branch-before-push).
- **Publish:** commit what was left, push `cinderstack/<REF>` (never the base), open or find the pull request, then, for a trusted team without conflicts, request the merge and wait for it every 15 seconds for at most 15 minutes; the wait holds the project's slot, as today. Set the merge status. Check the merge status of trusted Review issues every `MERGE_POLL_SECONDS` (2026-11-04-runner-waits-for-merge, 2026-11-04-merge-status, 0114).
- **Progress lines** at every step (2026-11-04-run-progress-updates).
- **Base branch:** the project's base branch, else the remote's default branch (2026-11-04-base-branch).
- **Prompts:** the same texts, with the issue text last and marked as data, and the CI-file rule for `.forgejo/`, `.github/`, `.gitlab/`, `.gitlab-ci.yml` and `bitbucket-pipelines.yml`. The only changes: "Ember" becomes "CinderStack", the MCP server is named `cinderstack`, and the branch is `cinderstack/<REF>`.
- **What changes on purpose:** the runner's own state (paused notes, client sessions) lives on a per-project state volume. Workflow state still lives only in issues (0091). Agent labels travel in the scoped token instead of the `X-Ember-Agent` header. The branch prefix is `cinderstack/` instead of `ember/`; branches of issues in Review at the switch are merged by hand once.
- **Done when:** every ported test passes against the fakes, and the old suite's cases are ticked off in a table in the pull request.

### M4. Real implementations; switch over

- **`Workspace`, volume-backed, two sides.** The **helper side**: a small pinned git image that alone holds the git token (through `GIT_ASKPASS`, as today) and owns the project's clone in a `repo` volume. It fetches, and it pushes. The **agent side**: each issue gets its own clone in a `runs` volume, made from the helper's clone with alternates, so objects are shared and the `repo` volume is mounted read-only into agent containers. The helper never runs git inside the agent's clone: to push, it fetches the branch from the agent's clone over a read-only mount into its own clone, then pushes from there, so a hook or `url.insteadOf` an agent planted can never run with the token or redirect it. Commits, base merges, the resolver's checks and conflict detection run in the agent's clone by a git container without the token. Line endings and file modes are fixed by the runner's own git configuration, never the host's.
- **`Environment`, default:** today's toolchain (PHP 8.4, Composer, Node 22, git) as an image without agent CLIs, built locally from `images/default/Dockerfile` until M5 publishes it. Every run gets a private network with a fresh MySQL 8 container, removed with the run, and the `DB_*` variables today's compose file sets. One agent container per run, started with `docker run --rm` and `--add-host host.docker.internal:host-gateway`; the team's `max_minutes` ends it with `docker stop`, which replaces today's process-group handling and works the same way on Windows. Output streams to the log; the last 4,000 characters are kept for comments, as today.
- **Tools volume:** the runner downloads pinned versions of Claude Code and OpenCode, and the ponytail plugin at a pinned tag, from their official sources on the user's machine. It checks them by hash (EMBER-57) and fills a read-only volume, one per pinned set and shared by every project behind a lock, that every agent container mounts. Downloading on the user's machine, rather than publishing an image with them, also avoids redistributing Claude Code (2026-11-04-licences-before-publishing). Checked on 2026-11-05 with arm64 builds in Docker:
  - **Claude Code** publishes native binaries per platform, glibc and musl separately, at `downloads.claude.ai/claude-code-releases/<version>/<platform>/claude`, with SHA-256 checksums in `<version>/manifest.json`. Each build runs only on its own libc: the glibc build on Debian, Ubuntu and `php:8.4-cli-bookworm`, the musl build on Alpine and `php:8.4-cli-alpine`. No Node is needed. About 320 MB per build.
  - **OpenCode** publishes the same split as npm platform packages (`opencode-linux-<arch>[-musl]`), whose tarballs hold one binary and carry a SHA-512 integrity hash. No Node is needed. The musl build also needs `libstdc++` and `libgcc_s`, which a bare Alpine image lacks. Shipping those two files in the tools volume, with `LD_LIBRARY_PATH` set for that run, works on bare Alpine.
  - So the runner detects the image's libc before a run (for example, whether `/lib/ld-musl-*` exists, as Claude Code's own installer checks) and mounts the matching builds.
- **`Client`:** Claude Code and OpenCode, with today's arguments, session handling, output parsing and turn limits (0122, 2026-11-03-per-team-ponytail-plugin). Sessions and config dirs on the state volume (0116).
- **`Forge`:** GitHub and GitHub Enterprise, GitLab, Forgejo and Gitea, Bitbucket Cloud, ported with their current quirks and tests (0117, 2026-11-04-gitlab-and-forge-per-project, 2026-11-04-github-enterprise, 2026-11-04-bitbucket-cloud).
- **Switch:** run the new runner on the runner project first. Then on the EMBER project, replacing the old runner (one runner per project, D7). After two weeks of real issues without a regression, `runner/` leaves the app repository and its README points at this one (EMBER-20 done).
- **Done when:** both projects run on the new runner, and the integration tests run in CI on Linux against real Docker and a throwaway Forgejo.

### M5. Distribution and life cycle

- **Releases** with GoReleaser on tags: binaries for six targets, a checksums file, GitHub artifact attestations (build provenance) instead of a signing key, and a changelog that names the pinned CLI versions (R14).
- **Install** with one line: a `curl … | sh` script for macOS and Linux and a PowerShell one for Windows. Each verifies the SHA-256 against the checksums file and puts `cinderstack` in `~/.local/bin` or `%LocalAppData%\Programs\cinderstack`, without sudo, and says so when PATH needs a line (EMBER-23). Homebrew stays later (D2).
- **`cinderstack service install` and `uninstall`** on the three platforms, at user level: a launchd LaunchAgent, a systemd user unit, a Windows logon scheduled task.
- **`cinderstack update`:** replaces the binary, refreshes the tools volume and images, and restarts the service once no issue is mid-run (EMBER-22). The same drain is `cinderstack stop --drain`.
- **`cinderstack uninstall`:** removes the service, volumes, images, configuration and keychain entries, and tells the person to revoke each runner token in CinderStack (EMBER-52).
- **Prerequisite checks with a clear message:** Docker installed, the daemon running, enough disk (R4).
- **No defaults that point at Rodo's instance** (EMBER-25).
- **Setup check for CI secrets:** `cinderstack add` warns when the repository's CI can read secrets from a runner branch, and the README explains the protected-environment setup (EMBER-56).
- **Platforms:** the supported OS versions, stated and tested in CI. Windows needs Docker Desktop with WSL2; agent containers are Linux everywhere (EMBER-30).
- **Default image:** published multi-arch on GHCR as `ghcr.io/rodo-software/cinderstack-runner-default`, without agent CLIs (EMBER-21, reframed).
- **Done when:** a clean macOS, Linux and Windows machine goes from the install line to a first pull request with only the README.

### M6. Environments

- **Per-project image (`docker`, EMBER-28):** CinderStack gets a project environment field set by admins (0098: choices in the app, never from issue text). The runner pulls or builds the image, mounts the tools volume and the clone, and starts the services.
- **Existing setup (`existing`, EMBER-29):** the runner runs agents in a service of the developer's own Docker Compose or Lando setup. It must be allowed in local configuration (0098), and the clone is never the developer's working tree.
- **Open (before M6):**
  - Where a per-project image's services (MySQL and others) are defined: in CinderStack, or in a compose file in the repository read from the base branch only.
  - For `existing`: how the tools get into a container that is already running (copy them in, or a sidecar), where the clone lives so that container can see it, and which secrets may enter a container the runner does not control.
- **Done when:** a non-PHP project (say Python) runs on its own image, and CinderStack itself runs in its Lando setup, both end to end.

### M7. Clients and reporting

- **Codex** (EMBER-37) and **Gemini CLI** (EMBER-38) as `Client` implementations, each with headless flags, session resume, a turn or step limit, JSON output parsing and a credential, checked against the current version of that CLI.
- **`cinderstack login <client>`** runs the CLI's own login in a container, without the CLI on the host (EMBER-39). For Claude Code this is the alternative to a stored `setup-token` token: the credentials then live on the project's state volume.
- **Detection:** per client, the binary is in the tools volume, its credential is configured and the credential works (EMBER-40). The result goes into the runner report (EMBER-42).
- **Errors reach CinderStack:** a repository mismatch, a missing key, an unsupported client or failed polls are reported rather than only logged (EMBER-45). **Open:** through the runner report, or as a comment on the skipped issue (D8 says computed, not stored).
- **Done when:** a team that mixes all four clients runs end to end, and a missing key shows up in CinderStack.

### M8. Network allowlist

- Agent containers sit on an internal network whose only way out is a small egress proxy with an allowlist (EMBER-58).
- **Open (before M8):** what the default allowlist holds (the model providers, the CinderStack URL, the forge, package registries such as Packagist and npm) and who can extend it (local configuration only, or also CinderStack).
- **Done when:** an agent can reach what its run needs and nothing else, tested in CI.

## 4. The app's side (stays in the app repository)

- M0: Go in `runner/Dockerfile`.
- M1: runner API, scoped agent tokens with kinds, runner report columns (EMBER-41), heartbeat (EMBER-43), versions (EMBER-44), the mirrored OpenAPI file and its route test.
- M6: the project environment field.
- After M4: remove `runner/`, update the README, AGENTS.md and the plans that describe the runner in Docker.
- **Not in this plan:** the app UI that uses the runner report (EMBER-46 to EMBER-51). Those issues stay in EMBER and become possible after M1 and M7.

## 5. Out of scope

- The `host` environment and `devcontainer.json` support.
- More than one run at a time per project (the scheduler allows it later).
- A tray app or local web page (the local API allows it later).
- A plugin system for clients or forges.
- Homebrew (D2: later).
- Several people sharing one runner install: it is a single-user program.
- The gate, send-backs and plan approval (EMBER-53, EMBER-54): Phase 9. The runner's job engine must leave room for an extra step before publishing, but builds none.
- Everything in the v1 plan's and the agent teams plan's "Out of scope" lists.

## 6. Launch backlog items this plan absorbs

| Issue | Where |
| --- | --- |
| EMBER-20 runner in its own repository | M0, M4 |
| EMBER-21 prebuilt image | M5 (default image without CLIs) |
| EMBER-22 `cinderstack update` | M5 |
| EMBER-23 one-line install | M5 |
| EMBER-24 several projects per install | M2 |
| EMBER-25 no defaults pointing at Rodo's instance | M5 |
| EMBER-26 setup that validates as it asks | M2 |
| EMBER-28 `docker` environment | M6 |
| EMBER-29 `existing` environment | M6 |
| EMBER-30 supported platforms | M5 |
| EMBER-37 Codex, EMBER-38 Gemini CLI | M7 |
| EMBER-39 `cinderstack login` | M7 |
| EMBER-40 client detection | M7 |
| EMBER-41 report columns | M1 |
| EMBER-42 runner report | M1 (endpoint), M7 (contents) |
| EMBER-43 heartbeat | M1, M2 |
| EMBER-44 protocol version | M1 |
| EMBER-45 errors reach CinderStack | M7 |
| EMBER-52 `cinderstack uninstall` | M5 |
| EMBER-55 agent runs apart from the runner | M4 (separate container, by design) |
| EMBER-56 CI secrets check | M5 |
| EMBER-57 pinned CLIs by hash | M4 |
| EMBER-58 network allowlist | M8 |

## 7. Checks

Checked on 2026-11-05:

- **Agent CLI binaries (M4):** Claude Code and OpenCode both have standalone glibc and musl builds with published hashes. Tested in Docker; see M4.
- **Unix sockets on Windows (M2):** every Windows that Docker Desktop supports has them. Checked on paper; M0's CI checks them by running code.
- **Claude Code's terms (M4, M7):** [Legal and compliance](https://code.claude.com/docs/en/legal-and-compliance), read on 2026-11-05. Claude Code is under Anthropic's Commercial Terms (API, Team, Enterprise) or Consumer Terms (Free, Pro, Max, Ultra). For products that run Claude Code "in hosted sandboxes or other agent infrastructure", it asks for:
  - the Commercial Terms;
  - the binary installed and run as published, unmodified;
  - every end user signing in with their own API key, subscription or cloud provider credential, billed to them, with no reselling or intermediating.
  
  The tools volume meets the binary condition: it holds unmodified binaries, downloaded from Anthropic on the user's own machine, and no image of ours redistributes them. Users bring their own keys (0104).
  
  The same page also says developers "may not collect, store, or intermediate Claude.ai credentials or session tokens — sign-in to a Claude account must complete through Anthropic's own flow". It also says subscription usage limits "assume ordinary, individual usage". Today's runner keeps a subscription token (`CLAUDE_CODE_OAUTH_TOKEN`) in its `.env` and hands it to the agent.

  **Decided on 2026-11-05 (decision 2026-11-05-runner-rewrite-details):** the keychain keeps doing the same, per project, because `claude setup-token` is Anthropic's own mechanism for headless use; `cinderstack login claude` (M7) adds the sign-in flow for people who prefer it. **Still open:** whether Rodo Software asks Anthropic if automated team runs on a subscription count as ordinary use.

Still to check:

- **PHP-FPM pool and nginx timeout on the production host (M1),** see M1.
- **Headless flags and session behaviour of Codex and Gemini CLI** at the time of M7.
