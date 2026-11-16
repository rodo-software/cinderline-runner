# `add` asks for the model keys; `key set` replaces one later

- **Status:** Accepted
- **Date:** 2026-11-10
- **Source:** Decided by Willem on CSR-150 (option A of three).

## Decision

- `cinderline add` asks, after the git token and its copy offer, for the four model keys (Anthropic API key or `claude setup-token` token, OpenRouter, OpenAI, Gemini), each optional: an empty answer or the end of input skips it. They are read in plain text like the tokens and stored with the other secrets of the project.
- `cinderline key set CLIENT --project KEY [-url URL]` (CLIENT: claude, opencode, codex, gemini) reads one key from stdin and sets or replaces it, then reloads a running runner as `add` and `login` do.
- No key is ever printed back. A stored key still wins over a `login` sign-in.
- No `key remove` (option C was not chosen): `remove` and `uninstall` stay the only commands that delete keys.

## Reason

No command stored the first model key, so OpenCode could not run at all and an API key could not be used instead of a subscription. `add` refuses a project that is already added, so updating keys through `add` would mean reworking its flow; a small separate command for later changes is less code and reuses `login`'s `CLIENT --project KEY` shape.
