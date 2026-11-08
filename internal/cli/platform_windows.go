//go:build windows

package cli

import (
	"syscall"
	"unsafe"
)

// Not syscall.GetVersion: it reports 9200 to programs without a compatibility manifest
var rtlGetVersion = syscall.NewLazyDLL("ntdll.dll").NewProc("RtlGetVersion")

func windowsBuild() uint32 {
	type OSVERSIONINFOW struct {
		dwOSVersionInfoSize uint32
		dwMajorVersion      uint32
		dwMinorVersion      uint32
		dwBuildNumber       uint32
		dwPlatformId        uint32
		szCSDVersion        [128]uint16
	}
	var v OSVERSIONINFOW
	v.dwOSVersionInfoSize = uint32(unsafe.Sizeof(v))
	if r, _, _ := rtlGetVersion.Call(uintptr(unsafe.Pointer(&v))); r != 0 {
		return 0
	}
	return v.dwBuildNumber
}
