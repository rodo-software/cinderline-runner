package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

const runNew = `{"kind":"run","issue":{"ref":"CSR-1","title":"Fix it"},"agent":"Coder","session":"new","token":"agent-token-1234567890","prompt":"Do it."}`

func (f *fixture) run(t *testing.T, app *fakeApp) (bool, error) {
	t.Helper()
	return f.p.run(context.Background(), app.client(t), proj, soloJob())
}

// A solo run: the first request carries the run's facts and nothing else;
// each later one the state echoed, a new id and the step just done; the
// run ends with the lead's session kept as the done step says.
func TestARunFollowsNextToDone(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("sess-1", "Done.")}
	f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{Number: 4, URL: "https://git.example/o/r/pulls/4"}}}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"publish"}`)}

	if started, err := f.run(t, app); !started || err != nil {
		t.Fatalf("run = %v, %v; want started", started, err)
	}
	reqs := app.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d next requests, want 3", len(reqs))
	}
	first := reqs[0]
	if first.State != nil || first.Last != nil || len(first.Sessions) != 0 || first.Workspace.Branch != "cinderline/CSR-1" || first.Workspace.Base != "main" || first.ID == "" {
		t.Errorf("first request = %+v", first)
	}
	if reqs[1].ID == first.ID || deref(reqs[1].State) != "s1" || deref(reqs[2].State) != "s2" {
		t.Errorf("ids %q %q, states %v %v", first.ID, reqs[1].ID, reqs[1].State, reqs[2].State)
	}
	run := reqs[1].Last
	if run.Step != "run" || run.Outcome != "finished" || deref(run.Report) != "Done." || run.Usage == nil || run.Usage.InputTokens != 3 || run.Usage.Issue != "CSR-1" {
		t.Errorf("run facts = %+v", run)
	}
	if s := reqs[2].Sessions; len(s) != 1 || s[0] != (cinderline.Session{Issue: "CSR-1", Agent: "Coder", Origin: "turn"}) {
		t.Errorf("sessions after the turn = %+v", s)
	}
	pub := reqs[2].Last
	if pub.Step != "publish" || pub.Outcome != "published" || deref(pub.PullRequest) != "https://git.example/o/r/pulls/4" || deref(pub.Commit) != "abc123" || pub.Merge != nil {
		t.Errorf("publish facts = %+v", pub)
	}
	turn := turns(f.cl)[0]
	if turn.Token != "agent-token-1234567890" || turn.Prompt != "Do it." || turn.Author != "Dev / Coder" || turn.MaxTurns != 50 || turn.Idle.Minutes() != 20 {
		t.Errorf("turn = %+v", turn)
	}
	n, okNote := f.p.readNote("CSR-1")
	if !okNote || n.Origin != "kept" || n.Session != "sess-1" || n.Dir != "/runs/CSR-1" || n.Client != "claude" {
		t.Errorf("note = %+v, %v; want the kept session", n, okNote)
	}
}

// Each step kind reports its facts.
func TestStepFacts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		step  string
		setup func(*fixture)
		want  cinderline.LastStep
	}{
		{"check passed", `{"kind":"check","issue":{"ref":"CSR-1","title":"Fix it"}}`,
			func(f *fixture) { f.env.Runs = []environment.RunReply{{Output: "ok"}} },
			cinderline.LastStep{Step: "check", Issue: "CSR-1", Agent: ptr("Coder"), Outcome: "passed"}},
		{"check failed", `{"kind":"check","issue":{"ref":"CSR-1","title":"Fix it"}}`,
			func(f *fixture) { f.env.Runs = []environment.RunReply{{Output: "boom", Err: exitError(1)}} },
			cinderline.LastStep{Step: "check", Issue: "CSR-1", Agent: ptr("Coder"), Outcome: "failed", Tail: ptr("boom")}},
		{"stop", `{"kind":"stop"}`, func(*fixture) {},
			cinderline.LastStep{Step: "stop", Issue: "CSR-1", Outcome: "pushed", Commit: ptr("abc123")}},
		{"stop not pushed", `{"kind":"stop"}`, func(f *fixture) { f.ws.Pushes = []error{errors.New("denied")} },
			cinderline.LastStep{Step: "stop", Issue: "CSR-1", Outcome: "not_pushed", Commit: ptr("abc123"), Error: ptr("denied")}},
		// Decision 2026-11-17-open-questions-answered, point 2: a known secret
		// stops the push, at once and without retries; a shape only warns.
		{"stop secret", `{"kind":"stop"}`, func(f *fixture) {
			f.ws.Pushes = []error{secretErr}
			f.ws.Found = [][]workspace.Secret{{knownSecret, shapeSecret}}
		}, cinderline.LastStep{Step: "stop", Issue: "CSR-1", Outcome: "secret", Commit: ptr("abc123"), Error: ptr(secretErr.Error()), Secrets: stepSecrets([]workspace.Secret{knownSecret, shapeSecret})}},
		{"publish secret", `{"kind":"publish","merge":true}`, func(f *fixture) {
			f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
			f.ws.Pushes = []error{secretErr}
			f.ws.Found = [][]workspace.Secret{{knownSecret}}
		}, cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "secret", Commit: ptr("abc123"), Error: ptr(secretErr.Error()), Secrets: stepSecrets([]workspace.Secret{knownSecret})}},
		{"publish warns", `{"kind":"publish"}`, func(f *fixture) {
			f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
			f.ws.Found = [][]workspace.Secret{{shapeSecret}}
			f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
		}, cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "published", PullRequest: ptr("u"), Commit: ptr("abc123"), Secrets: stepSecrets([]workspace.Secret{shapeSecret})}},
		{"publish unchanged", `{"kind":"publish"}`, func(f *fixture) { f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}} },
			cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "unchanged", Commit: ptr("abc123")}},
		{"publish merged", `{"kind":"publish","merge":true}`, func(f *fixture) {
			f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
			f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
			f.fg.Merges = []error{nil}
			f.fg.MergeStates = []forge.MergeStateReply{{State: forge.MergePending}, {State: forge.MergeMerged}}
		}, cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "published", PullRequest: ptr("u"), Merge: ptr("merged"), Commit: ptr("abc123")}},
		{"publish merge with conflicts", `{"kind":"publish","merge":true}`, func(f *fixture) {
			f.ws.MergeBases = append([]workspace.MergeBaseReply{{}, {Conflicts: []string{"a.go"}}}, f.ws.MergeBases...)
			f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
			f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
		}, cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "published", PullRequest: ptr("u"), Merge: ptr("failed"), Commit: ptr("abc123"), Conflicts: []string{"a.go"}}},
		{"publish failed", `{"kind":"publish"}`, func(f *fixture) {
			f.ws.Ancestors = fill(5, workspace.IsAncestorReply{})
			f.ws.Pushes = fill(5, errors.New("denied"))
		}, cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "failed", Error: ptr("denied"), Commit: ptr("abc123")}},
		{"turn failed", runNew, func(f *fixture) {
			f.cl.Runs = []client.RunReply{{Result: client.Result{Tail: "oops"}, Err: errors.New("exit 1")}}
		}, cinderline.LastStep{Step: "run", Issue: "CSR-1", Agent: ptr("Coder"), Outcome: "failed", Error: ptr("exit 1"), Tail: ptr("oops")}},
		{"turn idle", runNew, func(f *fixture) {
			f.cl.Runs = []client.RunReply{{Err: environment.ErrIdle}}
		}, cinderline.LastStep{Step: "run", Issue: "CSR-1", Agent: ptr("Coder"), Outcome: "idle", Error: ptr(environment.ErrIdle.Error())}},
		{"turn limit", runNew, func(f *fixture) {
			f.cl.Runs = []client.RunReply{{Result: client.Result{HitTurnLimit: true}, Err: errors.New("max turns")}}
		}, cinderline.LastStep{Step: "run", Issue: "CSR-1", Agent: ptr("Coder"), Outcome: "turn_limit", Error: ptr("max turns")}},
		// Decision 2026-11-17-local-model-providers: a turn whose provider the
		// project lacks is not started, with the client's reason.
		{"provider missing", runNew, func(f *fixture) {
			f.cl.Runs = []client.RunReply{{Err: fmt.Errorf("%w: model provider \"x\" is not configured", client.ErrNotStarted)}}
		}, cinderline.LastStep{Step: "run", Issue: "CSR-1", Agent: ptr("Coder"), Outcome: "not_started", Error: ptr(`turn not started: model provider "x" is not configured`)}},
		{"agent not on the team", strings.Replace(runNew, `"Coder"`, `"Ghost"`, 1), func(*fixture) {},
			cinderline.LastStep{Step: "run", Issue: "CSR-1", Agent: ptr("Ghost"), Outcome: "not_started", Error: ptr("agent Ghost is not on team Dev")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(f)
			app := &fakeApp{onNext: stepsOf(tc.step)}
			if _, err := f.run(t, app); err != nil {
				t.Fatal(err)
			}
			lasts := app.lasts()
			if len(lasts) != 1 {
				t.Fatalf("lasts = %+v", lasts)
			}
			got := lasts[0]
			got.Usage, got.Report = nil, nil
			if !sameLast(got, tc.want) {
				t.Errorf("last = %s\nwant   %s", show(got), show(tc.want))
			}
		})
	}
}

var (
	secretErr   = fmt.Errorf("push cinderline/CSR-1: %w: git token in c1 a.go", workspace.ErrSecret)
	knownSecret = workspace.Secret{Kind: "git token", Commit: "c1", Path: "a.go", Known: true}
	shapeSecret = workspace.Secret{Kind: "private key block", Commit: "c1"}
)

type exitError int

func (e exitError) Error() string { return "exit status 1" }
func (e exitError) ExitCode() int { return int(e) }

func sameLast(a, b cinderline.LastStep) bool { return show(a) == show(b) }

func show(l cinderline.LastStep) string {
	s := func(p *string) string { return deref(p) }
	var found []string
	for _, f := range l.Secrets {
		found = append(found, fmt.Sprint(f.Kind, "@", f.Commit, ":", s(f.Path), "/", f.Known))
	}
	return strings.Join([]string{l.Step, l.Issue, s(l.Agent), l.Outcome, s(l.Error), s(l.Tail), s(l.PullRequest), s(l.Merge), s(l.Commit), strings.Join(l.Conflicts, ","), strings.Join(found, ",")}, "|")
}

// A step the runner cannot do, and a failure in its own code, end the run
// with an unasked stop whose error names the runner's version; the work is
// pushed first.
func TestAnUnknownOrMalformedStepIsAnUnaskedStop(t *testing.T) {
	for _, step := range []string{
		`{"kind":"dance"}`,
		`{"kind":"done","session":"forever"}`,
		strings.Replace(runNew, `"new"`, `"sideways"`, 1),
		strings.Replace(runNew, `"ref":"CSR-1"`, `"ref":"CSR-10"`, 1),
		`{"kind":"resolve","agent":"Coder","session":"new","prompt":"x"}`,
	} {
		f := newFixture(t)
		// The app answers a stop with stop again while it waits for that report: done again.
		app := &fakeApp{onNext: stepsOf(step, `{"kind":"stop","reason":"x"}`, `{"kind":"done","session":"paused"}`)}
		if started, err := f.run(t, app); !started || err != nil {
			t.Fatalf("%s: run = %v, %v", step, started, err)
		}
		lasts := app.lasts()
		if len(lasts) != 2 || lasts[1].Step != "stop" || lasts[0].Step != "stop" || lasts[0].Outcome != "pushed" || !strings.Contains(deref(lasts[0].Error), "runner 0.0.0-test") {
			t.Errorf("%s: lasts = %+v", step, lasts)
		}
	}
}

// panicky is a client whose turn panics.
type panicky struct{}

func (panicky) Run(context.Context, client.Turn) (client.Result, error) { panic("bad") }

func TestAPanicIsAnUnaskedStop(t *testing.T) {
	f := newFixture(t)
	f.p.Clients["claude"] = panicky{}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"run"}`)} // anything but done ends the run after the stop
	if started, err := f.run(t, app); !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	lasts := app.lasts()
	if len(lasts) != 1 || lasts[0].Step != "stop" || !strings.Contains(deref(lasts[0].Error), "panic: bad") {
		t.Errorf("lasts = %+v", lasts)
	}
	if n, _ := f.p.readNote("CSR-1"); n.Origin != "" {
		t.Errorf("note = %+v; want none, as no session was made", n)
	}
}

