package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func sriSHA512(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

func serve(t *testing.T, files map[string][]byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDownloadChecksHash(t *testing.T) {
	body := []byte("the real binary")
	base := serve(t, map[string][]byte{"/claude": body})
	for _, tc := range []struct {
		name, sum string
		ok        bool
	}{
		{"sha256 match", sha256Hex(body), true},
		{"sha256 tampered", sha256Hex([]byte("other")), false},
		{"sha512 match", sriSHA512(body), true},
		{"sha512 tampered", sriSHA512([]byte("other")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "claude")
			err := download(context.Background(), http.DefaultClient, base+"/claude", dst, tc.sum)
			_, statErr := os.Stat(dst)
			if tc.ok {
				if err != nil || statErr != nil {
					t.Fatalf("download: %v, stat: %v", err, statErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "mismatch") {
				t.Fatalf("tampered download accepted: %v", err)
			}
			if statErr == nil {
				t.Fatal("tampered download left on disk")
			}
		})
	}
}

func TestDownloadRefusesHTTPError(t *testing.T) {
	base := serve(t, nil)
	dst := filepath.Join(t.TempDir(), "x")
	if err := download(context.Background(), http.DefaultClient, base+"/missing", dst, sha256Hex(nil)); err == nil {
		t.Fatal("404 accepted")
	}
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestExtractOpenCode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "oc.tgz")
	if err := os.WriteFile(src, tgz(t, map[string]string{"package/package.json": "{}", "package/bin/opencode": "ELF"}), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "opencode")
	if err := extract(src, "package/bin/opencode", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "ELF" {
		t.Fatalf("got %q", b)
	}
	if err := extract(src, "package/bin/missing", filepath.Join(dir, "m")); err == nil {
		t.Fatal("missing entry accepted")
	}
}

func TestPinsCoverEveryPlatform(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		for _, musl := range []bool{false, true} {
			s, err := pinned(arch, musl)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.ClaudeSHA256) != 64 || !strings.HasPrefix(s.OpenCodeSRI, "sha512-") {
				t.Fatalf("%s musl=%v: bad pins %+v", arch, musl, s)
			}
			// Codex ships static musl builds only, from its GitHub release;
			// glibc and musl images share them.
			target := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch] + "-unknown-linux-musl"
			if s.CodexBinary != "codex-"+target || len(s.CodexSHA256) != 64 ||
				s.CodexURL != "https://github.com/openai/codex/releases/download/rust-v"+CodexVersion+"/codex-"+target+".tar.gz" {
				t.Fatalf("%s musl=%v: bad Codex pins %+v", arch, musl, s)
			}
			// Codex runs code mode in this host, which it looks for next to itself.
			if s.CodexHostBinary != "codex-code-mode-host-"+target || len(s.CodexHostSHA256) != 64 ||
				s.CodexHostURL != "https://github.com/openai/codex/releases/download/rust-v"+CodexVersion+"/codex-code-mode-host-"+target+".tar.gz" {
				t.Fatalf("%s musl=%v: bad Codex code-mode host pins %+v", arch, musl, s)
			}
		}
	}
	if _, err := pinned("riscv64", false); err == nil {
		t.Fatal("unknown arch accepted")
	}
}

func TestVolumeNaming(t *testing.T) {
	g, _ := pinned("amd64", false)
	m, _ := pinned("amd64", true)
	a, _ := pinned("arm64", false)
	again, _ := pinned("amd64", false)
	if g.volume() != again.volume() || !strings.HasPrefix(g.volume(), "cinderline-tools-") {
		t.Fatalf("volume %q", g.volume())
	}
	names := map[string]bool{g.volume(): true, m.volume(): true, a.volume(): true}
	changed := g
	changed.PonytailCommit = strings.Repeat("0", 40)
	names[changed.volume()] = true
	if len(names) != 4 {
		t.Fatalf("pinned sets share a volume: %v", names)
	}
}

// fakeDocker answers docker commands by argv prefix and records them.
type fakeDocker struct {
	calls   [][]string
	answers map[string]string
	fail    map[string]error
}

func (f *fakeDocker) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	f.calls = append(f.calls, argv)
	key := strings.Join(argv[:3], " ")
	return f.answers[key], f.fail[key]
}

