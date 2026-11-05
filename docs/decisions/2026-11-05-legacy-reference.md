# Legacy reference for M3: copied into the M3 issue texts

- **Status:** Accepted
- **Date:** 2026-11-05
- **Source:** Decided by Willem on CSR-19, picking option B of the feasibility review (`docs/reviews/2026-11-05-plan-feasibility.md`, blocker 4).

## Decision

- M3 reaches the legacy reference through the issue texts. Willem copies the relevant parts of `legacy/`'s `runner.mjs`, `lib.mjs` and `node --test` suites into each M3 issue's text (CSR-20 to CSR-27), without any `.env` or token, before the issue enters To do.
- `legacy/` itself stays git-ignored on Willem's machine. Nothing from `legacy/` and no `.env` is ever committed to this public repository (decision 2026-11-05-runner-rewrite-details, point 5).
- Option A (a private branch or repository the runner clones) and option C (M3 issues written from the old suites by a person) are rejected.

## Reason

- Blocker 4 of the feasibility review: `legacy/` is git-ignored and lives only on Willem's machine, so a team could not see what M3 ports.
- Option B needs no new repository and no new credentials: each M3 team reads the excerpts in its own issue's text, and nothing from `legacy/` is committed anywhere.
