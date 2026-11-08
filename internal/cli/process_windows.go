//go:build windows

package cli

import "syscall"

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
