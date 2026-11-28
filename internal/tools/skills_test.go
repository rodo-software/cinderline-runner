package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
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
	installs []map[string]string // per fill, each download's path: its mode and content
	arch     string              // the engine's
	contents string              // what the check prints for a complete volume
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
		return d.contents, nil
	case strings.Contains(cmd, "git -C /t/repo fetch"):
		// Slow, so a second project's fill would overlap it.
		time.Sleep(20 * time.Millisecond)
		vol := strings.TrimSuffix(argv[slices.Index(argv, "-v")+1], ":/t")
		d.fetches = append(d.fetches, vol)
		if d.fetchErr != nil {
			return "", d.fetchErr
		}
	case strings.Contains(cmd, skillInstallScript):
		vol := strings.TrimSuffix(argv[slices.Index(argv, "-v")+1], ":/t")
		staging := strings.TrimSuffix(argv[slices.Index(argv, "-v")+3], ":/s:ro")
		files := map[string]string{}
		for i, rest := 0, argv[slices.Index(argv, skillInstallScript)+3:]; i < len(rest); i += 2 {
			b, err := os.ReadFile(filepath.Join(staging, fmt.Sprint(i/2)))
			if err != nil {
				return "", err
			}
			files[rest[i]] = rest[i+1] + " " + string(b)
		}
		d.installs = append(d.installs, files)
		d.complete[vol] = true
	case cmd == "docker version --format {{.Server.Arch}}":
		return d.arch + "\n", nil
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
			v, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "plugin", nil)
			if err != nil {
				t.Error(err)
			}
			vols[i] = v
		})
	}
	wg.Wait()
	want := SkillVolume(ponyURL, ponyCommit, "", nil)
	if vols[0] != want || vols[1] != want || len(d.fetches) != 1 || d.fetches[0] != want {
		t.Fatalf("volumes %q, fetches %q; want one fetch into %s", vols, d.fetches, want)
	}
	s := &Skills{State: state, Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if _, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "", nil); err != nil || len(d.fetches) != 1 {
		t.Errorf("third use: %v, %d fetches", err, len(d.fetches))
	}
}

// A fetch that fails, such as one whose commit is not the one named, leaves
// no complete volume: the next use fetches again.
func TestSkillFailedFetchIsRedone(t *testing.T) {
	d := &skillDocker{fetchErr: errors.New("fetched commit ffff, not 0123: refused")}
	s := &Skills{State: t.TempDir(), Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if _, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "", nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err %v", err)
	}
	d.fetchErr = nil
	if _, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "", nil); err != nil || len(d.fetches) != 2 {
		t.Errorf("second use: %v, %d fetches", err, len(d.fetches))
	}
}

// A path that is not a folder of the repository fails the package, without
// a new fetch.
func TestSkillPathMustBeAFolder(t *testing.T) {
	d := &skillDocker{}
	s := &Skills{State: t.TempDir(), Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	if _, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "plugin", nil); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("err %v", err)
	}
	if len(d.fetches) != 1 {
		t.Errorf("%d fetches", len(d.fetches))
	}
}

