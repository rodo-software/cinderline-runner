// Package config loads and saves the projects on this machine and computes
// the per-machine paths of the config file, log, host state and local socket.
package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// Version is the config file version this build reads and writes.
const Version = 1

// maxSocketPath is sun_path minus its NUL on macOS (104), the smallest of the platforms.
const maxSocketPath = 103

// Config is the content of config.json. It holds no secrets: those live in the keychain.
type Config struct {
	Version int `json:"version"`
	// KeepSessionsFor is how long a finished run's session and clone are
	// kept for a follow-up after its last run, such as "7d" or "36h"; "off"
	// keeps them until min_free_disk or a closed issue needs them gone.
	KeepSessionsFor string `json:"keep_sessions_for,omitempty"`
	// MinFreeDisk is the free space below which kept sessions are deleted,
	// oldest first, such as "10GB"; "off" never deletes them for space.
	MinFreeDisk string `json:"min_free_disk,omitempty"`
	// Providers are the runner's model providers for OpenCode agents, for
	// every project; their optional keys are runner-wide secrets (decision
	// 2026-11-17-local-model-providers).
	Providers []Provider `json:"providers,omitempty"`
	// StrictHosts keeps Cinderline from opening hosts: the allow_hosts of
	// a step are ignored, and every poll says so. Runner-wide, off by
	// default (decision 2026-11-17-hosts-from-cinderline).
	StrictHosts bool `json:"strict_hosts,omitempty"`
	// AllowHosts are extra host:port pairs runs of every current and future
	// project on this runner may reach (CLR-212).
	AllowHosts []string `json:"allow_hosts,omitempty"`
	// AllowSkills are the skill packages runs of every current and future
	// project may load on a runner with strict_hosts on (CLR-257).
	AllowSkills []AllowedSkill `json:"allow_skills,omitempty"`
	// MaxParallelRuns is every project's max_parallel_runs unless it sets
	// its own, 1 to MaxParallel; 0 is 1 (decision 2026-11-25-parallel-runs-default).
	MaxParallelRuns int `json:"max_parallel_runs,omitempty"`
	// Pinned holds agent CLIs at a version, by client, for a vendor's
	// broken release; a client left out follows its vendor's latest (CLR-317).
	// The runner checks each pin when it resolves it.
	Pinned map[string]string `json:"pinned,omitempty"`
	// TranscriptDays is how many days a run's transcript is kept, 0 to
	// MaxTranscriptDays, unless a project sets its own; nil is
	// DefaultTranscriptDays. A pointer: 0 keeps nothing after the run ends.
	// TranscriptMaxSize is the most disk all transcripts take, written like
	// memory, at least MinTranscriptSize; empty is DefaultTranscriptSize
	// (decision 2026-11-26-local-web-page).
	TranscriptDays    *int   `json:"transcript_days,omitempty"`
	TranscriptMaxSize string `json:"transcript_max_size,omitempty"`
	// Web turns the daemon's local web page on or off: "on", "off" or ""
	// for webDefaultOn; WebPort is its port on 127.0.0.1, 0 for
	// DefaultWebPort (decision 2026-11-26-local-web-page).
	Web      string    `json:"web,omitempty"`
	WebPort  int       `json:"web_port,omitempty"`
	Projects []Project `json:"projects"`
	// Dropped names the fields an older runner wrote that Load dropped, for
	// a warning; the next Save leaves them out (CLR-191).
	Dropped []string `json:"-"`
}

// Defaults of keep_sessions_for and min_free_disk.
const (
	DefaultKeepSessionsFor = "7d"
	DefaultMinFreeDisk     = "10GB"
)

// webDefaultOn is whether the page is on when web is unset: on by default;
// `web off` turns it off (decision 2026-11-26-local-web-page).
const webDefaultOn = true

// DefaultWebPort is the page's port when web_port is unset.
const DefaultWebPort = 7420

