# Runner protocol 2: the runner polls, follows steps and reports facts

- **Status:** Accepted
- **Date:** 2026-11-16
- **Source:** Willem, after the audit of the runner and its API on 2026-11-16: "do all 20 fixes … regardless of how things were or what was decided before … the best, KISS, DRY approach and cleanest architecture". Claude designed the protocol below. The same record is in the runner repository.

## Decision

The runner API is three requests under `/api/runner/v2`. The app decides, the runner does the mechanics and reports facts. The runner never reads or writes an issue: it has no issue, comment, team, project, claim, usage, progress, report, heartbeat, sync or agent-token request. Nothing is kept for older runners or apps: both are released together until the first public release.

Every request carries `Authorization: Bearer <runner token>` and `Cinderline-Runner-Version`. Every answer carries `Cinderline-Api-Version: 2.0.0`. A runner below `runner.minimum_version` gets `426` with `minimum_runner_version`. Both sides ignore properties they do not know.

### 1. `POST /poll`: the one timed request

The runner sends it every `poll_after` seconds, all the time, also during a run. It is the heartbeat.

Request:

```json
{
  "report": {
    "runner_version": "0.4.0",
    "clients": [{"name": "claude", "version": "2.1.0", "state": "ready"}],
    "environments": ["default", "docker"],
    "errors": [{"kind": "not_started", "issue": "EMBER-12", "message": "…"}],
    "refused_connections": 0,
    "model_providers": ["openrouter", "cerebellum"]
  },
  "running": ["EMBER-12"],
  "idle": true,
  "held": ["EMBER-9"],
  "review": [{"ref": "EMBER-7", "pull_request": "open", "conflicts": []}]
}
```

- `report`: always sent by the loop, always whole. A poll without it only asks which project the token belongs to (`add` and `key set` check a token that way): the answer has `poll_after`, `review_after` and `project`, and nothing is stored, blocked or offered. Client states are `ready`, `missing_binary`, `missing_credential`, `invalid_credential`. Error kinds are `repository_mismatch` (no issue) and `not_started` (with `issue`). `model_providers` (optional; left out: none) names the model providers the runner can use for OpenCode, `openrouter` when its key is set plus each configured one; each name is lower-case letters, digits and dashes, starting with a letter or digit (decision 2026-11-17-local-model-providers). `provider_models` (optional; left out: none) lists `{provider, models}`, the model ids each provider reports, without the provider (decision 2026-11-17-provider-models).
- `running`: the top-level issues the runner works on now, and those whose last report still waits in its outbox.
- `idle`: the runner can start a run now.
- `held` (optional): the issues the runner keeps a session or clone of. Sent when it is about to tidy up: at start and while idle.
- `review` (optional): what the runner found when it last kept the Review branches current. Sending the key, also with an empty list, asks for the list. `pull_request` is `open`, `merged`, `closed`, `failed` or `none`; `conflicts` are the files of a base merge that conflicted.
- An issue reference with the project's key from before a rename (decision 2026-11-12-rename-project-keys) names the same issue, in `running`, `held` and `review` as in the paths of `next` and of attachments. A run that began before a rename keeps its key: `next` takes its old-key references in `last` and `sessions`, and its steps name the issue and attachments with the key of the request's path.

Answer `200`:

```json
{
  "poll_after": 30,
  "review_after": 60,
  "project": {
    "key": "EMBER", "name": "Ember", "workspace": "rodo",
    "repository_url": "https://…", "forge": "forgejo", "base_branch": "main",
    "environment": "docker", "compose_file": null, "agent_service": null,
    "check_command": "composer test", "idle_minutes": 20,
    "linked_projects": [{"key": "CSR", "name": "Runner", "repository_url": "https://…", "base_branch": "main"}]
  },
  "job": {
    "issue": {"ref": "EMBER-12", "title": "…", "body": "…"},
    "team": {
      "name": "Builders", "max_minutes": 60, "max_turns": 50, "ponytail": false,
      "agents": [{"name": "Coder", "client": "claude", "model": null, "effort": null}]
    }
  },
  "stop": ["EMBER-12"],
  "held": [{"ref": "EMBER-9", "session": "expire"}],
  "review": [{"ref": "EMBER-7", "title": "…", "merge": true}]
}
```

