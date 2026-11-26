package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// skillExit is a command that ran and exited with its code.
type skillExit int

func (e skillExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e skillExit) ExitCode() int { return int(e) }

// skillDocker keeps skill package volumes: which are complete, the folders of
// their repositories, the fetches and removals done, and in-use volumes.
type skillDocker struct {
	mu       sync.Mutex
	complete map[string]bool
	folders  []string // the folders every fetched repository has, as path
	fetches  []string // the volumes fetched into
	removed  []string
	inUse    []string
	fetchErr error
}

func (d *skillDocker) exec(_ context.Context, _ []string, argv ...string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.complete == nil {
		d.complete = map[string]bool{}
	}
	cmd := strings.Join(argv, " ")
	switch {
	case strings.HasPrefix(cmd, "docker volume ls"):
		var vols []string
		for v := range d.complete {
			vols = append(vols, v)
		}
		slices.Sort(vols)
		return strings.Join(vols, "\n") + "\n", nil
	case strings.HasPrefix(cmd, "docker volume rm --force "):
		delete(d.complete, argv[4])
	case strings.HasPrefix(cmd, "docker volume rm "):
		if slices.Contains(d.inUse, argv[3]) {
			return "", errors.New("volume is in use")
		}
		delete(d.complete, argv[3])
		d.removed = append(d.removed, argv[3])
	case strings.HasPrefix(cmd, "docker volume create "):
		d.complete[argv[3]] = false
	case strings.Contains(cmd, skillCheckScript):
		vol := strings.TrimSuffix(argv[slices.Index(argv, "-v")+1], ":/t:ro")
		if !d.complete[vol] {
			return "", skillExit(3)
		}
		if p := argv[len(argv)-1]; p != "" && !slices.Contains(d.folders, p) {
			return "", skillExit(4)
		}
	case strings.Contains(cmd, "git -C /t/repo fetch"):
		// Slow, so a second project's fill would overlap it.
		time.Sleep(20 * time.Millisecond)
		vol := strings.TrimSuffix(argv[slices.Index(argv, "-v")+1], ":/t")
		d.fetches = append(d.fetches, vol)
		if d.fetchErr != nil {
			return "", d.fetchErr
		}
		d.complete[vol] = true
	default:
		return "", fmt.Errorf("unexpected %q", argv)
	}
	return "", nil
}

const (
	ponyURL    = "https://github.com/o/ponytail"
	ponyCommit = "0123456789abcdef0123456789abcdef01234567"
)

// Two projects' runs that load one package at once fill its volume once and
// share it; a later run only checks it.
func TestSkillFilledOnceForTwoProjects(t *testing.T) {
	d := &skillDocker{folders: []string{"plugin"}}
	state := t.TempDir()
	vols := make([]string, 2)
	var wg sync.WaitGroup
	for i, proj := range []string{"CSR", "WEB"} {
		s := &Skills{State: state, Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: proj}
		wg.Go(func() {
			v, err := s.Fill(context.Background(), ponyURL, ponyCommit, "plugin")
			if err != nil {
				t.Error(err)
			}
			vols[i] = v
		})
	}
	wg.Wait()
	want := SkillVolume(ponyURL, ponyCommit)
	if vols[0] != want || vols[1] != want || len(d.fetches) != 1 || d.fetches[0] != want {
		t.Fatalf("volumes %q, fetches %q; want one fetch into %s", vols, d.fetches, want)
	}
	s := &Skills{State: state, Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if _, err := s.Fill(context.Background(), ponyURL, ponyCommit, ""); err != nil || len(d.fetches) != 1 {
		t.Errorf("third use: %v, %d fetches", err, len(d.fetches))
	}
}

// A fetch that fails, such as one whose commit is not the one named, leaves
// no complete volume: the next use fetches again.
func TestSkillFailedFetchIsRedone(t *testing.T) {
	d := &skillDocker{fetchErr: errors.New("fetched commit ffff, not 0123: refused")}
	s := &Skills{State: t.TempDir(), Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if _, err := s.Fill(context.Background(), ponyURL, ponyCommit, ""); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err %v", err)
	}
	d.fetchErr = nil
	if _, err := s.Fill(context.Background(), ponyURL, ponyCommit, ""); err != nil || len(d.fetches) != 2 {
		t.Errorf("second use: %v, %d fetches", err, len(d.fetches))
	}
}

// A path that is not a folder of the repository fails the package, without
// a new fetch.
func TestSkillPathMustBeAFolder(t *testing.T) {
	d := &skillDocker{}
	s := &Skills{State: t.TempDir(), Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "plugin"); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("err %v", err)
	}
	if len(d.fetches) != 1 {
		t.Errorf("%d fetches", len(d.fetches))
	}
}

func TestSkillVolumeAndRoot(t *testing.T) {
	v := SkillVolume(ponyURL, ponyCommit)
	if !skillVolume.MatchString(v) || v == SkillVolume(ponyURL+"x", ponyCommit) || v == SkillVolume(ponyURL, strings.Repeat("f", 40)) {
		t.Errorf("volume %s", v)
	}
	if SkillRoot("") != "repo" || SkillRoot("plugin") != "repo/plugin" {
		t.Errorf("roots %q %q", SkillRoot(""), SkillRoot("plugin"))
	}
}

// Tidy removes only the volumes no run used for 30 days; one without a
// record gets one and stays, one in use stays.
func TestSkillTidy(t *testing.T) {
	state := t.TempDir()
	vol := func(n int) string { return SkillVolume(ponyURL, strings.Repeat(fmt.Sprint(n), 40)) }
	d := &skillDocker{complete: map[string]bool{vol(1): true, vol(2): true, vol(3): true, vol(4): true, "cinderline-skill-other": true}, inUse: []string{vol(4)}}
	s := &Skills{State: state, Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if err := os.MkdirAll(state+"/skills", 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-SkillUnused - time.Hour)
	for n, at := range map[int]time.Time{1: time.Now().Add(-SkillUnused + time.Hour), 2: old, 4: old} {
		f := s.lockFile(vol(n))
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, at, at); err != nil {
			t.Fatal(err)
		}
	}
	s.Tidy(context.Background())
	if !slices.Equal(d.removed, []string{vol(2)}) {
		t.Errorf("removed %q, want only %s", d.removed, vol(2))
	}
	if fi, err := os.Stat(s.lockFile(vol(3))); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("volume without a record: %v", err)
	}
	// Kept until it has been unused for 30 days from now.
	s.Tidy(context.Background())
	if len(d.removed) != 1 {
		t.Errorf("second tidy removed %q", d.removed)
	}
}