// When next stays unavailable the work is pushed, the session kept as
// paused and the request queued with its id; a replay delivers it.
func TestAnUnreachableNextPausesAndQueues(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("sess-1", "Done.")}
	down := true
	app := &fakeApp{}
	app.onNext = func(n int, ref string, req cinderline.NextRequest) reply {
		if n > 0 && down {
			return reply{503, `{"message":"deploying"}`}
		}
		return stepsOf(runNew)(n, ref, req)
	}
	c := app.client(t)
	c.Outbox = filepath.Join(t.TempDir(), "outbox.json")
	if started, err := f.p.run(context.Background(), c, proj, soloJob()); !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	reqs := app.requests()
	if len(reqs) != 4 || reqs[1].ID != reqs[3].ID {
		t.Fatalf("%d requests; want the first and three tries of the second with one id", len(reqs))
	}
	if n, _ := f.p.readNote("CSR-1"); n.Origin != "paused" || n.Session != "sess-1" {
		t.Errorf("note = %+v; want the paused session", n)
	}
	if !slices.Contains(methods(f.ws), "Push") {
		t.Errorf("workspace calls %v; want a push", methods(f.ws))
	}
	if q := c.Queued(); !slices.Equal(q, []string{"CSR-1"}) {
		t.Fatalf("queued = %v", q)
	}
	if _, err := os.Stat(c.Outbox); err != nil {
		t.Errorf("outbox file: %v", err)
	}
	app.mu.Lock()
	down = false
	app.mu.Unlock()
	c.Replay(context.Background())
	if reqs := app.requests(); reqs[len(reqs)-1].ID != reqs[1].ID || len(c.Queued()) != 0 {
		t.Errorf("replay sent %+v, queued %v", reqs[len(reqs)-1], c.Queued())
	}
}

