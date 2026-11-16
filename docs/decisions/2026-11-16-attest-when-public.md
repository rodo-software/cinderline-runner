# Releases skip build provenance while the GitHub repository is private

- **Status:** Accepted
- **Date:** 2026-11-16
- **Source:** Decided by Willem after the v0.1.7 release failed at its attestation step; proposed by Claude. Amends decision 2026-11-05-runner-rewrite-details item 16 and decision 2026-11-05-ci.

## Decision

- The `actions/attest-build-provenance` steps in `release.yml` and `default-image.yml` run only when `github.com/rodo-software/cinderline-runner` is public (`!github.event.repository.private`).
- While it is private, releases and the default image carry no attestation; the installers' SHA-256 check against `checksums.txt` still holds.
- Once the repository is made public (decision 2026-11-05-forgejo-daily-github-public), attestations come back without a change.

## Reason

- GitHub stores artifact attestations for private repositories of an organization only on Enterprise Cloud; on the current plan the step fails with "Feature not available for the rodo-software organization", which fails the whole release.
- A condition on the repository's visibility needs no edit when it goes public, unlike removing the step.
