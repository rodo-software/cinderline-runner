# Cinderline runner: decisions in force

What is true for the runner now, as of 2026-11-17. This file replaces the records in [archive/](archive/), which are history and not binding: where a rule here and an archived record differ, this file wins. The dated records next to this file are in force too; see [README.md](README.md).

A new decision gets its own dated record next to this file, `YYYY-MM-DD-topic.md`, and is folded in here from time to time. Each rule ends with its sources: record names (in archive/ unless they sit next to this file), or Willem's answers of 2026-11-17. Decisions of the app repository are cited by their name there.

## What the runner is and is not

- The runner does the mechanics and reports facts; the app decides every step and writes every text a person reads on an issue. The runner never reads or writes an issue. (2026-11-16-runner-protocol-v2)
- What stays in the runner: containers, git, the forge calls, the agent CLIs, sessions and their notes, the limits for expiring sessions, verifying a resolved merge, the repository pin, and what may enter a container. (2026-11-16-runner-protocol-v2)
- The app writes every prompt: the agent's own instructions first, then the rules, then the issue text last, marked as data. The runner passes it on unchanged. (2026-11-06-prompts; 2026-11-16-runner-protocol-v2)
- Issues are the only workflow state (app decision 0091). The runner keeps only caches it may lose: session notes, the outbox, clones. (2026-11-05-runner-state-location; 2026-11-16-runner-protocol-v2)
- There are no writer teams and no writer run. (2026-11-06-no-writer-teams)
- The runner serves no local web page; runners are shown read-only in the hosted app. Controlling a runner from the app needs Willem's decision first, because it would move secrets through the server. (2026-11-11-runners-in-the-app)
- The runner runs no model itself: no model backends, no downloads, no GPU work. Running models is the customer's own matter. (2026-11-11-no-local-model-work; 2026-11-17-local-model-providers)

## Licence, repositories and releases

- The runner is source available under FSL-1.1-ALv2, copyright Rodo Software: any use except a competing commercial product, and Apache 2.0 two years after each release. Reason: no competitor may profit, while Cinderline's users may run it. The app stays closed source. (2026-11-16-fsl-licence; Willem 2026-11-17, answer 10)
- Day-to-day work, pull requests and the CI gate are on the private Forgejo repository. The public GitHub repository is a push mirror of main and tags, for public source, issues, releases and the macOS checks. (2026-11-05-forgejo-daily-github-public; Willem 2026-11-17, answer 19)
- Agents never edit `.forgejo/` or `.github/`, and never commit secrets, tokens, `.env` files or anything from `legacy/`, which stays git-ignored. (2026-11-05-forgejo-daily-github-public; 2026-11-05-legacy-reference)
- Releases and the default image carry build provenance attestations, because the GitHub repository is public; the attest steps skip themselves on a private repository. The installers check SHA-256 against `checksums.txt` either way. (2026-11-16-attest-when-public; 2026-11-05-runner-rewrite-details; Willem 2026-11-17, answer 19)
- The module and the GHCR images are `github.com/rodo-software/cinderline-runner`. (2026-11-05-runner-rewrite-details)
- Install is no-sudo: `~/.local/bin` on macOS and Linux, `%LocalAppData%\Programs\cinderline` on Windows. When that folder is not on PATH, `install.sh` adds the export to the shell's profile (not for fish and other syntaxes) and `install.ps1` to the user PATH; `CINDERLINE_NO_MODIFY_PATH=1` opts out. `cinderline update` runs the install script. (2026-11-05-runner-rewrite-details; 2026-11-11-install-path; 2026-11-11-audit-fixes)
- On a `426` the project takes no new work, logs once at Error, shows "cinderline needs runner ≥ X; run `cinderline update` and then `cinderline run`" in `status`, and polls again only after an hour. Auto-update is not built; it would be opt-in. (2026-11-16-upgrade-required)

## Language, dependencies and code rules

