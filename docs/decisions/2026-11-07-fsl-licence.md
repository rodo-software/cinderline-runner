# Runner licence: FSL-1.1-MIT

- **Status:** Accepted
- **Date:** 2026-11-07
- **Source:** Decided by Willem, CSR-83. Replaces the MIT licence of decision 2026-11-04-licences-before-publishing (app repository) for the runner.

## Decision

The runner is licensed under the Functional Source License, Version 1.1, MIT Future License (FSL-1.1-MIT, https://fsl.software), licensor Rodo Software. `LICENSE` holds the verbatim template text; `NOTICE`, `README.md` and the OpenAPI file's `info.license` name it. The README calls the runner fair source, never open source: FSL is not OSI-approved.

Versions already pushed to the public GitHub mirror under MIT stay MIT. Every commit so far is by Willem or his own agents, so no contributor consent was needed.

## Reason

The runner stays readable, modifiable and runnable by anyone, including against a self-built backend for internal use, but competitors may not ship or host it as part of a commercial product or service that substitutes for CinderStack without a commercial licence from Rodo Software. Each release converts to MIT two years after its publication.

## Alternatives considered

- **AGPL-3.0:** does not stop competing use; a competitor can ship or host it as long as they publish their changes.
- **Elastic License 2.0:** only forbids offering it as a hosted service; it misses a competitor pointing the runner at their own tracker.
- **BSL 1.1:** works, but needs a bespoke additional use grant to say what is allowed.

## Known limit

A competitor who tells their customers to download the runner themselves and point it at the competitor's tracker is a grey area the licence alone does not close. Trademark and the closed server side of the runner API are the complement.
