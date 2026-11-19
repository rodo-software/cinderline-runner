package run

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// attachmentsDir is the host folder the agent containers of the run on ref
// mount read-only at workspace.AttachmentsPath. One per issue, as the
// project may run two issues at once (CLR-194): every run step with
// attachments refills it.
func attachmentsDir(state string, p config.Project, ref string) string {
	return filepath.Join(state, "attachments", p.ID(), ref)
}

// resetAttachments empties ref's attachments folder; it stays, so a bind
// mount never makes it as root.
func (p *Project) resetAttachments(ref string) error {
	dir := attachmentsDir(p.State, p.Config, ref)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755) // read by the container's agent user
}

// dropAttachments deletes ref's attachments folder, once its run's clone is
// gone (CLR-194): a removed run's files must not linger.
func (p *Project) dropAttachments(ref string) error {
	return os.RemoveAll(attachmentsDir(p.State, p.Config, ref))
}

// fileName is an attachment's untrusted name made safe as one file name on every host
// and as one line of a prompt: no path separators, characters Windows
// refuses, control characters or backticks, and never "." or "..".
func fileName(id int, untrusted string) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`+"`", r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(untrusted))
	if strings.Trim(name, ".") == "" {
		return "attachment-" + strconv.Itoa(id)
	}
	return name
}
