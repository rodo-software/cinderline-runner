package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/transcript"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLR-278: each run leaves one transcript file per step, with its start and
// end lines (decision 2026-11-26-local-web-page, section Transcripts).

// withTranscripts gives f's project a transcript store in a temporary folder.
func withTranscripts(t *testing.T, f *fixture) *transcript.Store {
	s := &transcript.Store{Dir: t.TempDir(), Log: discard()}
	f.p.Transcripts = s
	return s
}

// transcriptSteps returns the one run's step file names and, per step, its
// lines without their time.
func transcriptSteps(t *testing.T, s *transcript.Store) ([]string, [][]map[string]any) {
	t.Helper()
	runs := s.List()
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one", runs)
	}
	r := runs[0]
	var names []string
	var lines [][]map[string]any
	for _, st := range s.Steps(r.Project, r.Issue, r.Run) {
		rc, err := s.Open(r.Project, r.Issue, r.Run, st.N)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		var ls []map[string]any
		for l := range strings.Lines(string(b)) {
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("line %q: %v", l, err)
			}
			delete(m, "t")
			ls = append(ls, m)
		}
		names, lines = append(names, fmt.Sprintf("%02d-%s.ndjson", st.N, st.Kind)), append(lines, ls)
	}
	return names, lines
}

func jsonOf(m map[string]any) string { b, _ := json.Marshal(m); return string(b) }

func TestARunLeavesOneTranscriptFilePerStep(t *testing.T) {
	f := newFixture(t)
	s := withTranscripts(t, f)
	f.cl.Runs = fill(1, ok("sess-1", "Done."))
	f.env.Runs = []environment.RunReply{{Output: "ok"}}
	f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"check","issue":{"ref":"CSR-1","title":"Fix it"}}`, `{"kind":"publish"}`, `{"kind":"stop"}`)}
	jb := soloJob()
	jb.Team.Agents[0].Model = ptr("opus")

	if started, err := f.p.run(context.Background(), app.client(t), proj, jb, 0); !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	if r := s.List(); r[0].Project != "CSR" || r[0].Issue != "CSR-1" || !r[0].Ended {
		t.Errorf("run = %+v", r)
	}
	names, lines := transcriptSteps(t, s)
	if want := []string{"01-run.ndjson", "02-check.ndjson", "03-publish.ndjson", "04-stop.ndjson"}; strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("files = %v, want %v", names, want)
	}
	starts := []string{
		`{"agent":"Coder","client":"claude","kind":"run","model":"opus","type":"start"}`,
		`{"kind":"check","type":"start"}`,
		`{"kind":"publish","type":"start"}`,
		`{"kind":"stop","type":"start"}`,
	}
	sizes := []int{3, 2, 2, 2} // the run step holds its prompt between them too (CLR-279)
	for i, ls := range lines {
		if len(ls) != sizes[i] || jsonOf(ls[0]) != starts[i] || jsonOf(ls[len(ls)-1]) != `{"error":"","outcome":"ok","type":"end"}` {
			t.Errorf("%s = %v", names[i], ls)
		}
	}
}

func TestAFailingStepEndsAsFailedWithItsError(t *testing.T) {
	f := newFixture(t)
	s := withTranscripts(t, f)
	resume := strings.Replace(runNew, `"session":"new"`, `"session":"resume"`, 1)
	app := &fakeApp{onNext: stepsOf(resume)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	_, lines := transcriptSteps(t, s)
	end := lines[0][len(lines[0])-1]
	if len(lines) != 1 || end["outcome"] != "failed" || !strings.Contains(end["error"].(string), "resumes a session the runner does not hold") {
		t.Errorf("lines = %v", lines)
	}
}

func TestAPanickingStepEndsAsFailed(t *testing.T) {
	f := newFixture(t)
	s := withTranscripts(t, f)
	f.p.Clients["claude"] = panicky{}
	if _, err := f.run(t, &fakeApp{onNext: stepsOf(runNew)}); err != nil {
		t.Fatal(err)
	}
	_, lines := transcriptSteps(t, s)
	if end := lines[0][len(lines[0])-1]; end["type"] != "end" || end["outcome"] != "failed" || end["error"] != "panic: bad" {
		t.Errorf("lines = %v", lines)
	}
}

// A turn the poll's stop ended is stopped, though the step has no error.
func TestAStoppedStepEndsAsStopped(t *testing.T) {
	end := func(last *cinderline.LastStep, err error) string {
		o, _ := stepEnd(context.Background(), last, err)
		return o
	}
	if o := end(&cinderline.LastStep{Outcome: "stopped"}, nil); o != "stopped" {
		t.Errorf("stopped turn: %s", o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if o, e := stepEnd(ctx, nil, ctx.Err()); o != "stopped" || e != "" {
		t.Errorf("ended run: %s, %q", o, e)
	}
}

func TestAManagerRunsTranscriptIsMarked(t *testing.T) {
	f := newFixture(t)
	s := withTranscripts(t, f)
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	f.cl.Runs = fill(1, ok("s-1", "Planned."))
	app := &fakeApp{onNext: stepsOf(managerRun)}
	if _, err := f.p.run(context.Background(), app.client(t), proj, managerJob(), managerSlot); err != nil {
		t.Fatal(err)
	}
	_, lines := transcriptSteps(t, s)
	if want := `{"agent":"Project Manager","client":"claude","kind":"run","manager":true,"type":"start"}`; jsonOf(lines[0][0]) != want {
		t.Errorf("start = %s, want %s", jsonOf(lines[0][0]), want)
	}
}

func TestWithoutTranscriptsNothingIsWritten(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = fill(1, ok("sess-1", "Done."))
	if started, err := f.run(t, &fakeApp{onNext: stepsOf(runNew, `{"kind":"stop"}`)}); !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	if _, err := os.Stat(filepath.Join(f.p.State, "transcripts")); !os.IsNotExist(err) {
		t.Errorf("transcripts folder: %v", err)
	}
}

// CLR-279: a turn's step holds the prompt the agent got and each line its
// container printed, in order, between start and end.
func TestATurnsStepHoldsItsPromptAndOutput(t *testing.T) {
	f := newFixture(t)
	s := withTranscripts(t, f)
	f.env.Runs = []environment.RunReply{{Output: "working\ndone"}}
	f.p.Clients["claude"] = &client.CLI{Name: "claude", Project: "CSR", Env: f.env,
		Exec: func(context.Context, []string, ...string) (string, error) { return "", nil }}
	if _, err := f.run(t, &fakeApp{onNext: stepsOf(runNew, `{"kind":"stop"}`)}); err != nil {
		t.Fatal(err)
	}
	_, lines := transcriptSteps(t, s)
	var got []string
	for _, l := range lines[0][1 : len(lines[0])-1] {
		got = append(got, jsonOf(l))
	}
	want := []string{`{"text":"Do it.","type":"prompt"}`, `{"line":"working","type":"out"}`, `{"line":"done","type":"out"}`}
	if lines[0][0]["type"] != "start" || lines[0][len(lines[0])-1]["type"] != "end" || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines = %v", lines[0])
	}
}
