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

// runSh runs install.sh with HOME in a temporary directory and a PATH
// without ~/.local/bin. It returns the install path, output and error.
func runSh(t *testing.T, srv *httptest.Server) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	home := t.TempDir()
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(), "HOME="+home, urlEnv+"="+srv.URL)
	out, err := cmd.CombinedOutput()
	return filepath.Join(home, ".local", "bin", name.Name), string(out), err
}

func TestInstallShGoodChecksumInstallsIntoLocalBin(t *testing.T) {
	path, out, err := runSh(t, release(t, false))
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("binary not installed: %v\n%s", err, out)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("installed %q, want %q", got, binary)
	}
	if info, _ := os.Stat(path); info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
}

func TestInstallShPrintsPathLineWhenLocalBinNotOnPath(t *testing.T) {
	_, out, err := runSh(t, release(t, false))
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Fatalf("no PATH line in output:\n%s", out)
	}
}

func TestInstallShTamperedChecksumRefuses(t *testing.T) {
	path, out, err := runSh(t, release(t, true))
	if err == nil {
		t.Fatalf("install.sh succeeded with a tampered checksum:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "checksum") {
		t.Fatalf("refusal does not mention the checksum:\n%s", out)
	}
	if _, err := os.Stat(path); err == nil {
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
	env = append(env, "LOCALAPPDATA="+local, urlEnv+"="+srv.URL)
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