func TestDetectMusl(t *testing.T) {
	for out, want := range map[string]bool{"musl\n": true, "glibc\n": false} {
		f := &fakeDocker{answers: map[string]string{"docker run --rm": out}}
		got, err := detectMusl(context.Background(), f.exec, "php:8.4-cli-alpine")
		if err != nil || got != want {
			t.Fatalf("%q: got %v, %v", out, got, err)
		}
		wantArgv := []string{"docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", "php:8.4-cli-alpine",
			"-c", "ls /lib/ld-musl-* >/dev/null 2>&1 && echo musl || echo glibc"}
		if !reflect.DeepEqual(f.calls[0], wantArgv) {
			t.Fatalf("argv %q", f.calls[0])
		}
	}
	f := &fakeDocker{answers: map[string]string{"docker run --rm": "what\n"}}
	if _, err := detectMusl(context.Background(), f.exec, "x"); err == nil {
		t.Fatal("odd answer accepted")
	}
}

func TestLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		u, err := lock(path)
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("second lock taken while the first is held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not taken after unlock")
	}
}

// pinsServer serves the claude binary, the opencode tarball, the codex
// tarballs and the gemini tarball of the musl arm64 set, which takes node
// from the musl libraries image instead of a download.
func pinsServer(t *testing.T, claude, oc []byte) (string, set) {
	s, _ := pinned("arm64", true)
	codex := tgz(t, map[string]string{s.CodexBinary: "CODEX"})
	host := tgz(t, map[string]string{s.CodexHostBinary: "HOST"})
	gemini := tgz(t, map[string]string{"package/bundle/gemini.js": "GEMINI"})
	s.ClaudeSHA256 = sha256Hex(claude)
	s.OpenCodeSRI = sriSHA512(oc)
	s.CodexSHA256 = sha256Hex(codex)
	s.CodexHostSHA256 = sha256Hex(host)
	s.GeminiSRI = sriSHA512(gemini)
	base := serve(t, map[string][]byte{"/claude": claude, "/oc.tgz": oc, "/codex.tgz": codex, "/host.tgz": host, "/gemini.tgz": gemini})
	s.ClaudeURL, s.OpenCodeURL, s.CodexURL, s.CodexHostURL, s.GeminiURL = base+"/claude", base+"/oc.tgz", base+"/codex.tgz", base+"/host.tgz", base+"/gemini.tgz"
	return base, s
}

// Gemini CLI is the npm package, the same tarball on every platform, checked
// by its npm integrity. Node comes from nodejs.org on glibc, checked by the
// SHA-256 of its SHASUMS256.txt, and from the pinned musl libraries image on
// musl, so a musl set downloads none (decision 2026-11-08-gemini-node-in-tools-volume).
func TestGeminiPins(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		a := map[string]string{"amd64": "x64", "arm64": "arm64"}[arch]
		for _, musl := range []bool{false, true} {
			s, err := pinned(arch, musl)
			if err != nil {
				t.Fatal(err)
			}
			if s.GeminiURL != "https://registry.npmjs.org/@google/gemini-cli/-/gemini-cli-"+GeminiVersion+".tgz" || !strings.HasPrefix(s.GeminiSRI, "sha512-") {
				t.Fatalf("%s musl=%v: bad Gemini pins %+v", arch, musl, s)
			}
			if musl {
				if s.NodeURL != "" || s.NodeSHA256 != "" || s.NodeBinary != "" {
					t.Fatalf("%s musl: node is downloaded, want it from the musl libraries image: %+v", arch, s)
				}
				continue
			}
			dir := "node-v" + NodeVersion + "-linux-" + a
			if s.NodeURL != "https://nodejs.org/dist/v"+NodeVersion+"/"+dir+".tar.gz" || len(s.NodeSHA256) != 64 || s.NodeBinary != dir+"/bin/node" {
				t.Fatalf("%s glibc: bad Node pins %+v", arch, s)
			}
		}
	}
}

// The fill script unpacks the gemini package into gemini/ (gemini/bundle/gemini.js)
// and installs a downloaded node next to the other CLIs.
func TestFillScriptInstallsGemini(t *testing.T) {
	for _, want := range []string{"/s/gemini.tgz", "gemini", "/s/node", "bin/node"} {
		if !strings.Contains(fillScript, want) {
			t.Fatalf("fill script lacks %q:\n%s", want, fillScript)
		}
	}
}

