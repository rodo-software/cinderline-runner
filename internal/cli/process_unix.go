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
