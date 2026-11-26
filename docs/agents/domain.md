# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`GLOSSARY.md`** at the repo root.
- **`docs/decisions/`**: this repo's ADRs. Read `current.md` first, then the dated records next to it that touch the area you're about to work in. `archive/` is history, and `current.md` wins where they differ. There is no `docs/adr/`: new decisions go in `docs/decisions/YYYY-MM-DD-topic.md` (see its README).

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill (reached via `/grill-with-docs` and `/improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

## File structure

Single-context repo:

```
/
├── GLOSSARY.md
├── docs/decisions/
│   ├── current.md
│   ├── 2026-11-16-runner-protocol-v2.md
│   └── archive/
└── internal/
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `GLOSSARY.md`. Don't drift to synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

## Flag decision conflicts

If your output contradicts an existing decision, surface it explicitly rather than silently overriding:

> _Contradicts decision 2026-11-16-runner-protocol-v2, but worth reopening because…_
