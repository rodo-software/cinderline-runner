package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Clients are the agent CLIs of the tools volume, as a team's agent names
// its client.
var Clients = []string{"claude", "codex", "gemini", "opencode"}

// Versions are the agent CLIs of a tools volume, each with the hashes its
// vendor publishes. The runner resolves them from the vendors' release
// channels and makes them live once they pass a check (CLR-317, decision
// 2026-11-27-agent-cli-updates); Node and ripgrep stay in pins.go.
type Versions struct {
	Claude       string            `json:"claude"`
	ClaudeSHA256 map[string]string `json:"claude_sha256"` // by platform: linux-x64, linux-arm64, each also -musl
	OpenCode     string            `json:"opencode"`
	OpenCodeSRI  map[string]string `json:"opencode_sri"` // by platform
	Codex        string            `json:"codex"`
	CodexSHA256  map[string]string `json:"codex_sha256"` // by arch: amd64, arm64
	// CodexHostSHA256 is per arch, the code-mode host of the same release.
	CodexHostSHA256 map[string]string `json:"codex_host_sha256"`
	Gemini          string            `json:"gemini"`
	GeminiSRI       string            `json:"gemini_sri"`
}

// Version is v's version of client; "" for another name.
func (v Versions) Version(client string) string {
	return map[string]string{"claude": v.Claude, "opencode": v.OpenCode, "codex": v.Codex, "gemini": v.Gemini}[client]
}

// String names each client's version, for log lines.
func (v Versions) String() string {
	return fmt.Sprintf("claude %s, codex %s, gemini %s, opencode %s", v.Claude, v.Codex, v.Gemini, v.OpenCode)
}

// same says a and b fill the same volumes. fmt prints maps sorted.
func same(a, b Versions) bool { return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b) }

// with is v with old's version of each client in clients.
func (v Versions) with(old Versions, clients []string) Versions {
	for _, c := range clients {
		switch c {
		case "claude":
			v.Claude, v.ClaudeSHA256 = old.Claude, old.ClaudeSHA256
		case "opencode":
			v.OpenCode, v.OpenCodeSRI = old.OpenCode, old.OpenCodeSRI
		case "codex":
			v.Codex, v.CodexSHA256, v.CodexHostSHA256 = old.Codex, old.CodexSHA256, old.CodexHostSHA256
		case "gemini":
			v.Gemini, v.GeminiSRI = old.Gemini, old.GeminiSRI
		}
	}
	return v
}

var (
	// VersionPat is a version the runner accepts from a vendor or a pin:
	// it becomes part of a URL.
	VersionPat = regexp.MustCompile(`^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(-[0-9A-Za-z.]{1,40})?$`)
	sha256Pat  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sriPat     = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
)

var (
	platforms    = []string{"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl"}
	codexTargets = map[string]string{"amd64": "x86_64-unknown-linux-musl", "arm64": "aarch64-unknown-linux-musl"}
)

// valid checks every version and hash, so a vendor's odd answer or a
// damaged state file never reaches a URL or a volume.
func (v Versions) valid() error {
	for _, c := range Clients {
		if !VersionPat.MatchString(v.Version(c)) {
			return fmt.Errorf("%s: bad version %q", c, v.Version(c))
		}
	}
	for _, p := range platforms {
		if !sha256Pat.MatchString(v.ClaudeSHA256[p]) || !sriPat.MatchString(v.OpenCodeSRI[p]) {
			return fmt.Errorf("no hash of claude or opencode for %s", p)
		}
	}
	for arch := range codexTargets {
		if !sha256Pat.MatchString(v.CodexSHA256[arch]) || !sha256Pat.MatchString(v.CodexHostSHA256[arch]) {
			return fmt.Errorf("no hash of codex for %s", arch)
		}
	}
	if !sriPat.MatchString(v.GeminiSRI) {
		return errors.New("no hash of gemini")
	}
	return nil
}

