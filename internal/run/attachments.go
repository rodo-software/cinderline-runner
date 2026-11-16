package run

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// attachmentsDir is the host folder every agent container of the project
// mounts read-only at workspace.AttachmentsPath (CSR-173). One per project,
// as the project has one slot: every run, and every sub-issue's, refills it.
func attachmentsDir(state string, p config.Project) string {
	return filepath.Join(state, "attachments", p.ID())
}

// resetAttachments empties the project's attachments folder; it stays, so a
// bind mount never makes it as root.
func (p Project) resetAttachments() error {
	dir := attachmentsDir(p.State, p.Config)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755) // read by the container's agent user
}

// fileName is a's name, untrusted, made safe as one file name on every host
// and as one line of a prompt: no path separators, characters Windows
// refuses, control characters or backticks, and never "." or "..".
func fileName(a cinderline.Attachment) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`+"`", r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(a.Name))
	if strings.Trim(name, ".") == "" {
		return "attachment-" + strconv.Itoa(a.ID)
	}
	return name
}
