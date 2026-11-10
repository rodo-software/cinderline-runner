# The runner rewrite plan's open points, settled before M0

- **Status:** Accepted
- **Date:** 2026-11-05
- **Source:** Decided by Willem on 2026-11-05 in one batched round of questions before implementation starts; proposed by Claude. Completes decision 2026-11-04-runner-rewrite-host-program (app repository) and the plan `docs/runner-rewrite-plan.md`.

## Decision

1. **Who implements:** Cinderline teams through today's runner, as the plan says. Not local agents.
2. **Repository and module:** `github.com/rodo-software/cinderline-runner`. The same org and name for the default image on GHCR.
3. **M1 before M2:** the runner API is built in the app repository first; M2 then talks to the real API.
4. **AGENTS.md:** the app repository's AGENTS.md, adapted for a Go repository built by teams.
5. **`legacy/`:** the old runner stays on disk as the porting reference for M3 and is git-ignored; its `.env` is deleted, never committed.
6. **Scoped agent tokens:** the agent tools (`get_issue`, `list_comments`, `add_comment`, `create_issue` for sub-issues, `update_issue` within the issue tree, `ask_person`, `reply_person`). The runner asks for a token with a `kind`: `orchestrator`, `writer`, or `reader` for the orchestrator's own sub-issue, which gets only `get_issue` and `list_comments`.
7. **OpenAPI file:** authored in this repository under `docs/runner-api/`, mirrored into the app repository and tested there against the routes.
8. **Local backend from a container:** agent containers always get `host.docker.internal:host-gateway`; a self-hosted backend on the same machine is configured as `http://host.docker.internal:<port>`. *Replaced for agent containers by decision 2026-11-09-network-allowlist: they sit on an internal network and never reach `host.docker.internal`; a local backend waits for CSR-91.*
9. **Claude subscriptions:** the keychain holds, per project, an API key or the token from `claude setup-token`, as today. `cinderline login claude` (M7) adds the sign-in-in-container path later. Whether Rodo Software asks Anthropic about automated team runs on a subscription stays open.
10. **Model credentials per project**, as the plan says; `cinderline add` offers to copy them from an existing project. The runner is a single-user program.
11. **Service identity:** user level everywhere. A launchd LaunchAgent and a systemd user unit through `kardianos/service`; on Windows a logon scheduled task, not a Windows service.
12. **Git isolation:** each issue gets its own clone in a runs volume, sharing objects read-only through alternates with the helper's clone. The token helper never runs git inside the agent's clone: it fetches the branch from it over a read-only mount and pushes from its own clone. Commits, merges and conflict checks run in the agent's clone by a git container without the token.
13. **MySQL:** a fresh MySQL container per run on the run's private network, removed with it.
14. **`cinderline stop`** stops at once; the issue goes back to To do at the next start. A drain arrives in M5 with `cinderline update`.
15. **Logs:** the daemon keeps a rotating log file in its state folder; `cinderline logs` prints the tail and follows.
16. **Release signing:** GitHub artifact attestations (build provenance); the install scripts verify SHA-256 against the checksums file.
17. **Install location:** `~/.local/bin` on macOS and Linux, `%LocalAppData%\Programs\cinderline` on Windows, no sudo.
18. **M6, M7 and M8's open items** stay open until their milestone.
19. **Name:** the backend app is Cinderline from now on, not Ember. The command, config folder, service, keychain entries, branch prefix, MCP server name and version header are `cinderline`; the name lives in one constant. Issue keys such as EMBER-20 and the names of existing decisions stay as they are.

## Reason

- 1, 3: the plan's own sequence; teams dogfood the runner, and M2 can only be finished against a real API.
- 5: the public repository must never carry the old code or its tokens.
- 6: today the orchestrator's own sub-issue is restrained by prompt text only; a `reader` kind makes the restriction a policy. A per-request tool list would move policy into the runner.
- 7: a runner builder reads the public repository, not the app's.
- 8: the backend will be self-hostable, so a backend on the developer's own machine is a normal case.
- 9: `claude setup-token` is Anthropic's own mechanism for headless use; storing nothing would pull `login` into M4 for no gain yet.
- 10: one code path, and the runner serves one person.
- 11: the keychain and Docker Desktop belong to a user; a Windows service runs as LocalSystem and sees neither, and asking for an account password at install is worse than a scheduled task.
- 12: with one shared volume of worktrees, the agent could write the clone's `.git/config` and hooks; a `url.<x>.insteadOf` there would redirect the helper's push, and its token, to any host, and a `pre-push` hook would run inside the token helper. Separate clones close that at the cost of one `git fetch` per push.
- 13: no volume to manage and a known state per run; 10 to 20 seconds per run is acceptable.
- 14, 15: parity with today, and the drain is needed by `update` anyway.
- 16, 17: no keys to manage; no sudo in an install line.
- 19: the rename was coming (plan section 2); the runner's own names can change now because every name the runner chooses is runner-side, including the MCP server name and the branch prefix. The branch prefix changes with it so that no name has to change twice; the branches of issues in Review at the switch are merged by hand once.
