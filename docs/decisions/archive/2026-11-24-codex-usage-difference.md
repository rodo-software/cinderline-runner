# Codex usage on a resumed session

- **Status:** Accepted
- **Date:** 2026-11-24
- **Source:** CLR-232, from the second repo audit of 2026-11-24. Willem on CLR-232, answer B: the runner sends the difference. Claude designed the rest.

## Decision

1. **The finding.** Codex 0.167.0's `turn.completed` usage is the thread's total so far, and `codex exec resume` restores that total: a resumed turn reports the earlier turns' tokens again. This was measured with the pinned binary against a local fake Responses API: turn 1's call 1000 input / 100 cached / 10 output reported 1000/100/10; the resumed turn's one call 2000/200/20 reported 3000/300/30. Gemini CLI 0.67.0's headless `--resume` does not restore its token stats (only the interactive session browser does), per its bundle; Claude Code and OpenCode report per call.
2. **The runner sends the difference.** A client marks its usage as the session's total (`Result.UsageTotal`, Codex only). The runner keeps each held session's last total next to its ID, in memory during a run and in the lead's session note (`total`) across runs, and reports a resumed turn's usage as the total less the one before, each count at least 0. A new session, or a turn that names another session, starts at zero; a turn without usage keeps the total.
3. **The contract is unchanged.** Usage stays per turn for every client.

## Reason

The app adds usage up per turn, so counting a resumed session's earlier turns again ran a budget out early. Option A, a contract change so the app took Codex usage as "the latest total wins", would make one client's entries non-additive and needs a change in the app first. The total lives where the session ID already does, so it is lost only with the session: a run that cannot resume starts a new session at zero.
