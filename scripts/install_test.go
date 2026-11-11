// Package scripts tests the one-line installers install.sh and install.ps1
// against a local release served by httptest (CSR-42).
package scripts

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

var binary = []byte("#!/bin/sh\necho fake cinderline\n")

// release serves an archive for this machine's OS and arch plus checksums.txt.
// With tamper, checksums.txt lists a hash that does not match the archive.
func release(t *testing.T, tamper bool) *httptest.Server {
	t.Helper()
	archive, file := tarGz(t), fmt.Sprintf("%s_%s_%s.tar.gz", name.Name, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		archive, file = zipped(t), fmt.Sprintf("%s_%s_%s.zip", name.Name, runtime.GOOS, runtime.GOARCH)
	}
	sum := sha256.Sum256(archive)
	if tamper {
		sum[0] ^= 0xff
	}
	other := sha256.Sum256([]byte("other"))
	checksums := fmt.Sprintf("%x  %s_plan9_amd64.tar.gz\n%x  %s\n", other, name.Name, sum, file)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+file, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(checksums)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func tarGz(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []string{"LICENSE", "README.md", name.Name} {
		body := []byte("text\n")
		if f == name.Name {
			body = binary
		}
		if err := tw.WriteHeader(&tar.Header{Name: f, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipped(t *testing.T) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []string{"LICENSE", "README.md", name.Name + ".exe"} {
		w, err := zw.Create(f)
		if err != nil {
			t.Fatal(err)
		}
		body := []byte("text\n")
		if f == name.Name+".exe" {
			body = binary
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var urlEnv = strings.ToUpper(name.Name) + "_DOWNLOAD_URL"

// runSh runs install.sh with HOME in a temporary directory, a PATH without
// ~/.local/bin and SHELL set to zsh; env overrides any of these. It returns
// HOME, output and error.
func runSh(t *testing.T, srv *httptest.Server, env ...string) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	out, err := runShIn(t, home, srv, env...)
	return home, out, err
}

// runShIn is runSh with a given HOME.
func runShIn(t *testing.T, home string, srv *httptest.Server, env ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(append(os.Environ(), "HOME="+home, urlEnv+"="+srv.URL, "SHELL=/bin/zsh", noModifyEnv+"="), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func installed(home string) string { return filepath.Join(home, ".local", "bin", name.Name) }

func TestInstallShGoodChecksumInstallsIntoLocalBin(t *testing.T) {
	home, out, err := runSh(t, release(t, false))
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(installed(home))
	if err != nil {
		t.Fatalf("binary not installed: %v\n%s", err, out)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed %q, want %q", got, binary)
	}
	if info, _ := os.Stat(installed(home)); info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
}

const pathLine = `export PATH="$HOME/.local/bin:$PATH"`

var noModifyEnv = strings.ToUpper(name.Name) + "_NO_MODIFY_PATH"

// files lists every file under home except the installed binary.
func files(t *testing.T, home string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && p != installed(home) {
			got = append(got, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestInstallShAddsPathLineToTheShellsProfile(t *testing.T) {
	bash := ".bashrc"
	if runtime.GOOS == "darwin" {
		bash = ".bash_profile"
	}
	for shell, profile := range map[string]string{"/bin/zsh": ".zshrc", "/usr/local/bin/bash": bash, "/bin/dash": ".profile"} {
		home, out, err := runSh(t, release(t, false), "SHELL="+shell)
		if err != nil {
			t.Fatalf("%s: install.sh failed: %v\n%s", shell, err, out)
		}
		got, err := os.ReadFile(filepath.Join(home, profile))
		if err != nil {
			t.Fatalf("%s: %s not written: %v\n%s", shell, profile, err, out)
		}
		if !strings.Contains(string(got), pathLine+"\n") {
			t.Errorf("%s: %s lacks the PATH line:\n%s", shell, profile, got)
		}
		if !strings.Contains(out, filepath.Join(home, profile)) || !strings.Contains(out, "new terminal") {
			t.Errorf("%s: output does not name %s and a new terminal:\n%s", shell, profile, out)
		}
		if f := files(t, home); len(f) != 1 {
			t.Errorf("%s: files changed %v, want only %s", shell, f, profile)
		}
	}
}

func TestInstallShTwiceAddsPathLineOnce(t *testing.T) {
	srv := release(t, false)
	home, out, err := runSh(t, srv)
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	before, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if out, err := runShIn(t, home, srv); err != nil {
		t.Fatalf("second install.sh failed: %v\n%s", err, out)
	}
	after, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if !bytes.Equal(before, after) || strings.Count(string(after), pathLine) != 1 {
		t.Fatalf(".zshrc changed by the second run:\n%s\nthen\n%s", before, after)
	}
}

func TestInstallShLeavesProfilesAloneWhenLocalBinOnPath(t *testing.T) {
	home := t.TempDir()
	out, err := runShIn(t, home, release(t, false), "PATH="+filepath.Join(home, ".local", "bin")+":"+os.Getenv("PATH"))
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if f := files(t, home); len(f) != 0 {
		t.Fatalf("files changed %v, want none\n%s", f, out)
	}
	if strings.Contains(out, "Warning") {
		t.Fatalf("warned although ~/.local/bin is on the PATH:\n%s", out)
	}
}

// The opt-out, a shell with a different syntax and a profile that cannot be
// written all leave the profiles alone, print today's warning and exit 0.
func TestInstallShPrintsPathLineWhenItDoesNotEditTheProfile(t *testing.T) {
	for _, tc := range []struct {
		desc string
		env  []string
	}{
		{"opt-out", []string{noModifyEnv + "=1"}},
		{"fish", []string{"SHELL=/usr/bin/fish"}},
		{"tcsh", []string{"SHELL=/bin/tcsh"}},
	} {
		home, out, err := runSh(t, release(t, false), tc.env...)
		if err != nil {
			t.Fatalf("%s: install.sh failed: %v\n%s", tc.desc, err, out)
		}
		if f := files(t, home); len(f) != 0 {
			t.Errorf("%s: files changed %v, want none", tc.desc, f)
		}
		if !strings.Contains(out, "Warning") || !strings.Contains(out, pathLine) {
			t.Errorf("%s: no warning with the PATH line:\n%s", tc.desc, out)
		}
	}
}

func TestInstallShUnwritableProfileWarnsAndSucceeds(t *testing.T) {
	home := t.TempDir()
	// A directory cannot be appended to, even by root.
	if err := os.Mkdir(filepath.Join(home, ".zshrc"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runShIn(t, home, release(t, false))
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Warning") || !strings.Contains(out, pathLine) {
		t.Fatalf("no warning with the PATH line:\n%s", out)
	}
}

func TestInstallShTamperedChecksumRefuses(t *testing.T) {
	home, out, err := runSh(t, release(t, true))
	if err == nil {
		t.Fatalf("install.sh succeeded with a tampered checksum:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "checksum") {
		t.Fatalf("refusal does not mention the checksum:\n%s", out)
	}
	if _, err := os.Stat(installed(home)); err == nil {
		t.Fatal("binary installed despite checksum mismatch")
	}
}

// runPs runs install.ps1 with LOCALAPPDATA in a temporary directory. It
// returns the install path, output and error.
func runPs(t *testing.T, srv *httptest.Server) (string, string, error) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows with PowerShell") // macOS runners have powershell too, but the script reads Windows-only variables
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		if ps, err = exec.LookPath("pwsh"); err != nil {
			t.Skip("install.ps1 runs on Windows with PowerShell")
		}
	}
	local := t.TempDir()
	// pwsh's PSModulePath stops Windows PowerShell 5.1 from finding Get-FileHash.
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if strings.EqualFold(key, "PSModulePath") {
			continue
		}
		env = append(env, e)
	}
	// The opt-out keeps the test from adding a temporary folder to the
	// machine's real user PATH.
	env = append(env, "LOCALAPPDATA="+local, urlEnv+"="+srv.URL, noModifyEnv+"=1")
	cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return filepath.Join(local, "Programs", name.Name, name.Name+".exe"), string(out), err
}

func TestInstallPs1GoodChecksumInstallsIntoLocalAppData(t *testing.T) {
	path, out, err := runPs(t, release(t, false))
	if err != nil {
		t.Fatalf("install.ps1 failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("binary not installed: %v\n%s", err, out)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed %q, want %q", got, binary)
	}
}

func TestInstallPs1TamperedChecksumRefuses(t *testing.T) {
	path, out, err := runPs(t, release(t, true))
	if err == nil {
		t.Fatalf("install.ps1 succeeded with a tampered checksum:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "checksum") {
		t.Fatalf("refusal does not mention the checksum:\n%s", out)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("binary installed despite checksum mismatch")
	}
}

// The installers download <name>_<os>_<arch>.<ext> from the latest release,
// so the release must name its archives and binary that way.
func TestGoReleaserNamesArchivesForTheInstallers(t *testing.T) {
	cfg, err := os.ReadFile(filepath.Join("..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"project_name: " + name.Name,
		`name_template: "{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}"`,
	} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf(".goreleaser.yaml lacks %q", want)
		}
	}
}
