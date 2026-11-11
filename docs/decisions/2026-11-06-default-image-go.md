# Go in the default image

- **Status:** Accepted
- **Date:** 2026-11-06
- **Source:** Decided by Willem on 2026-11-06 on CSR-31, option A of three; proposed by Claude. Changes the plan's M4 `Environment` item.

## Decision

The default image (`images/default/Dockerfile`) holds Go 1.27.4 with staticcheck and govulncheck, the same pins as today's runner image and with `GOTOOLCHAIN=local`, besides PHP 8.4, Composer, Node 22 and git.

This is a stopgap. One image cannot hold every project's toolchain, certainly not once Cinderline is offered as SaaS, so a more flexible solution is needed later. It is tracked as an issue at the end of the backlog (CSR-77).

Rejected: keeping the image as planned and switching the runner project only in M6 (B), and pulling M6's per-project image forward for the runner project (C).

## Reason

The runner project is Go, and its teams need Go, staticcheck and govulncheck for AGENTS.md's checks. Without them it can't switch to the new runner in M4, and M4 is done when both projects run on it. Per-project images (`docker` environment) only come in M6. About 250 MB more image is the cost.
