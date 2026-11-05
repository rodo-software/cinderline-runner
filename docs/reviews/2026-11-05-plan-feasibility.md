# Feasibility review of the runner rewrite plan

Written on 2026-11-05 for CSR-1, by a team agent running through today's runner. It covers `docs/runner-rewrite-plan.md`, decision 2026-11-05-runner-rewrite-details and `docs/2026-11-04-runner-rewrite-host-program.md`. It changes no decision; the points marked **Willem** need his call.

## Verdict

**Feasible.** The architecture holds up technically: nothing in it relies on something Go, Docker or the three OSes cannot do. What is not yet feasible is the plan's **"who builds it"** line (teams through today's runner): four process gaps, found from inside this very run, block M0 and M3 as written. They are cheap to close, but they must close before M0's issues are queued.

## Blockers for "teams build it"

Found by looking at this run's own container and worktree.

1. **No Go in today's runner image.** `go version` fails in this run, so M0's app-side item (Go in `runner/Dockerfile`) is not done yet. `staticcheck` and `govulncheck` are missing too; `govulncheck` also needs network access to `vuln.go.dev`. Until these land, no team can meet AGENTS.md's definition of done. **Willem:** do the app-side M0 item first, and include `staticcheck` and `govulncheck` at pinned versions.
2. **Agents may not edit `.github/`, but M0 asks them to write the CI.** The runner's prompt and AGENTS.md both forbid touching `.github/`. M0's skeleton includes "a GitHub Actions CI ... on Linux, macOS and Windows" plus GoReleaser, so a team cannot deliver M0 as written. **Willem:** write the workflow yourself (recommended; it is small and it is the security boundary), or let a team draft it as a file elsewhere for you to move.
3. **The remote is not GitHub.** This worktree's `origin` is `https://git.rodo.nl/willem/cinderstack-runner.git`, while the plan relies on GitHub-hosted macOS and Windows runners, GitHub artifact attestations and GHCR. Forgejo or Gitea Actions have no hosted macOS or Windows runners, and attestations are GitHub-only. **Willem:** confirm `git.rodo.nl` is only a mirror and CI runs on `github.com/rodo-software/cinderstack-runner` (recommended), or plan self-hosted macOS and Windows runners and another signing scheme.
4. **`legacy/` is not in the agents' worktree.** It is git-ignored and lives only on Willem's machine (AGENTS.md); `ls legacy` fails here. M3 is "a port of the `node --test` suites in `legacy/`", so a team cannot see what it is porting. **Willem:** before M3, make the reference readable to the runner project without its `.env`. Options: commit only the `legacy/` test files and `lib.mjs`/`runner.mjs` to a private branch or repo the runner can clone (recommended), copy them into each M3 issue's text, or have M3 issues written from the old suites by a person.

## Risks that do not block, with the mitigation

5. **Docker work is done blind.** Agents have no Docker, so M4's `Workspace`, `Environment` and tools volume are developed without running them; each check is a full push and CI round trip. Mitigation: keep the `docker` and `git` command lines behind one small exec seam and test the argv they build against expected output, so CI only has to confirm what unit tests already pinned. Size M4 issues small.
6. **Socket "0700 folder" means nothing on Windows.** `os.Chmod` on Windows only sets the read-only bit; the local API's "no further auth" rests on the folder's ACL. Put the socket under the user's profile (`%LocalAppData%`), whose default ACL is already per-user, and say so in the code. Unix socket paths are limited to about 104 bytes on macOS and 108 elsewhere, so keep the path short.
7. **Keychain on headless Linux.** `go-keyring` on Linux needs a running, unlocked Secret Service over D-Bus, which a server or a systemd user unit without a desktop session usually lacks. The 0600 fallback must be chosen on that failure, not only when no keychain exists, and `cinderstack add` should say which one it used. Windows Credential Manager limits a secret to 2,560 bytes, enough for every token listed.
8. **Git isolation is sound, with two details.** Fetching from the agent's clone runs `git upload-pack` there, which runs no hooks and ignores `uploadpack.packObjectsHook` in repository config, so decision 12 holds. But: the helper and agent containers will likely run as different UIDs, so git's `safe.directory` check refuses the fetch unless the helper sets it for that path; and the `repo` volume must be mounted at the same path in both containers, or the alternates break.
9. **Docker networks run out.** A private network per run, plus networks left behind by crashes or `cinderstack stop`, eats Docker's default address pools (a few dozen networks). Label every container, network and volume the runner creates with the project and issue, and remove stale ones during recovery.
10. **`host.docker.internal` on Linux.** `host-gateway` works on Docker Engine 20.10 and later, but a backend listening only on `127.0.0.1` (for example `php artisan serve`) is unreachable from a container on Linux. Document that it must listen on the bridge address. M8's internal network will also cut this path unless the proxy allows it.
11. **Claude Code updates itself.** Run it from the read-only tools volume with `DISABLE_AUTOUPDATER=1`, or it tries to update on every run.
12. **Windows logon task.** A task made with plain `schtasks /create` is not restarted when the runner crashes; create it from XML with restart-on-failure settings.
13. **M2's "done when" needs a real instance.** It can't be checked in CI, so Willem checks it by hand on three platforms; say so in the M2 issues.
14. **Long-poll on PHP-FPM:** already marked **Unchecked** in the plan; still open.
15. **Claude subscription use:** already marked **Still open** in the plan. A legal question, not a technical one, but it decides whether subscription-backed runs are a supported path at launch.

## Small inconsistencies in the documents

- Plan M2 puts state "beside" the config folder on the host; M3 puts paused notes and sessions on "a per-project state volume". Sessions must sit in a volume (the CLI runs in a container); paused notes can be either. Pick one wording before M2.
- `docs/2026-11-04-runner-rewrite-host-program.md` still says the command is `ember` and the daemon is a Windows service; both were superseded by decision 2026-11-05-runner-rewrite-details (points 11 and 19). It also sits in `docs/`, not `docs/decisions/`, and links to `../runner-rewrite-plan.md`, which from `docs/` points outside the folder.