- Go's standard library first; every dependency needs a one-line reason in a record. The allowed ones:
  - `github.com/zalando/go-keyring`: the OS keychain on three systems without cgo. (2026-11-05-go-keyring)
  - `github.com/kardianos/service`: the LaunchAgent and the systemd user unit. (2026-11-07-kardianos-service)
  - `golang.org/x/term`: hidden input in `key set`; the Go team's, adding only `x/sys`. (2026-11-16-x-term)
- The name `cinderline` (command, config folder, service, keychain, branch prefix, MCP server name, version header) lives in one constant. (2026-11-05-runner-rewrite-details)
- The runner shells out to the `docker` command, never the Docker SDK. The only interfaces are `Client`, `Forge`, `Environment` and `Workspace`. (AGENTS.md)

## The runner API

- The contract is [2026-11-16-runner-protocol-v2](2026-11-16-runner-protocol-v2.md), amended by [2026-11-17-second-audit-decisions](2026-11-17-second-audit-decisions.md) and [2026-11-17-local-model-providers](2026-11-17-local-model-providers.md), with [`docs/runner-api/openapi.yaml`](../runner-api/openapi.yaml). Read them there; this file does not copy them.
- Three requests under `/api/runner/v2`: the poll, `runs/{ref}/next`, and the attachment download. A step never carries a command, a path, a docker option, an environment variable or a URL. (2026-11-16-runner-protocol-v2)
- The OpenAPI file is authored in the app repository and copied here; a contract change starts there. (AGENTS.md; 2026-11-16-runner-protocol-v2)
- `contractSHA256` in `internal/cinderline` pins the SHA-256 of the copied file, and a test fails when they differ: a copy updates the constant and the types in the same commit. (2026-11-26-contract-hash)
- A stop during publishing still pushes and opens the pull request but does not ask the forge to merge: `merge: null`. A stop while waiting for a merge already asked for reports `merge: pending`. (2026-11-17-second-audit-decisions)
- There is no `X-Cinderline-Agent` header; an MCP request's agent name is its token's name. (2026-11-17-second-audit-decisions)

## The loop, runs, sessions and tidying

- One loop per project and at most `max_parallel_runs` team runs at once: the project's own, else the runner-wide one at the top of `config.json`, else 1, each 1 to 10, set with `cinderline parallel`, each in its own slot, clone and branch, plus at most one Project Manager run next to them in its own slot: the poll always sends `manager_idle` and starts `manager_job`; with more than one team slot it sends `max_parallel` and starts `jobs`, one per free slot. (app decision 2026-11-24-lanes; 2026-11-24-housekeeping-next-to-runs; 2026-11-25-parallel-runs-default)
- The Review upkeep pass and tidying run next to the runs and never touch an issue in flight; a job for an issue of the pass in flight waits; tidying never overlaps a pass. (2026-11-24-housekeeping-next-to-runs)
- A manager run gets only `run` and `stop` steps, works in a fresh clone of the base removed at its end, and never commits or pushes, not at `stop` either (`not_pushed`); tidying never pushes its clone. (2026-11-16-runner-protocol-v2; app decision 2026-11-17-project-manager; CLR-194)
- A slot is offered (`idle`, `manager_idle`) only while its run fits in the runner's one memory budget, which all projects share: Docker's `MemTotal` less 1 GiB. A run counts its agent container's limit plus each other compose service's `mem_limit`, or 512 MiB without one. The slot reserves that memory when it sends the poll, frees it when the answer brings no job, and its run holds it until it ends; a run larger than the whole budget runs alone. Waiting for memory is not reported to the app. (2026-11-24-agent-memory-limit; CLR-216)
- A run that does not start leaves the issue in To do; the loop then waits 1 minute, doubling to 10. (2026-11-07-publish-resilience)
- A session resumes only when its clone still exists in the runs volume (2026-11-11-audit-fixes), except a Project Manager's, which resumes in a fresh clone at the same dir (CLR-194).
- A reload (`POST /reload`) re-reads `config.json`; a config that fails to load keeps the old one, and the caller gets the error. Each project loop then drains on its own and starts again with the newest config once nothing is in flight; a removed project drains and ends. No run is cut. (2026-11-05-reload-config; 2026-11-24-rolling-reload)
- The team's `max_turns` is passed to every agent turn; the app always sends a number. (2026-11-07-team-max-turns)
- The idle limit is the project's `idle_minutes`, for every agent turn. Claude Code runs with `--output-format stream-json --verbose`, so one idle timer on the output stream serves every client. (2026-11-14-run-failsafes)
- The check runs the project's `check_command` through `sh -c` in the clone, after committing and pushing, in the agent's environment without credentials, client state or tools, with the team's `max_minutes` as limit. Fixes, verdicts and what follows a failure are the app's. (2026-11-16-check-step)
- The runner verifies a resolved merge (clean tree, base and old head are ancestors, no conflict markers, no "Blocked" report) and undoes one it cannot verify. (2026-11-07-solo-team-resolves-merge-conflicts)
- Tidying pushes stranded clones (commits no remote branch has), never forced; a failure only logs. (2026-11-07-publish-resilience; 2026-11-11-audit-fixes)