// WebOn says whether the daemon serves the local web page.
func (c *Config) WebOn() bool {
	if c.Web == "" {
		return webDefaultOn
	}
	return c.Web == "on"
}

// WebAddr is the address the page listens on: loopback only.
func (c *Config) WebAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(cmp.Or(c.WebPort, DefaultWebPort)))
}

// KeepSessions is how long kept sessions live after their last run; 0 is off.
// Validate has checked the value.
func (c *Config) KeepSessions() time.Duration {
	d, _ := parseAge(cmp.Or(c.KeepSessionsFor, DefaultKeepSessionsFor))
	return d
}

// MinFree is the free disk space in bytes that pruning keeps; 0 is off.
// Validate has checked the value.
func (c *Config) MinFree() int64 {
	n, _ := parseSize(cmp.Or(c.MinFreeDisk, DefaultMinFreeDisk))
	return n
}

// parseAge reads "off", a number of days such as "7d", or a Go duration
// such as "36h".
func parseAge(s string) (time.Duration, error) {
	if s == "off" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, errors.New(`keep_sessions_for must be "off", days such as "7d" or hours such as "36h"`)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New(`keep_sessions_for must be "off", days such as "7d" or hours such as "36h"`)
	}
	return d, nil
}

// parseSize reads "off" or a whole number of MB, GB or TB (powers of 1000).
func parseSize(s string) (int64, error) {
	if s == "off" {
		return 0, nil
	}
	for unit, bytes := range map[string]int64{"MB": 1e6, "GB": 1e9, "TB": 1e12} {
		if num, ok := strings.CutSuffix(s, unit); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err == nil && n > 0 && n <= math.MaxInt64/bytes {
				return n * bytes, nil
			}
		}
	}
	return 0, errors.New(`min_free_disk must be "off" or a size such as "10GB", in MB, GB or TB`)
}

// DefaultMemory is the memory limit of an agent container whose project
// sets none, and MinMemory the least a project may set (decision
// 2026-11-24-agent-memory-limit).
const (
	DefaultMemory = "4g"
	MinMemory     = "256m"
)

// memorySize is a memory value as docker run --memory takes it, limited to
// whole MiB or GiB: one letter, so "GB" cannot be read as min_free_disk's 1000-based unit.
var memorySize = regexp.MustCompile(`^([1-9][0-9]{0,8})([mMgG])$`)

// ParseMemory reads a memory size such as "4g" or "512m", 1024-based as in
// docker run --memory, of at least MinMemory, in bytes.
func ParseMemory(s string) (int64, error) {
	return parseMiB("memory", s, MinMemory, DefaultMemory)
}

// parseMiB reads a size written like memory, of at least minimum, also
// written like memory, in bytes; field and example are for the error.
func parseMiB(field, s, minimum, example string) (int64, error) {
	n, ok := bytesOf(s)
	if !ok {
		return 0, fmt.Errorf("%s %q must be a whole number of MiB or GiB such as %q or %q, at least %s", field, s, example, "512m", minimum)
	}
	if lowest, _ := bytesOf(minimum); n < lowest {
		return 0, fmt.Errorf("%s %q is under the minimum of %s", field, s, minimum)
	}
	return n, nil
}

// bytesOf reads a size written like memory, in bytes.
func bytesOf(s string) (int64, bool) {
	m := memorySize.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.ParseInt(m[1], 10, 64) // at most 9 digits: no overflow below
	if strings.EqualFold(m[2], "g") {
		return n << 30, true
	}
	return n << 20, true
}

// Defaults and limits of transcript_days and transcript_max_size (decision
// 2026-11-26-local-web-page).
const (
	DefaultTranscriptDays = 14
	MaxTranscriptDays     = 365
	DefaultTranscriptSize = "1g"
	MinTranscriptSize     = "100m"
)

// ParseTranscriptSize reads a transcript_max_size such as "1g" or "512m",
// of at least MinTranscriptSize, in bytes.
func ParseTranscriptSize(s string) (int64, error) {
	return parseMiB("transcript_max_size", s, MinTranscriptSize, DefaultTranscriptSize)
}

