# Runner state: paused notes on the host, client sessions on a per-project state volume

- **Status:** Accepted
- **Date:** 2026-11-05
- **Source:** Decided by Willem on CSR-10; proposed by Claude. Settles the inconsistency between plan M2 and M3 found in `docs/reviews/2026-11-05-plan-feasibility.md`.

## Decision

- **Host state folder** beside the config (`os.UserConfigDir()/cinderstack/state`): the daemon's rotating log and the paused notes, one set per project.
- **Per-project state volume** (Docker): client sessions and client config dirs (0116), mounted into the agent container.
- Workflow state still lives only in issues (0091); both places hold caches the runner can lose.

## Reason

Sessions must sit in a volume because the client CLI runs in the agent container. Paused notes are read only by the runner itself (recovery at start drops notes whose issue is closed; teams with workers resume from them), so on the host it reads and drops them with plain file I/O and no helper container, and recovery works the same for the `existing` environment, which has no Docker volumes. Only what the container needs goes into the volume.
