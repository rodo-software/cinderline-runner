# CI log always sent on a failed check

- **Status:** accepted
- **Date:** 2026-11-25
- **Source:** Willem, CLR-244 (with CL-374, CL-375)

## Decision

1. Every failed check the runner sends has a `log`. It is the end of the CI log; a read that fails is tried once more. Without a log (external CI, a third-party app, a read that failed twice) it is a line saying why, then the forge's own text: GitHub's check output title, summary and text, else the check's description (GitLab's `failure_reason`, Forgejo's and Bitbucket's description), else a line that the forge says only that it failed. A failed read adds the error's short message.
2. The runner reads every failure the forge reports: GitHub's commit statuses and every page of check runs (100 a page, 10 pages at most), GitLab's external commit statuses (those whose target is not a `/-/jobs/` page), every page of Bitbucket's build statuses (10 pages at most), and the log of the first failed step of a Bitbucket Pipelines build.
3. A failed state without a failed check is one check that says what the forge says: GitLab's `pipeline` with its status and `yaml_errors`, Forgejo's `combined status`. Never `failed` with no check.
4. No text of a check (name, description, log) carries a URL: each is replaced with `<URL>` before sending. `url` keeps its own field; the app drops it from what the agent sees.

## Reason

The agent cannot reach the forge (CL-374) and never gets a forge URL (CL-375), so what the runner sends is all it has. A failure with only a name and a URL leaves it guessing.