What the app does, in this order:

1. Stores the report and the heartbeat time on the token.
2. **Unheld issues.** Every active top-level In progress issue of the project with a team that is not in `running` goes to Blocked with the app's note (it names the branch `<name>/<REF>` and says to move the issue to To do to run it again).
3. **`stop`:** the `running` issues whose turn must end: archived, unknown, Cancelled or Backlog, or Blocked; only a Blocked issue whose latest comment is an agent's is left alone for one poll interval, because an agent that asked a person a question is ending its turn itself.
4. **`job`** (only with `idle`, and never while the project is archived or its runners are paused): the first of the top 20 To do issues, in board order, that waits for nothing, has an active team, and whose team's agent clients are all `ready` in the report, with every OpenCode agent's model provider (the part of its model before the first `/`) in `model_providers` (decision 2026-11-17-local-model-providers). The client is checked first: `opencode` is `ready` when its binary is there and at least one provider is usable, and which provider a model needs is the app's check, by name. It is an offer: nothing is claimed. The first issue passed over because a client is not ready or a provider is missing gets one progress line saying which ("Not started: the runner has no `cerebellum` model provider." for a provider), written only when it differs from the issue's latest progress line; later ones in the same poll get none. `null`: no work.
5. **`held`** (when asked): per issue what to do with its session and clone. `keep`: part of a run (To do, In progress, Blocked). `expire`: the runner may delete it by its own age and disk limits. `drop`: delete it now (Done, Cancelled, archived, unknown).
6. **`review`** (when asked). First the facts: for a trusted team's Review issue, `merged` sets `merge_status` to `success`; `closed`, `failed` or a conflict sets it to `issue`; `open` and `none` change nothing. Then the list: the active top-level Review issues with a team whose `merge_status` is not `success`, in board order, at most 200. `merge` says the runner may ask the forge to merge the pull request once it is current (a trusted team).

`project` is always whole: `base_branch` null means the forge's default branch, `idle_minutes` and a team's `max_turns` always hold a number (the app applies the defaults). `team.agents` lists the lead first. The runner refuses to work when `repository_url` is not its configured repository, and reports `repository_mismatch`.

### 2. `POST /runs/{ref}/next`: every step of a run

`ref` is a top-level issue. The runner calls it before the first step and after every step, and does the step it gets, until it gets `done`.

Request:

```json
{
  "id": "k3J…",
  "state": null,
  "last": null,
  "sessions": [{"issue": "EMBER-12", "agent": "Coder", "origin": "kept"}],
  "workspace": {
    "branch": "cinderline/EMBER-12", "base": "main",
    "extra_repositories": [{"path": "/extra/docs", "url": "https://…"}],
    "linked_projects": [{"key": "CSR", "paths": ["CSR/runner"]}],
    "notes": ["Extra repository https://… not fetched; the run goes on without it"]
  }
}
```