## Git, workspaces and branches

- Each issue gets its own clone in a runs volume, sharing objects through alternates. The git token stays in a helper container, which never runs git in the agent's clone. (2026-11-05-runner-rewrite-details)
- An agent's or check's container mounts only its issue's folder of the runs volume, at `/runs/<REF>`, with the repo volume read-only; the runner's own git containers mount the whole volume. (2026-11-19-docker-26)
- Pushes are never forced. (2026-11-07-publish-resilience)
- Review upkeep is git only, no agent: one fetch per pass finds the branches that are behind; a stranded clone is pushed before it is replaced, and nothing is replaced when the list fails. (2026-11-07-solo-team-resolves-merge-conflicts; 2026-11-11-audit-fixes)
- A configured project is its Cinderline URL, workspace slug and key. Docker objects, keychain entries and state files are named by its `id`, set at `add` to `<workspace>-<KEY>`, or the key alone in workspace `default`; an entry without one gets that name from its key when it is loaded. `add` refuses a duplicate, and the daemon runs only the first of two with one ID. (2026-11-15-workspace-in-project-identity; 2026-11-17-project-key-rename)
- After a key rename in the app, `rename --project OLD` takes the new key from the instance and gives the entry the `id` of that key: settings stay, secrets move, and the old Docker volumes, session notes and sign-ins are removed. It refuses while the project runs an issue, has a paused run or has undelivered reports. The runner never changes a key or an `id` by itself. (2026-11-17-rename-moves-local-names)
- Fields an older runner wrote in `config.json` are ignored with a warning; any other unknown field is refused. (2026-11-17-project-key-rename)
- Project links are one way and never chain. A's agents may read and edit B's issues over MCP and read B's repositories read-only; every change stays in A's repository. The runner fetches B's code only when B is added on this runner in the same workspace; otherwise the run goes on without it. (2026-11-11-project-links; 2026-11-15-workspace-in-project-identity)

## Forges and pull requests

- Publish (base merge, push, pull request) is tried 5 times, waiting 30 seconds and doubling. (2026-11-07-publish-resilience)
- Forgejo finds a branch's pull request by `head.label`, walking the list, never `pulls/{base}/{head}`. (2026-11-07-publish-resilience)

## Environments, containers and the default image

