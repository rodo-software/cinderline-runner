# Decisions

[current.md](current.md) says what is decided for the runner now, one rule per line with its sources. It and the dated records next to it are binding.

[archive/](archive/) holds the records that current.md replaced on 2026-11-17, unchanged, with the review notes used to fold them in. It is history and not binding: a decision named in a code comment is a file there, and current.md wins where they differ.

## Adding a decision

Record every decision Willem makes, with its reason, as a new file next to current.md named `YYYY-MM-DD-topic.md`, and refer to it as `decision YYYY-MM-DD-topic`. Each record has: Status, Date, Source (who decided, which issue), Decision, Reason. Its rules are folded into current.md from time to time. Records in the Cinderline app repository are referred to by their name there; they are not copied here.

## Dated records in force

- [2026-11-19-app-host-addresses](2026-11-19-app-host-addresses.md): app approvals cannot reach private addresses, including through DNS; local config retains its private-network access.
- [2026-11-16-runner-protocol-v2](2026-11-16-runner-protocol-v2.md): the runner API contract, the same record in both repositories, with [`docs/runner-api/openapi.yaml`](../runner-api/openapi.yaml).
- [2026-11-17-second-audit-decisions](2026-11-17-second-audit-decisions.md): a stop during publishing; no agent header.
- [2026-11-17-local-model-providers](2026-11-17-local-model-providers.md): model providers for OpenCode agents.
- [2026-11-17-project-key-rename](2026-11-17-project-key-rename.md): a local `id` per project; `rename` takes a renamed key by hand; old config fields are ignored. That the `id` never changes is replaced by the next record.
- [2026-11-17-rename-moves-local-names](2026-11-17-rename-moves-local-names.md): `rename` also moves the project to the new key's `id`: secrets move, Docker volumes are removed.
- [2026-11-17-open-questions-answered](2026-11-17-open-questions-answered.md): Willem's answers after the clean-up: seccomp forced on agent containers; the push scan's rules.
- [2026-11-17-push-scan](2026-11-17-push-scan.md): the push scan, and the contract change the app makes first.
- [2026-11-17-provider-models](2026-11-17-provider-models.md): the models of the runner's own providers in the poll, and the contract change for the app.
- [2026-11-17-hosts-from-cinderline](2026-11-17-hosts-from-cinderline.md): a person's answer in Cinderline opens a refused host for one issue's runs; refused hosts per step; strict host blocking. Supersedes point 2 of 2026-11-09-network-allowlist.
- [2026-11-18-drain-before-reload](2026-11-18-drain-before-reload.md): commands that reload the runner drain it first while runs are in flight, with `s` to stop at once. Points 1 and 2 are replaced for config changes by the 2026-11-24 record below.
- [2026-11-18-verbose-agent-output](2026-11-18-verbose-agent-output.md): container output in `run -v` as plain lines with colours only, cleaned in the log file.
- [2026-11-19-login-replaces-key](2026-11-19-login-replaces-key.md): a successful `login` deletes the client's stored model key; a later `key set` wins again.
- [2026-11-19-docker-26](2026-11-19-docker-26.md): Docker Engine 26.0 or later, so an agent's and a check's container mount only their issue's own clone.

- [2026-11-19-https-instance](2026-11-19-https-instance.md): the Cinderline URL must be https; plain http only on localhost or a loopback address; an existing config with another http URL fails to load.
- [2026-11-19-compose-local-files](2026-11-19-compose-local-files.md): validate repository-local composition before Compose reads files; freeze the resolved composition for checks and operations.
- [2026-11-24-agent-memory-limit](2026-11-24-agent-memory-limit.md): every agent container is limited to 4 GiB without swap, or the project's `memory`, or its compose agent service's `mem_limit`; the proxy to 64 MiB; runs start only while they fit in one memory budget, Docker's memory less 1 GiB, shared by all projects.
- [2026-11-24-rolling-reload](2026-11-24-rolling-reload.md): a config change reloads each project once its own runs have finished; the runner keeps running; only `remove` and `rename` wait, for their own project.
