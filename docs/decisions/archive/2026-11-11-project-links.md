# Project links: one way, issues over MCP, repositories read-only

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** Willem, CSR-161 (runner) and EMBER-185 (app).

## Decision

1. A link goes one way: "A may reach B". B reaching A is a second link. A link is never implied by the other direction and never chains: if A may reach B and B may reach C, A does not reach C.
2. Through a link, A's agents may read and edit B's issues over MCP (the app's part, EMBER-185), and may read all of B's repositories, read-only: B's own repository at its base branch and B's extra repositories (CSR-160). Nothing more. All commits, pushes and pull requests stay in A's own repository.
3. This replaces "no kind reaches other issues" (plan, "Scoped agent tokens", and decision 2026-11-05-runner-rewrite-details point 6) for linked projects only: outside the issue tree, an `orchestrator` or `writer` token reaches only the issues of projects its project links to, with the tools the OpenAPI file lists. The `reader` kind does not change.
4. Links are stored and managed in the app, not in the runner's config. The runner only reads them, from `linked_projects` on `GET /project`; an app that does not send it means no links.
5. The runner fetches a linked project's code only when that project is also added on this runner (same Cinderline URL and key), with the tokens stored there, each only for its own URL. Otherwise the run goes on without the code and says so.

## Reason

Work on one project often needs the code and the issues of another, for example the runner (CSR) and the app (EMBER). CSR-160's extra repositories give code by hand, one repository at a time; a link gives the other project's code and issues at once and keeps them current. One-way, non-chaining links keep each grant explicit and easy to reason about, read-only code keeps every change in the run's own repository, and keeping links in the app keeps one place that decides who reaches what, while the runner never holds a token for a repository it was not given.
