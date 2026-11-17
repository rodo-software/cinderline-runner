# Answers to the open questions after the 2026-11-17 clean-up

- **Status:** Accepted
- **Date:** 2026-11-17
- **Source:** Willem, asked one question at a time by Claude on 2026-11-17, after the audits and the clean-up of the decision records. Answers that concern the app are in the app repository's record of the same name.

## Decision

1. **The runner forces Docker's built-in seccomp profile on every agent container** (`--security-opt seccomp=builtin`), and on the services of a project's own compose file. It no longer relies on how the machine's Docker is configured. To build.
2. **The push scan** (CSR-190). A commit that holds a secret the runner knows stops the push, and the app blocks the issue with a note that names the kind of secret and the place, never the value. A match on a well-known shape of secret the runner does not know (a private key block, common provider key formats) does not stop the push: it shows as a warning on the issue. After a hit the clone is kept as it is, with its session; a person moves the issue back to To do, the agent removes the secret, and the scan runs again at the next push. To build.

## Reason

1. Agent containers must not be able to create user namespaces (`unshare`), a common first step in escaping a container. Docker's default seccomp profile refuses it, but a Docker set to `seccomp=unconfined` runs containers without it, silently: the integration test found exactly that on Willem's own machine. A customer's Docker can be set the same way without them knowing.
2. Known secrets are the real risk, since Cinderline itself puts them near the agent, and matching them gives no false alarms. Patterns catch more but also match test fixtures and examples, so they only warn. Keeping the clone loses no work; the commit with the secret is on the developer's own machine and was not pushed.
