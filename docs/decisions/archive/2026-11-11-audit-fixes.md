# Audit fixes: paused runs resume, unpushed work is kept, one key per machine, a state volume per client, a minimum Docker Engine

- **Status:** Accepted
- **Date:** 2026-11-11
- **Source:** Willem, after the whole-repository audit of 2026-11-11 ("Fix all 11").

## Decision

1. **Paused runs resume.** A paused note is resumable when it names a session of the same client and its clone is still there. The runner used to check the clone's folder on the host: the folder is a path in the runs volume, which the host does not have, so the check always failed and every answered question started the run over in a fresh clone. CSR-145 found the same bug and checks the clone in the runs volume with `Workspace.Exists`; its fix is the one in the code.
2. **The review timer never removes unpushed work.** Before it replaces a Review issue's clone, the timer lists the stranded clones (decision 2026-11-07-publish-resilience point 3) and pushes the issue's clone with the same rules as at start, opening its pull request. While that push fails, or the clone is left alone (pull request merged or closed), the issue is skipped. If the list fails, no clone is replaced.
3. **One fetch per tick.** `Workspace.Behind` fetches once and lists the runner's branches that the base has moved past; only those get a fresh clone. Before, every Review issue cost a fetch and a clone every five minutes.
4. **One project per key on a machine.** Docker volumes, containers, networks and labels are named by the project key alone. `add` refuses a key that is already configured, also on another instance, and the daemon starts only the first project of a key and shows the reason in `status` for the others. The config file still loads with such a pair, so `remove -url` can clean it up.
5. **A state volume per client.** `cinderline-<KEY>-state-<client>` replaces `cinderline-<KEY>-state`, so a run mounts only its own client's sessions and sign-in. Existing sign-ins and sessions stay on the old volume: sign in again with `login`, and remove the old volume by hand.
6. **Docker Engine 25.0.5 or later, or 23.0.11 or later of 23.0.** Older engines forward DNS lookups from an `--internal` network through the host (CVE-2024-29018). The check does not tell Docker Desktop apart, which the advisory lists as not affected.
7. **`remove` cleans up.** It reloads a running runner and removes the project's containers, networks, volumes and paused notes, unless another configured project has the same key. A failure after the config is saved only warns.
8. **Smaller ones.** `update` uses the tools package's checked download and its extract; a network left by a failed removal is removed and made again; a Review issue found without a pull request is looked up again only after ten minutes; one copy of the issue-ref pattern and of the sleep helper; the test-only `geminiSettings` is gone.

## Reason

Items 1 and 2 lost agent work in normal use; item 4 could put one repository's code in another's branch; items 5 and 6 are the rule that an agent container gets only the one credential its run needs, and that the allowlist is the only way out. `allow_existing` and `Client.APIVersion` stay although nothing reads them yet: the first is M6's (decision 2026-11-08-m6-environments) and removing it would refuse config files that set it; the second records a header of the runner API contract.
