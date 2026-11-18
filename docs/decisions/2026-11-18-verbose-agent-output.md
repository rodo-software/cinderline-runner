# Container output in the verbose view and the log file

- **Status:** Accepted
- **Date:** 2026-11-18
- **Source:** Willem on CLR-198 (option C of three).

## Decision

1. `cinderline run -v` prints each line of container output (agent CLIs and checks) as its own plain line, `15:04:05 ISSUE │ line`, not as a quoted slog record. Other log lines keep the slog text format.
2. The line is cleaned first: only printable text, tabs and colour codes (SGR: `ESC [` digits, `;` and `:`, `m`) survive. Every other escape (cursor moves, OSC titles, clipboard and links, DCS, modes) and every control character is removed. A carriage return or a move to column 1 starts the line over, so a progress bar keeps its last frame. A line that keeps colours ends with a reset.
3. Colours are kept only when stderr is a terminal. The JSON log file keeps the cleaned line without colours, so `cinderline logs` is clean too.
4. The cleaned line is redacted again, since removing escapes can join a secret they split.

## Reason

The slog text handler quoted every escape, so `-v` showed `\x1b[32m✓\x1b[39m` instead of a green tick, and progress bars showed every frame. Container output is untrusted, so passing it to the terminal raw was not an option: OSC 52 writes to the clipboard, and other sequences change the title or the terminal's modes.
