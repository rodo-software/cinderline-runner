package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// vendors is a fake of the four vendors' release channels.
type vendors struct {
	claude, opencode, codex, gemini string // the latest of each
	mu                              sync.Mutex
	paths                           []string
}

func hex64(c string) string { return strings.Repeat(c, 64) }
func sri(c string) string   { return "sha512-" + strings.Repeat(c, 86) + "==" }

// serveVendors points the release channels at v for the test.
func serveVendors(t *testing.T, v *vendors) *http.Client {
	t.Helper()
	npm := func(w http.ResponseWriter, ver, latest string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": map[bool]string{true: latest, false: ver}[ver == "latest"], "dist": map[string]string{"integrity": sri("n")}})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.paths = append(v.paths, r.URL.Path)
		v.mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/claude/stable":
			_, _ = io.WriteString(w, v.claude+"\n")
		case strings.HasPrefix(p, "/claude/") && strings.HasSuffix(p, "/manifest.json"):
			ver := strings.TrimSuffix(strings.TrimPrefix(p, "/claude/"), "/manifest.json")
			pl := map[string]any{}
			for _, x := range platforms {
				pl[x] = map[string]string{"checksum": hex64("c")}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": ver, "platforms": pl})
		case strings.HasPrefix(p, "/npm/opencode-linux-"):
			npm(w, p[strings.LastIndex(p, "/")+1:], v.opencode)
		case strings.HasPrefix(p, "/npm/@google/gemini-cli/"):
			npm(w, p[strings.LastIndex(p, "/")+1:], v.gemini)
		case strings.HasPrefix(p, "/gh/repos/openai/codex/releases/"):
			tag := "rust-v" + v.codex
			if t, ok := strings.CutPrefix(p, "/gh/repos/openai/codex/releases/tags/"); ok {
				tag = t
			}
			var assets []map[string]string
			for _, target := range codexTargets {
				for _, n := range []string{"codex-", "codex-code-mode-host-"} {
					assets = append(assets, map[string]string{"name": n + target + ".tar.gz", "digest": "sha256:" + hex64("d")})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oc, on, og := claudeReleases, npmRegistry, githubAPI
	claudeReleases, npmRegistry, githubAPI = srv.URL+"/claude", srv.URL+"/npm", srv.URL+"/gh"
	t.Cleanup(func() { claudeReleases, npmRegistry, githubAPI = oc, on, og })
	return srv.Client()
}

func TestResolveLatest(t *testing.T) {
	v := &vendors{claude: "2.1.326", opencode: "1.19.6", codex: "0.172.0", gemini: "0.71.1"}
	got, err := Resolve(context.Background(), serveVendors(t, v), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Claude != "2.1.326" || got.OpenCode != "1.19.6" || got.Codex != "0.172.0" || got.Gemini != "0.71.1" {
		t.Fatalf("resolved %s", got)
	}
	if got.ClaudeSHA256["linux-arm64-musl"] != hex64("c") || got.OpenCodeSRI["linux-x64"] != sri("n") ||
		got.CodexSHA256["arm64"] != hex64("d") || got.CodexHostSHA256["amd64"] != hex64("d") || got.GeminiSRI != sri("n") {
		t.Fatalf("hashes %+v", got)
	}
	// Willem chose Claude Code's stable channel, not latest (CLR-317).
	if !slices.Contains(v.paths, "/claude/stable") || !slices.Contains(v.paths, "/gh/repos/openai/codex/releases/latest") {
		t.Fatalf("asked %v", v.paths)
	}
	s, err := got.set("arm64", true)
	if err != nil || !strings.Contains(s.ClaudeURL, "/2.1.326/linux-arm64-musl/claude") || !strings.Contains(s.CodexURL, "rust-v0.172.0/") ||
		!strings.HasSuffix(s.OpenCodeURL, "opencode-linux-arm64-musl-1.19.6.tgz") || !strings.HasSuffix(s.GeminiURL, "gemini-cli-0.71.1.tgz") {
		t.Fatalf("set %+v, %v", s, err)
	}
}

// A pin holds its client at that version, asked by version, not by channel.
func TestResolvePins(t *testing.T) {
	v := &vendors{claude: "2.1.326", opencode: "1.19.6", codex: "0.172.0", gemini: "0.71.1"}
	pins := map[string]string{"claude": "2.1.316", "opencode": "1.19.5", "codex": "0.167.0", "gemini": "0.67.0"}
	got, err := Resolve(context.Background(), serveVendors(t, v), pins)
	if err != nil {
		t.Fatal(err)
	}
	if got.Claude != "2.1.316" || got.OpenCode != "1.19.5" || got.Codex != "0.167.0" || got.Gemini != "0.67.0" {
		t.Fatalf("resolved %s", got)
	}
	for _, p := range v.paths {
		if strings.HasSuffix(p, "/stable") || strings.HasSuffix(p, "/latest") {
			t.Fatalf("asked a channel with every client pinned: %v", v.paths)
		}
	}
}

// What a vendor answers becomes part of a URL and a volume: an odd version
// or a missing hash is refused, and so is a bad pin.
func TestResolveRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    *vendors
		pins map[string]string
	}{
		{"claude version with a path", &vendors{claude: "../../x", opencode: "1.0.0", codex: "1.0.0", gemini: "1.0.0"}, nil},
		{"codex not a version", &vendors{claude: "1.0.0", opencode: "1.0.0", codex: "1.0.0?x=1", gemini: "1.0.0"}, nil},
		{"gemini empty", &vendors{claude: "1.0.0", opencode: "1.0.0", codex: "1.0.0", gemini: ""}, nil},
		{"pin of no client", &vendors{claude: "1.0.0", opencode: "1.0.0", codex: "1.0.0", gemini: "1.0.0"}, map[string]string{"cursor": "1.0.0"}},
		{"pin not a version", &vendors{claude: "1.0.0", opencode: "1.0.0", codex: "1.0.0", gemini: "1.0.0"}, map[string]string{"claude": "1.0.0/../x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Resolve(context.Background(), serveVendors(t, tc.v), tc.pins); err == nil {
				t.Fatalf("accepted %s", got)
			}
		})
	}
	// A manifest without a platform's hash.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stable" {
			_, _ = io.WriteString(w, "1.0.0")
			return
		}
		_, _ = io.WriteString(w, `{"version":"1.0.0","platforms":{"linux-x64":{"checksum":"`+hex64("c")+`"}}}`)
	}))
	defer srv.Close()
	old := claudeReleases
	claudeReleases = srv.URL
	defer func() { claudeReleases = old }()
	if _, _, err := resolveClaude(context.Background(), srv.Client(), ""); err != nil {
		t.Fatal(err) // the versions are checked whole by valid, below
	}
	v := Pinned()
	v.ClaudeSHA256 = map[string]string{"linux-x64": hex64("c")}
	if v.valid() == nil {
		t.Fatal("versions without every platform's hash accepted")
	}
}

