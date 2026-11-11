# Runners are inspected in the hosted app, not in a local page

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** Willem, CSR-155 (runner) and EMBER-188 (app).

## Decision

1. The runner serves no local web page. CSR-155 (a page served by the daemon on loopback, with `cinderline open`) is dropped without code, and the plan's "Out of scope: a tray app or local web page" stays as it is.
2. Runners are listed and inspected in the hosted Cinderline app, under a "Runners" menu (EMBER-188). It is read-only and uses what the runner already sends through `PUT /report` and `POST /heartbeat`. The runner API does not change for it.
3. Controlling a runner from the app (stop, drain, start-at-login, live log, adding or removing a project, client login) is not decided. It needs an app → runner command channel and log shipping. Adding a project that way would also send git tokens and model keys through the hosted server, while today they never leave the developer's machine. It needs Willem's decision before any work starts.

## Reason

Willem: "I think I want these features to be available through the standard web interface that we host ourselves. A 'Runners' menu somewhere that shows all connected Runners and allows us to inspect them. I think it makes more sense than having the Runner offer their own interface locally." The app already stores each runner's report and heartbeat, so the read-only view needs no runner work. Control is kept apart because it changes where secrets live.