// TranscriptDaysFor is how many days p's transcripts are kept: its own
// transcript_days, else the runner-wide one, else DefaultTranscriptDays.
func (c *Config) TranscriptDaysFor(p Project) int {
	switch {
	case p.TranscriptDays != nil:
		return *p.TranscriptDays
	case c.TranscriptDays != nil:
		return *c.TranscriptDays
	}
	return DefaultTranscriptDays
}

// TranscriptMax is the most bytes all transcripts take. Validate has
// checked the value.
func (c *Config) TranscriptMax() int64 {
	n, _ := ParseTranscriptSize(cmp.Or(c.TranscriptMaxSize, DefaultTranscriptSize))
	return n
}

// checkTranscriptDays reports a transcript_days out of range; nil is unset.
func checkTranscriptDays(days *int) string {
	if days != nil && (*days < 0 || *days > MaxTranscriptDays) {
		return fmt.Sprintf("transcript_days must be 0 to %d", MaxTranscriptDays)
	}
	return ""
}

// GiB writes bytes as GiB with one decimal, as the log and status show memory.
func GiB(bytes int64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64) + " GiB"
}

// MemoryLimit is the memory limit of the project's agent containers in
// bytes: Memory, or DefaultMemory. Validate has checked the value.
func (p Project) MemoryLimit() int64 {
	n, _ := ParseMemory(cmp.Or(p.Memory, DefaultMemory))
	return n
}

// MaxParallel is the most a max_parallel_runs may be, the contract's limit
// on max_parallel.
const MaxParallel = 10

// Parallel is the most team runs of the project at once: its own
// MaxParallelRuns, else runnerWide, the config's, else 1 (decision
// 2026-11-25-parallel-runs-default). Validate has checked both.
func (p Project) Parallel(runnerWide int) int { return cmp.Or(p.MaxParallelRuns, runnerWide, 1) }

// Project is one Cinderline project, identified by URL, Workspace and Key (CSR-97).
type Project struct {
	// LocalID is the local name ID returns. A key renamed in the app changes
	// nothing on this machine until `rename` moves it to the new key's name
	// (decision 2026-11-17-rename-moves-local-names).
	LocalID string `json:"id,omitempty"`
	URL     string `json:"url"`
	// Key is the project's key in the app: `rename` updates it, and LocalID with it.
	Key string `json:"key"`
	// Workspace is the slug of the project's workspace from GET project.
	Workspace  string `json:"workspace"`
	Repository string `json:"repository"`
	// OpenAIBaseURL is an OpenAI-compatible base URL for Codex; empty means OpenAI's API.
	OpenAIBaseURL string `json:"openai_base_url,omitempty"`
	// GeminiVertex says the Gemini key is a Vertex AI express-mode key; false means the Gemini API.
	GeminiVertex bool `json:"gemini_vertex,omitempty"`
	// AllowHosts are extra host:port pairs this project's runs may reach.
	// Only local config opens a host for every run; Cinderline opens one
	// for an issue's runs (decision 2026-11-17-hosts-from-cinderline).
	AllowHosts []string `json:"allow_hosts,omitempty"`
	// AllowSkills are the skill packages this project's runs may load on a
	// runner with strict_hosts on (CLR-257).
	AllowSkills []AllowedSkill `json:"allow_skills,omitempty"`
	// Memory is the memory limit of the project's agent containers, also
	// their swap limit, such as "8g" or "512m"; empty is DefaultMemory. A
	// mem_limit on the agent service of its compose file wins (decision
	// 2026-11-24-agent-memory-limit).
	Memory string `json:"memory,omitempty"`
	// MaxParallelRuns is the most team runs of the project at once, each
	// in its own slot, clone and branch, 1 to MaxParallel; 0 is the
	// config's MaxParallelRuns (see Parallel). The Project Manager's run is
	// not one of them (decision 2026-11-24-lanes in the app repository).
	MaxParallelRuns int `json:"max_parallel_runs,omitempty"`
	// TranscriptDays is the project's own transcript_days, which wins over
	// the config's; nil is the config's (see TranscriptDaysFor).
	TranscriptDays *int `json:"transcript_days,omitempty"`
	// ExtraRepositories are other repositories the project's runs may read,
	// mounted read-only (CSR-160). Configured only here, not in Cinderline
	// (app decision 2026-11-11-extra-repositories-in-runner).
	ExtraRepositories []ExtraRepository `json:"extra_repositories,omitempty"`
}

