# The old Node runner in legacy/ is deleted

- **Status:** Accepted
- **Date:** 2026-11-26
- **Source:** Willem, CLR-262 (2026-11-26), option A: drop the `.gitignore` line and the rule about `legacy/`, in place of keeping them as a guard.

## Decision

1. `legacy/`, the old Node runner, is deleted. It was never committed here: it lived git-ignored on Willem's machine only, so Willem removes it there with `rm -rf legacy`. The rewrite plan calls it a copy of the app repository's `runner/`, whose history keeps that code (M3 ported from it at commit `ecde51a`, docs/parity/m3.md).
2. `.gitignore` no longer lists `legacy/`, and the rule in current.md no longer names it. Agents still never commit secrets, tokens or `.env` files.
3. Delete the folder before pulling this change. A checkout that still has it would show it as untracked, and `git add -A` could put it on the public mirror.
4. Text that cites `legacy/` as history (the parity tables, the rewrite plan, the archive, the reviews) stays as it is. Its LICENSE and NOTICE cover nothing here, because nothing from it was ever copied into this repository.

Replaces decision 2026-11-05-legacy-reference, whose M3 port is long over.

## Reason

The Go runner is the only runner. A folder of code that no longer runs costs agents reading time and invites them to cite it. Once the folder is gone, an ignore line and a rule about it are dead text.
