# The copied runner contract is pinned by its SHA-256

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, CLR-259 (2026-11-26), option C: the hash now, a version line from the app later.

## Decision

1. `contractSHA256` in `internal/cinderline/types.go` holds the SHA-256 of `docs/runner-api/openapi.yaml`. A Go test fails when the file's hash differs from it.
2. A contract change copies the app's file unchanged, checks the types in `internal/cinderline` against it and updates the constant, in one commit.
3. To see whether the copy is behind the app, compare `sha256sum` of the app's `docs/runner-api/openapi.yaml` with the constant. CI cannot: the app repository is private.
4. Later, the app's file gets its own version line in `info`; a CL issue adds it. Until then the hash is the only marker.

## Reason

The app's `info.version` stayed `2.0.0` through every contract change since the protocol's second version, including `RefusedHost`, so it does not mark a change. A hash needs no change in the app, catches any byte of difference and cannot be forgotten the way a version bump can. It needs no dependency and no network.
