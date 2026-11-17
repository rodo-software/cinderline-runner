# Hosts a person opens in Cinderline, refused hosts per step, strict host blocking

- **Status:** Accepted
- **Date:** 2026-11-17
- **Source:** Willem on CL-242 (app decision `2026-11-17-blocked-hosts`), built by CLR-193. Supersedes point 2 of decision 2026-11-09-network-allowlist ("Only local config extends it") and that sentence of its line in current.md. Claude chose the details the issue left to the runner (points 4 and 6).

## Decision

1. **A person's answer in Cinderline can open a host for one issue's runs.** When the egress proxy refused a host, the app asks a person: *Yes, this run* / *No, not now* / *No, never*. After *Yes, this run*, every `run`, `resolve` and `check` step of that issue's next runs carries the host in `allow_hosts`, and the runner adds it to that step's allowlist. Nothing in Cinderline opens a host for every run: *always* still goes only through a project's local `allow_hosts` in `config.json`.
2. **The runner checks each host as it checks a local `allow_hosts` entry**: exact `host:port`, a DNS name or IPv4 address, a port in 1-65535, never `localhost`, a loopback address or `host.docker.internal`. An entry that fails is dropped with a log line; the step goes on without it. No field of the app's answer becomes anything but an allowlist entry.
3. **Strict host blocking**, off by default: with it on, the runner ignores every `allow_hosts` the app sends, and says `strict_hosts: true` in every poll's report so the app does not ask a question it cannot act on; the agent goes on without the host. The runner enforces it itself; the report only spares the question.
4. **Strict host blocking is runner-wide**: `strict_hosts` at the top of `config.json`, set with `cinderline strict-hosts on|off`, which reloads a running runner like the other config commands. It is the machine owner's choice about their own network, not a project's.
5. **Refused hosts per step.** The proxy's log is read when a turn ends. For each `run`, `resolve` and `check` step, the runner sends the `host:port` pairs the proxy refused during that step in the next `next` request as `last.refused_hosts`: each once, in lower case as the allowlist writes it, at most 20 of at most 260 characters, left out when there are none. `refused_connections` in the report still counts every refused connection.
6. **Only well-formed hosts are sent**, those that pass the check of point 2 apart from the runner's own machine, and none that holds a secret the runner knows: such an entry is left out rather than redacted or clipped, so every entry stays a `host:port` the app can show and a person can allow.

## Reason

1. The agent should not be stuck on a host a person would gladly open for this one issue, and editing `config.json` on the runner's machine for one issue is slow and too broad.
2. Decision 2026-11-09-network-allowlist kept the list local because "an allowlist that can be edited from Cinderline would open one on every runner of a project after a single stolen admin session". Opening a host only for one issue's runs, only one the proxy already refused for that issue, with a person's explicit answer, keeps that exposure small; strict host blocking is the way out for a runner owner who wants none of it.
3. Checking the app's hosts as local ones are keeps the one rule about the runner's own machine (decision 2026-11-17-local-model-providers) and keeps the app's answer from becoming anything but an allowlist entry.
4. Runner-wide is the simplest setting that fully answers the need; a per-project one can follow when someone needs it.

## Contract change (made in the app repository, CL-242)

All optional, so either side may ship first:

- `Report.strict_hosts: boolean`, always sent by this runner.
- `LastStep.refused_hosts: string[]`, at most 20 items of at most 260 characters, for `run`, `resolve` and `check`; left out when none.
- `Step.allow_hosts: string[]` on `run`, `resolve` and `check`; left out when none.

`docs/runner-api/openapi.yaml` is copied from the app repository once CL-242 has merged.
