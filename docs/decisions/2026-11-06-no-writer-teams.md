# No writer teams in the runner

- **Status:** Accepted
- **Date:** 2026-11-06
- **Source:** Decided by Willem on CSR-23 ("we're getting rid of writer teams", then option B).

## Decision

The runner has no writer teams. This replaces the plan's M3 "Writer teams" item (2026-11-03-issue-writer-teams) and the writer half of decision 2026-11-05-runner-rewrite-details point 6:

- No writer run: no original request kept as a comment, no writer prompt, no "written" or "not written" report.
- The runner never asks for a `writer` agent token; it asks only for `orchestrator` and `reader`.
- A paused note has no run kind, and a team that changes while its issue is paused is not detected: the run resumes as the coding run it was. CSR-22's `KindWrite`, `KindCode` and `DropChangedKind` are removed.

`docs/runner-api/openapi.yaml` still lists `writer` as a token kind. That file mirrors the app repository, which decides whether to drop it.

## Reason

Willem is getting rid of writer teams, so the runner does not port them.