// A 409 on the first call drops the job quietly; another refusal is a run
// that did not start. Either way a resumed note is put back.
func TestAFirstNextThatIsRefused(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{{409, false}, {422, true}} {
		f := newFixture(t)
		f.p.saveNote("CSR-1", Note{Dir: t.TempDir(), Session: "s0", Client: "claude", Origin: "paused"})
		app := &fakeApp{onNext: func(int, string, cinderline.NextRequest) reply {
			return reply{tc.status, `{"message":"no","status":"done"}`}
		}}
		started, err := f.run(t, app)
		if started || (err != nil) != tc.wantErr {
			t.Errorf("%d: run = %v, %v", tc.status, started, err)
		}
		if n, ok := f.p.readNote("CSR-1"); !ok || n.Session != "s0" || n.Origin != "paused" {
			t.Errorf("%d: note = %+v, %v; want it back", tc.status, n, ok)
		}
	}
}

// blocking is a client whose turn runs until its context ends.
type blocking struct {
	started chan struct{}
}

func (b *blocking) Run(ctx context.Context, _ client.Turn) (client.Result, error) {
	close(b.started)
	<-ctx.Done()
	return client.Result{Report: "half"}, errors.New("killed")
}

// The poll's stop ends the turn in flight; the step reports stopped.
func TestAStopEndsTheTurnAsStopped(t *testing.T) {
	f := newFixture(t)
	b := &blocking{started: make(chan struct{})}
	f.p.Clients["claude"] = b
	app := &fakeApp{onNext: stepsOf(runNew)}
	c := app.client(t)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = f.p.run(context.Background(), c, proj, soloJob()) })
	<-b.started
	f.p.stopTurn("CSR-2") // another run's: nothing
	f.p.stopTurn("CSR-1")
	wg.Wait()
	if lasts := app.lasts(); len(lasts) != 1 || lasts[0].Outcome != "stopped" || deref(lasts[0].Report) != "half" {
		t.Errorf("lasts = %+v", lasts)
	}
}

