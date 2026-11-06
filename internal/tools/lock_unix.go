//go:build !windows

package tools

import (
	"os"
	"syscall"
)

// lock takes an exclusive lock on the file at path, waiting for it, and
// returns its release. The OS releases it if the process dies.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
