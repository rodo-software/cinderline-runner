# Keep repository-local Compose composition

- **Status:** Accepted
- **Date:** 2026-11-19
- **Source:** Willem on CLR-203, answer “B: Keep repository-local files”.

## Decision

Keep repository-local `include` and external-file `extends`. Before invoking
Compose, recursively validate their file references, including include
`env_file` and `project_directory`. Refuse paths outside the archived repository
and dynamic references that cannot be validated before Compose reads them.

Use `go.yaml.in/yaml/v3` to parse YAML and resolve aliases and merge keys for
this preflight; Go's standard library has no YAML parser.

Then resolve composition without interpolation into a temporary JSON file in
the repository copy. Use that file for subsequent checks and operations, with
an explicit repository-root `.env` (or an empty dotenv file when absent).
Check service `env_file` paths before resolving their values. Continue using
the existing normalized configuration security checks.

## Reason

Willem chose to keep repository-local multi-file reuse rather than require a
single Compose file or add a containerized Compose runtime. The two-pass flow
alone would still import literal values from outside included Compose files;
preflight validation supplies the missing boundary before those reads occur.
