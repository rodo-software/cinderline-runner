# No writer teams in the runner

- **Status:** Accepted
- **Date:** 2026-11-06
- **Source:** Decided by Willem on CSR-23 ("we're getting rid of writer teams", then option B).

## Decision

The runner has no writer teams. This replaces the plan's M3 "Writer teams" item (2026-11-03-issue-writer-teams) and the writer half of decision 2026-11-05-runner-rewrite-details point 6:

- No writer run: no original request kept as a comment, no writer prompt, no "written" or "not written" report.
- The runner never asks for a `writer` agent token; it asks only for `orchestrator` and `reader`.
- A paused note has no run kind, and a team that changes while its issue is paused is not detected: the run resumes as the coding run it was. CSR-22's `KindWrite`, `KindCode` and `DropChangedKind` are removed.

`docs/runner-api/openapi.yaml` no longer lists `writer`: it mirrors the app, which dropped writer teams too (app decision 2026-11-11-no-writer-teams, EMBER-175, CSR-165).

## Reason

Willem is getting rid of writer teams, so the runner does not port them.

## Amendment 2026-11-15: a paused note says whether it is a generic run's

- **Source:** CSR-75, which Willem's issue text left to settle ("ask Willem if it is not obvious"); settled by porting legacy `dropChangedKind` as it is. Willem may veto it.
- **Decision:** generic runs (decision 2026-11-06-m3-parity-gaps) bring back one run kind in the paused note: `generic` (true for a generic team's run; false or missing for a coding run, so older notes stay coding runs). A note whose kind is not the team's current one is dropped with legacy's comment "The team changed while paused, so this run starts over.", plus the paused coding run's last commit for a coding note, and the run starts fresh. A change between coding teams is still not detected, as above.
- **Reason:** checking only the team's `generic_runs` on resume cannot tell that the flag changed while the issue was paused. Resuming a coding session under a generic run would discard its work, and resuming a generic session under a coding run would publish a worktree that was never meant to change. The team lock in the app (its decision 2026-11-11-no-writer-teams) keeps the issue's team, but not the team's own flag.