// Provider is an OpenAI-compatible model endpoint an OpenCode agent names
// as "<Name>/<model id>". Free says its models cost nothing per token, as
// whoever added it marked it (decision 2026-11-19-free-model-providers).
// MaxAgents is the most agent turns on it at once, over every project of
// the runner, 0 to MaxProviderAgents; 0 is no limit (CLR-243). Lookahead
// is how many runs past MaxAgents may be in play on it, 0 to
// MaxProviderAgents; 0, the default, is one run per place (CLR-253).
type Provider struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Free      bool   `json:"free,omitempty"`
	MaxAgents int    `json:"max_agents,omitempty"`
	Lookahead int    `json:"lookahead,omitempty"`
}

// MaxProviderAgents is the most a provider's max_agents, and its lookahead, may be.
const MaxProviderAgents = 100

// providerName is what a provider's name may be; the report caps names at 50.
var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)

// Validate reports a name that is not lower-case letters, digits and dashes
// or is openrouter, which OpenCode already has, a base URL that is not
// http(s)://host[:port][/path], without user info, query or fragment, a
// max_agents or lookahead out of range, or a lookahead without max_agents.
func (p Provider) Validate() error {
	if p.MaxAgents < 0 || p.MaxAgents > MaxProviderAgents {
		return fmt.Errorf("provider %s: max_agents must be 0 (no limit) to %d", p.Name, MaxProviderAgents)
	}
	if p.Lookahead < 0 || p.Lookahead > MaxProviderAgents {
		return fmt.Errorf("provider %s: lookahead must be 0 (none) to %d", p.Name, MaxProviderAgents)
	}
	if p.Lookahead > 0 && p.MaxAgents == 0 {
		return fmt.Errorf("provider %s: a lookahead needs max_agents: it is the runs past that limit", p.Name)
	}
	if !providerName.MatchString(p.Name) || p.Name == "openrouter" {
		return fmt.Errorf("provider name %q must be up to 50 lower-case letters, digits and dashes, and not openrouter", p.Name)
	}
	u, err := url.Parse(p.BaseURL)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.Opaque == "" &&
		!strings.ContainsAny(p.BaseURL, "?# \t\r\n") && (hostChars.MatchString(u.Hostname()) || net.ParseIP(u.Hostname()) != nil) &&
		(u.Port() == "" || ValidHostPort(net.JoinHostPort("h", u.Port()))) {
		return nil
	}
	s := p.BaseURL
	if err == nil && u.User != nil {
		u.User = nil // never a password in an error text
		s = u.String()
	}
	return fmt.Errorf("provider %s: base URL %q must be http or https, a host, an optional port and path such as /v1, without a user name or password", p.Name, s)
}

// DefaultWorkspace is the slug of a project on a Cinderline that sends no
// workspace (from before EMBER-159).
const DefaultWorkspace = "default"

// ID names the project's Docker volumes, containers, networks and labels,
// its keychain entries and its state files: LocalID, or Label for an entry
// that has none yet.
func (p Project) ID() string {
	return cmp.Or(p.LocalID, p.Label())
}

// Label is how the project is shown: "<workspace>-<KEY>", or the key alone
// in the default workspace (decision 2026-11-15-workspace-in-project-identity).
// It was the ID before CLR-191, and is still a new entry's ID.
func (p Project) Label() string {
	if p.Workspace == DefaultWorkspace {
		return p.Key
	}
	return p.Workspace + "-" + p.Key
}

