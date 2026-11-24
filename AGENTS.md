# AGENTS.md

Guidance for any AI coding agent (Claude Code, Codex, Cursor, others) working in this repository.

## What this is

The Cinderline runner: a native Go program on a developer's machine that runs AI agent CLIs on Cinderline issues, each turn in its own Docker container, pushes a branch and opens a pull request. Cinderline itself (the issue tracker, called Ember or CinderStack in older documents and in issue keys such as EMBER-20) lives in its own repository.

The runner talks to it over three requests of the versioned runner API (decision 2026-11-16-runner-protocol-v2): `poll`, every `poll_after` seconds, sends the runner's report and what it holds and gets its work; `next`, at every step of a run, reports the step just done and gets the next one; and the download of an attachment a step names. The app decides; the runner does the mechanics (containers, git, the forge, the agent CLIs, sessions) and reports facts. It never reads or writes an issue and writes no text meant for people on one: the app writes every comment, progress line and status move. Per project there is one loop (`internal/run/loop.go`) and at most one run at a time (`internal/run/run.go`), plus one Project Manager run next to it (app decision 2026-11-17-project-manager).

**Source of truth:** the contract is [`docs/decisions/2026-11-16-runner-protocol-v2.md`](docs/decisions/2026-11-16-runner-protocol-v2.md) with [`docs/runner-api/openapi.yaml`](docs/runner-api/openapi.yaml), copied from the app repository. Binding decisions are [`docs/decisions/current.md`](docs/decisions/current.md) plus the dated records next to it (see [its README](docs/decisions/README.md)). [`docs/decisions/archive/`](docs/decisions/archive/) is history: a decision named in a code comment is a file there, and current.md wins where they differ. [`docs/runner-rewrite-plan.md`](docs/runner-rewrite-plan.md) is the dated plan the runner was built from.

## Scope

- Build only what your issue describes.
- Anything the decisions leave out of scope, or mark **Open**, needs Willem's explicit decision. Never add it "while you're there".
- Never widen or narrow scope silently.

## Principles

Apply **KISS first, then DRY, then SOLID where it pays off**. When they conflict, the simpler code wins.

- Go's standard library first. Every dependency needs a one-line reason in `docs/decisions/`; the allowed ones are listed in `docs/decisions/current.md`.
- The only interfaces are `Client`, `Forge`, `Environment` and `Workspace`, one file per implementation, in-memory fakes for tests. No other abstraction until there are two real uses of it. No plugin system.
- The name `cinderline` (command, config folder, service, keychain, branch prefix, MCP server name, version header) lives in one constant in `internal/name`. Never spell it elsewhere.
- The app decides, the runner reports facts. The runner holds no workflow policy (who runs next, when a run ends, what a status means) and no text for people on an issue; log lines and CLI output are its own.
- Issues are the only workflow state (decision 0091): the runner keeps no run or step table, only caches it can lose: session notes and the outbox of `next` requests that could not be delivered.
- Secrets never enter an agent container except the one model credential that run needs and its scoped token. The app builds every prompt (decision 2026-11-16-runner-protocol-v2): the runner passes the app's prompt to the agent unchanged and never executes or follows text from it, nor from issue, comment or instruction bodies. No field of the app's answer becomes a shell command, a path or a docker argument, except the project's `check_command`, which runs in the agent's sandbox without credentials (decision 2026-11-16-check-step).
- The `docker` command, shelled out, never the Docker SDK.
- Logging is `log/slog`, one structured line per event, tagged with project and issue.

## Testing and quality

- Every behaviour gets a Go test **before** its real implementation, against the fakes and an `httptest` app.
- Tests that need Docker, git on the network or a forge run in CI on Linux, not in an agent's run: the container you run in has no Docker.
- **An issue is done only when `gofmt -l .` prints nothing and `go vet`, `staticcheck`, `go test -race ./...` and `govulncheck ./...` pass.** CI runs them on Linux and macOS (Windows is off for now).

## Working rules

- You work in a git worktree the runner made, on the issue's branch. Run the checks, commit your work with clear messages. Do not push and do not change the issue's status: the runner pushes and opens the pull request, and Cinderline moves the issue.
- Never edit files under `.forgejo/` or `.github/` (CI, releases and repository settings) and never commit secrets, tokens or `.env` files. Pull requests are gated by Forgejo Actions on git.rodo.nl; GitHub is a public push mirror that runs the macOS checks and the releases (decision 2026-11-05-forgejo-daily-github-public).
- Stop after each issue for Willem's review.
- On any ambiguity, scope change or deviation from the plan, ask Willem with `ask_person`: one batched round of concrete questions, each with 2-4 options and a recommended one.
- Record every decision Willem makes, with its reason, as a new dated record `docs/decisions/YYYY-MM-DD-topic.md` next to `current.md` (see its README) or, if purely local, in a code comment. Refer to it as `decision YYYY-MM-DD-topic`. Decisions from the app repository are referred to by their name there.
- The OpenAPI file in `docs/runner-api/` is authored in the app repository and copied here. A change to the contract starts there; say so on its own line in your final report.