// On glibc the checked node binary from the nodejs.org tarball and the
// gemini tarball are staged for the fill script, and no musl step runs.
func TestMountStagesGlibcNode(t *testing.T) {
	_, s := pinsServer(t, []byte("CLAUDE"), tgz(t, map[string]string{"package/bin/opencode": "OC"}))
	nodeTgz := tgz(t, map[string]string{"node-v" + NodeVersion + "-linux-arm64/bin/node": "NODE"})
	base := serve(t, map[string][]byte{"/node.tgz": nodeTgz})
	s.Musl, s.Platform = false, "linux-arm64"
	s.NodeURL, s.NodeSHA256, s.NodeBinary = base+"/node.tgz", sha256Hex(nodeTgz), "node-v"+NodeVersion+"-linux-arm64/bin/node"
	staging := ""
	var node, gemini []byte
	f := &fakeDocker{answers: map[string]string{"docker version --format": "arm64\n", "docker run --rm": "glibc\n"}}
	exec := func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, err := f.exec(ctx, env, argv...)
		if len(f.calls) == 3 {
			return "", errors.New("exit status 1")
		}
		for i, a := range argv {
			if strings.HasSuffix(a, ":/s:ro") {
				staging = strings.TrimSuffix(argv[i], ":/s:ro")
				node, _ = os.ReadFile(filepath.Join(staging, "node"))
				gemini, _ = os.ReadFile(filepath.Join(staging, "gemini.tgz"))
			}
		}
		return out, err
	}
	tl := &Tools{State: t.TempDir(), Exec: exec, pins: func(string, bool) (set, error) { return s, nil }}
	if _, err := tl.Mount(context.Background(), "img"); err != nil {
		t.Fatal(err)
	}
	if staging == "" || string(node) != "NODE" || len(gemini) == 0 {
		t.Fatalf("staged node %q, gemini %d bytes, staging %q", node, len(gemini), staging)
	}
	for _, c := range f.calls {
		if slices.Contains(c, muslLibsImage) {
			t.Fatalf("musl step on glibc: %q", c)
		}
	}
}

func TestMountRefusesTamperedGemini(t *testing.T) {
	_, s := pinsServer(t, []byte("CLAUDE"), tgz(t, map[string]string{"package/bin/opencode": "OC"}))
	s.GeminiSRI = sriSHA512([]byte("something else"))
	f := &fakeDocker{answers: map[string]string{"docker version --format": "arm64\n", "docker run --rm": "musl\n"}}
	exec := func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, err := f.exec(ctx, env, argv...)
		if len(f.calls) == 3 {
			return "", errors.New("exit status 1")
		}
		return out, err
	}
	tl := &Tools{State: t.TempDir(), Exec: exec, pins: func(string, bool) (set, error) { return s, nil }}
	if _, err := tl.Mount(context.Background(), "img"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered gemini accepted: %v", err)
	}
}

// The fill script puts codex and its code-mode host next to claude and opencode.
func TestFillScriptInstallsCodex(t *testing.T) {
	if !strings.Contains(fillScript, "/s/codex") || !strings.Contains(fillScript, "bin/codex") ||
		!strings.Contains(fillScript, "/s/codex-code-mode-host") || !strings.Contains(fillScript, "bin/codex-code-mode-host") {
		t.Fatalf("fill script does not install codex:\n%s", fillScript)
	}
}

func TestMountRefusesTamperedCodex(t *testing.T) {
	_, s := pinsServer(t, []byte("CLAUDE"), tgz(t, map[string]string{"package/bin/opencode": "OC"}))
	s.CodexSHA256 = sha256Hex([]byte("something else"))
	f := &fakeDocker{answers: map[string]string{"docker version --format": "arm64\n", "docker run --rm": "musl\n"}}
	n := 0
	exec := func(ctx context.Context, env []string, argv ...string) (string, error) {
		n++
		out, err := f.exec(ctx, env, argv...)
		if n == 3 {
			return "", errors.New("exit status 1")
		}
		return out, err
	}
	tl := &Tools{State: t.TempDir(), Exec: exec, pins: func(string, bool) (set, error) { return s, nil }}
	if _, err := tl.Mount(context.Background(), "img"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered codex accepted: %v", err)
	}
}