// workspaceSlug is what a workspace slug may be: it ends up in Docker
// names and compose project names, which allow no more.
var workspaceSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ExtraRepository is one read-only repository of a project. Its git token is
// stored under its URL, so it only ever goes to that URL.
type ExtraRepository struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"` // a branch or tag; empty is the default branch
}

// Name is the repository's folder under the extra mount: the last segment
// of its URL's path without .git.
func (r ExtraRepository) Name() string {
	s := strings.TrimSuffix(strings.TrimSuffix(r.URL, "/"), ".git")
	return s[strings.LastIndex(s, "/")+1:]
}

// repoName is what Name must look like to be a folder.
var repoName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// Validate reports a URL outside the allowlist, a ref that is not a plain
// branch or tag name, or a name that is not a plain folder name.
func (r ExtraRepository) Validate() error {
	if err := CheckRepositoryURL(r.URL); err != nil {
		return err
	}
	return r.CheckRefAndName()
}

// CheckRefAndName is Validate without the URL check, for a linked project's
// own repository: config checked its URL as a repository, and its ref comes
// from Cinderline (CSR-161).
func (r ExtraRepository) CheckRefAndName() error {
	if r.Ref != "" && !validRef(r.Ref) {
		return fmt.Errorf("%q is not a plain branch or tag name", r.Ref)
	}
	if n := r.Name(); !repoName.MatchString(n) {
		return fmt.Errorf("%s ends in %q, which cannot be a folder name", r.URL, n)
	}
	return nil
}

// CheckRepositoryURL allows https://host/path only, without a user name or
// password, whitespace, query or fragment: git gets the token as an https
// password, so no other form can work, and credentials in a URL would
// travel in docker's arguments and in error texts. Used for the project's
// repository and for extra repositories.
func CheckRepositoryURL(s string) error {
	u, err := url.Parse(s)
	if err == nil && u.Scheme == "https" && u.User == nil && u.Host != "" && strings.Trim(u.Path, "/") != "" &&
		strings.IndexFunc(s, unicode.IsSpace) < 0 && !strings.ContainsAny(s, "?#") {
		return nil
	}
	if err == nil && u.User != nil {
		u.User = nil // never a password in an error text
		s = u.String()
	}
	return fmt.Errorf("%q must be an https URL such as https://host/owner/repository, without a user name or password: git signs in with the token over https only", s)
}

// CheckInstanceURL allows an absolute https URL of a Cinderline instance, or
// plain http on localhost or a loopback address for local development: the
// runner token, the agents' scoped tokens and every prompt travel over it
// (decision 2026-11-19-https-instance). Its error leaves the URL out, which
// may hold a password.
func CheckInstanceURL(s string) error {
	u, err := url.Parse(s)
	if err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return nil
	}
	return errors.New("must be an absolute https URL; plain http only on localhost or a loopback address, for local development")
}

// validRef is a subset of git check-ref-format: plain names such as main,
// release/1.2 or v1.0.0, never an option, a range or a reflog expression.
func validRef(s string) bool {
	return refChars.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock") && s != "HEAD"
}

var refChars = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// Paths are the per-machine paths. Computing them creates nothing on disk.
type Paths struct {
	Dir    string // config folder
	File   string // config.json
	Log    string // the daemon's rotating log file
	State  string // host state folder
	Socket string // local API socket
}

