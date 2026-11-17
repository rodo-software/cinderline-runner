# Local model providers for OpenCode agents

- **Status:** Accepted
- **Date:** 2026-11-17
- **Source:** Willem, 2026-11-17: "Cerebellum is my own local LLM on a mac studio, we absolutely need Cinderline to work with local LLMs, so we reverse the CSR-91 decision." His answers the same day: a provider on another machine and one on the runner's own machine must both work; OpenAI-compatible endpoints with and without a key; a team names a local model as free text. Claude designed the rest. The same record is in the runner repository.

## Decision

1. **A runner can have model providers of its own.** A provider is added once on the runner, for all its projects (Willem, 2026-11-17: "a provider is added to Cinderline globally and projects can choose to use them or not"): a name (lower-case letters, digits and dashes; not `openrouter`), an OpenAI-compatible base URL (`http` or `https`, with a port or the scheme's default), and optionally an API key, kept with the runner's other secrets. A project uses a provider simply because one of its teams' agents names it in its model; there is no switch per project. The runner still runs no model itself: no downloads, no GPU work.
2. **OpenCode agents use them.** An OpenCode agent's model is `<provider>/<model id>`. `openrouter/...` works as before. For any other provider the runner gives OpenCode that provider's base URL and key for the turn. A turn whose provider the runner does not have is not started.
3. **Network.** For a turn that uses a provider, the runner's proxy also allows exactly that provider's host and port, and only for that turn. A provider on the runner's own machine (`localhost`, `127.0.0.1` or `host.docker.internal` in its URL) is reached as `host.docker.internal` on exactly its port, through the proxy only: the agent container still has no route to the host. Nothing else on the host becomes reachable: a project's `allow_hosts` refuses `localhost`, loopback addresses and `host.docker.internal`.
4. **The runner reports its providers.** The poll's report gains `model_providers`: the names this runner can use for OpenCode in that project, `openrouter` when the project's key is set, plus each provider of the runner (at most 50 names of at most 50 characters). The app does not learn URLs or keys.
5. **The app accepts a free-text model for OpenCode.** Besides the fixed list, an OpenCode agent's model may be any `<provider>/<model id>` (provider as in point 1; model id of letters, digits and `. _ : / -`; the whole model at most 100 characters, which the app's columns hold). The fixed cerebellum entry leaves the list: it is typed like any other local model.
6. **Job offers respect providers.** An issue whose team has an OpenCode agent on a provider the runner does not report is passed over, with the progress line the other passed-over cases get, naming the provider.
7. **Cost.** Usage on a model the app has no price for is stored without a cost, as today.

## What this replaces

- Runner decision `2026-11-11-no-local-model-work`: its second point (no provider from a configured URL) is reversed; its first point (the runner runs no model backends) stays.
- Runner decision `2026-11-09-network-allowlist`, point 3 ("`host.docker.internal` is never allowed"): it is now allowed on exactly a configured provider's port, through the proxy only.
- App decision `2026-11-03-agent-model-list`: the OpenCode list is no longer closed.

## Reason

Running teams on a model the customer hosts is a reason to choose Cinderline, and Willem's own teams need it. A named provider with one exact host and port keeps the network rule's point (an injected prompt cannot reach arbitrary hosts or the host's other ports) while making local models possible. Keeping URLs and keys on the runner keeps the app out of the customer's network.
