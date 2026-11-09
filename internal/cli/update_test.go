package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// releaseArchive is this OS's release archive (as GoReleaser names it) holding
// the binary with content bin.
func releaseArchive(t *testing.T, bin []byte) (file string, data []byte) {
	t.Helper()
	exe := name.Name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	var buf bytes.Buffer
	file = name.Name + "_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		file += ".zip"
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(exe)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(bin)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return file, buf.Bytes()
	}
	file += ".tar.gz"
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: exe, Mode: 0o755, Size: int64(len(bin))})
	tw.Write(bin)
	tw.Close()
	gz.Close()
	return file, buf.Bytes()
}

// releaseServer serves a release with bin as its binary and sets the download
// URL to it, as the install scripts' override does. A non-empty sum replaces
// the archive's real checksum.
func releaseServer(t *testing.T, bin []byte, sum string) {
	t.Helper()
	file, data := releaseArchive(t, bin)
	if sum == "" {
		h := sha256.Sum256(data)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + file:
			w.Write(data)
		case "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), "other_file.tar.gz")
			fmt.Fprintf(w, "%s  %s\n", sum, file)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(strings.ToUpper(name.Name)+"_DOWNLOAD_URL", srv.URL)
}

// updateCLI is a CLI whose binary is a file with content "old" and whose
// commands for the new binary go to a fake that answers version with
// newVersion and update --refresh with refreshErr.
func updateCLI(t *testing.T, newVersion string, refreshErr error) (c *CLI, exe string, calls func() [][]string) {
	t.Helper()
	c, _, _ = daemonCLI(t, "")
	exe = filepath.Join(t.TempDir(), name.Name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.exe = exe
	c.startService = func(string) (bool, error) { return false, nil } // no service: never touch the real one
	var mu sync.Mutex
	var got [][]string
	c.exec = func(_ context.Context, _ []string, argv ...string) (string, error) {
		mu.Lock()
		got = append(got, argv)
		mu.Unlock()
		if argv[0] == exe {
			return "", fmt.Errorf("ran the old binary: %q", argv)
		}
		switch strings.Join(argv[1:], " ") {
		case "version":
			return name.Name + " " + newVersion + "\n", nil
		case "update --refresh":
			return "", refreshErr
		}
		return "", fmt.Errorf("unexpected command %q", argv)
	}
	return c, exe, func() [][]string { mu.Lock(); defer mu.Unlock(); return got }
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// CSR-44: a download whose SHA-256 does not match checksums.txt is refused,
// nothing of it runs, and the old binary stays.
func TestUpdateChecksumMismatchKeepsOldBinary(t *testing.T) {
	releaseServer(t, []byte("new"), strings.Repeat("ab", 32))
	c, exe, calls := updateCLI(t, "9.9.9", nil)
	errb := c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 1 {
		t.Fatalf("code %d, want 1; stderr %q", code, errb.String())
	}
	if !strings.Contains(strings.ToLower(errb.String()), "checksum") {
		t.Errorf("stderr %q, want it to say the checksum failed", errb.String())
	}
	if got := content(t, exe); got != "old" {
		t.Errorf("binary = %q, want the old one kept", got)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("ran %q from an unverified download", calls())
	}
}

// CSR-44: a failed refresh of the tools volume and image keeps the old binary.
func TestUpdateRefreshFailureKeepsOldBinary(t *testing.T) {
	releaseServer(t, []byte("new"), "")
	c, exe, _ := updateCLI(t, "9.9.9", errors.New("docker build failed"))
	errb := c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 1 {
		t.Fatalf("code %d, want 1; stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "docker build failed") {
		t.Errorf("stderr %q, want it to say what failed", errb.String())
	}
	if got := content(t, exe); got != "old" {
		t.Errorf("binary = %q, want the old one kept", got)
	}
}

// CSR-44: a verified download is refreshed with the new binary itself (its
// pins and image), then replaces the old binary; no daemon runs, so nothing
// is drained.
func TestUpdateReplacesBinary(t *testing.T) {
	releaseServer(t, []byte("new"), "")
	c, exe, calls := updateCLI(t, "9.9.9", nil)
	out, errb := c.Stdout.(*bytes.Buffer), c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 0 {
		t.Fatalf("code %d; stderr %q", code, errb.String())
	}
	if got := content(t, exe); got != "new" {
		t.Errorf("binary = %q, want the new one", got)
	}
	refreshed := false
	for _, argv := range calls() {
		refreshed = refreshed || strings.Join(argv[1:], " ") == "update --refresh"
	}
	if !refreshed {
		t.Errorf("the new binary did not refresh the tools and image: %q", calls())
	}
	if !strings.Contains(out.String(), "9.9.9") {
		t.Errorf("stdout %q, want it to name the new version", out.String())
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) > 2 { // the binary and, on Windows, the renamed old one
		t.Errorf("left behind in the binary's folder: %v", entries)
	}
}

// CSR-44: when the latest release is the running version, nothing changes.
func TestUpdateAlreadyCurrent(t *testing.T) {
	releaseServer(t, []byte("new"), "")
	c, exe, calls := updateCLI(t, "1.2.3", nil) // newCLI's version
	errb := c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 0 {
		t.Fatalf("code %d; stderr %q", code, errb.String())
	}
	if got := content(t, exe); got != "old" {
		t.Errorf("binary = %q, want it untouched", got)
	}
	for _, argv := range calls() {
		if strings.Join(argv[1:], " ") == "update --refresh" {
			t.Errorf("refreshed although already current: %q", calls())
		}
	}
}

// CSR-44: a running daemon is drained after the binary is replaced, and
// update waits until it has exited.
func TestUpdateDrainsRunningDaemon(t *testing.T) {
	defer func(d time.Duration) { drainPoll = d }(drainPoll)
	drainPoll = 10 * time.Millisecond
	releaseServer(t, []byte("new"), "")
	c, exe, _ := updateCLI(t, "9.9.9", nil)
	saveConfig(t, c)
	done := startDaemon(t, c)
	errb := c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 0 {
		t.Fatalf("code %d; stderr %q", code, errb.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon.Run = %v, want nil", err)
		}
	default:
		t.Fatal("update returned before the daemon exited")
	}
	if got := content(t, exe); got != "new" {
		t.Errorf("binary = %q, want the new one", got)
	}
}

// TestHelperSleep is not a test: TestReplaceRunningExecutable starts a copy of
// the test binary with this env var set to keep a running executable.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("CSR44_HELPER_SLEEP") == "" {
		t.Skip("helper process only")
	}
	time.Sleep(30 * time.Second)
}

// CSR-44: replacing a binary that is running works on this OS (CI runs it on
// Linux, macOS and Windows; Windows needs the running exe renamed first).
func TestReplaceRunningExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, name.Name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), "CSR44_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	time.Sleep(200 * time.Millisecond) // let the OS map it

	next := filepath.Join(dir, "next")
	if err := os.WriteFile(next, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExe(exe, next); err != nil {
		t.Fatalf("replaceExe: %v", err)
	}
	if got := content(t, exe); got != "new" {
		t.Errorf("binary = %q, want the new one", got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(exe); err != nil || fi.Mode()&0o100 == 0 {
			t.Errorf("new binary not executable: %v %v", fi.Mode(), err)
		}
	}
}

// CSR-44: after the drain, update starts the installed service again with the
// new binary (decision 2026-11-07-service-restart: a clean exit is not restarted).
func TestUpdateRestartsServiceAfterDrain(t *testing.T) {
	defer func(d time.Duration) { drainPoll = d }(drainPoll)
	drainPoll = 10 * time.Millisecond
	releaseServer(t, []byte("new"), "")
	c, exe, _ := updateCLI(t, "9.9.9", nil)
	saveConfig(t, c)
	done := startDaemon(t, c)
	// CSR-109: the socket closes before the daemon's process exits, so update
	// must wait until the daemon's process is gone; here that is "Run returned".
	var asked []int
	exited := false // done is buffered: receive it once and remember
	c.processGone = func(pid int) bool {
		asked = append(asked, pid)
		if !exited {
			select {
			case <-done:
				exited = true
			default:
			}
		}
		return exited
	}
	var started []string
	c.startService = func(path string) (bool, error) {
		if !exited {
			t.Error("service started before the daemon exited")
		}
		started = append(started, path)
		return true, nil
	}
	out, errb := c.Stdout.(*bytes.Buffer), c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 0 {
		t.Fatalf("code %d; stderr %q", code, errb.String())
	}
	if len(started) != 1 || started[0] != exe {
		t.Fatalf("startService calls %q, want one with %q", started, exe)
	}
	if len(asked) == 0 || asked[0] != os.Getpid() {
		t.Errorf("processGone asked about %v, want the daemon's pid %d from its status", asked, os.Getpid())
	}
	if !strings.Contains(out.String(), "service") {
		t.Errorf("stdout %q, want it to say the service was started", out.String())
	}
}

// CSR-44: with no daemon running, update leaves the service alone: a runner
// someone stopped stays stopped.
func TestUpdateDoesNotStartServiceWhenNotRunning(t *testing.T) {
	releaseServer(t, []byte("new"), "")
	c, _, _ := updateCLI(t, "9.9.9", nil)
	c.startService = func(string) (bool, error) { t.Error("started the service"); return true, nil }
	if code := c.Run([]string{"update"}); code != 0 {
		t.Fatalf("code %d; stderr %q", code, c.Stderr.(*bytes.Buffer).String())
	}
}

// CSR-44: a failed service start after the update is reported, exit 1; the
// new binary stays.
func TestUpdateServiceStartFailure(t *testing.T) {
	defer func(d time.Duration) { drainPoll = d }(drainPoll)
	drainPoll = 10 * time.Millisecond
	releaseServer(t, []byte("new"), "")
	c, exe, _ := updateCLI(t, "9.9.9", nil)
	saveConfig(t, c)
	startDaemon(t, c)
	c.startService = func(string) (bool, error) { return true, errors.New("launchctl said no") }
	errb := c.Stderr.(*bytes.Buffer)
	if code := c.Run([]string{"update"}); code != 1 {
		t.Fatalf("code %d, want 1; stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "launchctl said no") {
		t.Errorf("stderr %q, want the start error", errb.String())
	}
	if got := content(t, exe); got != "new" {
		t.Errorf("binary = %q, want the new one kept", got)
	}
}