// DefaultPaths computes the paths for this user. It fails when the socket
// path is longer than a Unix socket allows.
func DefaultPaths() (Paths, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	var run string
	switch runtime.GOOS {
	case "darwin":
		// $TMPDIR is a per-user 0700 folder under /var/folders.
		run = os.TempDir()
	case "windows":
		// os.Chmod on Windows only sets the read-only bit, so a 0700 folder protects
		// nothing; the local API relies on %LocalAppData%'s per-user default ACL instead.
		run, err = os.UserCacheDir()
	default:
		if run = os.Getenv("XDG_RUNTIME_DIR"); run == "" {
			run, err = os.UserCacheDir()
		}
	}
	if err != nil {
		return Paths{}, err
	}
	dir := filepath.Join(cfgDir, name.Name)
	p := Paths{
		Dir:    dir,
		File:   filepath.Join(dir, "config.json"),
		Log:    filepath.Join(dir, "state", name.Name+".log"), // in the state folder (decision 2026-11-05-runner-state-location)
		State:  filepath.Join(dir, "state"),
		Socket: filepath.Join(run, name.Name, "sock"),
	}
	if len(p.Socket) > maxSocketPath {
		return Paths{}, fmt.Errorf("socket path %s is %d bytes, over the %d a Unix socket allows", p.Socket, len(p.Socket), maxSocketPath)
	}
	return p, nil
}