- There are two environments: `default` and `docker`. The `existing` environment is dropped. (Willem 2026-11-17, answer 5)
- Every agent turn runs in its own `docker run --rm` container on its own internal network, stopped with `docker stop` at the team's `max_minutes`, so no process outlives a turn. `Environment` is `Run` plus `Cleanup`. (2026-11-06-container-per-turn)
- The default environment is the default image behind the egress proxy, with no other service and no database. (2026-11-11-docker-compose-environment)
- A `docker` project's services come from a compose file in its repository, read from the base branch only (default `.cinderline/compose.yml`, agent service `agent`, both set in Cinderline), run on the run's internal network behind the proxy and removed with `down -v`. (2026-11-08-m6-environments)
- The base branch's tree is copied without symlinks; `docker compose config --format json` is checked against an allowlist of keys; an override puts every service on the run's network. Compose gets only the variables the docker CLI needs. Recovery removes labelled compose volumes. (2026-11-11-docker-compose-environment)
- The agent runs with `docker run` in the agent service's image, hardened as in the default environment, never with that service's command, entrypoint, user or mounts. (2026-11-11-docker-compose-environment)
- Every agent and check container runs with `--memory` equal to `--memory-swap`, so without swap: 4 GiB by default, the project's `memory` in `config.json` (at least `256m`, set with `cinderline memory`) instead, and a `mem_limit` on the compose file's agent service over both. The egress proxy gets a fixed 64 MiB; a compose file's other services get no default. (2026-11-24-agent-memory-limit)
- A project that needs another toolchain uses `docker`: a Dockerfile and `.cinderline/compose.yml` in its repository. An agent image needs `/bin/sh` and a glibc or musl loader and runs as uid 1000; agent CLIs come from the tools volume. (2026-11-16-project-toolchains)
- The default image holds PHP 8.4, Composer, Node 22 and git; Go left it once the runner project had its own image. (2026-11-16-project-toolchains point 3; CLR-185)
- The default image is built only when `images/default/` changes on main (or by hand), natively for linux/arm64 and linux/amd64 as one manifest list, tagged with the Dockerfile's sum. A failed pull of the pinned image is an error; the runner builds locally only a Dockerfile with no pin yet. (2026-11-12-ci-speedups; 2026-11-24-default-image-amd64)
- `add` and `service install` fail at once without docker, a running daemon or 5 GB free; `run` fails at once on missing docker or low disk but waits for the daemon, retrying every 10 seconds, because the service starts before Docker Desktop answers. Docker Engine must be 26.0+, for `volume-subpath` and the fix for CVE-2024-29018. (2026-11-08-docker-prereq; 2026-11-11-audit-fixes; 2026-11-19-docker-26)

## Network and the proxy

- Agents reach only exact `host:port` pairs through the run's egress proxy: their own client's provider (plus its sign-in hosts on a stored login), the project's Cinderline host, the listed package registries on 443, and the project's `allow_hosts`. Never a forge. Only local config opens a host for every run. (2026-11-09-network-allowlist; 2026-11-17-hosts-from-cinderline)
- A person's *Yes, this run* in Cinderline opens a refused host for one issue's runs: its `run`, `resolve` and `check` steps carry `allow_hosts`, each checked as a local `allow_hosts` entry and additionally refusing private, link-local unicast and unspecified IP literals, an invalid one dropped. The proxy also refuses private, loopback, link-local unicast and unspecified resolved addresses for app approvals, for HTTP and CONNECT; local config retains private-network access and takes precedence for the same host and port. Strict host blocking (`strict_hosts`, runner-wide, off by default, reported in every poll) ignores app approvals. Each of those steps reports the hosts the proxy refused during it as `last.refused`: at most 20, one per `host:port`, each with its reason (`not_allowed`, `private_address`, `strict`, `runner_machine`) and the first request's method and URL without query, fragment or user info when the proxy read it. For that the proxy ends the TLS of a refused HTTPS host with a certificate from the turn's intermediate of the runner's CA, reads only the first request and answers `403`; it never decrypts an allowed host or the runner's own machine. (2026-11-17-hosts-from-cinderline; 2026-11-19-app-host-addresses; app decision 2026-11-25-blocked-host-urls)
- A job's skill packages (`skills`) are each checked before any field is used, refused on a strict runner unless an `allow_skills` entry allows its name and URL (`cinderline allow-skill`), and fetched on the runner's own network without a token at the commit the app resolved, any other refused, into one read-only volume per URL and commit that every project shares. Each one named is reported in `workspace.skills` as `loaded`, `refused_strict` or `failed`; a volume no run used for 30 days is tidied. (2026-11-26-skill-package-fetch; app decision 2026-11-26-skill-packages)
- Every turn of a run mounts each loaded package's repository read-only at `/skills/<name>` and loads it per client: Claude Code one `--plugin-dir` per package with `--strict-mcp-config` kept; Gemini CLI an extension for a package with `gemini-extension.json`, else one mount per skill folder; OpenCode `skills.paths` and the package's `.opencode/{plugin,plugins}/*.{ts,js}`; Codex its `skills/` only. Nothing is written into the clone and nothing is added to the allowlist; hooks need their interpreter in the image. The ponytail pin is gone. (2026-11-26-skill-package-mounts)
- `host.docker.internal` is reachable only on the port of a model provider on the runner's own machine, for a turn that uses it, through the proxy only. `allow_hosts` refuses `localhost`, loopback addresses and `host.docker.internal`. (2026-11-17-local-model-providers; Willem 2026-11-17, extras)