// Fill says what the package's root holds, from the check's output, both
// when it fetched the package and when it only checked it.
func TestSkillFillReturnsContents(t *testing.T) {
	d := &skillDocker{contents: "skills/a\ngemini-extension.json\n.opencode/plugins/index.js\n"}
	s := &Skills{State: t.TempDir(), Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	want := SkillContents{Skills: []string{"a"}, Extension: true, Plugins: []string{".opencode/plugins/index.js"}}
	for range 2 {
		if _, c, err := s.Fill(context.Background(), ponyURL, ponyCommit, "", nil); err != nil || !reflect.DeepEqual(c, want) {
			t.Errorf("contents %+v, %v; want %+v", c, err, want)
		}
	}
	if len(d.fetches) != 1 {
		t.Errorf("%d fetches", len(d.fetches))
	}
}

// The check lists, in the package's root, only the skill folders with a
// SKILL.md, the Gemini manifest and the OpenCode plugin files OpenCode
// itself would load, each by a name of safe characters and never through a
// symlink; parseContents keeps only what it expects.
func TestSkillCheckScriptListsContents(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir()) // macOS's /var is a symlink: the script compares realpaths
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "repo", "plugin")
	for _, f := range []string{
		"skills/good/SKILL.md", "skills/also.good_1/SKILL.md", "skills/bad name/SKILL.md", "skills/.hidden/SKILL.md",
		"skills/bad,dst=x/SKILL.md", "skills/none/README.md", "skills/SKILL.md", "gemini-extension.json",
		".opencode/plugins/index.js", ".opencode/plugin/p.ts", ".opencode/plugins/helper.cjs", ".opencode/plugins/x.mjs",
		"outside/SKILL.md",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", strings.ReplaceAll(skillCheckScript, "/t", dir), "sh", "plugin").Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := SkillContents{Skills: []string{"also.good_1", "good"}, Extension: true, Plugins: []string{".opencode/plugin/p.ts", ".opencode/plugins/index.js"}}
	if got := parseContents(string(out)); !reflect.DeepEqual(got, want) {
		t.Errorf("contents %+v from\n%s\nwant %+v", got, out, want)
	}
	odd := "skills/a/b\nskills/../x\n/etc\ngemini-extension.json.bak\n.opencode/plugins/../../x.js\nskills/" + strings.Repeat("a", 101) + "\n"
	if got := parseContents(odd); !reflect.DeepEqual(got, SkillContents{}) {
		t.Errorf("odd lines parsed as %+v", got)
	}
}

func TestSkillVolume(t *testing.T) {
	v := SkillVolume(ponyURL, ponyCommit, "", nil)
	if !skillVolume.MatchString(v) || v == SkillVolume(ponyURL+"x", ponyCommit, "", nil) || v == SkillVolume(ponyURL, strings.Repeat("f", 40), "", nil) {
		t.Errorf("volume %s", v)
	}
}

// Tidy removes only the volumes no run used for 30 days; one without a
// record gets one and stays, one in use stays.
func TestSkillTidy(t *testing.T) {
	state := t.TempDir()
	vol := func(n int) string { return SkillVolume(ponyURL, strings.Repeat(fmt.Sprint(n), 40), "", nil) }
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

// A package's downloads for the engine's arch are fetched once, checked by
// hash and installed at their path and mode before the volume is complete;
// the other arch's are not fetched. Downloads name a volume of their own.
func TestSkillDownloads(t *testing.T) {
	hits := map[string]int{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		fmt.Fprint(w, "engine "+r.URL.Path)
	}))
	defer srv.Close()
	downloads := []cinderline.SkillDownload{
		{Arch: "amd64", Path: "bin/linux-x64/e", URL: srv.URL + "/x64", SHA256: sha256Hex([]byte("engine /x64")), Executable: true},
		{Arch: "arm64", Path: "bin/linux-arm64/e", URL: srv.URL + "/arm64", SHA256: sha256Hex([]byte("engine /arm64")), Executable: true},
		{Arch: "arm64", Path: "data.json", URL: srv.URL + "/data", SHA256: sha256Hex([]byte("engine /data"))},
	}
	d := &skillDocker{folders: []string{"plugin"}, arch: "arm64"}
	s := &Skills{State: t.TempDir(), Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	for range 2 {
		vol, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "plugin", downloads)
		if err != nil || vol != SkillVolume(ponyURL, ponyCommit, "plugin", downloads) {
			t.Fatalf("volume %s, %v", vol, err)
		}
	}
	want := []map[string]string{{"bin/linux-arm64/e": "755 engine /arm64", "data.json": "644 engine /data"}}
	if !reflect.DeepEqual(d.installs, want) || !reflect.DeepEqual(hits, map[string]int{"/arm64": 1, "/data": 1}) {
		t.Errorf("installs %q, hits %v; want %q once each", d.installs, hits, want)
	}
	plain := SkillVolume(ponyURL, ponyCommit, "plugin", nil)
	if plain == SkillVolume(ponyURL, ponyCommit, "plugin", downloads) || plain != SkillVolume(ponyURL, ponyCommit, "", nil) ||
		SkillVolume(ponyURL, ponyCommit, "plugin", downloads) == SkillVolume(ponyURL, ponyCommit, "", downloads) ||
		SkillVolume(ponyURL, ponyCommit, "plugin", downloads) == SkillVolume(ponyURL, ponyCommit, "plugin", downloads[1:]) {
		t.Error("the downloads and, with them, the root must name the volume")
	}
}

