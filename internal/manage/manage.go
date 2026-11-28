// Package manage holds the operations on the runner's config and secrets
// that both the CLI and the local web page call: one write path (decision
// 2026-11-26-local-web-page, Config management). Each loads config.json,
// changes it, validates it and saves it; reloading the daemon is the
// caller's.
package manage

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// Refused is a value the config cannot take: the request was wrong, not
// the runner.
type Refused struct{ Err error }

func (r Refused) Error() string { return r.Err.Error() }
func (r Refused) Unwrap() error { return r.Err }

// Load returns config.json, or an empty config when it does not exist yet.
func Load(paths config.Paths) (*config.Config, error) {
	cfg, err := config.Load(paths.File)
	if errors.Is(err, fs.ErrNotExist) {
		return &config.Config{}, nil
	}
	return cfg, err
}

// save writes cfg to config.json, or refuses it when it is not valid.
func save(paths config.Paths, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return Refused{err}
	}
	return cfg.Save(paths.File)
}

// SetStrictHosts turns strict host blocking on or off for the whole runner:
// when on, Cinderline cannot open a host for an issue's runs (decision
// 2026-11-17-hosts-from-cinderline).
func SetStrictHosts(paths config.Paths, on bool) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	cfg.StrictHosts = on
	return save(paths, cfg)
}

// SetTranscriptMaxSize sets the runner-wide transcript_max_size, such as
// "2g", or clears it with "" (decision 2026-11-26-local-web-page).
func SetTranscriptMaxSize(paths config.Paths, value string) error {
	size := strings.ToLower(value)
	if size != "" {
		if _, err := config.ParseTranscriptSize(size); err != nil {
			return Refused{err}
		}
	}
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	cfg.TranscriptMaxSize = size
	return save(paths, cfg)
}

// SetWeb turns the local web page on or off and sets its port, runner-wide;
// nil leaves either as it is (decision 2026-11-26-local-web-page).
func SetWeb(paths config.Paths, on *bool, port *int) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	if on != nil {
		cfg.Web = "off"
		if *on {
			cfg.Web = "on"
		}
	}
	if port != nil {
		cfg.WebPort = *port // save refuses a port out of range
	}
	return save(paths, cfg)
}

// Project names one project of the config, as the CLI's --project, -url and
// -workspace do and the routes' bodies with the same names: its key, and its
// URL or workspace when several projects share the key (CSR-97). An empty
// Key names none: the runner-wide value, where a setting has one. Not the
// key alone, as CLR-297 first had it: Willem chose the key with an optional
// URL and workspace, so the CLI keeps -url and -workspace and the page
// reaches a project whose key another shares.
type Project struct {
	Key       string `json:"project"`
	URL       string `json:"url,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// find returns the project of cfg that which names, or nil for none.
func find(cfg *config.Config, which Project) (*config.Project, error) {
	if which.Key == "" {
		if which.URL != "" || which.Workspace != "" {
			return nil, Refused{errors.New("url and workspace need a project")}
		}
		return nil, nil
	}
	p, err := FindByKey(cfg, which.Key, which.URL, which.Workspace)
	if err != nil {
		return nil, Refused{err}
	}
	return &cfg.Projects[slices.IndexFunc(cfg.Projects, func(q config.Project) bool {
		return q.URL == p.URL && q.Workspace == p.Workspace && q.Key == p.Key
	})], nil
}

// FindByKey returns the one project with key, at instance and in ws when
// those are not empty; a key in several places needs them.
func FindByKey(cfg *config.Config, key, instance, ws string) (config.Project, error) {
	want := strings.TrimRight(instance, "/")
	var matches []config.Project
	for _, p := range cfg.Projects {
		if p.Key == key && (want == "" || p.URL == want) && (ws == "" || p.Workspace == ws) {
			matches = append(matches, p)
		}
	}
	switch {
	case len(matches) == 0 && (want != "" || ws != ""):
		return config.Project{}, fmt.Errorf("no project %s at %s in workspace %s", key, cmp.Or(want, "any URL"), cmp.Or(ws, "any"))
	case len(matches) == 0:
		return config.Project{}, fmt.Errorf("no project %s", key)
	case len(matches) > 1:
		var places []string
		for _, p := range matches {
			places = append(places, Where(p))
		}
		return config.Project{}, fmt.Errorf("several projects have key %s; choose one with -url or -workspace: %s", key, strings.Join(places, ", "))
	}
	return matches[0], nil
}

// Where is p's URL and, outside the default one, its workspace: what tells
// apart two projects with one key (CSR-97).
func Where(p config.Project) string {
	if p.Workspace == config.DefaultWorkspace {
		return p.URL
	}
	return p.URL + " (workspace " + p.Workspace + ")"
}

// SetMemory sets the memory limit of a project's agent containers, such as
// "8g", or clears it with "" (decision 2026-11-24-agent-memory-limit).
func SetMemory(paths config.Paths, project Project, value string) error {
	if project.Key == "" {
		return Refused{errors.New("memory is set for a project; name one")}
	}
	size := strings.ToLower(value)
	if size != "" {
		if _, err := config.ParseMemory(size); err != nil {
			return Refused{err}
		}
	}
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	p, err := find(cfg, project)
	if err != nil {
		return err
	}
	p.Memory = size
	return save(paths, cfg)
}

// SetParallel sets max_parallel_runs, runner-wide or a project's own, which
// wins over it, or clears it with 0 (decision 2026-11-25-parallel-runs-default).
func SetParallel(paths config.Paths, project Project, n int) error {
	if n < 0 || n > config.MaxParallel {
		return Refused{fmt.Errorf("max_parallel_runs %q must be 1 to %d", strconv.Itoa(n), config.MaxParallel)}
	}
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	p, err := find(cfg, project)
	if err != nil {
		return err
	}
	if p != nil {
		p.MaxParallelRuns = n
	} else {
		cfg.MaxParallelRuns = n
	}
	return save(paths, cfg)
}

// SetTranscriptDays sets transcript_days, runner-wide or a project's own,
// which wins over it, or clears it with nil (decision 2026-11-26-local-web-page).
func SetTranscriptDays(paths config.Paths, project Project, days *int) error {
	if days != nil && (*days < 0 || *days > config.MaxTranscriptDays) {
		return Refused{fmt.Errorf("transcript_days %q must be 0 to %d", strconv.Itoa(*days), config.MaxTranscriptDays)}
	}
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	p, err := find(cfg, project)
	if err != nil {
		return err
	}
	if p != nil {
		p.TranscriptDays = days
	} else {
		cfg.TranscriptDays = days
	}
	return save(paths, cfg)
}
