# Client detection checks keys for free and leaves logins untested

- **Status:** Accepted
- **Date:** 2026-11-09
- **Source:** Decided by Willem on CSR-59 (option B of three).

## Decision

- Per project and client, detection reports `missing_binary` when the CLI's `--version` fails in a container with the tools volume, `missing_credential` when there is neither a keychain key nor a login file on the state volume (tested for existence only, never read), and otherwise `ready`, or `invalid_credential` when a keychain key is refused.
- A keychain key is tested with one free provider call (list models; OpenRouter's key endpoint), run with Node's `fetch` from the tools volume in a container; the key and URL travel only in the environment. Only HTTP 401 or 403 counts as refused; anything else counts as ready, so a flaky provider never blocks runs.
- A login is never tested: an expired login fails on the first run. A Claude `setup-token` and a Vertex Gemini key are not tested either, because no free endpoint is known to accept them.
- Detection runs at slot start (so after `add`, `login` and a reload, which `add` and `login` now trigger) and every hour; the result is held in memory only (0091), rides the runner report at most once per heartbeat and only when changed (2026-11-08-errors-reach-cinderline), and skip-before-claim uses the same result.

## Reason

Only a real model turn could prove a login works, at roughly $0.10-0.30 per project per day; Willem judged that not worth paying "just for checking if a session is alive": a run that fails says the same, for free.
