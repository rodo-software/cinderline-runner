package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/transcript"
)

type stepOut struct {
	N                                          int
	Kind, Agent, Client, Model, Outcome, Error string
	Start                                      time.Time
	End                                        *time.Time
}

type runOut struct {
	Project, Issue, Run string
	Start               time.Time
	Live, Manager       bool
	Outcome             string
	IssueURL            string `json:"issue_url"`
}

var (
	t1 = time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	t2 = t1.Add(time.Hour)
	t3 = t2.Add(time.Hour)
)

// runsHandler serves a store of three runs, CLR-12 (oldest, ended),
// CLR-12.1 (a manager's, live) and WEB-3 (newest, ended, of a project the
// config no longer has), with the config's CLR on https://cl.example.com/.
func runsHandler(t *testing.T) (dir, tok string, h http.Handler) {
	t.Helper()
	s := transcript.Store{Dir: t.TempDir()}
	r := s.Begin("CLR", "CLR-12", t1)
	a := r.Step("run", transcript.Start{Agent: "coder", Client: "claude", Model: "opus"})
	a.End("ok", "")
	r.Step("check", transcript.Start{}).End("failed", "go vet failed")
	s.Begin("CLR", "CLR-12.1", t2).Step("run", transcript.Start{Agent: "pm", Manager: true})
	s.Begin("WEB", "WEB-3", t3).Step("run", transcript.Start{}).End("ok", "")
	cfg := &config.Config{Projects: []config.Project{{Key: "CLR", URL: "https://cl.example.com/"}}}
	dir = t.TempDir()
	return dir, token(t, dir), Handler(dir, port, Handlers{Transcripts: s, Config: func() *config.Config { return cfg }})
}

func getJSON(t *testing.T, h http.Handler, tok, path string, v any) {
	t.Helper()
	w := doTo(t, h, "GET", path, tok, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s = %d, %q: %s", path, w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatal(err)
	}
}

func issues(runs []runOut) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.Issue)
	}
	return out
}

func TestRunsNewestFirst(t *testing.T) {
	_, tok, h := runsHandler(t)
	var got []struct {
		runOut
		Steps int
	}
	getJSON(t, h, tok, "/api/runs", &got)
	if len(got) != 3 {
		t.Fatalf("got %d runs: %+v", len(got), got)
	}
	want := []runOut{
		{Project: "WEB", Issue: "WEB-3", Run: "20261128T110000Z", Start: t3, Outcome: "ok"},
		{Project: "CLR", Issue: "CLR-12.1", Run: "20261128T100000Z", Start: t2, Live: true, Manager: true, IssueURL: "https://cl.example.com/projects/CLR/issues/12.1"},
		{Project: "CLR", Issue: "CLR-12", Run: "20261128T090000Z", Start: t1, Outcome: "failed", IssueURL: "https://cl.example.com/projects/CLR/issues/12"},
	}
	for i, n := range []int{1, 1, 2} {
		if got[i].runOut != want[i] || got[i].Steps != n {
			t.Errorf("run %d = %+v, want %+v with %d steps", i, got[i], want[i], n)
		}
	}
}

func TestRunsFilterAndLimit(t *testing.T) {
	_, tok, h := runsHandler(t)
	var got []runOut
	getJSON(t, h, tok, "/api/runs?project=CLR", &got)
	if is := issues(got); len(is) != 2 || is[0] != "CLR-12.1" || is[1] != "CLR-12" {
		t.Errorf("project=CLR: %v, want CLR-12.1 then CLR-12", is)
	}
	got = nil
	getJSON(t, h, tok, "/api/runs?project=CLR&limit=1", &got)
	if is := issues(got); len(is) != 1 || is[0] != "CLR-12.1" {
		t.Errorf("project=CLR&limit=1: %v, want CLR-12.1", is)
	}
	got = nil
	getJSON(t, h, tok, "/api/runs?project=NONE", &got)
	if len(got) != 0 {
		t.Errorf("project=NONE: %v, want none", got)
	}
}

