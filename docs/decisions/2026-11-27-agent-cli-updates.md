# Decision 2026-11-27-agent-cli-updates

- **Status:** Accepted
- **Date:** 2026-11-27
- **Source:** Willem on CLR-317, answers to two questions: option A of question 1 (all four CLIs follow their vendors' latest release) and option B of question 2 (Claude Code's `stable` channel). The defaults stated with question 2 were not vetoed.

## Decision

1. **The agent CLIs follow their vendors.** Claude Code, OpenCode, Codex and Gemini CLI are no longer held at the versions of a runner release. The runner resolves each one's latest release from its vendor and the hashes the vendor publishes for it, over TLS, and trusts those. Claude Code: the `stable` channel of `downloads.claude.ai/claude-code-releases/` and that version's `manifest.json`. OpenCode: npm `opencode-linux-<arch>[-musl]` at `latest`, `dist.integrity` per platform. Codex: the latest GitHub release `rust-v*` of openai/codex, `assets[].digest`. Gemini CLI: npm `@google/gemini-cli` at `latest`, `dist.integrity`. A version is accepted only as digits and dots with an optional suffix, and every hash only in its own form.
2. **Cadence.** Five minutes after start, so that the projects' first detection and model lists are there, and then every hour. One request per vendor and platform; a download only when a version changes.
3. **A check before a version goes live.** The new set's volume is filled next to the live one, and each running project checks it with its own credentials. A client fails when its `--version` prints no version, or when its live CLI listed models and the new one lists none. A client that fails keeps its live version, and the others go live without it. A failure is a warning in the runner's log only: the poll then reports the version that runs. An entry in the report would need a new error kind in the app's contract. The check is tried again every hour.
4. **Between runs only.** A run takes its tools mount at its prepare and passes it in every turn, so a run keeps its CLIs while new ones go live. Detection and model listing take the live ones.
5. **The live versions are kept** in the state folder (`tools/versions.json`) with the previous ones, so a restart, a login and every project use them. Without that file the runner uses pins.go's versions, so it starts without a network.
6. **One previous version is kept.** The volumes of the current and previous versions stay. Any other tools volume goes once no mount used it for 24 hours. The lock file in the state folder records each mount, as for skill packages, so a run in flight keeps its volume.
7. **Escape hatch.** `cinderline pin CLIENT VERSION` holds one CLI at a version for every project (`pinned` in config.json), and `cinderline pin CLIENT latest` lets it follow its vendor again. A pinned version passes the same check.
8. **Node and ripgrep stay pinned** in pins.go: they are runtime, not agent CLIs.
9. **The poll already reports each client's version** (`clients[].version`, from `--version`). This needs no contract change.
10. This replaces decision 2026-11-26-client-smoke-check for the live versions. The smoke check still gates a bump of pins.go, which is now only the fallback.

## Reason

The pins fell behind quickly. Claude Code was held at 2.1.316 while 2.1.332 was out, so the runner could not list Fable 5.2, and teams on that model never started. New models, fixes and protocol changes all waited on a runner release.

For Claude Code, Willem chose the `stable` channel over `latest`. On 2026-11-27 `stable` was at 2.1.326 and `latest` at 2.1.332: a stable release has settled for about a week, and new models wait about that long.

The check before switching catches a CLI that does not start or no longer lists its models. It does not catch a changed turn output (session ID, usage, stream format). `cinderline pin` holds such a release back.
