# A Cinderline instance over https, plain http only on the runner's own machine

- **Status:** Accepted
- **Date:** 2026-11-19
- **Source:** Willem, CLR-208 (security audit of 2026-11-19, finding 6): option A, "fail loudly".

## Decision

1. A project's `url`, the Cinderline instance, must be `https`. Plain `http` is allowed only when the host is `localhost` or a loopback address (`127.0.0.0/8`, `::1`), for local development. `host.docker.internal` and private network addresses are not the runner's own machine for this rule and need `https`.
2. `add` refuses such a URL before it sends the runner token to it.
3. `config.json` that already holds a plain `http` URL on another host fails to load: the daemon does not start, a reload keeps the old config, and every CLI command reports the error until the `url` is changed to `https` by hand. There is no warn-only path.

## Reason

The runner token, the agents' scoped tokens and every prompt travel over this URL. A warning would leave them crossing the network in clear until someone acts on it. This is the same rule as repository URLs, which are https only.
