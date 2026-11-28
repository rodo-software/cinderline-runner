//go:build windows

package manage

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// ServiceInstalled reports whether the logon task that runs the runner is
// registered.
func ServiceInstalled() (bool, error) {
	err := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), "/query", "/tn", name.Name).Run()
	return err == nil, nil
}
