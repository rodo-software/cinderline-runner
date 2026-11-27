package tools

import "fmt"

// ReleaseNotes is the header of every GitHub release: the agent CLIs this
// build starts with until it has checked newer ones (R14; CLR-317), generated from pins.go so it cannot drift. The release
// workflow prints it with `go run ./internal/tools/releasenotes` and hands
// the file to GoReleaser as --release-header.
func ReleaseNotes() string {
	return fmt.Sprintf("The tools volume starts with these agent CLIs and then follows their vendors' releases:\n\n- Claude Code %s\n- OpenCode %s\n- Codex %s\n- Gemini CLI %s\n- Node.js %s\n- ripgrep %s\n",
		ClaudeVersion, OpenCodeVersion, CodexVersion, GeminiVersion, NodeVersion, RipgrepVersion)
}
