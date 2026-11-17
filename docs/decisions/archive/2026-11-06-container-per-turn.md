# A container per agent turn, with its own network and MySQL

- **Status:** Accepted
- **Date:** 2026-11-06
- **Source:** Decided by Willem on 2026-11-06 on CSR-31, option A of three; proposed by Claude. Refines decision 2026-11-05-runner-rewrite-details point 13 and the plan's M4 `Environment`.

## Decision

Every agent turn (one run of the Claude Code or OpenCode CLI: the orchestrator, a worker, a retry, a resume, the Merge Resolver) runs in its own container, started with `docker run --rm` and stopped with `docker stop` at the team's `max_minutes`. Its private network and a fresh MySQL are created for that turn and removed with it. Only files in the issue's clone carry over between turns.

The `Environment` interface is `Run(spec) -> output` plus `Cleanup` for recovery at start; the Clients call `Run`. It replaces CSR-13's `Start`, `Stop` and `Tail`: the run code reads the output tail from `client.Result.Tail`.

Rejected: one container per issue with `docker exec` per turn (B), and a container per turn with the network and MySQL kept for the whole issue (C).

## Reason

- Secrets: a process one turn leaves running (from an injected instruction, a repository script, or a dev server) would, in a shared container, run as the same user as the next turn and could read that turn's scoped token and model key from `/proc/<pid>/environ`. A container per turn ends every process before the next turn starts. The shared clone remains a path (a planted script the next agent runs), but that needs the agent to run it and shows in the diff.
- Time limit: `docker stop` ends everything in the container on every OS; killing a `docker exec` does not end the process inside.
- Simplicity: the run code has no database life cycle (no teardown at a pause or after a crash); moving to C later changes only who creates and removes the services, not the Clients.
- Cost accepted: MySQL starts each turn (10 to 20 seconds, mostly hidden while the agent reads code), and the database does not survive between turns.