// The vendors' release channels; variables so tests can serve them.
var (
	claudeReleases = "https://downloads.claude.ai/claude-code-releases"
	npmRegistry    = "https://registry.npmjs.org"
	githubAPI      = "https://api.github.com"
)

// Resolve asks each vendor for its latest release, or the version pins
// holds for the client, and the hashes it publishes for it: Claude Code's
// stable channel (Willem, CLR-317) and the release's manifest.json, npm's
// latest dist-tag and dist.integrity for OpenCode and Gemini CLI, and
// Codex's latest GitHub release with its assets' digests.
func Resolve(ctx context.Context, hc *http.Client, pins map[string]string) (Versions, error) {
	for c, p := range pins {
		if !slices.Contains(Clients, c) || !VersionPat.MatchString(p) {
			return Versions{}, fmt.Errorf("bad pin %s %q", c, p)
		}
	}
	var v Versions
	var err error
	if v.Claude, v.ClaudeSHA256, err = resolveClaude(ctx, hc, pins["claude"]); err != nil {
		return Versions{}, fmt.Errorf("claude: %w", err)
	}
	v.OpenCodeSRI = map[string]string{}
	ver := cmp.Or(pins["opencode"], "latest")
	for _, p := range platforms {
		var n npmVersion
		if err := getJSON(ctx, hc, npmRegistry+"/opencode-"+p+"/"+ver, &n); err != nil {
			return Versions{}, fmt.Errorf("opencode: %w", err)
		}
		// The first platform's latest names the version of all four.
		if ver == "latest" {
			if !VersionPat.MatchString(n.Version) {
				return Versions{}, fmt.Errorf("opencode: bad version %q", n.Version)
			}
			ver = n.Version
		}
		if n.Version != ver {
			return Versions{}, fmt.Errorf("opencode-%s is %q, not %s", p, n.Version, ver)
		}
		v.OpenCodeSRI[p] = n.Dist.Integrity
	}
	v.OpenCode = ver
	if v.Codex, v.CodexSHA256, v.CodexHostSHA256, err = resolveCodex(ctx, hc, pins["codex"]); err != nil {
		return Versions{}, fmt.Errorf("codex: %w", err)
	}
	var g npmVersion
	if err := getJSON(ctx, hc, npmRegistry+"/@google/gemini-cli/"+cmp.Or(pins["gemini"], "latest"), &g); err != nil {
		return Versions{}, fmt.Errorf("gemini: %w", err)
	}
	if pins["gemini"] != "" && g.Version != pins["gemini"] {
		return Versions{}, fmt.Errorf("gemini: got %q, not %s", g.Version, pins["gemini"])
	}
	v.Gemini, v.GeminiSRI = g.Version, g.Dist.Integrity
	return v, v.valid()
}

// npmVersion is the part of an npm registry version document Resolve reads.
type npmVersion struct {
	Version string `json:"version"`
	Dist    struct {
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func resolveClaude(ctx context.Context, hc *http.Client, pin string) (string, map[string]string, error) {
	ver := pin
	if ver == "" {
		b, err := get(ctx, hc, claudeReleases+"/stable")
		if err != nil {
			return "", nil, err
		}
		ver = strings.TrimSpace(string(b))
	}
	if !VersionPat.MatchString(ver) {
		return "", nil, fmt.Errorf("bad version %q", ver)
	}
	var m struct {
		Version   string `json:"version"`
		Platforms map[string]struct {
			Checksum string `json:"checksum"`
		} `json:"platforms"`
	}
	if err := getJSON(ctx, hc, claudeReleases+"/"+ver+"/manifest.json", &m); err != nil {
		return "", nil, err
	}
	if m.Version != ver {
		return "", nil, fmt.Errorf("manifest of %s is for %q", ver, m.Version)
	}
	sums := map[string]string{}
	for _, p := range platforms {
		sums[p] = m.Platforms[p].Checksum
	}
	return ver, sums, nil
}

func resolveCodex(ctx context.Context, hc *http.Client, pin string) (string, map[string]string, map[string]string, error) {
	url := githubAPI + "/repos/openai/codex/releases/latest"
	if pin != "" {
		url = githubAPI + "/repos/openai/codex/releases/tags/rust-v" + pin
	}
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, hc, url, &r); err != nil {
		return "", nil, nil, err
	}
	ver, ok := strings.CutPrefix(r.Tag, "rust-v")
	if !ok || !VersionPat.MatchString(ver) || pin != "" && ver != pin {
		return "", nil, nil, fmt.Errorf("unexpected release %q", r.Tag)
	}
	digest := func(asset string) string {
		for _, a := range r.Assets {
			if a.Name == asset {
				return strings.TrimPrefix(a.Digest, "sha256:")
			}
		}
		return ""
	}
	bin, host := map[string]string{}, map[string]string{}
	for arch, target := range codexTargets {
		bin[arch] = digest("codex-" + target + ".tar.gz")
		host[arch] = digest("codex-code-mode-host-" + target + ".tar.gz")
	}
	return ver, bin, host, nil
}

// maxReleaseBody is the most of a vendor's answer Resolve reads; Codex's
// release lists some hundred assets.
const maxReleaseBody = 8 << 20

func get(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxReleaseBody))
}

