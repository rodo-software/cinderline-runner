# No local-model work in the runner

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** Decided by Willem on CSR-91, which he closed without building it.

## Decision

- The runner does not run model backends: no Ollama run service, no `cinderline models pull`, no GPU passthrough, no host-backend warning. CSR-91 is closed, not deferred.
- No other local-model work for now either; the smaller alternative offered on CSR-91 (an OpenCode `cerebellum` provider from a configured URL) was declined too.
- Decision 2026-11-09-network-allowlist point 3 stands as is: `host.docker.internal` is never allowed.

## Reason

Cinderline manages issues, progress, history and developer decisions, and runs the available agents in isolated containers, several models on one issue. Helping people run their own models is outside that. Willem's own backend (cerebellum) already serves its model outside the runner.