func TestMountFillsMissingVolume(t *testing.T) {
	oc := tgz(t, map[string]string{"package/bin/opencode": "OC"})
	_, s := pinsServer(t, []byte("CLAUDE"), oc)
	vol := s.volume()
	state := t.TempDir()
	f := &fakeDocker{
		answers: map[string]string{"docker version --format": "arm64\n", "docker run --rm": "musl\n"},
		fail:    map[string]error{},
	}
	// The completeness check is the third call; make it fail like `test -f`.
	exec := func(ctx context.Context, env []string, argv ...string) (string, error) {
		out, err := f.exec(ctx, env, argv...)
		if len(f.calls) == 3 {
			return "", errors.New("exit status 1")
		}
		return out, err
	}
	tl := &Tools{State: state, Exec: exec, pins: func(string, bool) (set, error) { return s, nil }}
	m, err := tl.Mount(context.Background(), "php:8.4-cli-alpine")
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(state, "tools", vol)
	want := [][]string{
		{"docker", "version", "--format", "{{.Server.Arch}}"},
		{"docker", "run", "--rm", "--network", "none", "--entrypoint", "sh", "php:8.4-cli-alpine", "-c", "ls /lib/ld-musl-* >/dev/null 2>&1 && echo musl || echo glibc"},
		{"docker", "run", "--rm", "--network", "none", "-v", vol + ":/t:ro", "--entrypoint", "test", workspace.GitImage, "-f", "/t/complete"},
		{"docker", "volume", "rm", "--force", vol},
		{"docker", "volume", "create", vol},
		{"docker", "run", "--rm", "--platform", "linux/arm64", "--network", "none", "-v", vol + ":/t", "--entrypoint", "sh", muslLibsImage,
			"-c", "mkdir -p /t/lib /t/bin && cp -L /usr/lib/libstdc++.so.6 /usr/lib/libgcc_s.so.1 /t/lib/ && cp /usr/local/bin/node /t/bin/node"},
		{"docker", "run", "--rm", "-v", vol + ":/t", "-v", staging + ":/s:ro", "--entrypoint", "sh", workspace.GitImage, "-c", fillScript,
			"sh", PonytailRepo, s.PonytailTag, s.PonytailCommit},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv\n got %q\nwant %q", f.calls, want)
	}
	wantMount := Mount{
		Args:        []string{"-v", vol + ":" + Path + ":ro"},
		Env:         []string{"DISABLE_AUTOUPDATER=1"},
		OpenCodeEnv: []string{"LD_LIBRARY_PATH=" + Path + "/lib"},
	}
	if !reflect.DeepEqual(m, wantMount) {
		t.Fatalf("mount %+v", m)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging left behind: %v", err)
	}
}

func TestMountSkipsCompleteVolume(t *testing.T) {
	s, _ := pinned("amd64", false)
	f := &fakeDocker{answers: map[string]string{"docker version --format": "amd64\n", "docker run --rm": "glibc\n"}}
	tl := &Tools{State: t.TempDir(), Exec: f.exec}
	m, err := tl.Mount(context.Background(), "img")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls %q", f.calls)
	}
	if m.OpenCodeEnv != nil || m.Args[1] != s.volume()+":"+Path+":ro" {
		t.Fatalf("mount %+v", m)
	}
}

func TestMountRefusesTamperedDownload(t *testing.T) {
	_, s := pinsServer(t, []byte("CLAUDE"), tgz(t, map[string]string{"package/bin/opencode": "OC"}))
	s.ClaudeSHA256 = sha256Hex([]byte("something else"))
	f := &fakeDocker{answers: map[string]string{"docker version --format": "arm64\n", "docker run --rm": "musl\n"}}
	n := 0
	exec := func(ctx context.Context, env []string, argv ...string) (string, error) {
		n++
		out, err := f.exec(ctx, env, argv...)
		if n == 3 {
			return "", errors.New("exit status 1")
		}
		return out, err
	}
	tl := &Tools{State: t.TempDir(), Exec: exec, pins: func(string, bool) (set, error) { return s, nil }}
	if _, err := tl.Mount(context.Background(), "img"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered download accepted: %v", err)
	}
	for _, c := range f.calls {
		if c[1] == "volume" && c[2] == "create" {
			t.Fatal("volume filled from a tampered download")
		}
	}
}
