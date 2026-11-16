package tools

import "fmt"

// ReleaseNotes is the header of every GitHub release: the agent CLIs this
// build pins (R14), generated from pins.go so it cannot drift. The release
// workflow prints it with `go run ./internal/tools/releasenotes` and hands
// the file to GoReleaser as --release-header.
func ReleaseNotes() string {
	return fmt.Sprintf("Pinned agent CLIs in the tools volume:\n\n- Claude Code %s\n- OpenCode %s\n- Codex %s\n- Gemini CLI %s\n- Node.js %s\n- ponytail %s (%s)\n",
		ClaudeVersion, OpenCodeVersion, CodexVersion, GeminiVersion, NodeVersion, PonytailTag, PonytailCommit)
}
