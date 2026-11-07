# Team max_turns on the runner API

- **Status:** Accepted
- **Date:** 2026-11-07
- **Source:** Decided by Willem on CSR-79, option A.

## Decision

The runner API's Team carries `max_turns`, the team's per-run turn limit, and the runner passes it as every agent turn's `MaxTurns` (orchestrator, workers, Merge Resolver). When the app does not send it (0), the runner uses 50, the app's default for a new team. The app repository must return `max_turns` in the runner API's team and update its route test, as it mirrors `docs/runner-api/openapi.yaml`.

## Reason

A team's turn limit is a person's setting that the legacy runner honoured through get_team; a fixed limit in the runner would make changing it in the app silently do nothing. Without any value, Claude Code would get `--max-turns 0` and stop at once.
