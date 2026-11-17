# CI speed-ups and macOS-only checks

- **Status:** Accepted, temporary for the platform parts
- **Date:** 2026-11-12
- **Source:** Willem, in a Claude Code session on slow and duplicated GitHub Actions runs

## Decision

1. GitHub's `ci` tests on macOS only for now; Windows is off until there are Windows users. Linux stays on Forgejo. Release archives are still built for all six targets: cross-compiling them takes seconds and the installers expect them.
2. The default image is built only when `images/default/` changes on main, or by hand, not on every release tag, and is tagged with the Dockerfile's sum. It is built for `linux/arm64` only, natively on `ubuntu-24.04-arm` with the GitHub Actions layer cache, instead of amd64 and arm64 under QEMU. Machines without a matching image build it locally, as they already do when the pull fails.
3. The draft workflows `platforms.yml` and `integration.yml` move from `.github/workflows/` to `docs/ci/`: on GitHub the first duplicated `ci` on five systems and the second waited forever for a `linux-host` runner.
4. Forgejo's `ci` skips the Linux checks on tag pushes, whose commit passed them on main, and each push mirrors only its own ref, so one tag starts one release.
5. `ci` cancels an older run on the same branch or pull request.

## Reason

Each release tag spent 15 to 24 minutes rebuilding an unchanged image under emulation, every main push ran `ci` twice, and one tag started its release twice. Willem uses the runner on macOS only, so Windows and amd64 work was spent on no user.
