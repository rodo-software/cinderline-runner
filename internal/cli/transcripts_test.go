package cli

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// CLR-282, decision 2026-11-26-local-web-page: transcripts shows, sets and
// clears the runner-wide transcript_days or, with --project, a project's
// own, sets transcript_max_size runner-wide only, refuses values out of
// range, and reloads the runner after each change.
func TestTranscripts(t *testing.T) {
	c, out, _ := reloadCLI(t, "")
	errb := &strings.Builder{}
	c.Stderr = errb
	three := 3
	web := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "WEB", Repository: "https://git.example.com/o/web"}
	api := config.Project{Workspace: "default", URL: "https://a.example.com", Key: "API", Repository: "https://git.example.com/o/api", TranscriptDays: &three}
	saveConfig(t, c, web, api)
	f := serveFakeRunner(t, c, "", false)
	reloaded := func() bool {
		t.Helper()
		f.mu.Lock()
		defer f.mu.Unlock()
		r := f.reloaded
		f.reloaded = false
		return r
	}
	load := func() *config.Config {
		t.Helper()
		cfg, err := config.Load(c.Paths.File)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	run := func(args ...string) int {
		t.Helper()
		out.Reset()
		errb.Reset()
		return c.Run(append([]string{"transcripts"}, args...))
	}
	shows := func(args []string, want ...string) {
		t.Helper()
		if code := run(args...); code != 0 {
			t.Errorf("%q: code %d, stderr %q", args, code, errb.String())
		}
		for _, w := range want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%q: %q, want %q in it", args, out.String(), w)
			}
		}
	}

	shows(nil, "Runs are kept for 14 days, the default", "at most 1g of disk in all, the default", "API: kept for 3 days, its own transcript_days")
	if strings.Contains(out.String(), "WEB") {
		t.Errorf("show lists WEB, which has no transcript_days of its own: %q", out.String())
	}
	shows([]string{"--project", "WEB"}, "Runs of WEB are kept for 14 days, the default")
	shows([]string{"--project", "API"}, "Runs of API are kept for 3 days, its own transcript_days")

	if code := run("30"); code != 0 || !reloaded() || !strings.Contains(out.String(), "Runs are kept for 30 days\n") {
		t.Fatalf("set runner-wide: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if cfg := load(); cfg.TranscriptDays == nil || *cfg.TranscriptDays != 30 || cfg.Projects[0].TranscriptDays != nil || *cfg.Projects[1].TranscriptDays != 3 {
		t.Errorf("after set runner-wide: %+v", cfg)
	}
	shows([]string{"--project", "WEB"}, "Runs of WEB are kept for 30 days, the runner-wide transcript_days")

	// 0 is a value of its own: kept in config.json, not dropped as unset.
	if code := run("--project", "WEB", "0"); code != 0 || !reloaded() || !strings.Contains(out.String(), "Runs of WEB are kept only while they run, its own transcript_days") {
		t.Fatalf("set WEB 0: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if cfg := load(); cfg.Projects[0].TranscriptDays == nil || *cfg.Projects[0].TranscriptDays != 0 || cfg.TranscriptDaysFor(cfg.Projects[0]) != 0 {
		t.Errorf("after set WEB 0: %v", cfg.Projects[0].TranscriptDays)
	}

	if code := run("--max-size", "2G"); code != 0 || !reloaded() || !strings.Contains(out.String(), "at most 2g of disk in all\n") {
		t.Fatalf("max-size: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if cfg := load(); cfg.TranscriptMaxSize != "2g" || cfg.TranscriptMax() != 2<<30 {
		t.Errorf("after max-size: %q", cfg.TranscriptMaxSize)
	}

	for _, bad := range [][]string{{"366"}, {"seven"}, {"1.5"}, {"--project", "WEB", "366"}} {
		if code := run(bad...); code != 1 || !strings.Contains(errb.String(), "0 to 365") || reloaded() {
			t.Errorf("%q: code %d, stderr %q; want 1 and the range", bad, code, errb.String())
		}
	}
	for _, bad := range []string{"99m", "1GB", "1073741824", "0g", "1.5g", "lots"} {
		if code := run("--max-size", bad); code != 1 || !strings.Contains(errb.String(), "100m") || reloaded() {
			t.Errorf("--max-size %s: code %d, stderr %q; want 1 and the minimum", bad, code, errb.String())
		}
	}
	if code := run("--project", "WEB", "--max-size", "2g"); code != 2 || !strings.Contains(errb.String(), "runner-wide") || reloaded() {
		t.Errorf("--max-size with --project: code %d, stderr %q; want 2", code, errb.String())
	}
	if cfg := load(); *cfg.TranscriptDays != 30 || *cfg.Projects[0].TranscriptDays != 0 || cfg.TranscriptMaxSize != "2g" {
		t.Errorf("refused changes changed the config: %+v", cfg)
	}

	if code := run("--project", "WEB", "clear"); code != 0 || !reloaded() || !strings.Contains(out.String(), "Runs of WEB are kept for 30 days, the runner-wide transcript_days") {
		t.Errorf("clear WEB: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if code := run("--project", "WEB", "clear"); code != 0 || reloaded() || !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("clear WEB again: code %d, %q", code, out.String())
	}
	if code := run("clear"); code != 0 || !reloaded() || !strings.Contains(out.String(), "Runs are kept for 14 days, the default") {
		t.Errorf("clear runner-wide: code %d, %q, stderr %q", code, out.String(), errb.String())
	}
	if cfg := load(); cfg.TranscriptDays != nil || cfg.Projects[0].TranscriptDays != nil || *cfg.Projects[1].TranscriptDays != 3 {
		t.Errorf("after clear runner-wide: %+v", cfg)
	}
	if code := run("clear"); code != 0 || reloaded() || !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("clear runner-wide again: code %d, %q", code, out.String())
	}

	for _, usage := range [][]string{{"7", "8"}, {"7", "--project", "WEB"}, {"--max-size", "2g", "7"}, {"-1"}, {"-url", "https://a.example.com"}, {"--grow"}} {
		if code := run(usage...); code != 2 {
			t.Errorf("transcripts %q: code %d, want 2", usage, code)
		}
	}
	if code := run("--project", "NOPE"); code != 1 {
		t.Errorf("show unknown project: code %d, want 1", code)
	}
}
