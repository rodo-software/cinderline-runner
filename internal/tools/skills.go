package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// SkillUnused is how long a skill package's volume is kept after the last
// run that loaded it (CLR-257).
const SkillUnused = 30 * 24 * time.Hour

// skillVolume matches the volumes SkillVolume names.
var skillVolume = regexp.MustCompile("^" + name.Name + `-skill-[0-9a-f]{32}$`)

// SkillVolume is the volume of the skill package at url and commit, which
// every project shares, named from their hash as a tools set's volume is.
func SkillVolume(url, commit string) string {
	h := sha256.Sum256([]byte(url + "\n" + commit))
	return name.Name + "-skill-" + hex.EncodeToString(h[:16])
}

// SkillRoot is the package's root in its volume, for a mount's subpath:
// path ("": the repository's root) in the repository's folder.
func SkillRoot(path string) string {
	if path == "" {
		return workspace.SkillRepo
	}
	return workspace.SkillRepo + "/" + path
}

// skillCheckScript exits 3 when the volume at /t is not complete, and 4 when
// $1 is no folder of the repository, or reaches one through a symlink.
const skillCheckScript = `test -f /t/complete || exit 3
p=/t/` + workspace.SkillRepo + `${1:+/$1}
test -d "$p" && test "$(realpath "$p")" = "$p" || exit 4`

// Skills fills and tidies the volumes of skill packages (decision
// 2026-11-26-skill-packages in the app repository).
type Skills struct {
	State   string         // the host state folder: a lock per volume, whose time is its last use
	Exec    workspace.Exec // runs docker
	Log     *slog.Logger
	Project string // the project whose run fills, for labels and log lines
}

// lockFile is the lock of volume, whose modification time is its last use.
func (s *Skills) lockFile(volume string) string {
	return filepath.Join(s.State, "skills", volume+".lock")
}

// Fill returns the volume of the package at url and commit, filled first
// unless it is complete, behind a file lock so that projects fill it once
// and share it. It fails when path is not a folder of the repository, and
// records the use for Tidy. url, commit and path are checked by the caller.
func (s *Skills) Fill(ctx context.Context, url, commit, path string) (string, error) {
	vol := SkillVolume(url, commit)
	if err := os.MkdirAll(filepath.Join(s.State, "skills"), 0o700); err != nil {
		return "", err
	}
	unlock, err := lock(s.lockFile(vol))
	if err != nil {
		return "", err
	}
	defer unlock()
	// Under the lock, so Tidy never removes a volume between this use and its record.
	now := time.Now()
	if err := os.Chtimes(s.lockFile(vol), now, now); err != nil {
		return "", err
	}
	err = s.check(ctx, vol, path)
	if exit := (interface{ ExitCode() int })(nil); errors.As(err, &exit) && exit.ExitCode() == 3 {
		// An interrupted fill left no marker: fill it anew, in an empty volume.
		for _, argv := range [][]string{{"docker", "volume", "rm", "--force", vol}, {"docker", "volume", "create", vol}} {
			if _, err := s.Exec(ctx, nil, argv...); err != nil {
				return "", err
			}
		}
		if err := workspace.FetchSkill(ctx, s.Exec, s.Log, s.Project, vol, url, commit); err != nil {
			return "", err
		}
		err = s.check(ctx, vol, path)
	}
	if exit := (interface{ ExitCode() int })(nil); errors.As(err, &exit) && exit.ExitCode() == 4 {
		return "", fmt.Errorf("%q is not a folder of the repository", path)
	}
	if err != nil {
		return "", err
	}
	return vol, nil
}

// check runs skillCheckScript on vol, read-only.
func (s *Skills) check(ctx context.Context, vol, path string) error {
	_, err := s.Exec(ctx, nil, "docker", "run", "--rm", "--network", "none", "-v", vol+":/t:ro",
		"--entrypoint", "sh", workspace.GitImage, "-c", skillCheckScript, "sh", path)
	return err
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
