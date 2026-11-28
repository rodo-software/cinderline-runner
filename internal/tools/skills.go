package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// SkillUnused is how long a skill package's volume is kept after the last
// run that loaded it (CLR-257).
const SkillUnused = 30 * 24 * time.Hour

// skillVolume matches the volumes SkillVolume names.
var skillVolume = regexp.MustCompile("^" + name.Name + `-skill-[0-9a-f]{32}$`)

// A skill package's download is refused when it is larger or slower.
const (
	skillDownloadMax  = 200 << 20
	skillDownloadTime = 10 * time.Minute
)

// SkillVolume is the volume of the skill package at url and commit, which
// every project shares, named from their hash as a tools set's volume is.
// Downloads go into the package's root, so with downloads the root and the
// downloads are in the hash too: changing them gives a new volume.
func SkillVolume(url, commit, path string, downloads []cinderline.SkillDownload) string {
	key := url + "\n" + commit
	if len(downloads) > 0 {
		b, _ := json.Marshal(downloads) // a slice of plain structs always marshals
		key += "\n" + path + "\n" + string(b)
	}
	h := sha256.Sum256([]byte(key))
	return name.Name + "-skill-" + hex.EncodeToString(h[:16])
}

// skillInstallScript writes the checked downloads /s/0, /s/1... into the
// package's root $1 of the volume at /t, the rest of the arguments a path
// relative to the root and a mode for each, then marks the volume complete.
// A download's folder must not be reached through a symlink, and its path
// must not be a symlink or a folder, so no file lands outside the root.
const skillInstallScript = `set -e
r=/t/` + workspace.SkillRepo + `${1:+/$1}
shift
if [ $# -gt 0 ]; then
  test -d "$r" && test "$(realpath "$r")" = "$r" || { echo "the package's path is not a folder of the repository" >&2; exit 1; }
fi
i=0
while [ $# -gt 0 ]; do
  f=$r/$1
  d=$(dirname "$f")
  mkdir -p "$d"
  test "$(realpath "$d")" = "$d" && test ! -L "$f" && test ! -d "$f" ||
    { echo "download path $1 is reached through a symlink or is a folder: refused" >&2; exit 1; }
  cp "/s/$i" "$f"
  chmod "$2" "$f"
  i=$((i+1))
  shift 2
done
touch /t/complete`

// skillCheckScript exits 3 when the volume at /t is not complete, and 4 when
// $1 is no folder of the repository, or reaches one through a symlink. Else
// it prints what the package's root holds for SkillContents, one path per
// line: each skills/<name> with a SKILL.md, gemini-extension.json, and the
// OpenCode plugin files. Only names of safe characters, never through a
// symlink, so each stays a folder of the volume as a mount's subpath.
const skillCheckScript = `test -f /t/complete || exit 3
p=/t/` + workspace.SkillRepo + `${1:+/$1}
test -d "$p" && test "$(realpath "$p")" = "$p" || exit 4
cd "$p"
for f in skills/* gemini-extension.json .opencode/plugin/*.ts .opencode/plugin/*.js .opencode/plugins/*.ts .opencode/plugins/*.js; do
  case ${f##*/} in .*|*[!A-Za-z0-9._-]*) continue ;; esac
  case $f in skills/*) test -f "$f/SKILL.md" ;; *) test -f "$f" ;; esac &&
    test "$(realpath "$f")" = "$p/$f" && echo "$f"
done
true`

// SkillContents is what a skill package's root holds for the clients that
// do not load it whole as Claude Code does (docs/skills/client-loading.md).
type SkillContents struct {
	Skills    []string // the folders of skills/ that hold a SKILL.md, by name
	Extension bool     // gemini-extension.json: Gemini CLI loads the root as an extension
	// Plugins are the OpenCode plugin files, relative to the root, found
	// as OpenCode finds a project's: .opencode/{plugin,plugins}/*.{ts,js}.
	Plugins []string
}

var (
	skillFolder  = regexp.MustCompile(`^skills/([A-Za-z0-9_-][A-Za-z0-9._-]{0,99})$`)
	openCodeFile = regexp.MustCompile(`^\.opencode/plugins?/[A-Za-z0-9_-][A-Za-z0-9._-]{0,99}\.(ts|js)$`)
)

// parseContents reads skillCheckScript's output; a line it does not expect
// is left out, so nothing else of the package becomes a path.
func parseContents(out string) SkillContents {
	var c SkillContents
	for _, line := range strings.Split(out, "\n") {
		if m := skillFolder.FindStringSubmatch(line); m != nil {
			c.Skills = append(c.Skills, m[1])
		} else if line == "gemini-extension.json" {
			c.Extension = true
		} else if openCodeFile.MatchString(line) {
			c.Plugins = append(c.Plugins, line)
		}
	}
	return c
}

// Skills fills and tidies the volumes of skill packages (decision
// 2026-11-26-skill-packages in the app repository).
type Skills struct {
	State   string         // the host state folder: a lock per volume, whose time is its last use, and the downloads
	Exec    workspace.Exec // runs docker
	HTTP    *http.Client   // fetches the downloads; nil means http.DefaultClient
	Log     *slog.Logger
	Project string // the project whose run fills, for labels and log lines
}

// lockFile is the lock of volume, whose modification time is its last use.
func (s *Skills) lockFile(volume string) string {
	return filepath.Join(s.State, "skills", volume+".lock")
}