// A sub-issue's work is committed as "<ref>: <title>" and pushed after its turn.
func TestASubIssueTurnIsCommittedAndPushed(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("w1", "Sub done.")}
	app := &fakeApp{onNext: stepsOf(`{"kind":"run","issue":{"ref":"CSR-1.2","title":"Part"},"agent":"Coder","session":"new","token":null,"prompt":"Part."}`)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	var commit workspace.Call
	for _, c := range f.ws.Calls() {
		if c.Method == "Commit" {
			commit = c
		}
	}
	if commit.Args[2] != "CSR-1.2: Part" {
		t.Errorf("last commit = %v", commit.Args)
	}
	if got := methods(f.ws); got[len(got)-1] != "Push" {
		t.Errorf("workspace calls %v; want a push last", got)
	}
	if turns(f.cl)[0].Token != "" {
		t.Error("a null token gave the turn one")
	}
}

// A conflicting base merge at publish goes to next as a merge step; the
// resolve that follows is verified, and undone when it does not hold.
func TestAPublishConflictIsResolvedAndVerified(t *testing.T) {
	for _, verified := range []bool{true, false} {
		f := newFixture(t)
		f.ws.MergeBases = append([]workspace.MergeBaseReply{{}, {Conflicts: []string{"a.go"}}}, f.ws.MergeBases...)
		f.ws.Logs = []workspace.LogReply{{Log: "abc fix"}}
		f.ws.Cleans = []workspace.BoolReply{{Yes: verified}}
		f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}, {Yes: true}}
		f.ws.Markers = []workspace.BoolReply{{}}
		f.ws.Resets = []error{nil}
		f.cl.Runs = []client.RunReply{ok("r1", "Merged.")}
		app := &fakeApp{onNext: stepsOf(`{"kind":"publish","resolve_conflicts":true}`, `{"kind":"resolve","agent":"Coder","session":"new","token":null,"prompt":"Resolve."}`)}
		if _, err := f.run(t, app); err != nil {
			t.Fatal(err)
		}
		lasts := app.lasts()
		if len(lasts) != 2 || lasts[0].Step != "merge" || lasts[0].Outcome != "conflict" || !slices.Equal(lasts[0].Conflicts, []string{"a.go"}) || deref(lasts[0].Commits) != "abc fix" {
			t.Fatalf("lasts = %+v", lasts)
		}
		want := map[bool]string{true: "finished", false: "not_verified"}[verified]
		if lasts[1].Step != "resolve" || lasts[1].Outcome != want {
			t.Errorf("verified %v: resolve = %+v", verified, lasts[1])
		}
		if undone := slices.Contains(methods(f.ws), "Reset"); undone == verified {
			t.Errorf("verified %v: reset %v", verified, undone)
		}
	}
}

