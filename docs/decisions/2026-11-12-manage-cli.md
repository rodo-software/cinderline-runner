# Interactive management screen: github.com/charmbracelet/huh

- **Status:** Accepted
- **Date:** 2026-11-12
- **Source:** Decided by Willem on CSR-172, answering which dependencies `cinderline manage` may use. Widens plan section 3 M2 (Dependencies), which allowed only go-keyring and kardianos/service.

## Decision

- `internal/cli` uses `github.com/charmbracelet/huh` (on Bubble Tea and lipgloss) for `cinderline manage`: arrow-key menus, hidden token input, a confirmation before a delete. `github.com/charmbracelet/x/term`, already one of huh's modules, tells whether stdin and stdout are a terminal.
- `manage` refuses to run without a terminal and points to `add`, `key set` and `remove`, which stay line-based on stdin so scripts keep working. Its "Add a project" runs `add`'s own flow and checks, with tokens and keys hidden as they are typed.
- Options rejected: `golang.org/x/term` alone with numbered menus (plainer), and the standard library alone (tokens shown as typed).

## Reason

The issue asks for a modern, user-friendly CLI built on existing packages; huh gives that on macOS, Linux and Windows terminals. Its roughly 20 indirect modules are covered by govulncheck and the three-OS CI matrix.
