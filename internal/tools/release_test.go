package tools

import (
	"strings"
	"testing"
)

// The release notes header GoReleaser prepends to each release names the
// pinned agent CLIs from pins.go, generated, not typed by hand (CSR-41, R14).
func TestReleaseNotesNamePins(t *testing.T) {
	got := ReleaseNotes()
	for _, want := range []string{
		"Claude Code " + ClaudeVersion,
		"OpenCode " + OpenCodeVersion,
		"ponytail " + PonytailTag,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ReleaseNotes() lacks %q:\n%s", want, got)
		}
	}
}
