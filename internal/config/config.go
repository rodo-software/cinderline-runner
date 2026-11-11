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
	// MergePollSeconds is how often the merge status of trusted Review
	// issues is checked, legacy's MERGE_POLL_SECONDS; 0 is its default.
	MergePollSeconds int `json:"merge_poll_seconds,omitempty"`
	// BuildImage makes the runner build the default image locally instead of
	// pulling the published one (decision on CSR-49).
	BuildImage bool `json:"build_image,omitempty"`
	// KeepSessionsFor is how long a finished run's session and clone are
	// kept for a follow-up after its last run, such as "7d" or "36h"; "off"
	// keeps them until min_free_disk or a closed issue needs them gone.
	KeepSessionsFor string `json:"keep_sessions_for,omitempty"`
	// MinFreeDisk is the free space below which kept sessions are deleted,
	// oldest first, such as "10GB"; "off" never deletes them for space.
	MinFreeDisk string    `json:"min_free_disk,omitempty"`
	Projects    []Project `json:"projects"`
}

// MergePoll is the merge check interval, 60 seconds unless set.
func (c *Config) MergePoll() time.Duration {
	if c.MergePollSeconds > 0 {
		return time.Duration(c.MergePollSeconds) * time.Second
	}
	return 60 * time.Second
}

// Defaults of keep_sessions_for and min_free_disk.
const (
	DefaultKeepSessionsFor = "7d"
	DefaultMinFreeDisk     = "10GB"
)

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

// Project is one Cinderline project, identified by the pair (URL, Key).
type Project struct {
	URL           string `json:"url"`
	Key           string `json:"key"`
	Repository    string `json:"repository"`
	AllowExisting bool   `json:"allow_existing,omitempty"`
	// OpenAIBaseURL is an OpenAI-compatible base URL for Codex; empty means OpenAI's API.
	OpenAIBaseURL string `json:"openai_base_url,omitempty"`
	// GeminiVertex says the Gemini key is a Vertex AI express-mode key; false means the Gemini API.
	GeminiVertex bool `json:"gemini_vertex,omitempty"`
	// AllowHosts are extra host:port pairs this project's runs may reach.
	// Only local config extends the allowlist (decision 2026-11-09-network-allowlist).
	AllowHosts []string `json:"allow_hosts,omitempty"`
	// ExtraRepositories are other repositories the project's runs may read,
	// mounted read-only (CSR-160). Configured only here, not in Cinderline
	// (Ember decision 2026-11-11-extra-repositories-in-runner).
	ExtraRepositories []ExtraRepository `json:"extra_repositories,omitempty"`
}

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
	return s[strings.LastIndexAny(s, "/:")+1:]
}

// repoName is what Name must look like to be a folder.
var repoName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// Validate reports a URL outside the allowlist, a ref that is not a plain
// branch or tag name, or a name that is not a plain folder name.
func (r ExtraRepository) Validate() error {
	if err := CheckRepositoryURL(r.URL); err != nil {
		return err
	}
	if r.Ref != "" && !validRef(r.Ref) {
		return fmt.Errorf("%q is not a plain branch or tag name", r.Ref)
	}
	if n := r.Name(); !repoName.MatchString(n) {
		return fmt.Errorf("%s ends in %q, which cannot be a folder name", r.URL, n)
	}
	return nil
}

// CheckRepositoryURL allows https://host/path, ssh://git@host/path and
// git@host:path, without credentials, whitespace, query or fragment.
func CheckRepositoryURL(s string) error {
	bad := fmt.Errorf("%q must be https://host/path, ssh://git@host/path or git@host:path, without credentials", s)
	if strings.IndexFunc(s, unicode.IsSpace) >= 0 || strings.ContainsAny(s, "?#") {
		return bad
	}
	if rest, ok := strings.CutPrefix(s, "git@"); ok && !strings.Contains(s, "://") {
		host, path, ok := strings.Cut(rest, ":")
		if !ok || !hostChars.MatchString(host) || strings.Trim(path, "/") == "" || strings.HasPrefix(path, "-") {
			return bad
		}
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return bad
	}
	switch {
	case u.Scheme == "https" && u.User == nil:
	case u.Scheme == "ssh" && u.User != nil && u.User.String() == "git":
	default:
		return bad
	}
	return nil
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
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Version != Version {
		return nil, fmt.Errorf("%s: version %d is not supported by this build (wants %d); update %s", path, cfg.Version, Version, name.Name)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Validate reports a negative merge_poll_seconds or the first invalid
// project. Zero projects is valid.
func (c *Config) Validate() error {
	if c.MergePollSeconds < 0 {
		return errors.New("merge_poll_seconds must not be negative")
	}
	if _, err := parseAge(cmp.Or(c.KeepSessionsFor, DefaultKeepSessionsFor)); err != nil {
		return err
	}
	if _, err := parseSize(cmp.Or(c.MinFreeDisk, DefaultMinFreeDisk)); err != nil {
		return err
	}
	type id struct{ url, key string }
	seen := map[id]bool{}
	for i, p := range c.Projects {
		var problem string
		if u, err := url.Parse(p.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			problem = "url must be an absolute http or https URL"
		} else if p.Key == "" || strings.IndexFunc(p.Key, unicode.IsSpace) >= 0 {
			problem = "key must be non-empty without whitespace"
		} else if u, err := url.Parse(p.Repository); err != nil || !u.IsAbs() || u.Host == "" {
			problem = "repository must be an absolute URL with a host"
		} else if seen[id{p.URL, p.Key}] {
			problem = "another project has the same url and key"
		} else if i := slices.IndexFunc(p.AllowHosts, func(hp string) bool { return !validHostPort(hp) }); i >= 0 {
			problem = fmt.Sprintf("allow_hosts entry %q must be host:port, without wildcards", p.AllowHosts[i])
		} else {
			problem = checkExtraRepositories(p.ExtraRepositories)
		}
		if problem != "" {
			return fmt.Errorf("project %d (%s): %s", i+1, p.Key, problem)
		}
		seen[id{p.URL, p.Key}] = true
	}
	return nil
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

// hostChars are what an allow_hosts host may hold: a DNS name or an IPv4
// address. The egress proxy matches it exactly.
var hostChars = regexp.MustCompile(`(?i)^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// validHostPort says hp is a host and a port in 1-65535.
func validHostPort(hp string) bool {
	host, port, err := net.SplitHostPort(hp)
	n, perr := strconv.Atoi(port)
	return err == nil && hostChars.MatchString(host) && perr == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port
}

// Save validates c, sets its version and writes it to path atomically.
func (c *Config) Save(path string) (err error) {
	if err := c.Validate(); err != nil {
		return err
	}
	c.Version = Version
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*.json") // created 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// The rename makes the write atomic: a crash leaves the old file or the new one, never a torn one.
	return os.Rename(f.Name(), path)
}

// SameRepository reports whether a and b name the same repository: host and
// path compared case-insensitively without user info, port, a trailing / or
// .git, so ssh://git@host/o/r.git and git@host:o/r are https://host/o/r. An
// empty or unparsable URL is never the same as anything.
func SameRepository(a, b string) bool {
	key := func(s string) string {
		if i := strings.Index(s, ":"); i > 0 && !strings.Contains(s, "://") {
			s = "ssh://" + s[:i] + "/" + s[i+1:] // scp form: git@host:o/r
		}
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return ""
		}
		path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
		return strings.ToLower(u.Hostname() + path)
	}
	ka := key(a)
	return ka != "" && ka == key(b)
}