// pins.go's versions are the fallback, valid on their own.
func TestPinnedValid(t *testing.T) {
	if err := Pinned().valid(); err != nil {
		t.Fatal(err)
	}
}

// Without a state file, or with a damaged one, new runs take pins.go's.
func TestCurrent(t *testing.T) {
	state := t.TempDir()
	if !same(Current(state), Pinned()) {
		t.Fatal("no state file: not the pinned versions")
	}
	v := Pinned()
	v.Claude = "2.1.326"
	p := Pinned()
	if err := saveLive(state, live{Current: &v, Previous: &p}); err != nil {
		t.Fatal(err)
	}
	if got := Current(state); got.Claude != "2.1.326" {
		t.Fatalf("current %s", got)
	}
	v.Claude = "x"
	_ = saveLive(state, live{Current: &v})
	if !same(Current(state), Pinned()) {
		t.Fatal("damaged state file used")
	}
	if err := os.WriteFile(liveFile(state), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !same(Current(state), Pinned()) {
		t.Fatal("unreadable state file used")
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// noVolumes is an Exec with no tools volumes to prune.
func noVolumes(context.Context, []string, ...string) (string, error) { return "", nil }

func TestUpdate(t *testing.T) {
	latest := func() *vendors {
		return &vendors{claude: "2.1.326", opencode: "1.19.6", codex: "0.172.0", gemini: "0.71.1"}
	}
	for _, tc := range []struct {
		name     string
		latest   *vendors
		failed   []string // by Check, for any candidate
		checkErr error
		want     string // the live versions after, "" for unchanged
		checks   int
	}{
		{"all pass", latest(), nil, nil, "claude 2.1.326, codex 0.172.0, gemini 0.71.1, opencode 1.19.6", 1},
		{"codex fails", latest(), []string{"codex"}, nil, "claude 2.1.326, codex " + CodexVersion + ", gemini 0.71.1, opencode 1.19.6", 2},
		{"every one fails", latest(), []string{"claude", "codex", "gemini", "opencode"}, nil, "", 1},
		{"check fails", latest(), nil, errors.New("no image"), "", 1},
		{"nothing new", &vendors{claude: ClaudeVersion, opencode: OpenCodeVersion, codex: CodexVersion, gemini: GeminiVersion}, nil, nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			hc := serveVendors(t, tc.latest)
			checks, switched := 0, 0
			u := &Updater{State: state, Exec: noVolumes, HTTP: hc, Log: discardLog(), Switched: func(context.Context) { switched++ },
				Check: func(_ context.Context, v Versions) ([]string, error) {
					checks++
					return tc.failed, tc.checkErr
				}}
			// The fake serves one hash per kind, so make the pinned set match it
			// where a client keeps its version.
			p := Pinned()
			p.ClaudeSHA256, p.OpenCodeSRI = latestHashes("c"), latestHashes("n")
			p.CodexSHA256 = map[string]string{"amd64": hex64("d"), "arm64": hex64("d")}
			p.CodexHostSHA256, p.GeminiSRI = p.CodexSHA256, sri("n")
			if err := saveLive(state, live{Current: &p}); err != nil {
				t.Fatal(err)
			}
			u.update(context.Background())
			l := loadLive(state)
			if checks != tc.checks {
				t.Errorf("%d checks, want %d", checks, tc.checks)
			}
			if tc.want == "" {
				if !same(*l.Current, p) || l.Previous != nil || switched != 0 {
					t.Fatalf("live %s, previous %v, switched %d; want unchanged", l.Current, l.Previous, switched)
				}
				return
			}
			if l.Current.String() != tc.want || l.Previous == nil || !same(*l.Previous, p) || switched != 1 {
				t.Fatalf("live %s, previous %v, switched %d; want %s", l.Current, l.Previous, switched, tc.want)
			}
		})
	}
}

func latestHashes(c string) map[string]string {
	m := map[string]string{}
	for _, p := range platforms {
		m[p] = map[string]string{"c": hex64("c"), "n": sri("n")}[c]
	}
	return m
}

// A vendor that does not answer keeps the live versions and checks nothing.
func TestUpdateOffline(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	old := claudeReleases
	claudeReleases = srv.URL
	defer func() { claudeReleases = old }()
	u := &Updater{State: t.TempDir(), Exec: noVolumes, HTTP: srv.Client(), Log: discardLog(),
		Check: func(context.Context, Versions) ([]string, error) { t.Fatal("checked"); return nil, nil }}
	u.update(context.Background())
	if _, err := os.Stat(liveFile(u.State)); err == nil {
		t.Fatal("state file written")
	}
}

// Pruning keeps the current and previous sets' volumes, any volume mounted
// within toolsUnused, and every volume that is not a tools set's.
func TestPrune(t *testing.T) {
	state := t.TempDir()
	cur, prev := Pinned(), Pinned()
	prev.Claude = "2.1.300"
	curSet, _ := cur.set("amd64", false)
	prevSet, _ := prev.set("arm64", true)
	old, recent := "cinderline-tools-0123456789ab", "cinderline-tools-ba9876543210"
	if err := os.MkdirAll(filepath.Join(state, "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	for vol, age := range map[string]time.Duration{old: 2 * toolsUnused, recent: time.Minute, curSet.volume(): 2 * toolsUnused, prevSet.volume(): 2 * toolsUnused} {
		f := toolsLock(state, vol)
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		_ = os.Chtimes(f, at, at)
	}
	var removed []string
	exec := func(_ context.Context, _ []string, argv ...string) (string, error) {
		switch strings.Join(argv[:3], " ") {
		case "docker volume ls":
			return strings.Join([]string{old, recent, curSet.volume(), prevSet.volume(), "cinderline-tools-state", "cinderline-tools-abc"}, "\n"), nil
		case "docker volume rm":
			removed = append(removed, argv[3])
		}
		return "", nil
	}
	u := &Updater{State: state, Exec: exec, Log: discardLog()}
	u.prune(context.Background(), []Versions{cur, prev})
	if !slices.Equal(removed, []string{old}) {
		t.Fatalf("removed %v, want only %s", removed, old)
	}
}

// Every mount records its use on the volume's lock, which pruning reads.
func TestMountRecordsUse(t *testing.T) {
	_, s := pinsServer(t, []byte("CLAUDE"), tgz(t, map[string]string{"package/bin/opencode": "OC"}))
	f := &fakeDocker{answers: map[string]string{"docker version --format": "arm64\n", "docker run --rm": "musl\n"}}
	state := t.TempDir()
	tl := &Tools{State: state, Exec: f.exec, pins: func(string, bool) (set, error) { return s, nil }}
	lockFile := toolsLock(state, s.volume())
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-2 * toolsUnused)
	_ = os.Chtimes(lockFile, at, at)
	if _, err := tl.Mount(context.Background(), "img"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(lockFile); err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("use not recorded: %v %v", fi, err)
	}
}
