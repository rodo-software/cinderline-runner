# Decisions

[current.md](current.md) says what is decided for the runner now, one rule per line with its sources. It and the dated records next to it are binding.

[archive/](archive/) holds the records whose rules current.md carries, unchanged, with the review notes of the first fold on 2026-11-17. It is history and not binding: a decision named in a code comment is a file there, and current.md wins where they differ.

## Adding a decision

Record every decision Willem makes, with its reason, as a new file next to current.md named `YYYY-MM-DD-topic.md`, and refer to it as `decision YYYY-MM-DD-topic`. Each record has: Status, Date, Source (who decided, which issue), Decision, Reason. Records in the Cinderline app repository are referred to by their name there; they are not copied here.

A record is folded and archived once it is built: each binding point goes into current.md in one or two lines with the record's name, only what holds now, and the record moves unchanged into archive/. It stays next to current.md only while an open issue still implements it or it holds detail current.md cannot carry in a few lines.

## Dated records in force

- [2026-11-16-runner-protocol-v2](2026-11-16-runner-protocol-v2.md): the runner API contract, the same record in both repositories, with [`docs/runner-api/openapi.yaml`](../runner-api/openapi.yaml). Kept for its detail.
- [2026-11-17-push-scan](2026-11-17-push-scan.md): the push scan, and the contract change the app makes first. Kept until the app has made that change and the contract is copied here.
- [2026-11-26-local-web-page](2026-11-26-local-web-page.md): the runner's local web page. Kept until the last `Web NN` issue has built it.
- [2026-11-27-web-look](2026-11-27-web-look.md): the look of that page, which amends its "follows Syncthing" line. Kept with it.
- [2026-11-27-agent-cli-updates](2026-11-27-agent-cli-updates.md): the agent CLIs follow their vendors' releases (CLR-317). Folded into current.md; archived once CLR-317 is merged.
- [2026-11-28-skill-downloads](2026-11-28-skill-downloads.md): a skill package's downloads (CLR-318). Kept until the app has added `downloads` to its contract and it is copied here.
- [2026-11-28-web-remove-project](2026-11-28-web-remove-project.md): removing and renaming a project from the page (CLR-302). Kept with 2026-11-26-local-web-page.
- [2026-11-28-update-restart](2026-11-28-update-restart.md): the daemon re-execs itself after an update from the page (CLR-303). Kept with 2026-11-26-local-web-page.
