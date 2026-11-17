# Keychain library: github.com/zalando/go-keyring

- **Status:** Accepted
- **Date:** 2026-11-05
- **Source:** Decided by Willem in the plan, section 2 (Secrets) and M2 (Dependencies), and on CSR-12.

## Decision

- `internal/secrets` uses `github.com/zalando/go-keyring` for the keychain.
- A keychain failure falls back to a 0600 file in the state folder (CSR-12 review item 7).

## Reason

- It reaches the macOS Keychain, Windows Credential Manager and Linux Secret Service on three OSes without cgo.
