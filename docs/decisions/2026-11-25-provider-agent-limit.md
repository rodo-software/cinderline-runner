# A limit on the agent turns per model provider

- **Status:** Accepted
- **Date:** 2026-11-25
- **Source:** Willem, CLR-243: "Allow setting maximum agents per AI provider (across projects, to limit parallel calls to own AI models primarily)". Claude designed the rest; each point below is vetoable.

## Decision

1. A runner's model provider (decision 2026-11-17-local-model-providers) may have `max_agents`: the most agent turns on it at once, over every project of the runner. 0, or no field, is no limit; at most 100. `add-provider --max-agents N` and `set-provider NAME --max-agents N` set it (`0` removes it), `list-providers` shows it, and the change reloads a running runner.
2. A turn is on a provider when its agent is an OpenCode agent whose model is `<provider>/<model id>`, read as OpenCode reads it. OpenRouter and the Claude Code, Codex and Gemini clients' own providers have no limit: they are not the runner's providers.
3. A turn of a `run` or `resolve` step past the limit waits, inside its step, until a turn on that provider ends; it then holds its place until it ends. A check, a publish and the time between steps hold no place. The runner logs once when a turn starts to wait. The poll's stop ends a waiting turn as `stopped`, without running it.
4. The wait is the runner's alone: no contract change, and the app is not told. The run keeps its slot and its memory reservation (decision 2026-11-24-agent-memory-limit) while it waits, and its issue stays In progress.
5. The count is one for the runner process, so it holds across a reload: turns still in flight on the older config count against the new limit.

## Reason

A model on the customer's own machine serves few requests at once; more turns at once only slow each other down or fail. The runner alone sees every project's turns on one provider, so it is where the limit can hold. Waiting at the turn, not at the poll, needs no change to the contract: the poll cannot know which provider a job's agents will use before the app sends the job.
