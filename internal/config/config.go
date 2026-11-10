// Package config loads and saves the projects on this machine and computes
// the per-machine paths of the config file, log, host state and local socket.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
	BuildImage bool      `json:"build_image,omitempty"`
	Projects   []Project `json:"projects"`
}

// MergePoll is the merge check interval, 60 seconds unless set.
func (c *Config) MergePoll() time.Duration {
	if c.MergePollSeconds > 0 {
		return time.Duration(c.MergePollSeconds) * time.Second
	}
	return 60 * time.Second
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
}

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
		}
		if problem != "" {
			return fmt.Errorf("project %d (%s): %s", i+1, p.Key, problem)
		}
		seen[id{p.URL, p.Key}] = true
	}
	return nil
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