- `id`: a new random string per request, the same on a retry. The app keeps the answer to the run's latest request for ten minutes, encrypted, and gives it again for the same `id` without doing anything twice; a request with another `id` replaces it. The writes of one request happen all together or not at all.
- `state`: the last answer's, unchanged; null on the first call. A state that is not of this issue's run (garbage, changed, another issue's, or of an older app) is refused with `422` on `state`, before anything is written. The app keeps every state it makes within 65,536 bytes, whatever the runner's texts hold.
- `last`: the step the runner just did; null on the first call, unless the merge of the base into continued work conflicted, which is then the first `last`.
- `sessions`: the client sessions the runner can resume now. `origin` is the word the last `done` gave (`paused` or `kept`), or `turn` for a session of this run.
- `workspace.notes`: lines for the person watching, about what the runner could not prepare. The app writes them as progress lines.

`last`:

```json
{
  "step": "run", "issue": "EMBER-12", "agent": "Coder", "outcome": "finished",
  "error": null, "report": "…", "tail": null,
  "usage": {"issue": "EMBER-12", "agent": "Coder", "model": null, "input_tokens": 1, "output_tokens": 1, "cache_read_tokens": 0, "cache_write_tokens": 0, "cost_usd": null},
  "conflicts": [], "commits": null,
  "pull_request": null, "merge": null, "commit": null
}
```

| `step` | `outcome` | Other facts |
| --- | --- | --- |
| `run` | `finished`, `failed`, `turn_limit`, `time_limit`, `out_of_memory` (killed at the container's memory limit), `stopped` (the poll's `stop`), `idle` (the idle limit), `not_started` | `report`, `tail`, `error`, `usage`, `memory_limit` (bytes, with `out_of_memory`) |
| `resolve` | as `run` except `out_of_memory`, and `not_verified` (the runner undid a merge it could not verify) | as `run` |
| `check` | `passed`, `failed`, `out_of_memory` (killed at the container's memory limit), `not_started`, `stopped` (the poll's `stop` cancelled the check) | `tail`, `error`, `report` (the checked turn's), `memory_limit` |
| `merge` | `conflict` | `conflicts`, `commits` |
| `publish` | `published`, `unchanged` (nothing to push), `failed` | `pull_request` (URL), `conflicts`, `merge` (`merged`, `closed`, `failed`; `pending`: the runner asked the forge to merge and had not seen the merge when it moved on, at the wait's limit or because the issue was stopped during the wait; null: the runner did not ask the forge to merge, because the step did not say `merge` or the issue was stopped before it asked), `commit`, `error` |
| `stop` | `pushed`, `not_pushed` | `commit`, `error` |

A `stop` the app did not ask for is the runner's own failure (an error or panic in its code): it pushes what it can and reports `stop` with `error`.

A `check` that was `stopped` ends like a `run` turn that was `stopped`: the app answers `stop`, then writes the "Review this manually" note and answers `done` with session `paused`; there is no fix turn.

When the poll stops an issue while the runner publishes it, before it asked the forge to merge, the runner still pushes and opens the pull request but does not ask the forge to merge it: it reports `published` with `merge: null`, also when the step said `merge: true` (decision 2026-11-17-second-audit-decisions). For a trusted team the app's finish report says the pull request is open and was not merged automatically because the issue was stopped during publishing, so a person merges it by hand; `merge_status` stays as it is, and the move to Review is refused harmlessly when the issue is no longer In progress. A stop while the runner waits for a merge it already asked for is reported as `merge: pending`, like a wait that reached its limit; the finish report then says the forge merges it when CI is green and it had not merged yet when the runner moved on (Willem, 2026-11-17).

A `run` or `check` with `out_of_memory` goes on as after `failed`: a retry, a fix turn, or the failure. The app first writes a progress line naming the limit and how to raise it, and puts that line in the failure's text (decision 2026-11-24-out-of-memory-outcome). A runner that cannot tell keeps sending `failed`.

`usage` is recorded on its issue, which must be the run's issue or one of its sub-issues (otherwise `422`). An agent that is no longer on the team, because it was renamed or removed during the run, is recorded under the name the runner sent; it never refuses the step.

**Caps.** The app refuses a request over these (`422`); the runner clips to them. Lengths are in characters, `state` is ASCII.

| Request | Field | Cap |
| --- | --- | --- |
| `poll` | `report.runner_version` | 100 |
| | `report.clients` | 50 items; `name`, `version` 100 each |
| | `report.environments` | 10 items |
| | `report.errors` | 50 items; `message` 2,000, `issue` 50 |
| | `report.model_providers` | 50 items of 50 |
| | `report.provider_models` | 50 items; `provider` 50; `models` 200 items of 150 |
| | `running` | 20 items of 50 |
| | `held` | 200 items of 50 |
| | `review` | 200 items; `ref` 50; `conflicts` 1,000 items of 1,000 |
| `next` | `id` | 8 to 64 |
| | `state` | 65,536 |
| | `last.issue` | 30; `last.agent` 100 |
| | `last.error` | 16,000 |
| | `last.report` | 65,000 |
| | `last.tail` | 4,000 |
| | `last.memory_limit` | at least 1 |
| | `last.conflicts` | 1,000 items of 1,000 |
| | `last.commits` | 65,000 |
| | `last.pull_request` | 2,000; `last.commit` 100 |
| | `last.usage` | `issue` 30, `agent` 100, `model` 100, `kind` 50; token counts and `cost_usd` at least 0 |
| | `sessions` | 2 items; `issue` 30, `agent` 100 |
| | `workspace.branch`, `workspace.base` | 255 each |
| | `workspace.extra_repositories` | 50 items; `path` 255, `url` 2,000 |
| | `workspace.linked_projects` | 50 items; `key` 30; `paths` 50 items of 255 |
| | `workspace.notes` | 20 items of 1,000 |

Answer `200`: `{"state": "…", "step": {…}}`. The first call claims the issue (To do to In progress) and writes the "Started" progress line; when the issue is not in To do the answer is `409` with `message` and `status`, and the runner drops the job. The steps, a closed set:

| `kind` | Fields | The runner |
| --- | --- | --- |
| `run` | `issue` {`ref`, `title`}, `agent`, `session` (`new`, `resume`), `token` (string or null), `prompt`, `attachments` (list of {`issue`, `id`, `name`}, or null: leave the folder) | runs the agent's turn on the issue or sub-issue; commits and pushes a sub-issue's work |
| `resolve` | `agent`, `session`, `token`, `prompt` | has the agent resolve the conflict it reported, verifies the merge, undoes it otherwise |
| `check` | `issue` {`ref`, `title`} | commits, pushes, runs the project's `check_command` |
| `publish` | `resolve_conflicts`, `merge` | commits, merges the base (a conflict goes to `next` first when `resolve_conflicts`), pushes, opens or finds the pull request and, with `merge`, asks the forge to merge it and waits |
| `stop` | none | commits and pushes what the clone holds |
| `done` | `session` (`paused`, `kept`) | keeps the lead's session under that word and ends the run |

Every step may carry `reason`, which the runner only logs. A step never carries a command, a path, a docker option, an environment variable or a URL. `token` is the scoped MCP token for that turn: the app makes it, and deletes the run's earlier ones, when it answers; at `done` it deletes them all. A kind the runner does not know ends the run with an unasked `stop` whose `error` names the runner's version.

The app writes everything a person reads: progress lines, sub-issue reports, the finish report with the pull request, the check verdicts, the failure, budget, idle and stop notes, `merge_status`, and every status move. It writes them when it answers the report of the step that ends the work, so the push always comes first: `publish` and `stop` are answered with `done`.

**When `next` does not answer.** The runner tries again after 2 and 8 seconds, the first call too. A first call (`state` null) that still gets no answer is never put in the outbox: the run did not start, and the issue stays in To do. For any later call the runner then commits and pushes, keeps the session as `paused`, ends the run and puts the request in its outbox. It sends outbox requests again before each poll, in order, and ignores their answers. Once the outbox is empty the issue is no longer `running`, so the poll moves it to Blocked with the note unless the delivered report ended the run.

### 3. `GET /issues/{ref}/attachments/{id}`

Unchanged: the bytes of a file a `run` step named.

### What stays in the runner

Containers, git, the forge calls, the agent CLIs, sessions and their notes, the limits for expiring sessions, verifying a resolved merge, the repository pin, and what may enter a container. The check command is run as the project names it.

## What this replaces

- Runner decisions `2026-11-15-server-decides-steps` (kept in spirit, superseded in detail), `2026-11-14-poll-after`, `2026-11-16-unheld-in-progress`, `2026-11-08-comment-cursor`, `2026-11-11-usage-and-budget`, `2026-11-08-errors-reach-cinderline`, and the long-poll and Review requests of `2026-11-05-runner-rewrite-details`.
- App decisions `2026-11-05-runner-api-in-app` (the endpoint list), `2026-11-16-next-step-for-solo-runs`, `2026-11-16-next-step-for-teams-and-merges` and `2026-11-16-runner-follows-next` (their protocol; the policy they describe stays).
- The scalability rule "runner-facing endpoints answer unchanged polls with a 304 from one indexed query" becomes: one poll per runner per interval, answered from a handful of indexed, capped queries; no request waits.

Decision 0091 holds: no run or step table. A run's counters travel in `state`; the idempotency answers are a cache.

## Reason

`next` already moved the middle of a run to the app, but finding work, claiming, ending a run and keeping Review current still went through seventeen requests in which the runner read and wrote issues itself. Two styles side by side were hard to reason about, kept policy and texts in the public runner, and cost four to five requests a minute per idle project. Three requests with one direction each (the app decides, the runner reports) are easier to read, cheaper, and keep the value in the app.
