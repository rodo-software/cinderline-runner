# Docker prerequisite check

- **Status:** Accepted
- **Date:** 2026-11-08
- **Source:** Decided by Willem on CSR-46.

## Decision

- `add` and `service install` fail at once when docker is missing, the daemon does not answer, or less than 5 GB is free.
- `run` fails at once on missing docker or low disk but waits for the daemon, saying so once and retrying every 10 s.

## Reason

The service starts `run` at login, often before Docker Desktop answers, and decision 2026-11-07-service-restart restarts only on failure (on macOS not at all), so failing would leave the runner down until the next login. Free disk is measured with df in the pinned git image; threshold 5 GB (the tools volume alone is ~320 MB per Claude Code build, plus the default image and MySQL).