func getJSON(ctx context.Context, hc *http.Client, url string, v any) error {
	b, err := get(ctx, hc, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

// live is the state file of the agent CLIs new runs take, and the ones
// before, kept for a rollback.
type live struct {
	Current  *Versions `json:"current"`
	Previous *Versions `json:"previous,omitempty"`
}

func liveFile(state string) string { return filepath.Join(state, "tools", "versions.json") }

// loadLive reads the state file; a missing or damaged one is empty.
func loadLive(state string) live {
	var l live
	b, err := os.ReadFile(liveFile(state))
	if err != nil || json.Unmarshal(b, &l) != nil || l.Current == nil || l.Current.valid() != nil {
		return live{}
	}
	if l.Previous != nil && l.Previous.valid() != nil {
		l.Previous = nil
	}
	return l
}

// Current is the agent CLIs new runs take: the last ones that passed their
// check, else pins.go's.
func Current(state string) Versions {
	if l := loadLive(state); l.Current != nil {
		return *l.Current
	}
	return Pinned()
}

// saveLive replaces the state file at once, so a reader never sees half.
func saveLive(state string, l live) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(liveFile(state)), 0o700); err != nil {
		return err
	}
	tmp := liveFile(state) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, liveFile(state))
}

// updateEvery is how often the runner looks for new agent CLIs after the
// first time (CLR-317), as often as the client detection.
const updateEvery = time.Hour

// toolsUnused is how long a tools volume of neither the current nor the
// previous versions is kept after its last mount. shortcut: a run holds its
// mount from its start, so one that runs longer than this after two
// updates loses its CLIs; track runs' volumes when runs get that long.
const toolsUnused = 24 * time.Hour

// toolsVolume matches the volumes set.volume names.
var toolsVolume = regexp.MustCompile("^" + name.Name + `-tools-[0-9a-f]{12}$`)

// Updater keeps the agent CLIs current: it resolves the vendors' releases,
// has Check try them and makes them live for the runs that start after.
type Updater struct {
	State string         // the host state folder
	Exec  workspace.Exec // runs docker
	HTTP  *http.Client   // asks the vendors
	Pins  map[string]string
	// Check fills the tools volume of v and returns the clients whose CLI in
	// it failed; an error means it could not check.
	Check func(ctx context.Context, v Versions) (failed []string, err error)
	// Switched is called after new versions went live; nil: none.
	Switched func(ctx context.Context)
	Log      *slog.Logger
}

// firstUpdate is how long after start the first update waits, so the
// projects' first detection and model lists, which Check compares with,
// are there. shortcut: a fixed wait; wait for the projects' first lists if
// a check at start keeps missing them.
const firstUpdate = 5 * time.Minute

