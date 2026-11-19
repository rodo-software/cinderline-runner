# A successful login deletes the client's stored model key

- **Status:** Accepted
- **Date:** 2026-11-19
- **Source:** Willem on CLR-210.

## Decision

1. Once `cinderline login claude|codex|gemini` has confirmed the credentials are on the state volume, it deletes that client's stored model key for the project and says so in its success line. A sign-in that fails, or leaves no credentials file, keeps the key.
2. Detection and runs keep their order: a stored key still wins over a login. So the last of `login` and `key set` decides: a later login replaces the key, a later `key set` overrides the login.

## Reason

A person who signs in expects the runs to use that sign-in. With a stored key left in place, an invalid key kept every run and the runner report on `invalid_credential` after a login that reported success (runner 0.5.2).
