package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
)

const parallelUsage = "show|set N|clear [" + projectUsage + "]"

// parallel shows, sets or clears max_parallel_runs in config.json: the
// runner-wide one, or with --project that project's own, which wins over it,
// and reloads a running runner after a change (decision
// 2026-11-25-parallel-runs-default).
func (c *CLI) parallel(args []string) int {
	// N comes before the flags, as memory's size does: flag parsing stops at it.
	nargs := 0
	if len(args) > 0 && args[0] == "set" {
		nargs = 1
	}
	if len(args) < 1+nargs || nargs == 0 && args[0] != "show" && args[0] != "clear" || nargs == 1 && strings.HasPrefix(args[1], "-") {
		c.flags("parallel", parallelUsage).Usage()
		return 2
	}
	verb := args[0]
	f := c.flags("parallel", parallelUsage)
	project := f.String("project", "", "project key (omit for the runner-wide value)")
	instance, ws := projectFlags(f)
	if !c.parsed(f, args[1+nargs:], 0) {
		return 2
	}
	if *project == "" && (*instance != "" || *ws != "") {
		f.Usage()
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	which := manage.Project{Key: *project, URL: *instance, Workspace: *ws}
	var p *config.Project
	if *project != "" {
		found, err := manage.FindByKey(cfg, *project, *instance, *ws)
		if err != nil {
			return c.fail(err)
		}
		p = &cfg.Projects[slices.IndexFunc(cfg.Projects, func(q config.Project) bool {
			return q.URL == found.URL && q.Workspace == found.Workspace && q.Key == found.Key
		})]
	}
	value := &cfg.MaxParallelRuns
	if p != nil {
		value = &p.MaxParallelRuns
	}
	switch verb {
	case "show":
		if p != nil {
			fmt.Fprintf(c.Stdout, "%s runs up to %s at once, %s\n", p.Label(), teamRuns(p.Parallel(cfg.MaxParallelRuns)), parallelSource(cfg, *p))
			return 0
		}
		def := ""
		if cfg.MaxParallelRuns == 0 {
			def = ", the default"
		}
		fmt.Fprintf(c.Stdout, "Runner-wide: up to %s at once per project%s\n", teamRuns(max(cfg.MaxParallelRuns, 1)), def)
		for _, q := range cfg.Projects {
			fmt.Fprintf(c.Stdout, "  %s: %d, %s\n", q.Label(), q.Parallel(cfg.MaxParallelRuns), parallelSource(cfg, q))
		}
		return 0
	case "set":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 { // 0 would clear; SetParallel checks the top
			return c.fail(fmt.Errorf("max_parallel_runs %q must be 1 to %d", args[1], config.MaxParallel))
		}
		*value = n
	default:
		if *value == 0 {
			if p != nil {
				fmt.Fprintf(c.Stdout, "Project %s has no max_parallel_runs of its own; nothing changed\n", p.Label())
			} else {
				fmt.Fprintln(c.Stdout, "No runner-wide max_parallel_runs; nothing changed")
			}
			return 0
		}
		*value = 0 // unset: the runner-wide value, or 1
	}
	if err := manage.SetParallel(c.Paths, which, *value); err != nil {
		return c.fail(err)
	}
	// Decision 2026-11-24-rolling-reload: a loop takes its new slots once it has drained.
	if p != nil {
		fmt.Fprintf(c.Stdout, "%s runs up to %s at once, %s, once its runs in flight have finished\n", p.Label(), teamRuns(p.Parallel(cfg.MaxParallelRuns)), parallelSource(cfg, *p))
	} else {
		def := ""
		if cfg.MaxParallelRuns == 0 {
			def = ", the default"
		}
		var own []string
		for _, q := range cfg.Projects {
			if q.MaxParallelRuns != 0 {
				own = append(own, q.Label())
			}
		}
		fmt.Fprintf(c.Stdout, "Projects without their own max_parallel_runs run up to %s at once%s, each once its runs in flight have finished", teamRuns(max(cfg.MaxParallelRuns, 1)), def)
		switch len(own) {
		case 0:
		case 1:
			fmt.Fprintf(c.Stdout, "; %s keeps its own", own[0])
		default:
			fmt.Fprintf(c.Stdout, "; %s keep their own", strings.Join(own, ", "))
		}
		fmt.Fprintln(c.Stdout)
	}
	c.reload()
	return 0
}

// parallelSource says where p's max_parallel_runs comes from.
func parallelSource(cfg *config.Config, p config.Project) string {
	switch {
	case p.MaxParallelRuns != 0:
		return "its own max_parallel_runs"
	case cfg.MaxParallelRuns != 0:
		return "the runner-wide value"
	}
	return "the default"
}

// teamRuns is "1 team run" or "N team runs".
func teamRuns(n int) string {
	if n == 1 {
		return "1 team run"
	}
	return strconv.Itoa(n) + " team runs"
}