## Secrets and redaction

- Secrets are in the OS keychain; when the keychain fails, a 0600 file in the state folder. (2026-11-05-go-keyring)
- No secret enters an agent container except the one model credential the turn needs and its scoped token. (AGENTS.md)
- Every secret the runner holds (runner token, git tokens, model and provider keys, scoped agent tokens) is replaced by a marker in log lines, in everything sent to the app, and in pull request titles and bodies, also base64, hex and URL-encoded. Values under 16 characters are skipped. It is a second line of defence only. (2026-11-11-secret-redaction; 2026-11-17-local-model-providers)
- The runner never reads the sign-in files on the state volumes. (2026-11-11-secret-redaction)
- Every push is scanned first: a value the runner holds, in any redacted form, in the header, message or added lines of a commit no other remote branch has stops the push and keeps the clone; a well-known shape of secret only warns. `publish` and `stop` report it with the outcome `secret` and the list `secrets` (kind, commit, path, never the value); the agent rewrites its unpushed commits. (2026-11-17-open-questions-answered, point 2; 2026-11-17-push-scan)

## Agent clients and the tools volume

- Detection per client: `missing_binary` when `--version` fails; `missing_credential` with neither key nor login file (checked for existence only); `invalid_credential` only on HTTP 401 or 403 from one free provider call; else `ready`. Logins, setup-tokens and Vertex keys are never tested. Detection runs at start and every hour. (2026-11-09-client-detection)
- Client sessions and config live on one Docker state volume per client and project; the host state folder holds the log, session notes, the outbox and attachments. Both are caches. (2026-11-05-runner-state-location; 2026-11-11-audit-fixes)
- A turn without a scoped token runs without the project's `CLAUDE.md`, `AGENTS.md` and OpenCode config. (2026-11-06-m3-parity-gaps)
- The tools volume carries Gemini CLI's npm tarball (checked by sha512) and Node 22: from nodejs.org checked by SHA-256 on glibc, from the digest-pinned `node:22-alpine` on musl, so Gemini runs on any agent image. (2026-11-08-gemini-node-in-tools-volume)
- Model keys are per project; `add` asks for the four, each optional, and offers to copy them from another project. A stored key wins over a login; a successful `login` deletes the client's stored key, a later `key set` wins again. No key is printed back. (2026-11-05-runner-rewrite-details; 2026-11-10-model-keys; 2026-11-19-login-replaces-key)

## Model providers

- A provider is added once on the runner, for all its projects: a name (lower-case letters, digits and dashes; not `openrouter`), an OpenAI-compatible base URL, and an optional key kept with the other secrets. A project uses one by naming it in an agent's model; there is no switch per project. (2026-11-17-local-model-providers)
- An OpenCode agent's model is `<provider>/<model id>`, at most 100 characters. The runner gives OpenCode the provider's URL and key for that turn only. A turn whose provider the runner lacks is not started. (2026-11-17-local-model-providers; Willem 2026-11-17, answer 22)
- The poll reports `model_providers` by name: `openrouter` when the project's key is set, plus each provider of the runner. The app never learns URLs or keys. (2026-11-17-local-model-providers)
- The poll reports each ready client's models (not OpenCode's) as its CLI lists them for its credential or login, its default first, plus Anthropic's list for a Claude Code API key and Google's `generateContent` models for a Gemini API key; and each provider's, `openrouter` first for a project with its key. Each list is fetched at start and every 10 minutes in the background, keeps its last good fetch when one fails, and goes with `models_age`, the seconds since that fetch; at most 500 ids the app accepts. (2026-11-24-model-lists; 2026-11-17-provider-models; CLR-218)
- A provider is marked free or not by whoever adds it (`add-provider --free`, `set-provider NAME --free=true|false`; not free by default). The poll reports the free ones as `free_providers`, at most 50, never `openrouter`, only names also in `model_providers`, left out when none is; the app prices their models at $0. (app decision 2026-11-19-free-model-providers; CLR-214)

