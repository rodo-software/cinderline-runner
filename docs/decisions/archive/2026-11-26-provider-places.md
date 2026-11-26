# Runs admitted per model provider: places in play, reported in the poll, and a lookahead

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, CLR-253 (2026-11-25), implementing the app's decision `2026-11-25-provider-places` (CL-420). The default `lookahead` of 0 is Willem's, 2026-11-25, on CLR-253; the app's record says 1, and this runner's setting is the runner's to decide.

## Decision

1. A runner's provider with `max_agents` keeps limiting **turns** as before (decision 2026-11-25-provider-agent-limit, points 1 to 3 and 5).
2. New: it also has `max_agents + lookahead` places for **runs in play**. A run is in play on a provider from when its job was taken with that provider (the job's `provider`), or from its first turn on it, until the run ends; a check, a publish and the time between steps do not end it. A turn's run enters play also when the provider is full: a run that goes is never refused, it only keeps new runs out.
3. `lookahead` is how many runs past `max_agents` may be in play: 0 to 100, 0 or no field being none, and refused above 0 without `max_agents`. `add-provider` and `set-provider` take `--lookahead N`; `list-providers` shows it.
4. Every poll that offers a slot (`idle` or `manager_idle`) reserves each limited provider's free places, `max(0, max_agents + lookahead - in play - reserved by other polls)`, and sends them as `provider_places`; a provider without `max_agents` is not listed, and with none the field is left out. A place is in one poll in flight at a time, so two projects never both offer it. A started job that names a listed provider puts its run in play at once; what the answer did not use goes back when it is handled, also after a failed poll. A poll that offers no slot sends and reserves nothing.
5. The count is one for the runner process, over every project and Cinderline instance, and holds across a reload.
6. When a run leaves play on a limited provider, every project's loop polls at once instead of after `poll_after`. A place given back unused by a poll wakes no loop: two loops would wake each other for ever; it reaches the others at their next poll.
7. The runner logs once when a poll reports 0 free places for a provider, again only after it had a place in between.

This changes point 4 of 2026-11-25-provider-agent-limit: the app is now told, through `provider_places`, and leaves an issue in To do when the provider of its first turn has no free place, instead of starting it to wait In progress. A turn past `max_agents` still waits inside its step and keeps its slot and memory; reporting which runs wait (`waiting`) is CLR-254.

## Reason

With the limit known only to the runner, the app started issues that then waited In progress with nothing running (CL-400 and CL-419 on 2026-11-25). The runner alone sees every project that shares a provider, so it counts; the app orders within the places it is told.

`lookahead` defaults to 0, one run per place finished before the next starts, because on Willem's local llama.cpp server with one slot a conversation pushed out of the slot by another run is lost: reading an 85k-token context again took about 14 minutes. A higher `lookahead` is for providers that keep several conversations cached.
