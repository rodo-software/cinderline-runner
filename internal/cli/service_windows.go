//go:build windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

func installService(exe string) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	userName := u.Username
	xmlData := taskXML(exe, userName)

	f, err := os.CreateTemp("", name.Name+"-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(xmlData); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := schtasks(schtasksArgs("create", f.Name())...); err != nil {
		return err
	}
	return schtasks(schtasksArgs("run", "")...)
}

func uninstallService(exe string) error {
	_ = schtasks(schtasksArgs("end", "")...) // may not be running
	return schtasks(schtasksArgs("delete", "")...)
}

// startService starts the installed service; installed is false, and nothing is done, when there is none.
func startService(exe string) (installed bool, err error) {
	if err := schtasks("/query", "/tn", name.Name); err != nil {
		return false, nil
	}
	return true, schtasks(schtasksArgs("run", "")...)
}

func schtasks(args ...string) error {
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %w: %s", args[0], err, string(out))
	}
	return nil
}
