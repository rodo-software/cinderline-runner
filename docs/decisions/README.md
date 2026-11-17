# Decisions

[current.md](current.md) says what is decided for the runner now, one rule per line with its sources. It and the dated records next to it are binding.

[archive/](archive/) holds the records that current.md replaced on 2026-11-17, unchanged, with the review notes used to fold them in. It is history and not binding: a decision named in a code comment is a file there, and current.md wins where they differ.

## Adding a decision

Record every decision Willem makes, with its reason, as a new file next to current.md named `YYYY-MM-DD-topic.md`, and refer to it as `decision YYYY-MM-DD-topic`. Each record has: Status, Date, Source (who decided, which issue), Decision, Reason. Its rules are folded into current.md from time to time. Records in the Cinderline app repository are referred to by their name there; they are not copied here.

## Dated records in force

- [2026-11-16-runner-protocol-v2](2026-11-16-runner-protocol-v2.md): the runner API contract, the same record in both repositories, with [`docs/runner-api/openapi.yaml`](../runner-api/openapi.yaml).
- [2026-11-17-second-audit-decisions](2026-11-17-second-audit-decisions.md): a stop during publishing; no agent header.
- [2026-11-17-local-model-providers](2026-11-17-local-model-providers.md): model providers for OpenCode agents.
- [2026-11-17-open-questions-answered](2026-11-17-open-questions-answered.md): Willem's answers after the clean-up: seccomp forced on agent containers; the push scan's rules.
