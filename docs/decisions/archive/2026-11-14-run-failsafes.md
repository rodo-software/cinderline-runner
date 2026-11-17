# Run failsafes: Claude Code streams its output; the idle limit is a project setting

- **Status:** Accepted
- **Date:** 2026-11-14 (point 1), 2026-11-15 (point 2)
- **Source:** Decided by Willem on CSR-159: point 1 option A; point 2 in his own words, "I want to set this in the Cinderline web panel, but not per team, but per project."

## Decision

1. **Claude Code runs with `--output-format stream-json --verbose`**, like Codex, Gemini CLI and OpenCode already stream. Idle detection is then one timer on the output stream in the Environment (`Spec.Idle`, `ErrIdle`) for every client; the runner does not watch session transcripts on the state volume. `parseClaude` is unchanged: the stream's last JSON line is the same result object, or for a stopped turn the last message, which carries only the session.
2. **The idle limit is the project's `idle_minutes`**, set by admins in the Cinderline web panel and sent on the runner API's Project. Null or absent means 20 minutes. It applies to every agent turn of the project: orchestrator, workers and the Merge Resolver. The app repository must add the field, its setting in the project's panel, return it on `GET /project` and update its route test, as it mirrors `docs/runner-api/openapi.yaml`.

## Reason

1. With `--output-format json` Claude Code prints nothing until its turn ends, so a silent output would mean nothing; polling four CLIs' transcripts with `docker exec` costs more for the same signal. The cost is a busier runner log and failure comments that show the stream's last lines.
2. A tool call is silent until it ends, so the limit must exceed the slowest silent command a project runs (a test suite, a build), which differs per project rather than per team; a setting in the app changes it without a release.
