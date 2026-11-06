# M3 parity gaps: generic runs and token use are part of parity

- **Status:** Accepted
- **Date:** 2026-11-06
- **Source:** Decided by Willem on CSR-28 (option A), proposed by Claude.

## Decision

- The M3 parity table (`docs/parity/m3.md`) found 12 cases of the old suite that no M3 issue ported, from two features the old runner gained after the plan was written: generic runs (EMBER-123, app repository decision 2026-11-05-generic-runs-and-agent-proposals) and token use, cost and budget (EMBER-112, app repository decision 2026-11-05-cost-per-issue-and-budget). Both are part of parity, in addition to the plan's M3 list. They are ported by their own backlog issues: CSR-75 (generic runs) and CSR-76 (token use and budget).
- The app repository's decision 2026-11-05-workers-skip-project-files (workers and the Merge Resolver run without the project's `CLAUDE.md`, `AGENTS.md` and project OpenCode config) is part of the real clients: CSR-33 and CSR-34 now name it, with the usage parsing CSR-76 relies on.

## Reason

Today's runner has these behaviours, so leaving them out would be a regression when projects switch to the new runner (CSR-40).
