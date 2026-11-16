package cli

import (
	"io"
	"os"
	"os/exec"
	"testing"
)

// CSR-109: processGone tells a live process from an exited one, on every OS.
func TestProcessGone(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "CINDERLINE_TEST_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if processGone(pid) {
		t.Errorf("processGone(%d) = true while the process runs", pid)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if !processGone(pid) {
		t.Errorf("processGone(%d) = false after the process exited", pid)
	}
}

// TestHelperProcess is the child of TestProcessGone: it lives until its stdin closes.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CINDERLINE_TEST_HELPER") == "" {
		t.Skip("helper for TestProcessGone")
	}
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