// A kept session is fast-forwarded and resumed; the first request names it.
func TestAKeptSessionIsResumed(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.p.saveNote("CSR-1", Note{Dir: dir, Session: "old", Client: "claude", Origin: "kept"})
	f.ws.FastForwards = []error{nil}
	f.cl.Runs = []client.RunReply{ok("old", "Again.")}
	app := &fakeApp{onNext: stepsOf(strings.Replace(runNew, `"new"`, `"resume"`, 1))}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	if s := app.requests()[0].Sessions; len(s) != 1 || s[0].Origin != "kept" {
		t.Errorf("sessions = %+v", s)
	}
	if turn := turns(f.cl)[0]; turn.SessionID != "old" || turn.Dir != dir {
		t.Errorf("turn = %+v", turn)
	}
	if slices.Contains(methods(f.ws), "Prepare") {
		t.Error("a resumed run made a fresh clone")
	}
}

// A note whose session tidying deleted becomes a workspace note.
func TestAPrunedSessionIsANote(t *testing.T) {
	f := newFixture(t)
	f.p.saveNote("CSR-1", Note{Pruned: prunedDisk})
	app := &fakeApp{onNext: stepsOf()}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	if notes := app.requests()[0].Workspace.Notes; len(notes) != 1 || !strings.Contains(notes[0], prunedDisk) {
		t.Errorf("notes = %q", notes)
	}
}

// What stops preparation means the run did not start, and nothing is sent.
func TestARunThatCannotPrepareDoesNotStart(t *testing.T) {
	f := newFixture(t)
	f.ws.Prepares = []workspace.PrepareReply{{Err: errors.New("disk full")}}
	app := &fakeApp{onNext: stepsOf()}
	if started, err := f.run(t, app); started || err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("run = %v, %v", started, err)
	}
	if len(app.requests()) != 0 {
		t.Error("a run that did not start called next")
	}
}

