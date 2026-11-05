# The runner is rewritten in Go as a native host program in its own repository; agents run in containers

> **Status note (2026-11-05):** points 11 and 19 of decision [2026-11-05-runner-rewrite-details](decisions/2026-11-05-runner-rewrite-details.md) supersede this record's name and service lines: the command is `cinderstack`, not `ember`, and on Windows the daemon is a logon scheduled task, not a Windows service.

- **Status:** Accepted
- **Date:** 2026-11-04
- **Source:** Decided by Willem on 2026-11-04 while preparing EMBER-20, one batched round of questions; proposed by Claude. The plan is [`docs/runner-rewrite-plan.md`](runner-rewrite-plan.md). Amends 0095 (agents and runner share one container), 0104 (others build runners against the MCP interface), 0112 (the runner is a Node script in `runner/`), and decision 2026-11-03-mvp-launch-decisions on D2 (distribution as a prebuilt runner image), D3 ("the runner stays in Docker") and the exclusion of native Windows.

## Decision

- **Shape:** the runner becomes a native program on the host (`ember`) for macOS, Linux and Windows. It holds the configuration and secrets of every project on the machine, talks to Ember and the forges, and starts containers through the `docker` command. Each agent run gets its own short-lived container with the agent CLIs, the project's toolchain, the worktree and only the credentials that agent needs. The runner's Ember token and the git token stay outside it.
- **Language:** Go. Willem weighed Rust for its compile-time checks and its appeal, and chose Go: the runner mostly waits on the network and on child processes, Go builds one static binary for every platform with standard tools, and plain Go is easier for a customer's developer to audit, which is what 0104 asks of the runner.
- **Repository:** a new public MIT repository on GitHub, started with the rewrite instead of moving `runner/` as it is. `runner/` stays here until the new runner replaces it. Runner issues get their own Ember project; communication between projects is a later feature.
- **Concurrency:** one run at a time per project, projects in parallel, on a scheduler with one slot per project.
- **Environments:** the default image, a per-project image (`docker`) and an existing setup (`existing`). `host` and `devcontainer.json` are not in the plan.
- **Agent CLIs:** a read-only tools volume of pinned CLI binaries, downloaded on the user's machine and mounted into any image.
- **Workspace:** Docker volumes with git in a helper container for Docker environments; the host's filesystem and git for `existing`.
- **Daemon:** installed as a service (launchd, systemd, Windows services), with a local API the CLI uses, and that a later tray app or local web page can use too.
- **Secrets:** the OS keychain, with a 0600 file where there is none.
- **Extensibility:** in-tree Go interfaces (`Client`, `Forge`, `Environment`, `Workspace`), one file per implementation; no plugin system.
- **Protocol:** the runner talks only to a versioned REST runner API in Ember (`/api/runner/v1`, token ability `runner`, long-polling for work), described in an OpenAPI file. Agents keep MCP, each run with its own short-lived token scoped to the issue, instead of the runner's token.
- **Scope:** everything today's runner does, plus every runner-side item of the launch backlog. The Ember UI that uses the runner report (EMBER-46 to 51) and Phase 9's gate and plan approval stay outside it.
- **Name:** `ember` for now; a rename is coming, so the name lives in one constant.
- **Who builds it:** teams through today's runner, once Go is added to its image.

## Reason

One container for runner and agents lets an agent read the runner's tokens (S1 in the launch inventory), ties the runner to one toolchain image (D4), keeps it from reaching the host for 0098's `existing` environment, limits an install to one project (R5) and needs WSL on Windows. A host program with a container per run removes all five and keeps the sandbox for agents. Willem prefers to take the rewrite now, closer to the eventual design, over moving code that would be replaced soon after.

The REST runner API keeps runner powers (claims, merge status, reports) off the tool list agents see. It allows long-polling and ETags, which matter when a hosted Ember serves many runners, and gives other runner builders a contract. Scoped agent tokens close the gap where an agent holds the runner's own token. The tools volume works with any project image and avoids publishing an image that contains Claude Code. Volumes for Docker environments avoid slow shared folders on macOS and Windows. The service and the local API leave room for a graphical front end without a rewrite.