func TestRunsRefusesABadQuery(t *testing.T) {
	_, tok, h := runsHandler(t)
	for _, q := range []string{"limit=x", "limit=0", "limit=501", "limit=-1", "project=..", "project=a%2Fb"} {
		if w := doTo(t, h, "GET", "/api/runs?"+q, tok, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, w.Code)
		}
	}
	var got []runOut
	getJSON(t, h, tok, "/api/runs?limit=500", &got)
}

func TestRunWithItsSteps(t *testing.T) {
	_, tok, h := runsHandler(t)
	var got struct {
		runOut
		Steps []stepOut
	}
	getJSON(t, h, tok, "/api/runs/CLR/CLR-12/20261128T090000Z", &got)
	want := runOut{Project: "CLR", Issue: "CLR-12", Run: "20261128T090000Z", Start: t1, Outcome: "failed", IssueURL: "https://cl.example.com/projects/CLR/issues/12"}
	if got.runOut != want || len(got.Steps) != 2 {
		t.Fatalf("run = %+v, want %+v with 2 steps", got, want)
	}
	a, b := got.Steps[0], got.Steps[1]
	if a.N != 1 || a.Kind != "run" || a.Agent != "coder" || a.Client != "claude" || a.Model != "opus" || a.Outcome != "ok" || a.Error != "" || a.Start.IsZero() || a.End == nil || a.End.Before(a.Start) {
		t.Errorf("step 1 = %+v", a)
	}
	if b.N != 2 || b.Kind != "check" || b.Agent != "" || b.Outcome != "failed" || b.Error != "go vet failed" || b.End == nil {
		t.Errorf("step 2 = %+v", b)
	}

	var live struct {
		runOut
		Steps []stepOut
	}
	getJSON(t, h, tok, "/api/runs/CLR/CLR-12.1/20261128T100000Z", &live)
	if !live.Live || !live.Manager || live.Outcome != "" || len(live.Steps) != 1 || live.Steps[0].End != nil || live.Steps[0].Outcome != "" {
		t.Errorf("live run = %+v", live)
	}
}

func TestRunUnknownOrNotPlain(t *testing.T) {
	_, tok, h := runsHandler(t)
	for path, code := range map[string]int{
		"/api/runs/CLR/CLR-12/20261128T080000Z":  http.StatusNotFound, // no such start
		"/api/runs/CLR/CLR-13/20261128T090000Z":  http.StatusNotFound,
		"/api/runs/XYZ/CLR-12/20261128T090000Z":  http.StatusNotFound,
		"/api/runs/CLR/CLR-12/latest":            http.StatusNotFound, // not a run id
		"/api/runs/CLR/%2E%2E/20261128T090000Z":  http.StatusBadRequest,
		"/api/runs/CLR/a%5Cb/20261128T090000Z":   http.StatusBadRequest,
		"/api/runs/a..b/CLR-12/20261128T090000Z": http.StatusBadRequest,
	} {
		if w := doTo(t, h, "GET", path, tok, nil); w.Code != code {
			t.Errorf("GET %s = %d, want %d", path, w.Code, code)
		}
	}
}

func TestIssueURL(t *testing.T) {
	cfg := &config.Config{Projects: []config.Project{{Key: "CLR", URL: "https://cl.example.com"}}}
	for issue, want := range map[string]string{
		"CLR-12":   "https://cl.example.com/projects/CLR/issues/12",
		"CLR-12.1": "https://cl.example.com/projects/CLR/issues/12.1",
	} {
		if got := issueURL(cfg, "CLR", issue); got != want {
			t.Errorf("issueURL(%s) = %q, want %q", issue, got, want)
		}
	}
	if got := issueURL(cfg, "WEB", "WEB-1"); got != "" {
		t.Errorf("issueURL of a project not in the config = %q, want empty", got)
	}
}
