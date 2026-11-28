package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
)

const transcriptsUsage = "[" + projectUsage + "] [DAYS|clear] | --max-size SIZE"

// transcripts shows or sets how long and how much the runner keeps of its
// runs: transcript_days, runner-wide or with --project that project's own,
// which wins, and the runner-wide transcript_max_size, in config.json; it
// reloads a running runner after a change (decision 2026-11-26-local-web-page).
func (c *CLI) transcripts(args []string) int {
	f := c.flags("transcripts", transcriptsUsage)
	project := f.String("project", "", "project key (omit for the runner-wide value)")
	instance, ws := projectFlags(f)
	maxSize := f.String("max-size", "", "the most disk all transcripts take, runner-wide, such as 512m or 2g, at least "+config.MinTranscriptSize)
	if err := f.Parse(args); err != nil {
		return 2 // Parse printed the usage
	}
	if *maxSize != "" && *project != "" {
		fmt.Fprintln(c.Stderr, "error: --max-size is runner-wide only; leave out --project")
		return 2
	}
	if f.NArg() > 1 || *maxSize != "" && f.NArg() != 0 || *project == "" && (*instance != "" || *ws != "") {
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
	days := &cfg.TranscriptDays
	if p != nil {
		days = &p.TranscriptDays
	}
	switch value := f.Arg(0); {
	case *maxSize != "":
		if err := manage.SetTranscriptMaxSize(c.Paths, *maxSize); err != nil {
			return c.fail(err)
		}
		cfg.TranscriptMaxSize = strings.ToLower(*maxSize)
		c.showTranscripts(cfg, p)
		c.reload()
		return 0
	case value == "":
		c.showTranscripts(cfg, p)
		return 0
	case value == "clear":
		if *days == nil {
			fmt.Fprintln(c.Stdout, "No transcript_days to clear; nothing changed")
			return 0
		}
		*days = nil
	default:
		n, err := strconv.Atoi(value)
		if err != nil {
			return c.fail(fmt.Errorf("transcript_days %q must be 0 to %d", value, config.MaxTranscriptDays))
		}
		*days = &n
	}
	if err := manage.SetTranscriptDays(c.Paths, which, *days); err != nil {
		return c.fail(err)
	}
	c.showTranscripts(cfg, p)
	c.reload()
	return 0
}

// showTranscripts prints p's transcript_days or, for nil, the runner-wide
// settings and each project's own transcript_days.
func (c *CLI) showTranscripts(cfg *config.Config, p *config.Project) {
	if p != nil {
		fmt.Fprintf(c.Stdout, "Runs of %s are kept %s, %s\n", p.Label(), keptFor(cfg.TranscriptDaysFor(*p)), transcriptSource(cfg, *p))
		return
	}
	def := func(unset bool) string {
		if unset {
			return ", the default"
		}
		return ""
	}
	fmt.Fprintf(c.Stdout, "Runs are kept %s%s\n", keptFor(cfg.TranscriptDaysFor(config.Project{})), def(cfg.TranscriptDays == nil))
	fmt.Fprintf(c.Stdout, "Transcripts take at most %s of disk in all%s\n", cmp.Or(cfg.TranscriptMaxSize, config.DefaultTranscriptSize), def(cfg.TranscriptMaxSize == ""))
	for _, q := range cfg.Projects {
		if q.TranscriptDays != nil {
			fmt.Fprintf(c.Stdout, "  %s: kept %s, its own transcript_days\n", q.Label(), keptFor(*q.TranscriptDays))
		}
	}
}

// transcriptSource says where p's transcript_days comes from.
func transcriptSource(cfg *config.Config, p config.Project) string {
	switch {
	case p.TranscriptDays != nil:
		return "its own transcript_days"
	case cfg.TranscriptDays != nil:
		return "the runner-wide transcript_days"
	}
	return "the default"
}

// keptFor says how long a run is kept for a transcript_days of n.
func keptFor(n int) string {
	switch n {
	case 0:
		return "only while they run"
	case 1:
		return "for 1 day"
	}
	return "for " + strconv.Itoa(n) + " days"
}
