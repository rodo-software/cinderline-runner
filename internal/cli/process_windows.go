//go:build windows

package cli

import "os"

// On Windows, os.FindProcess opens the process handle, so it fails when the
// process no longer exists. On Unix, FindProcess never fails for a PID that
// was once valid, so we must Signal(0) instead; see process_unix.go.
func processGone(pid int) bool {
	_, err := os.FindProcess(pid)
	return err != nil
}
