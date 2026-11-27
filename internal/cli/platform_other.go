//go:build !windows

package cli

import (
	"os/exec"
	"runtime"
)

func windowsBuild() uint32 {
	return 0
}

// browser opens url in the default browser: open on macOS, xdg-open elsewhere.
func browser(url string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url)
	}
	return exec.Command("xdg-open", url)
}
