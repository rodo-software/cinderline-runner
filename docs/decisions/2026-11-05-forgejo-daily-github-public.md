# Day-to-day work on the private Forgejo repository; GitHub is the public mirror for source, issues and releases

- **Status:** Accepted
- **Date:** 2026-11-05
- **Source:** Decided by Willem on 2026-11-05 when the first M0 pull request waited on a Forgejo runner. Amends the "Repository" row of decision 2026-11-05-runner-rewrite-details.

## Decision

- The runner project in CinderStack points at the private repository on git.rodo.nl. Teams push branches and open pull requests there; a trusted team merges there.
- `github.com/rodo-software/cinderstack-runner` is a push mirror of it: the public source, public issues from users, GoReleaser releases on tags, and the GitHub Actions matrix (Linux, macOS, Windows) plus the snapshot build.
- Forgejo Actions on git.rodo.nl is the gate for pull requests, with a Linux runner (label `docker`) on the Hetzner server that hosts Forgejo. The Go checks and the Linux Docker integration tests run there. macOS, Willem's own platform, is covered by the GitHub matrix on the mirror and by Willem running the runner locally.
- Agents got a one-time permission to write the CI configuration in M0; after that the rule in AGENTS.md holds again: no edits under `.forgejo/` or `.github/`.

## Reason

The repository stays private until it is worth publishing, and Forgejo already runs the agent teams. GitHub's hosted runners are the only free way to test Windows and macOS in a matrix and to attach build provenance to releases, so that part runs on the mirror.