// A download that does not have its hash fails the package before its
// volume is touched, and nothing of it is kept.
func TestSkillDownloadHashMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "tampered") }))
	defer srv.Close()
	d := &skillDocker{arch: "amd64"}
	state := t.TempDir()
	s := &Skills{State: state, Exec: d.exec, Log: slog.New(slog.DiscardHandler), Project: "CSR"}
	downloads := []cinderline.SkillDownload{{Arch: "amd64", Path: "e", URL: srv.URL + "/e", SHA256: sha256Hex([]byte("engine"))}}
	if _, _, err := s.Fill(context.Background(), ponyURL, ponyCommit, "", downloads); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("err %v", err)
	}
	vol := SkillVolume(ponyURL, ponyCommit, "", downloads)
	if _, ok := d.complete[vol]; ok || len(d.fetches) != 0 || len(d.installs) != 0 {
		t.Errorf("volume touched: %v, fetches %q, installs %q", d.complete, d.fetches, d.installs)
	}
	if _, err := os.Stat(filepath.Join(state, "skills", vol)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging kept: %v", err)
	}
}

// A body over the limit is refused and not kept, even with the right hash.
func TestDownloadLimit(t *testing.T) {
	body := []byte("0123456789x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "f")
	if err := download(context.Background(), http.DefaultClient, srv.URL, dst, sha256Hex(body), 10); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file kept: %v", err)
	}
	if err := download(context.Background(), http.DefaultClient, srv.URL, dst, sha256Hex(body), 11); err != nil {
		t.Errorf("at the limit: %v", err)
	}
}

// The install script writes each download at its path in the package's
// root with its mode and marks the volume complete; a path reached through
// a symlink, or one that is a symlink, is refused without the marker.
func TestSkillInstallScript(t *testing.T) {
	run := func(t *testing.T, setup func(root, outside string), args ...string) (string, string, error) {
		dir, err := filepath.EvalSymlinks(t.TempDir()) // macOS's /var is a symlink: the script compares realpaths
		if err != nil {
			t.Fatal(err)
		}
		tdir, sdir, outside := filepath.Join(dir, "t"), filepath.Join(dir, "s"), filepath.Join(dir, "outside")
		root := filepath.Join(tdir, "repo", "plugin")
		for _, p := range []string{root, sdir, outside} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for i := range len(args) / 2 {
			if err := os.WriteFile(filepath.Join(sdir, fmt.Sprint(i)), []byte(fmt.Sprint("file", i)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if setup != nil {
			setup(root, outside)
		}
		script := strings.NewReplacer("/t/", tdir+"/", "/s/", sdir+"/").Replace(skillInstallScript)
		out, err := exec.Command("sh", append([]string{"-c", script, "sh", "plugin"}, args...)...).CombinedOutput()
		return tdir, string(out), err
	}
	tdir, out, err := run(t, nil, "bin/linux-x64/e", "755", "data.json", "644")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for f, mode := range map[string]os.FileMode{"bin/linux-x64/e": 0o755, "data.json": 0o644} {
		fi, err := os.Stat(filepath.Join(tdir, "repo", "plugin", f))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v", f, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tdir, "complete")); err != nil {
		t.Error("no complete marker")
	}

	for name, setup := range map[string]func(root, outside string){
		"folder through a symlink": func(root, outside string) { os.Symlink(outside, filepath.Join(root, "bin")) },
		"a symlink itself":         func(root, outside string) { os.Symlink(filepath.Join(outside, "e"), filepath.Join(root, "e")) },
		"a folder":                 func(root, _ string) { os.Mkdir(filepath.Join(root, "e"), 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			path := "e"
			if strings.HasPrefix(name, "folder") {
				path = "bin/e"
			}
			tdir, out, err := run(t, setup, path, "755")
			if err == nil || !strings.Contains(out, "refused") {
				t.Errorf("%v: %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(tdir, "complete")); err == nil {
				t.Error("complete after a refusal")
			}
			if _, err := os.Stat(filepath.Join(tdir, "..", "outside", "e")); err == nil {
				t.Error("written outside the root")
			}
		})
	}
}
