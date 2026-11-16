//go:build !windows

package cli

import (
	"os"
)

// replaceExe replaces exe with next atomically (a rename on the same
// filesystem). The running process keeps its old inode.
func replaceExe(exe, next string) error {
	if err := os.Chmod(next, 0o755); err != nil {
		return err
	}
	return os.Rename(next, exe)
}
