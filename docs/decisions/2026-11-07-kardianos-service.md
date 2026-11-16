# User-level service: github.com/kardianos/service

- **Status:** Accepted
- **Date:** 2026-11-07
- **Source:** Plan section 3 M2 (Dependencies) and M5, decision 2026-11-05-runner-rewrite-details point 11, CSR-43.

## Decision

- `internal/cli` uses `github.com/kardianos/service` for `cinderline service install|uninstall` on macOS (LaunchAgent) and Linux (systemd `--user` unit).
- On Windows the runner registers a logon scheduled task from generated XML with `schtasks /create /xml` instead, so it runs as the user who owns the keychain and Docker Desktop and is restarted on failure.

## Reason

User-level launchd and systemd units without hand-written templates.
