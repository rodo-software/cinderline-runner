//go:build !windows

package cli

import (
	"errors"
	"os"
	"syscall"
)

// processGone reports whether no process with pid exists; a permission error means it does.
func processGone(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// reexec replaces this process with the binary at exe, which an update
// replaced, with the same arguments and the same PID, so the service
// manager or terminal keeps it (decision 2026-11-28-update-restart). It
// returns only on failure.
func reexec(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
