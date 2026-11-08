// Package tools fills and mounts the tools volume: the pinned agent CLIs,
// downloaded on the user's machine from their official sources and checked
// by hash (EMBER-57), in a read-only volume per pinned set that every agent
// container mounts. No image of ours carries them, so Claude Code is never
// redistributed (2026-11-04-licences-before-publishing).
package tools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Path is where agent containers see the volume: the CLIs in Path/bin, the
// ponytail plugin in Path/ponytail and, on musl, OpenCode's libraries in
// Path/lib.
const Path = "/opt/" + name.Name

// fillScript fills the volume at /t from the checked downloads at /s, then
// clones ponytail at its pinned tag and refuses any other commit. The
// marker comes last, so an interrupted fill is redone.
const fillScript = `set -e
cd /t
mkdir -p bin
cp /s/claude /s/opencode bin/
chmod 755 bin/claude bin/opencode
git clone --quiet --depth 1 --branch "$2" "$1" ponytail
test "$(git -C ponytail rev-parse HEAD)" = "$3"
rm -rf ponytail/.git
touch complete`

// Mount is how an agent container gets the tools volume.
type Mount struct {
	Args []string // docker run flags that mount the volume read-only at Path
	// Env is for every agent container. DISABLE_AUTOUPDATER stops Claude
	// Code from updating itself on every run from a read-only volume
	// (review item 11).
	Env []string
	// OpenCodeEnv is added for OpenCode runs: on musl, the libraries it
	// needs from the volume. Only for those runs, so the image's own
	// programs keep their libraries.
	OpenCodeEnv []string
}

// Tools fills and mounts the tools volume.
type Tools struct {
	State string         // the host state folder: the lock and the downloads
	Exec  workspace.Exec // nil means workspace.RunCommand
	HTTP  *http.Client   // nil means http.DefaultClient

	pins func(arch string, musl bool) (set, error) // nil means pinned; tests swap it
}

// Mount detects image's libc, fills the volume of the matching pinned set
// if it isn't complete yet, and says how to mount it.
func (t *Tools) Mount(ctx context.Context, image string) (Mount, error) {
	x := t.Exec
	if x == nil {
		x = workspace.RunCommand
	}
	// The engine's arch, not the runner's: an amd64 runner can drive an
	// arm64 engine.
	arch, err := x(ctx, nil, "docker", "version", "--format", "{{.Server.Arch}}")
	if err != nil {
		return Mount{}, err
	}
	musl, err := detectMusl(ctx, x, image)
	if err != nil {
		return Mount{}, err
	}
	pins := t.pins
	if pins == nil {
		pins = pinned
	}
	s, err := pins(strings.TrimSpace(arch), musl)
	if err != nil {
		return Mount{}, err
	}
	if err := t.fill(ctx, x, s); err != nil {
		return Mount{}, fmt.Errorf("tools volume %s: %w", s.volume(), err)
	}
	m := Mount{Args: []string{"-v", s.volume() + ":" + Path + ":ro"}, Env: []string{"DISABLE_AUTOUPDATER=1"}}
	if musl {
		m.OpenCodeEnv = []string{"LD_LIBRARY_PATH=" + Path + "/lib"}
	}
	return m, nil
}

// fill makes s's volume unless it is complete, behind a file lock so
// projects running in parallel fill it once and share it.
func (t *Tools) fill(ctx context.Context, x workspace.Exec, s set) error {
	vol := s.volume()
	dir := filepath.Join(t.State, "tools")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(dir, vol+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	// A failed check means not filled (or not completely): fill it anew.
	if _, err := x(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", vol+":/t:ro", "--entrypoint", "test", workspace.GitImage, "-f", "/t/complete"); err == nil {
		return nil
	}

	// Download and check everything before the volume is touched.
	staging := filepath.Join(dir, vol)
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	hc := t.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	if err := download(ctx, hc, s.ClaudeURL, filepath.Join(staging, "claude"), s.ClaudeSHA256); err != nil {
		return err
	}
	tgz := filepath.Join(staging, "opencode.tgz")
	if err := download(ctx, hc, s.OpenCodeURL, tgz, s.OpenCodeSRI); err != nil {
		return err
	}
	if err := extract(tgz, "package/bin/opencode", filepath.Join(staging, "opencode")); err != nil {
		return err
	}

	steps := [][]string{
		{"docker", "volume", "rm", "--force", vol},
		{"docker", "volume", "create", vol},
	}
	if s.Musl {
		steps = append(steps, []string{"docker", "run", "--rm", "--platform", "linux/" + s.Arch, "--network", "none", "-v", vol + ":/t",
			"--entrypoint", "sh", muslLibsImage, "-c", "mkdir -p /t/lib && cp -L /usr/lib/libstdc++.so.6 /usr/lib/libgcc_s.so.1 /t/lib/"})
	}
	steps = append(steps, []string{"docker", "run", "--rm", "-v", vol + ":/t", "-v", staging + ":/s:ro",
		"--entrypoint", "sh", workspace.GitImage, "-c", fillScript, "sh", PonytailRepo, s.PonytailTag, s.PonytailCommit})
	for _, argv := range steps {
		if _, err := x(ctx, nil, argv...); err != nil {
			return err
		}
	}
	return nil
}

// set is the pinned tools for one platform; its hash names its volume.
type set struct {
	Arch, Platform            string
	Musl                      bool
	ClaudeURL, ClaudeSHA256   string
	OpenCodeURL, OpenCodeSRI  string
	PonytailTag               string
	PonytailCommit, LibsImage string
}

// pinned is the set for a Docker engine arch (amd64 or arm64) and libc.
func pinned(arch string, musl bool) (set, error) {
	a := map[string]string{"amd64": "x64", "arm64": "arm64"}[arch]
	if a == "" {
		return set{}, fmt.Errorf("no pinned tools for arch %q", arch)
	}
	p := "linux-" + a
	if musl {
		p += "-musl"
	}
	return set{
		Arch: arch, Platform: p, Musl: musl,
		ClaudeURL:    "https://downloads.claude.ai/claude-code-releases/" + ClaudeVersion + "/" + p + "/claude",
		ClaudeSHA256: claudeSHA256[p],
		OpenCodeURL:  "https://registry.npmjs.org/opencode-" + p + "/-/opencode-" + p + "-" + OpenCodeVersion + ".tgz",
		OpenCodeSRI:  openCodeSRI[p],
		PonytailTag:  PonytailTag, PonytailCommit: PonytailCommit, LibsImage: muslLibsImage,
	}, nil
}

func (s set) volume() string {
	h := sha256.Sum256(fmt.Appendf(nil, "%+v", s))
	return name.Name + "-tools-" + hex.EncodeToString(h[:6])
}

// download fetches url into dst and keeps it only if it matches sum: a hex
// SHA-256, or an npm integrity "sha512-<base64>".
func download(ctx context.Context, hc *http.Client, url, dst, sum string) error {
	var h hash.Hash
	var encode func([]byte) string
	if b64, ok := strings.CutPrefix(sum, "sha512-"); ok {
		h, encode, sum = sha512.New(), base64.StdEncoding.EncodeToString, b64
	} else {
		h, encode = sha256.New(), hex.EncodeToString
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && encode(h.Sum(nil)) != sum {
		err = fmt.Errorf("download %s: hash mismatch, refused", url)
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// extract writes the regular file entry of the gzipped tarball src to dst.
func extract(src, entry, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: no %s", src, entry)
		}
		if err != nil {
			return err
		}
		if hdr.Name != entry || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
}
