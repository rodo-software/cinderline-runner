# The output kept of a turn or a check

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** CLR-226, from the repo audit of 2026-11-24. Willem on CLR-226, answer C: cap the output with larger sizes. Claude designed the rest.

## Decision

1. **Head and tail.** The runner keeps a turn's or a check's first 1 MB of output and its last 128 MB, in a rolling buffer of up to twice that, as `forge.tail` does. Past 129 MB, the middle is cut and replaced by one line `[N bytes of output cut]`, and the runner logs a warning with the bytes cut.
2. **What a cut costs.** The session ID (the start) and the report, final usage and turn and usage limits (the end) are still read. The tool-call counts of every client, and OpenCode's tokens and cost, which it only prints per step, miss the cut middle: they are undercounted for such a turn. A check uses only its tail, so it loses nothing.
3. **The log.** Output is logged a line at a time, as before. A line still unfinished after 1 MB is not logged: one placeholder line stands for it, and logging goes on after its end. A part of a line could hold part of a secret that redaction no longer recognises.

## Reason

A check or an agent that prints without end, for as long as `max_minutes`, made the runner hold all of it in memory, and a runner out of memory takes every lane of every project down with it. Exact counts would need every client to parse its output while it streams (option B, several times the change). Willem chose larger sizes than the audit's 1 MB + 32 MB (option C) to make the undercount rarer, at up to about 257 MB of memory per running lane in the worst case.
