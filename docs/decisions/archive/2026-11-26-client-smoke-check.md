# A pin bump of an agent CLI is checked by real turns through the runner's own code

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, CLR-261 (2026-11-26), option A: a Go smoke test with a thin script, in place of a shell script that extends `scripts/skills-spike.sh`.

## Decision

1. The smoke check is `TestSmokeClients` in `internal/client/smoke_test.go`, behind the `smoke` build tag. Per client it runs short real turns through the runner's own `client.CLI` on `environment.Docker`, with the pinned tools volume, so the argv, environment, mounts, proxy and parsing are the runner's.
2. `scripts/smoke-clients.sh` runs it and prints one PASS, FAIL or SKIP line per check per client; it exits non-zero on any fail, and when no client ran. A client whose key is not in the environment is skipped.
3. The checks: the turn ran, its session ID and usage were parsed, a second turn resumed the session, the turn limit was recognised (Claude Code and Gemini CLI; Codex and OpenCode have none), the CLI listed the tools of the MCP server the runner configured with the turn's token, a mounted skill package loaded, and the proxy refused no host. The proxy allows only the client's own `hosts` and the test's MCP server: no `Registries`, as decision 2026-11-25-client-own-traffic keeps a client's own traffic out of them.
4. A pin bump in `internal/tools/pins.go` is done only when the script passed on Willem's machine (`AGENTS.md`). It cannot run in CI (no model keys) or in an agent's container (no Docker).
5. `scripts/skills-spike.sh` stays as it is. Its probe is plain skills only, while the spike also checks a real package's hooks, Gemini CLI extension and OpenCode plugin (CLR-256).

## Reason

A shell script would repeat each client's argv and environment a third time, next to `internal/client` and `skills-spike.sh`, and would not follow when they change. It also could not check the runner's own parsing of session, usage and turn limit, which is Go. The integration tests in `internal/run` already drive the real Client and Environment behind a build tag; the smoke test follows them.
