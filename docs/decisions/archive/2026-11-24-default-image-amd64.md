# The default image for amd64, and no local build when the pinned pull fails

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** CLR-240, from the third repo audit of 2026-11-24, written by Willem. Replaces the arm64-only part of point 2 of 2026-11-12-ci-speedups.

## Decision

1. **Both platforms.** The default image is built natively for linux/arm64 (`ubuntu-24.04-arm`) and linux/amd64 (`ubuntu-24.04`), each pushed by digest, and merged into one manifest list tagged with the Dockerfile's sum. The attestation and the pin are that list's digest. The workflow is drafted in `docs/ci/default-image.yml`, because agents may not edit `.github/`.
2. **No fallback.** When a pin exists for the embedded Dockerfile and its pull fails, `Image` returns the error: the run does not start and the next poll tries again. The runner builds the default image locally only for a Dockerfile that has no pin yet.
3. **Order.** Pin only a manifest list that has both platforms. A test fails when the pin matches the Dockerfile while `.github/workflows/default-image.yml` does not name linux/amd64.

## Reason

With an arm64-only image, every amd64 machine (Linux servers, Intel Macs) tried a pull that could not succeed on every prepare, turn and model listing, then ran a local build from unpinned inputs (`mlocati/php-extension-installer` without a tag, `composer:2`, NodeSource's setup script run as root). The digest pin and its attestation protected Apple silicon only. The same fallback let a network or registry failure swap the pinned image for an unpinned one on any machine.