// Attachments land under their safe names; a name or issue the runner does
// not accept means the turn does not start.
func TestStepAttachments(t *testing.T) {
	for _, tc := range []struct {
		files, want string
	}{
		{`[{"issue":"CSR-1.1","id":7,"name":"spec.pdf"}]`, "finished"},
		{`[{"issue":"CSR-1","id":7,"name":"../x"}]`, "not_started"},
		{`[{"issue":"CSR-1","id":7,"name":"a.txt"},{"issue":"CSR-1","id":8,"name":"A.TXT"}]`, "not_started"},
		{`[{"issue":"CSR-2","id":7,"name":"spec.pdf"}]`, "not_started"},
	} {
		f := newFixture(t)
		f.cl.Runs = []client.RunReply{ok("s", "")}
		step := strings.Replace(runNew, `"prompt"`, `"attachments":`+tc.files+`,"prompt"`, 1)
		app := &fakeApp{onNext: stepsOf(step), files: map[string]string{"CSR-1.1/7": "PDF", "CSR-1/7": "x"}}
		if _, err := f.run(t, app); err != nil {
			t.Fatal(err)
		}
		if got := app.lasts()[0].Outcome; got != tc.want {
			t.Errorf("%s: outcome %s, want %s", tc.files, got, tc.want)
		}
		if tc.want == "finished" {
			b, err := os.ReadFile(filepath.Join(attachmentsDir(f.p.State, cfg), "spec.pdf"))
			if string(b) != "PDF" || err != nil {
				t.Errorf("spec.pdf = %q, %v", b, err)
			}
		}
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{"a.pdf": "a.pdf", "../etc": ".._etc", "..": "attachment-3", "a`b": "a_b", " x ": "x"} {
		if got := fileName(3, in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInTree(t *testing.T) {
	for issue, want := range map[string]bool{"CSR-1": true, "CSR-1.2": true, "CSR-1.2.3": true, "CSR-10": false, "CSR-1.": false, "CSR-1.x": false, "CSR-2": false} {
		if got := inTree("CSR-1", issue); got != want {
			t.Errorf("inTree(CSR-1, %s) = %v", issue, got)
		}
	}
}

func TestReportsBlocked(t *testing.T) {
	for msg, want := range map[string]bool{"Blocked: no access": true, "**Blocked** by x": true, "- blocked": true, "Not blocked": false, "Done.": false} {
		if got := reportsBlocked(msg); got != want {
			t.Errorf("reportsBlocked(%q) = %v", msg, got)
		}
	}
}

// A first request the app does not answer is tried three times and never
// queued: the run did not start, a resumed kept session stays kept, and
// nothing is pushed.
func TestAFirstNextTheAppDoesNotAnswerIsNotQueued(t *testing.T) {
	f := newFixture(t)
	f.ws.FastForwards = []error{nil}
	f.p.saveNote("CSR-1", Note{Dir: t.TempDir(), Session: "s0", Client: "claude", Origin: "kept"})
	app := &fakeApp{onNext: func(int, string, cinderline.NextRequest) reply { return reply{503, `{"message":"deploying"}`} }}
	c := app.client(t)
	c.Outbox = filepath.Join(t.TempDir(), "outbox.json")
	started, err := f.p.run(context.Background(), c, proj, soloJob())
	if started || err == nil {
		t.Errorf("run = %v, %v; want not started with the error", started, err)
	}
	if n := len(app.requests()); n != 3 {
		t.Errorf("%d requests, want 3 tries", n)
	}
	if q := c.Queued(); len(q) != 0 {
		t.Errorf("queued %v", q)
	}
	if slices.Contains(methods(f.ws), "Push") {
		t.Errorf("workspace calls %v; want no push", methods(f.ws))
	}
	if n, ok := f.p.readNote("CSR-1"); !ok || n.Session != "s0" || n.Origin != "kept" {
		t.Errorf("note = %+v, %v; want the kept session back", n, ok)
	}
}

// blockingEnv runs every container until its context ends.
type blockingEnv struct {
	environment.Fake
	started chan struct{}
}

func (b *blockingEnv) Run(ctx context.Context, _ environment.Spec) (string, error) {
	close(b.started)
	<-ctx.Done()
	return "", errors.New("killed")
}

// The poll's stop cancels the check in flight; the step reports stopped.
func TestAStopEndsTheCheckAsStopped(t *testing.T) {
	f := newFixture(t)
	env := &blockingEnv{started: make(chan struct{})}
	f.p.Env = env
	app := &fakeApp{onNext: stepsOf(`{"kind":"check","issue":{"ref":"CSR-1","title":"Fix it"}}`)}
	c := app.client(t)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = f.p.run(context.Background(), c, proj, soloJob()) })
	<-env.started
	f.p.stopTurn("CSR-1")
	wg.Wait()
	if lasts := app.lasts(); len(lasts) != 1 || lasts[0].Step != "check" || lasts[0].Outcome != "stopped" {
		t.Errorf("lasts = %+v", lasts)
	}
}

// stoppingForge stops the run while it opens the pull request.
type stoppingForge struct {
	*forge.Fake
	stop func()
}

func (s stoppingForge) OpenPullRequest(ctx context.Context, in forge.PullRequestInput) (forge.PullRequest, error) {
	s.stop()
	return s.Fake.OpenPullRequest(ctx, in)
}

// A stop during the publish, before the runner asked the forge to merge,
// lets the push and the pull request finish, but the merge is not asked for
// and merge stays null.
func TestAStopDuringThePublishSkipsTheMerge(t *testing.T) {
	f := newFixture(t)
	f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
	f.fg.Merges = []error{nil}
	f.fg.MergeStates = []forge.MergeStateReply{{State: forge.MergeMerged}}
	f.p.Forge = func(string) (forge.Forge, error) {
		return stoppingForge{f.fg, func() { f.p.stopTurn("CSR-1") }}, nil
	}
	app := &fakeApp{onNext: stepsOf(`{"kind":"publish","merge":true}`)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	want := cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "published", PullRequest: ptr("u"), Commit: ptr("abc123")}
	if lasts := app.lasts(); len(lasts) != 1 || !sameLast(lasts[0], want) {
		t.Errorf("lasts = %+v", lasts)
	}
	for _, c := range f.fg.Calls() {
		if c.Method == "RequestMerge" {
			t.Error("the merge was asked for after the stop")
		}
	}
}

// stopWhileWaiting stops the run while it waits for the merge it asked for.
type stopWhileWaiting struct {
	*forge.Fake
	stop func()
}

func (s stopWhileWaiting) MergeState(ctx context.Context, branch string) (forge.MergeState, error) {
	s.stop()
	return s.Fake.MergeState(ctx, branch)
}

// Willem's decision of 2026-11-17: a stop while the runner waits for a
// merge it has already asked the forge for reports merge "pending" (asked,
// not seen yet), not null.
func TestAStopWhileWaitingForTheMergeReportsItPending(t *testing.T) {
	f := newFixture(t)
	f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
	f.fg.Merges = []error{nil}
	f.fg.MergeStates = []forge.MergeStateReply{{State: forge.MergePending}}
	f.p.Forge = func(string) (forge.Forge, error) {
		return stopWhileWaiting{f.fg, func() { f.p.stopTurn("CSR-1") }}, nil
	}
	app := &fakeApp{onNext: stepsOf(`{"kind":"publish","merge":true}`)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	want := cinderline.LastStep{Step: "publish", Issue: "CSR-1", Outcome: "published", PullRequest: ptr("u"), Commit: ptr("abc123"), Merge: ptr("pending")}
	if lasts := app.lasts(); len(lasts) != 1 || !sameLast(lasts[0], want) {
		t.Errorf("lasts = %+v", lasts)
	}
}

// Before a fresh clone replaces the issue's clone, what it holds is
// committed and pushed; when the push fails the clone stays and the run
// does not start.
func TestAFreshCloneKeepsTheWorkOfTheOldOne(t *testing.T) {
	for _, pushErr := range []error{nil, errors.New("denied")} {
		f := newFixture(t)
		f.ws.Clones = []string{"/runs/CSR-1"}
		f.ws.Strandeds = []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-1", Branch: "cinderline/CSR-1"}}}}
		f.ws.Pushes = append([]error{pushErr}, f.ws.Pushes...)
		app := &fakeApp{onNext: stepsOf(`{"kind":"done","session":"paused"}`)}
		started, err := f.run(t, app)
		calls := f.ws.Calls()
		if len(calls) < 4 || calls[1].Method != "Commit" || calls[1].Args[0] != "/runs/CSR-1" || calls[1].Args[2] != "CSR-1: Fix it" || calls[3].Method != "Push" {
			t.Fatalf("push error %v: workspace calls %v; want Exists, Commit, Stranded, Push first", pushErr, methods(f.ws))
		}
		prepared := slices.Contains(methods(f.ws), "Prepare")
		if pushErr == nil && (!started || err != nil || !prepared) {
			t.Errorf("run = %v, %v, prepared %v; want a fresh clone after the push", started, err, prepared)
		}
		if pushErr != nil && (started || err == nil || !strings.Contains(err.Error(), "kept") || prepared) {
			t.Errorf("run = %v, %v, prepared %v; want the clone kept and no run", started, err, prepared)
		}
	}
}
