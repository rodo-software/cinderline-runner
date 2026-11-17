# Redact the secrets the runner knows in what it logs and posts; the push scan follows

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** Decided by Willem on CSR-157 (choice 1, order: "if the audit fixes are on main, both now; if not, redaction now and a follow-up issue for the push scan"). The audit fixes (decision 2026-11-11-audit-fixes) were not on main, so redaction only.

## Decision

- The runner replaces every secret it holds for a project with a marker such as `[redacted anthropic key]`: the runner token, the git token, the model credentials (Anthropic, OpenRouter, OpenAI, Gemini) and the scoped agent tokens it creates for a run. Each value is also caught base64-encoded (at all three byte alignments, so inside a Basic auth header too), hex-encoded and URL-encoded. Values shorter than 16 characters are skipped.
- One package, `internal/redact`, holds the values for the whole process. `secrets.Get` registers every value it returns, and `cinderline.Client.CreateAgentToken` every token it is issued, so no caller has to remember to.
- Redaction happens where text leaves the runner: every log line (the daemon's slog handler, which gets agent output a complete line at a time), every comment and progress line and the report's error messages (`cinderline.Client`), and pull request titles and bodies. That covers the output tail and the agent's report in failure comments, sub-issue and Merge Resolver reports, and error texts the scheduler posts.
- The push scan (part 1 of CSR-157) is a follow-up issue, built on the final `Workspace` after the audit fixes land. Pattern warnings for unknown secrets and the push-scan choices (keep or reset a clone after a hit) are decided there.
- The sign-in files on the state volumes (`cinderline login`) are not read: the rule that the runner never reads them stands until Willem decides otherwise.

## Reason

An agent that prints its environment, by accident or because a hostile issue asks it to, leaked the key into the runner's log and into the failure comment's output tail. The runner knows the exact values, so it can find them with almost no false alarms.

**This is a second line of defence, not a guarantee.** It catches accidents and lazy prompt injection. Splitting, reversing or encrypting a value, or any encoding beyond the three above, gets past it. What an agent posts through MCP itself (`add_comment`, `reply_person`, `create_issue`) never passes the runner and is not redacted. The real protection stays the short-lived scoped tokens, the network allowlist (decision 2026-11-09-network-allowlist) and CI secrets kept off the runner's branches.