// Load reads and validates the config file at path. A missing file gives an
// error that matches fs.ErrNotExist.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var old oldConfig
	if err := dec.Decode(&old); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg := old.Config
	cfg.Projects = make([]Project, len(old.Projects))
	cfg.Dropped = old.dropped()
	for i, op := range old.Projects {
		cfg.Projects[i] = op.Project
		if op.NamesByKey && op.LocalID == "" {
			cfg.Projects[i].LocalID = op.Key // its names were the key alone
		}
		cfg.Projects[i].LocalID = cfg.Projects[i].ID() // pinned before anyone edits the key
	}
	if cfg.Version != Version {
		return nil, fmt.Errorf("%s: version %d is not supported by this build (wants %d); update %s", path, cfg.Version, Version, name.Name)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// oldConfig is Config with the fields an older runner wrote: Load drops them
// instead of refusing the file (CLR-191). Any other unknown field is still
// refused, so a typo does not pass silently.
type oldConfig struct {
	Config
	MergePollSeconds json.RawMessage `json:"merge_poll_seconds"`
	BuildImage       json.RawMessage `json:"build_image"`
	Projects         []oldProject    `json:"projects"`
}

type oldProject struct {
	Project
	NamesByKey    bool            `json:"names_by_key"`
	AllowExisting json.RawMessage `json:"allow_existing"`
}

// dropped names the old fields the file held, once each.
func (c oldConfig) dropped() []string {
	var out []string
	add := func(name string, present bool) {
		if present && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	add("merge_poll_seconds", c.MergePollSeconds != nil)
	add("build_image", c.BuildImage != nil)
	for _, p := range c.Projects {
		add("names_by_key", p.NamesByKey)
		add("allow_existing", p.AllowExisting != nil)
	}
	return out
}

// Validate reports an invalid limit or the first invalid project. Zero
// projects is valid.
func (c *Config) Validate() error {
	if _, err := parseAge(cmp.Or(c.KeepSessionsFor, DefaultKeepSessionsFor)); err != nil {
		return err
	}
	if _, err := parseSize(cmp.Or(c.MinFreeDisk, DefaultMinFreeDisk)); err != nil {
		return err
	}
	if problem := checkProviders(c.Providers); problem != "" {
		return errors.New(problem)
	}
	if problem := checkAllowHosts(c.AllowHosts); problem != "" {
		return errors.New(problem)
	}
	if problem := checkParallel(c.MaxParallelRuns); problem != "" {
		return errors.New(problem)
	}
	if problem := checkAllowSkills(c.AllowSkills); problem != "" {
		return errors.New(problem)
	}
	if problem := checkTranscriptDays(c.TranscriptDays); problem != "" {
		return errors.New(problem)
	}
	if _, err := ParseTranscriptSize(cmp.Or(c.TranscriptMaxSize, DefaultTranscriptSize)); err != nil {
		return err
	}
	if c.Web != "" && c.Web != "on" && c.Web != "off" {
		return fmt.Errorf("web %q must be on or off", c.Web)
	}
	if c.WebPort != 0 && (c.WebPort < 1024 || c.WebPort > 65535) {
		return fmt.Errorf("web_port %d must be 1024 to 65535", c.WebPort)
	}
	// A provider on the runner's own machine opens its port to agents, so
	// it must never be the page's, whether the page is on or not (CLR-273).
	webPort := strconv.Itoa(cmp.Or(c.WebPort, DefaultWebPort))
	for _, p := range c.Providers {
		if u, _ := url.Parse(p.BaseURL); runnersMachine(u.Host) && u.Port() == webPort {
			return fmt.Errorf("provider %s: base URL is the runner's own machine on port %s, the web page's port, which agents must never reach; move the provider or the page (`%s web port N`)", p.Name, webPort, name.Name)
		}
	}
	type id struct{ url, workspace, key string }
	seen := map[id]bool{}
	for i, p := range c.Projects {
		var problem string
		if err := CheckInstanceURL(p.URL); err != nil {
			problem = "url " + err.Error()
		} else if p.Key == "" || strings.IndexFunc(p.Key, unicode.IsSpace) >= 0 {
			problem = "key must be non-empty without whitespace"
		} else if strings.IndexFunc(p.LocalID, unicode.IsSpace) >= 0 {
			problem = "id must be without whitespace"
		} else if err := CheckRepositoryURL(p.Repository); err != nil {
			problem = "repository " + err.Error()
		} else if !workspaceSlug.MatchString(p.Workspace) {
			problem = "workspace must be letters, digits, - and _"
		} else if seen[id{p.URL, p.Workspace, p.Key}] {
			problem = "another project has the same url, workspace and key"
		} else if errMsg := checkAllowHosts(p.AllowHosts); errMsg != "" {
			problem = errMsg
		} else if _, err := ParseMemory(cmp.Or(p.Memory, DefaultMemory)); err != nil {
			problem = err.Error()
		} else if errMsg := checkParallel(p.MaxParallelRuns); errMsg != "" {
			problem = errMsg
		} else if errMsg := checkAllowSkills(p.AllowSkills); errMsg != "" {
			problem = errMsg
		} else if errMsg := checkTranscriptDays(p.TranscriptDays); errMsg != "" {
			problem = errMsg
		} else {
			problem = checkExtraRepositories(p.ExtraRepositories)
		}
		if problem != "" {
			return fmt.Errorf("project %d (%s): %s", i+1, p.Key, problem)
		}
		seen[id{p.URL, p.Workspace, p.Key}] = true
	}
	return nil
}

// checkAllowHosts reports the first invalid allow_hosts entry; "" means none.
func checkAllowHosts(allowHosts []string) string {
	if i := slices.IndexFunc(allowHosts, runnersMachine); i >= 0 {
		return fmt.Sprintf("allow_hosts entry %q is the runner's own machine, which allow_hosts never opens; add a model provider instead with `%s add-provider NAME URL`", allowHosts[i], name.Name)
	}
	if i := slices.IndexFunc(allowHosts, func(hp string) bool { return !ValidHostPort(hp) }); i >= 0 {
		return fmt.Sprintf("allow_hosts entry %q must be host:port, without wildcards", allowHosts[i])
	}
	return ""
}

// AllowedSkill is a skill package a runner with strict_hosts on may load:
// by its name and its repository's URL, so a catalog entry that later points
// elsewhere is refused again (decision 2026-11-26-skill-packages).
type AllowedSkill struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// skillName is a skill package's name as the app's catalog has it: lower-case
// letters and digits in groups joined by single hyphens.
var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidSkillName says s can be a skill package's name, at most 64 characters.
func ValidSkillName(s string) bool { return len(s) <= 64 && skillName.MatchString(s) }

// CheckSkillURL allows a skill package's repository: an https URL as
// CheckRepositoryURL allows it, of at most 500 characters. Its error leaves
// the URL out, which may hold a password.
func CheckSkillURL(s string) error {
	if len(s) <= 500 && CheckRepositoryURL(s) == nil {
		return nil
	}
	return errors.New("a skill package's URL must be https://host/path of a public repository, without user info, query or fragment, at most 500 characters")
}

// checkAllowSkills reports the first invalid allow_skills entry or a name
// listed twice; "" means none.
func checkAllowSkills(skills []AllowedSkill) string {
	names := map[string]bool{}
	for _, s := range skills {
		if !ValidSkillName(s.Name) {
			return fmt.Sprintf("allow_skills name %q must be lower-case letters and digits with single hyphens, at most 64 characters", s.Name)
		}
		if err := CheckSkillURL(s.URL); err != nil {
			return "allow_skills " + s.Name + ": " + err.Error()
		}
		if names[s.Name] {
			return "allow_skills names " + s.Name + " twice"
		}
		names[s.Name] = true
	}
	return ""
}

// checkParallel reports a max_parallel_runs out of range; 0, unset, is
// valid. "" means none.
func checkParallel(n int) string {
	if n < 0 || n > MaxParallel {
		return fmt.Sprintf("max_parallel_runs must be 1 to %d", MaxParallel)
	}
	return ""
}

// checkExtraRepositories reports the first invalid extra repository or two
// with one folder name; "" means none.
func checkExtraRepositories(rs []ExtraRepository) string {
	names := map[string]bool{}
	for _, r := range rs {
		if err := r.Validate(); err != nil {
			return "extra repository: " + err.Error()
		}
		if names[r.Name()] {
			return "two extra repositories are named " + r.Name()
		}
		names[r.Name()] = true
	}
	return ""
}

// checkProviders reports the first invalid provider or two with one name; "" means none.
func checkProviders(ps []Provider) string {
	names := map[string]bool{}
	for _, p := range ps {
		if err := p.Validate(); err != nil {
			return err.Error()
		}
		if names[p.Name] {
			return "two providers are named " + p.Name
		}
		names[p.Name] = true
	}
	return ""
}

// runnersMachine says hp names the runner's own machine: localhost, a
// loopback address or host.docker.internal (decision 2026-11-17-local-model-providers).
func runnersMachine(hp string) bool {
	host, _, err := net.SplitHostPort(hp)
	if err != nil {
		return false
	}
	return loopbackHost(host) || strings.EqualFold(host, "host.docker.internal")
}

// loopbackHost says host is localhost or a loopback address.
func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

// AllowedHost says hp may join a run's allowlist as an allow_hosts entry
// would: host:port, and not the runner's own machine.
func AllowedHost(hp string) bool { return ValidHostPort(hp) && !runnersMachine(hp) }

// hostChars are what an allow_hosts host may hold: a DNS name or an IPv4
// address. The egress proxy matches it exactly.
var hostChars = regexp.MustCompile(`(?i)^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// ValidHostPort says hp is a host and a port in 1-65535, as the egress proxy
// matches it.
func ValidHostPort(hp string) bool {
	host, port, err := net.SplitHostPort(hp)
	n, perr := strconv.Atoi(port)
	return err == nil && hostChars.MatchString(host) && perr == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port
}

// Save validates c, sets its version and writes it to path atomically.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.Version = Version
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(data, '\n'))
}

// WriteFile writes data to path atomically, readable by this user only: a
// crash leaves the old file or the new one, never a torn one.
func WriteFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*") // created 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// SameRepository reports whether a and b are https URLs of the same
// repository: host and path compared case-insensitively, without a port,
// a trailing / or .git. Any other form is never the same as anything.
func SameRepository(a, b string) bool {
	key := func(s string) string {
		if CheckRepositoryURL(s) != nil {
			return ""
		}
		u, _ := url.Parse(s)
		path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
		return strings.ToLower(u.Hostname() + path)
	}
	ka := key(a)
	return ka != "" && ka == key(b)
}
