# Skill packages: the runner fetches each at its pinned commit, refuses it on a strict runner unless allowed, and reports its state

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, issue CLR-257 (2026-11-26), the runner's side of the app's decision `2026-11-26-skill-packages` (CL-425) and its contract. Points 9 to 12 are the implementation's choices in CLR-257, open to Willem's veto in its review.

## Decision

1. **A job carries `skills`,** each `{name, url, commit, path}`, and the `next` request's `workspace.skills` reports each one named as `{name, state}`: `loaded`, `refused_strict` or `failed`. Facts only: the app writes the text a person reads. `Team.ponytail` stays until the mounts issue (CLR-258).
2. **Checked before use.** Nothing of an entry becomes a path or an argument before it passes: `name` lower-case letters and digits with single hyphens, at most 64 characters; `url` https without user info, query or fragment, at most 500; `commit` 40 lower-case hex; `path` relative, clean, without `..`, at most 200, or null. An entry that fails is `failed`, logged, and the run goes on. Only the first 10 are used; a second entry with the same name is `failed`.
3. **Fetched in the run's prepare,** in a throwaway git container on the runner's own network, never through an agent's proxy, and without a token: `git fetch --depth 1 <url> <commit>`, a detached checkout, a refusal when `rev-parse HEAD` is not the commit, and `.git` removed.
4. **One volume per URL and commit,** `cinderline-skill-<hash>`, shared by every project, filled once behind a file lock in the state folder, with a `complete` marker written last so an interrupted fill is redone. Agents will mount it read-only.
5. **Strict runners refuse.** With `strict_hosts` on, a run loads only the packages an `allow_skills` entry allows, runner-wide or the project's; any other is `refused_strict` and not fetched. The allowance is by name and URL, so a catalog entry that later points elsewhere is refused again. Without `strict_hosts`, runs load what the job names.
6. **Tidying.** The loop's tidy pass removes the package volumes no run has used for 30 days; a volume in use stays. `uninstall` removes them with the tools volumes.
7. **Handed to the run, not yet mounted.** A run holds its loaded packages with their volume and root; mounting them into turns and the client flags are CLR-258. Until then a package reported `loaded` is fetched but not in the agent's container.
8. **Private repositories and tokens are out of scope.** Git's credential prompt is off in the fetch, so a repository that asks for one fails.
9. **`cinderline allow-skill add NAME URL|list|remove NAME [--project KEY]`** writes `allow_skills` in `config.json`, `[{name, url}]` at the top level or per project, as `allow-host` writes `allow_hosts`: without `--project` it applies to all current and future projects and asks for a confirmation. `add` takes the URL, since the allowance is by name and URL; one URL per name, so adding a name again with another URL replaces it and says so. The URL must match the catalog's exactly.
10. **The volume's layout:** the repository in `repo/`, the marker at the root, so a file named `complete` in a repository cannot pass for it. A package's root is `repo/<path>`.
11. **`path` is also refused** when it holds anything but letters, digits and `. _ @ + - /`, so it stays safe in a docker `--mount` value. It must be a folder of the fetched repository, reached without a symlink, or the package is `failed`.
12. **A volume's last use** is the modification time of its lock file, set under the lock at every use. A volume without a lock file gets one when tidying first sees it, and is kept 30 days from then. Lock files are never removed: removing one could let two fills take two locks.

## Reason

A package is code from a URL Cinderline names, run next to the model credential and the scoped token, so the runner checks every field itself, accepts only the commit the app resolved, and, when its owner has said Cinderline may not open hosts, loads only what that owner allowed. Fetching once per URL and commit and sharing the volume keeps a package that many runs use to one download. The fetch needs no credential, so it runs without one; an agent's proxy would only add hosts to its allowlist.
