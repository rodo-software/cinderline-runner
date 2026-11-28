//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// os.FindProcess succeeds for an exited process while a handle to it stays
// open, so wait on the handle instead.
func processGone(pid int) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err != syscall.ERROR_ACCESS_DENIED
	}
	defer syscall.CloseHandle(h)
	ev, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && ev == syscall.WAIT_OBJECT_0
}

// reexec starts the binary at exe, which an update replaced, with the same
// arguments; this process then exits. Windows has no exec, and this is
// untested while the Windows checks are off (decision 2026-11-28-update-restart).
func reexec(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}
