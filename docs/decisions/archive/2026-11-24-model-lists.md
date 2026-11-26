# Each client's and provider's current model list in the poll

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** CLR-218, the runner side of app decision 2026-11-24-model-lists-from-runners (CL-291, Willem's answer B there: runner catalogs with 30 minutes of grace). Willem on CLR-218, answer B: every client asks its own CLI, but a Claude Code API key lists Anthropic's models. Claude designed the rest. Supersedes points 3 and 4 of 2026-11-17-provider-models; the rest of that record stands.

## Decision

1. **Each client asks its own CLI.** The pinned CLI runs in a short container of the default image with the client's state volume, the tools and the client's turn environment (its credential or login), on Docker's default network as detection's key check does, and lists what its account can run. No turn runs and no tokens are spent:
   - Claude Code answers the Agent SDK's `initialize` control request (`claude -p --input-format stream-json`) with its `models`; each is reported as the full id its alias resolves to (`resolvedModel`), as teams store `claude-opus-5-5`, never `opus`. `current_model` comes first.
   - Codex answers app-server's `model/list` (`codex app-server`, JSON-RPC), hidden models left out, the one marked `isDefault` first.
   - Gemini CLI prints `gemini models list --output-format json`; its first entry, the auto choice, is its default.
   A small node script from the tools volume speaks to Claude Code and Codex and stops the CLI after its answer, or after 60 seconds.
2. **API keys add the vendor's list.** A Claude Code API key (not a `setup-token`) lists Anthropic's `GET /v1/models` after the default the CLI names (Willem, CLR-218, answer B). A Gemini API key (not Vertex) adds Google's `GET /v1beta/models` that support `generateContent` after the CLI's list, so a paid key keeps every Gemini model (app decision all-gemini-models). A Codex API key has the CLI's list only: OpenAI's list cannot be narrowed to what Codex runs. Both vendors are asked from the runner, with the key in a header, one page of up to 1000.
3. **OpenCode sends none:** its models are its providers'. Only clients detected `ready` are asked.
4. **OpenRouter** is a provider in `provider_models`, first, for a project whose OpenRouter key is usable: its public list of models that take tools (`GET https://openrouter.ai/api/v1/models?supported_parameters=tools`), asked without a key, once for the runner and only while some project has the key.
5. **When:** at start, after each detection and every 10 minutes, in the background, never in a poll's path; the providers' fetch as before. Each project asks its own clients, as credentials are per project.
6. **The last good list stays.** A client or provider whose fetch fails keeps its last good list and the time it was fetched; `models_age` is the whole seconds since then, so the app stops using it after 40 minutes. Never fetched well: a client sends no `models`, a provider an empty list and no age. An answer without a list (no `data`, no answer to the request) is a failed fetch; a list without entries is an empty list.
7. **Limits**, cut by the runner before it sends: at most 500 ids per client and provider; a client's id at most 100 characters, a provider's at most 150, and only `A-Z a-z 0-9 . _ : / -` (ids such as `claude-opus-5-5[1m]` are left out).

## Reason

Only the CLIs know what a login can run and which model is their default; the vendors' HTTP lists need an API key and name no default. One path per client keeps logins working, and the CLI refreshes its own login. Willem preferred Anthropic's full list for an API key over the CLI's short picker list. Keeping the last good list carries a short outage of a vendor's endpoint through the app's 30 minutes of grace.

## Unverified

The CLIs' answers were read from the pinned packages (Claude Code 2.1.316, Codex 0.167.0, Gemini CLI 0.67.0), not run: the agent that built this had no Docker or credentials. The first run on a real machine must confirm each client's list and default in the log (`models of a client not listed` is logged at Info on failure).