## CLI, service and configuration

- `key list`, `key set` and `key delete` manage a project's secrets; `key set` reads from stdin, hidden on a terminal, and also takes `provider:NAME`. Each change reloads a running runner. (2026-11-10-model-keys; 2026-11-16-x-term; Willem 2026-11-17, extras)
- `add-provider`, `set-provider`, `list-providers` and `remove-provider` manage the runner's providers. (2026-11-17-local-model-providers; CLR-214)
- `remove` deletes a project's secrets, Docker objects and notes, unless another project shares its ID. (2026-11-11-audit-fixes)
- The service is user level everywhere: a LaunchAgent, a systemd user unit, a Windows logon task from XML via `schtasks /create /xml`. (2026-11-05-runner-rewrite-details; 2026-11-07-kardianos-service)
- The service restarts only on failure, so `cinderline stop` stops it until the next login; macOS does not restart a crash. (2026-11-07-service-restart)
- `config.json` holds no secrets; providers are in its top-level `providers` list. (2026-11-17-local-model-providers)

## Logging and errors

- Logging is `log/slog`, one structured line per event, tagged with project and issue. (AGENTS.md)
- The daemon's log rotates at 10 MB keeping 3; `cinderline logs` prints the tail and follows; `/logs` sends the last 100 lines. The socket folder is 0700, and a held socket is detected by dialing. (2026-11-05-reload-config; 2026-11-05-runner-rewrite-details)
- Container output is logged at Debug one line at a time, cleaned to printable text: `run -v` prints it as `time ISSUE │ line` with only its colour codes kept, and only on a terminal; the log file keeps the plain text. (2026-11-18-verbose-agent-output)
- Errors reach the app only in the poll's report and in `next`; the runner posts no comments. (2026-11-16-runner-protocol-v2)

## Testing and CI

- An issue is done only when `gofmt -l .` prints nothing and `go vet`, `staticcheck`, `go test -race ./...` and `govulncheck ./...` pass. (AGENTS.md)
- Linux checks run on Forgejo (`runs-on: docker`); the macOS checks, the GoReleaser snapshot and the release run on GitHub Actions, because only GitHub gives macOS runners and attestations. Windows checks are off until there are Windows users; releases still build all six targets. (2026-11-05-ci; 2026-11-12-ci-speedups)
- Forgejo skips the Linux checks on tag pushes and mirrors only its own ref; a newer CI run cancels an older one on the same branch. Draft workflows live in `docs/ci/`. (2026-11-12-ci-speedups)
- Tests that need Docker, network git or a forge run in CI on a `linux-host` Forgejo runner, which exists. (2026-11-06-integration-ci-host-runner; Willem 2026-11-17, answer 18)
- `.forgejo/workflows/integration.yml` runs the integration tests on that runner on pull requests and pushes to main. (Willem 2026-11-17, answer 18)

## To do and to build

- The app accepts the push scan's outcome `secret` and list `secrets` and writes the note and warnings; then copy the contract here and merge CLR-190. (2026-11-17-push-scan)
- Slim the default image back to PHP 8.4, Composer, Node 22 and git, then publish and pin it again. (2026-11-16-project-toolchains; Willem 2026-11-17, answer 21)
- Force Docker's built-in seccomp profile on every agent container and on a project's compose services (`--security-opt seccomp=builtin`), so isolation does not depend on the machine's Docker settings. (2026-11-17-open-questions-answered, point 1)

## Known gaps between the rules and the code

- Until the app has the push scan's contract change, it refuses a step reported with the outcome `secret` (`422`). (2026-11-17-push-scan)