// Fill returns the volume of the package at url and commit, filled first
// unless it is complete, behind a file lock so that projects fill it once
// and share it, and what the package's root at path holds. It fails when
// path is not a folder of the repository or a download fails, and records
// the use for Tidy. url, commit, path and downloads are checked by the caller.
func (s *Skills) Fill(ctx context.Context, url, commit, path string, downloads []cinderline.SkillDownload) (string, SkillContents, error) {
	vol := SkillVolume(url, commit, path, downloads)
	if err := os.MkdirAll(filepath.Join(s.State, "skills"), 0o700); err != nil {
		return "", SkillContents{}, err
	}
	unlock, err := lock(s.lockFile(vol))
	if err != nil {
		return "", SkillContents{}, err
	}
	defer unlock()
	// Under the lock, so Tidy never removes a volume between this use and its record.
	now := time.Now()
	if err := os.Chtimes(s.lockFile(vol), now, now); err != nil {
		return "", SkillContents{}, err
	}
	out, err := s.check(ctx, vol, path)
	if exit := (interface{ ExitCode() int })(nil); errors.As(err, &exit) && exit.ExitCode() == 3 {
		// An interrupted fill left no marker: fill it anew, in an empty volume.
		if err := s.fill(ctx, vol, url, commit, path, downloads); err != nil {
			return "", SkillContents{}, err
		}
		out, err = s.check(ctx, vol, path)
	}
	if exit := (interface{ ExitCode() int })(nil); errors.As(err, &exit) && exit.ExitCode() == 4 {
		return "", SkillContents{}, fmt.Errorf("%q is not a folder of the repository", path)
	}
	if err != nil {
		return "", SkillContents{}, err
	}
	return vol, parseContents(out), nil
}

// fill fills vol anew: the package's downloads for the Docker engine's arch
// are fetched and checked first, on the runner's own network (decision
// 2026-11-28-skill-downloads), then the repository, then the downloads go
// in and the complete marker last.
func (s *Skills) fill(ctx context.Context, vol, url, commit, path string, downloads []cinderline.SkillDownload) error {
	staging := filepath.Join(s.State, "skills", vol)
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	install := []string{"docker", "run", "--rm", "--network", "none", "-v", vol + ":/t", "-v", staging + ":/s:ro",
		"--entrypoint", "sh", workspace.GitImage, "-c", skillInstallScript, "sh", path}
	if len(downloads) > 0 {
		out, err := s.Exec(ctx, nil, "docker", "version", "--format", "{{.Server.Arch}}")
		if err != nil {
			return err
		}
		arch := strings.TrimSpace(out)
		hc := s.HTTP
		if hc == nil {
			hc = http.DefaultClient
		}
		n := 0 // the files fetched: /s/0, /s/1...
		for _, d := range downloads {
			if d.Arch != arch {
				continue
			}
			dctx, cancel := context.WithTimeout(ctx, skillDownloadTime)
			err := download(dctx, hc, d.URL, filepath.Join(staging, strconv.Itoa(n)), d.SHA256, skillDownloadMax)
			cancel()
			if err != nil {
				return err
			}
			mode := "644"
			if d.Executable {
				mode = "755"
			}
			install = append(install, d.Path, mode)
			n++
		}
	}
	for _, argv := range [][]string{{"docker", "volume", "rm", "--force", vol}, {"docker", "volume", "create", vol}} {
		if _, err := s.Exec(ctx, nil, argv...); err != nil {
			return err
		}
	}
	if err := workspace.FetchSkill(ctx, s.Exec, s.Log, s.Project, vol, url, commit); err != nil {
		return err
	}
	_, err := s.Exec(ctx, nil, install...)
	return err
}

// check runs skillCheckScript on vol, read-only.
func (s *Skills) check(ctx context.Context, vol, path string) (string, error) {
	return s.Exec(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", vol+":/t:ro",
		"--entrypoint", "sh", workspace.GitImage, "-c", skillCheckScript, "sh", path)
}

// Tidy removes the skill package volumes no run has used for SkillUnused,
// each behind its lock. A volume without a record of its use gets one now,
// so it is kept that long too. A volume in use stays. Failures only log.
func (s *Skills) Tidy(ctx context.Context) {
	out, err := s.Exec(ctx, nil, "docker", "volume", "ls", "--quiet", "--filter", "name="+name.Name+"-skill-")
	if err != nil {
		s.Log.Warn("skill package volumes not listed", "project", s.Project, "err", err)
		return
	}
	for _, vol := range strings.Fields(out) {
		if !skillVolume.MatchString(vol) || s.used(vol) {
			continue
		}
		if err := s.remove(ctx, vol); err != nil {
			s.Log.Warn("skill package volume not removed", "project", s.Project, "volume", vol, "err", err)
		}
	}
}

// used says vol was used within SkillUnused; an error says so too.
func (s *Skills) used(vol string) bool {
	fi, err := os.Stat(s.lockFile(vol))
	return err == nil && time.Since(fi.ModTime()) < SkillUnused || err != nil && !errors.Is(err, os.ErrNotExist)
}

// remove removes vol unless a run used it since used looked. The lock file
// stays: removing it could let two fills take two locks.
func (s *Skills) remove(ctx context.Context, vol string) error {
	if err := os.MkdirAll(filepath.Join(s.State, "skills"), 0o700); err != nil {
		return err
	}
	unlock, err := lock(s.lockFile(vol))
	if err != nil {
		return err
	}
	defer unlock()
	if s.used(vol) {
		return nil // used meanwhile, or no record until lock made one
	}
	// No --force: docker refuses a volume a container uses.
	if _, err := s.Exec(ctx, nil, "docker", "volume", "rm", vol); err != nil {
		return err
	}
	s.Log.Info("skill package volume removed: no run used it for 30 days", "project", s.Project, "volume", vol)
	return nil
}