// Run updates firstUpdate after start and then every updateEvery until ctx
// is done.
func (u *Updater) Run(ctx context.Context) {
	wait := time.NewTimer(firstUpdate)
	defer wait.Stop()
	select {
	case <-ctx.Done():
		return
	case <-wait.C:
	}
	t := time.NewTicker(updateEvery)
	defer t.Stop()
	for u.update(ctx); ; u.update(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// update makes the resolved versions live once they pass the check. A
// client that fails keeps its live version and is tried again at the next
// update; the others go live without it. Failures only log.
func (u *Updater) update(ctx context.Context) {
	l := loadLive(u.State)
	cur := Current(u.State)
	keep := []Versions{cur}
	if l.Previous != nil {
		keep = append(keep, *l.Previous)
	}
	defer func() { u.prune(ctx, keep) }()
	resolved, err := Resolve(ctx, u.HTTP, u.Pins)
	if err != nil {
		if ctx.Err() == nil {
			u.Log.Warn("agent CLI releases not resolved, keeping the live ones", "live", cur.String(), "err", err)
		}
		return
	}
	keep = append(keep, resolved) // its volume is filled: the next try need not download it again
	cand := resolved
	for !same(cand, cur) {
		failed, err := u.Check(ctx, cand)
		if err != nil {
			if ctx.Err() == nil {
				u.Log.Warn("agent CLI update not checked, keeping the live ones", "new", cand.String(), "err", err)
			}
			return
		}
		// A client the update leaves as it is did not fail by it.
		failed = slices.DeleteFunc(failed, func(c string) bool { return cand.Version(c) == cur.Version(c) })
		if len(failed) == 0 {
			if err := saveLive(u.State, live{Current: &cand, Previous: &cur}); err != nil {
				u.Log.Warn("agent CLI update not saved, keeping the live ones", "err", err)
				return
			}
			keep = []Versions{cand, cur, resolved}
			u.Log.Info("agent CLIs updated for the next runs", "live", cand.String(), "previous", cur.String())
			if u.Switched != nil {
				u.Switched(ctx)
			}
			return
		}
		for _, c := range failed {
			u.Log.Warn("agent CLI update failed its check, keeping the live version", "client", c, "new", cand.Version(c), "live", cur.Version(c))
		}
		cand = cand.with(cur, failed)
	}
}

// prune removes the tools volumes of sets other than keep's that no mount
// used for toolsUnused, each behind its lock, as Skills.Tidy does. A volume
// in use stays: no --force. Failures only log.
func (u *Updater) prune(ctx context.Context, keep []Versions) {
	kept := map[string]bool{}
	for _, v := range keep {
		for arch := range codexTargets {
			for _, musl := range []bool{false, true} {
				if s, err := v.set(arch, musl); err == nil {
					kept[s.volume()] = true
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(u.State, "tools"), 0o700); err != nil {
		u.Log.Warn("tools volumes not pruned", "err", err)
		return
	}
	out, err := u.Exec(ctx, nil, "docker", "volume", "ls", "--quiet", "--filter", "name="+name.Name+"-tools-")
	if err != nil {
		u.Log.Warn("tools volumes not listed", "err", err)
		return
	}
	for _, vol := range strings.Fields(out) {
		if !toolsVolume.MatchString(vol) || kept[vol] {
			continue
		}
		if err := u.remove(ctx, vol); err != nil {
			u.Log.Info("old tools volume not removed", "volume", vol, "err", err)
		}
	}
}

func (u *Updater) remove(ctx context.Context, vol string) error {
	unlock, err := lock(toolsLock(u.State, vol))
	if err != nil {
		return err
	}
	defer unlock()
	fi, err := os.Stat(toolsLock(u.State, vol))
	if err != nil || time.Since(fi.ModTime()) < toolsUnused {
		return err // used lately, or lock just made the record
	}
	if _, err := u.Exec(ctx, nil, "docker", "volume", "rm", vol); err != nil {
		return err
	}
	u.Log.Info("old tools volume removed", "volume", vol)
	return nil
}

// toolsLock is vol's lock file, whose time is its last mount.
func toolsLock(state, vol string) string { return filepath.Join(state, "tools", vol+".lock") }
