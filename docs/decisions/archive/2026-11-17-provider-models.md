# The models of the runner's own providers in the poll

- **Status:** Accepted; points 3 and 4 superseded by [2026-11-24-model-lists](2026-11-24-model-lists.md): a failed fetch keeps the last good list with its age, and `openrouter` is reported. The limit is now 500 ids.
- **Date:** 2026-11-17
- **Source:** CLR-192, building on decision 2026-11-17-local-model-providers. Willem on CLR-192: `openrouter` is not reported (choice A), and the models `opencode models` lists are not reported now (choice A). The rest follows the issue's proposal; Claude designed the details. The app side is CL-237.

## Decision

1. **The runner asks its own providers for their models.** For each provider in its config it asks the OpenAI-compatible `GET <base URL>/models`, with the provider's key as a bearer token when it has one, and takes the `id` of each entry of `data`. A provider configured as `host.docker.internal` is asked at `127.0.0.1` on the same port, because the runner asks from its own machine.
2. **Once for the runner, never in a poll's path.** One fetch serves every project, at start and then every 10 minutes; each provider has 5 seconds and they are asked at once. Each poll's report takes the last list; before the first fetch ends the list is empty.
3. **A provider that fails has no models.** No answer in time, a status other than 200, or a body that is not a JSON object (read up to 4 MiB) gives an empty list and an Info log line. Each fetch replaces the last list, so a provider that stops answering shows as empty: the app then offers the free-text model for it. It is never an error in the report and never blocks a poll or a run.
4. **`openrouter` is not reported** (Willem, CLR-192, choice A): the app already has OpenRouter's public list for its prices.
5. **Not now: `opencode models`** for OpenCode's own built-in providers (Willem, CLR-192, choice A). It would mean running the pinned CLI per project with that project's credentials, and what version 1.19.5 prints is unchecked; a later issue can add it after that check.
6. **Limits**, cut by the runner before it sends, as the other report fields: at most 50 providers, at most 200 ids each; an id that is not 1 to 150 of letters, digits and `. _ : / -` (the app's `AgentClient::TYPED_MODEL`) is left out, and the next one takes its place.
7. Nothing about a provider's URL or key leaves the runner, as before.

## Contract change (for the app repository)

`Report` gains an optional `provider_models` (left out: none), next to `model_providers`, which stays as it is. The runner always sends it, as `[]` when it has no providers. An app without a rule for it ignores it, so the runner may ship first.

```yaml
        provider_models:
          type: array
          maxItems: 50
          description: The model ids each of the runner's own providers served when the runner last asked its OpenAI-compatible `GET <base URL>/models`, on start and then every 10 minutes (decision 2026-11-17-provider-models). Not `openrouter`. A provider that did not answer with a model list has an empty `models`; the app then keeps the free-text model for it. No URL or key.
          items:
            type: object
            required: [provider, models]
            properties:
              provider:
                type: string
                maxLength: 50
                pattern: '^[a-z0-9][a-z0-9-]{0,49}$'
              models:
                type: array
                maxItems: 200
                items:
                  type: string
                  pattern: '^[A-Za-z0-9._:/-]{1,150}$'
```

The change starts in the app repository; `docs/runner-api/openapi.yaml` here is copied from there afterwards, as for decision 2026-11-17-push-scan.

## Reason

1. A typo in a typed model is found today only when the turn does not start; the runner can reach the provider, the app cannot.
2. A fetch in the background with a short timeout keeps a slow or switched-off provider (a laptop, a Mac Studio asleep) from ever holding up a poll, and one fetch for all projects asks each provider once.
3. Reporting an empty list rather than the last good one keeps the report a fact about now; the app's fallback, the free-text field, is right for a provider that does not answer.
