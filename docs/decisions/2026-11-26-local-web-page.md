# Decision 2026-11-26-local-web-page

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, the spec on CLR-266; copied here by CLR-267 (Web 01).
- **Amended by:** 2026-11-27-web-look (the look: calm and mostly unboxed instead of Syncthing's panels).

## What and why

The runner serves a web page on the user's own machine. It shows and changes the local runner config, and it shows runs in far more detail than the hosted app: every step, the agent's output as it comes, CPU and memory.

This replaces the line "The runner serves no local web page" of 2026-11-11-runners-in-the-app. That decision's worry was secrets moving through the hosted server; a page on the user's own machine moves none. The rest of it stands: the hosted app shows runners read-only and does not control them.

## Serving

- The daemon serves the page itself, in the package `internal/web`. No second process. When the daemon is stopped there is no page; starting the daemon stays a CLI or service matter.
- It listens on `127.0.0.1` only, default port 7420. No LAN or remote option.
- Config, runner-wide: `web` (`"on"`, `"off"` or absent for the default) and `web_port`. The default is on; `web off` turns it off.
- `cinderline web` opens the page in the browser; `cinderline web on|off`, `cinderline web port N` and `cinderline web --new-token` change it.
- A port that is taken is logged once at Error; the daemon runs on without the page.
- Nothing about the page goes to Cinderline: no poll field, no change to the OpenAPI contract.

## Access

- A token of 32 random bytes, made once, in the file `web-token` in the state folder, readable by the user only. `--new-token` replaces it.
- `GET /?token=…` with the right token sets a cookie (HttpOnly, SameSite=Strict) and redirects to `/`. Every other request needs the cookie; without it the answer is 401 with a line that says to run `cinderline web`.
- The `Host` header must be `127.0.0.1:<port>` or `localhost:<port>`, else 403 (DNS rebinding).
- Every request that is not GET or HEAD must come from the page's own origin (`Origin` header, or `Sec-Fetch-Site: same-origin`), else 403.
- Agent containers must not reach the port.

## Frontend

- Plain HTML, CSS and JavaScript in `internal/web/static/`, embedded with `go:embed`. No npm, no bundler, no framework, no new Go dependency.
- Live data comes over server-sent events (standard library).
- The look follows Syncthing: a top bar, panels, clear status colours. Light and dark follow the system. English only. Current Chrome, Firefox, Safari and Edge; usable by keyboard and in a phone-width window.
- Pages: **Overview** (the daemon, the memory budget, each project's connection and runs in flight), **Runs** (live and finished, each opens its timeline), **Projects** (one page per project: repositories, hosts, skills, memory, parallel runs, transcript days, secrets; add, rename, remove), **Providers**, **Settings** (strict hosts, runner-wide hosts, skills, parallel runs and transcripts, the CA, the web port, update, stop, drain), **Log** (the runner log, followed).

## Config management

- Everything the CLI can do, except `login`, `uninstall`, `service` and `run`: for those the page shows the exact command to type.
- One write path: the logic of each CLI command moves out of `internal/cli` into functions that both the CLI and the web handlers call. The CLI keeps working with the daemon stopped. No new interface.
- Secrets are write-only: the page can set and delete a token or key and shows whether one exists, never its value.
- Removing or renaming a project needs its key typed. Rotating the CA, stop, drain and update ask for a plain confirmation. After a stop the page says the runner is stopped and names the command that starts it.

## Runs

- The page only watches. No button stops, retries or skips a single run; that is the app's policy.
- A run is shown as its steps in order (`run`, `resolve`, `check`, `publish`, `stop`), each with its outcome, the hosts the egress proxy refused, and a link to the issue in Cinderline.
- An agent turn shows the prompt the app sent (collapsed) and a timeline parsed from the client's output: the agent's text, each tool call with its input and result, tokens and cost where reported. Parsers: Claude and Codex first. Any other client, and any line a parser does not know, is shown as the raw line. The raw lines are always one click away.
- Each run in flight shows one line of what it does now (its step, and for an agent turn the latest tool call or text) on Overview and Runs.
- Project Manager runs are shown and stored like team runs, marked as such.
- Live CPU and memory of the agent container and of each compose service, not of the egress proxy: the daemon polls `docker stats` every few seconds only while a page watches that run, and keeps the numbers in memory only.

## Transcripts

- The runner keeps each run on disk so it can be opened after it ended or after a restart: under `transcripts/` in the state folder, one folder per run, one file per step, one JSON object per line. It is a cache the runner may lose; there is no run table.
- Every line passes `internal/redact` before it is stored or sent to the page. There is no way to see an unredacted line.
- The agent's output no longer goes into the runner log, which holds only the runner's own events.
- `transcript_days`: how long a run is kept. Runner-wide, default 14; a project may set its own, which wins. 0 to 365; 0 keeps nothing after the run ends, so a run can be followed live but not opened afterwards.
- `transcript_max_size`: the most disk the transcripts take in all, runner-wide only, default 1g, at least 100m, written like `memory`. Over it, the oldest runs are deleted first.
- `cinderline transcripts [--project KEY] [DAYS|clear]` and `cinderline transcripts --max-size SIZE`.

## Out of scope

Browser notifications, editing `config.json` as raw text in the page, searching across transcripts, access from another machine, Windows-specific testing (same code, untested while the Windows CI checks are off).

## How it is built

Mock pages first, for Willem's approval of the look. Then the issues `Web NN` in order. The last one (CLR-313) turns the page on by default; `web off` turns it off.
