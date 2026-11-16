package tools

import (
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx, which syscall doesn't wrap; the standard library is enough.
var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

// lock takes an exclusive lock on the file at path, waiting for it, and
// returns its release. The OS releases it if the process dies.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	const exclusive = 2 // LOCKFILE_EXCLUSIVE_LOCK
	var ol syscall.Overlapped
	if r, _, err := lockFileEx.Call(f.Fd(), exclusive, 0, 1, 0, uintptr(unsafe.Pointer(&ol))); r == 0 {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
