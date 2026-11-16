//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
)

// replaceExe replaces exe with next on Windows, where a running exe can be
// renamed but not overwritten. It renames the running exe to .old and moves
// the new one into place, undoing the rename on failure.
func replaceExe(exe, next string) error {
	old := exe + ".old"
	os.Remove(old) // a leftover from a past update; ignore errors (it may be running)

	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return errors.Join(err, fmt.Errorf("undo rename: %w", rerr))
		}
		return err
	}
	return nil
}
