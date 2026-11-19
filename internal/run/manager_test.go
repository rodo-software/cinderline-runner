package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLR-194: a Project Manager run (decision 2026-11-17-project-manager in
// the app repository) goes in its own slot next to the team's run, reads a
// fresh clone of the base and never commits or pushes.

const managerJobJSON = `"manager_job":{"issue":{"ref":"CSR-2","title":"Plan","body":"Plan it."},"team":{"name":"Project Manager","max_minutes":10,"max_turns":50,"agents":[{"name":"Project Manager","client":"claude"}]}}`

const managerRun = `{"kind":"run","issue":{"ref":"CSR-2","title":"Plan"},"agent":"Project Manager","session":"new","token":"manager-token-1234567890","prompt":"Plan it."}`

// managerJob is a Project Manager job: a team of one agent.
func managerJob() cinderline.Job {
	return cinderline.Job{
		Issue: cinderline.JobIssue{Ref: "CSR-2", Title: "Plan", Body: "Plan it."},
		Team:  cinderline.Team{Name: "Project Manager", MaxMinutes: 10, MaxTurns: 50, Agents: []cinderline.TeamAgent{{Name: "Project Manager", Client: "claude"}}},
	}
}

// perRef answers next with each run's own steps in order, then with done kept.
func perRef(steps map[string][]string) func(int, string, cinderline.NextRequest) reply {
	var mu sync.Mutex
	n := map[string]int{}
	return func(_ int, ref string, _ cinderline.NextRequest) reply {
		mu.Lock()
		defer mu.Unlock()
		i := n[ref]
		n[ref]++
		step := `{"kind":"done","session":"kept"}`
		if i < len(steps[ref]) {
			step = steps[ref][i]
		}
		return reply{200, fmt.Sprintf(`{"state":"%s-%d","step":%s}`, ref, i+1, step)}
	}
}

// gate is a client whose turn in each dir waits until it is released or
// stopped; a released turn finishes in session "s-<REF>".
type gate struct {
	linger  time.Duration // how long a stopped turn takes to end
	mu      sync.Mutex
	started map[string]chan struct{}
	release map[string]chan struct{}
}

func newGate(dirs ...string) *gate {
	g := &gate{started: map[string]chan struct{}{}, release: map[string]chan struct{}{}}
	for _, d := range dirs {
		g.started[d], g.release[d] = make(chan struct{}), make(chan struct{})
	}
	return g
}

func (g *gate) Run(ctx context.Context, t client.Turn) (client.Result, error) {
	g.mu.Lock()
	started, release := g.started[t.Dir], g.release[t.Dir]
	g.mu.Unlock()
	close(started)
	select {
	case <-release:
		return client.Result{SessionID: "s-" + path.Base(t.Dir), Report: "Done."}, nil
	case <-ctx.Done():
		time.Sleep(g.linger)
		return client.Result{SessionID: "s-" + path.Base(t.Dir), Report: "half"}, errors.New("killed")
	}
}

func (g *gate) isStarted(dir string) bool {
	select {
	case <-g.started[dir]:
		return true
	default:
		return false
	}
}

// callsOn are the workspace's calls whose first argument is dir, by name.
func callsOn(ws *workspace.Fake, dir string) []string {
	var out []string
	for _, c := range ws.Calls() {
		if len(c.Args) > 0 && c.Args[0] == dir {
			out = append(out, c.Method)
		}
	}
	return out
}

// lastsOf are the last facts of ref's next requests.
func lastsOf(app *fakeApp, ref string) []cinderline.LastStep {
	app.mu.Lock()
	defer app.mu.Unlock()
	var out []cinderline.LastStep
	for _, n := range app.nexts {
		if n.ref == ref && n.req.Last != nil {
			out = append(out, *n.req.Last)
		}
	}
	return out
}

// A manager job starts while a team run is going: the poll says the slot
// is free, the manager's ref joins running, the status names both runs,
// and the manager's clone is removed at its end without a commit or push.
func TestAManagerJobStartsWhileATeamRunIsGoing(t *testing.T) {
	f := newFixture(t)
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-1"}, {Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	g := newGate("/runs/CSR-1", "/runs/CSR-2")
	f.p.Clients["claude"] = g
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}, "CSR-2": {managerRun}})}
	var offered, both cinderline.PollRequest
	var during, after localapi.ProjectStatus
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			return answer(t, proj, jobJSON)
		case phase == 1 && g.isStarted("/runs/CSR-1"):
			phase, offered = 2, req
			return answer(t, proj, managerJobJSON)
		case phase == 2 && g.isStarted("/runs/CSR-2"):
			phase, both, during = 3, req, f.p.Status()
			close(g.release["/runs/CSR-1"])
			close(g.release["/runs/CSR-2"])
		case phase == 3 && len(req.Running) == 0:
			after = f.p.Status()
			stop()
		}
		return answer(t, proj, "")
	})
	if phase != 3 {
		t.Fatalf("stopped in phase %d", phase)
	}
	if !slices.Equal(offered.Running, []string{"CSR-1"}) || offered.Idle || !offered.ManagerIdle {
		t.Errorf("poll that got the manager job: running %v, idle %v, manager_idle %v", offered.Running, offered.Idle, offered.ManagerIdle)
	}
	if !slices.Equal(both.Running, []string{"CSR-1", "CSR-2"}) || both.Idle || both.ManagerIdle {
		t.Errorf("poll during both runs: running %v, idle %v, manager_idle %v", both.Running, both.Idle, both.ManagerIdle)
	}
	if during.Issue != "CSR-1" || during.Manager != "CSR-2" {
		t.Errorf("status during both runs: issue %q, manager %q", during.Issue, during.Manager)
	}
	if after.Issue != "" || after.Manager != "" {
		t.Errorf("status after both runs: issue %q, manager %q", after.Issue, after.Manager)
	}
	if l := lastsOf(app, "CSR-2"); len(l) != 1 || l[0].Outcome != "finished" || l[0].Issue != "CSR-2" {
		t.Errorf("manager lasts = %+v", l)
	}
	if got := callsOn(f.ws, "/runs/CSR-2"); !slices.Equal(got, []string{"Remove"}) {
		t.Errorf("calls on the manager's clone = %v, want only Remove", got)
	}
	if n, ok := f.p.readNote("CSR-2"); !ok || !n.Manager || n.Session != "s-CSR-2" || n.Origin != "kept" {
		t.Errorf("manager note = %+v, %v", n, ok)
	}
}

// While a manager run goes, no second manager job starts, though the poll
// keeps offering one; a team job still does, and no tidying or upkeep pass
// starts on the clones while the manager's run is in flight.
func TestNoSecondManagerJobWhileOneRuns(t *testing.T) {
	f := newFixture(t)
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-2"}, {Dir: "/runs/CSR-1"}}
	f.ws.Removes = []error{nil}
	g := newGate("/runs/CSR-1", "/runs/CSR-2")
	f.p.Clients["claude"] = g
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}, "CSR-2": {managerRun}})}
	second := strings.Replace(managerJobJSON, "CSR-2", "CSR-3", 1)
	var whileManager []cinderline.PollRequest
	var afterTeam *cinderline.PollRequest
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		if slices.Contains(req.Running, "CSR-2") {
			whileManager = append(whileManager, req)
		}
		switch {
		case phase == 0 && req.ManagerIdle:
			phase = 1
			return answer(t, proj, managerJobJSON)
		case phase == 1 && g.isStarted("/runs/CSR-2"):
			if g.isStarted("/runs/CSR-1") {
				phase = 2
				close(g.release["/runs/CSR-1"])
			}
			return answer(t, proj, jobJSON+","+second)
		case phase == 2 && !slices.Contains(req.Running, "CSR-1"):
			phase, afterTeam = 3, &req
			close(g.release["/runs/CSR-2"])
			return answer(t, proj, second)
		case phase == 3 && len(req.Running) == 0:
			stop()
		}
		return answer(t, proj, "")
	})
	if phase != 3 {
		t.Fatalf("stopped in phase %d", phase)
	}
	if len(whileManager) == 0 {
		t.Fatal("no poll while the manager ran")
	}
	for _, req := range whileManager {
		if req.ManagerIdle {
			t.Errorf("poll while the manager ran said manager_idle: %+v", req)
		}
	}
	if afterTeam == nil || afterTeam.Held != nil || afterTeam.Review != nil {
		t.Errorf("poll after the team run, during the manager's: %+v; want no held, no review", afterTeam)
	}
	app.mu.Lock()
	for _, n := range app.nexts {
		if n.ref == "CSR-3" {
			t.Errorf("a second manager job started: %+v", n)
		}
	}
	app.mu.Unlock()
	if l := lastsOf(app, "CSR-1"); len(l) != 1 || l[0].Outcome != "finished" {
		t.Errorf("team lasts = %+v", l)
	}
}

// An answer without manager_job, as an app before CL-241 sends, runs the
// job alone; the poll still says the slot is free throughout.
func TestAnAnswerWithoutManagerJobRunsTheJobAlone(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("s", "Done.")}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"done","session":"paused"}`)}
	offered := -1
	polls := runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case offered < 0 && req.Idle:
			offered = n
			return answer(t, proj, jobJSON)
		case offered >= 0 && len(req.Running) == 0 && n > offered+1:
			stop()
		}
		return answer(t, proj, "")
	})
	for i, p := range polls[1:] {
		if !p.ManagerIdle {
			t.Errorf("poll %d: manager_idle false with no manager run", i+1)
		}
	}
	if l := app.lasts(); len(l) != 1 || l[0].Outcome != "finished" {
		t.Errorf("lasts = %+v", l)
	}
	if st := f.p.Status(); st.Manager != "" {
		t.Errorf("status names manager run %q", st.Manager)
	}
}

// A manager run's stop does not commit or push, even when the agent changed
// files: it reports not_pushed. Its run starts from a fresh clone, without
// keeping earlier work or merging the base, and removes it at the end.
func TestAManagerRunsStopDoesNotPush(t *testing.T) {
	f := newFixture(t)
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	f.ws.Clones = []string{"/runs/CSR-2"} // left by an earlier run: replaced, never pushed
	f.cl.Runs = []client.RunReply{ok("s-2", "Planned.")}
	app := &fakeApp{onNext: stepsOf(managerRun, `{"kind":"stop"}`, `{"kind":"done","session":"paused"}`)}
	started, err := f.p.run(context.Background(), app.client(t), proj, managerJob(), true)
	if !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	reqs := app.requests()
	if len(reqs) != 3 || reqs[0].Workspace.Branch != "cinderline/CSR-2" || reqs[0].Workspace.Base != "main" || reqs[0].Last != nil {
		t.Fatalf("requests = %+v", reqs)
	}
	stop := reqs[2].Last
	if stop.Step != "stop" || stop.Issue != "CSR-2" || stop.Outcome != "not_pushed" || stop.Commit != nil || stop.Secrets != nil {
		t.Errorf("stop facts = %+v", stop)
	}
	if got := methods(f.ws); !slices.Equal(got, []string{"Prepare", "Remove"}) {
		t.Errorf("workspace calls = %v, want Prepare and Remove only", got)
	}
	if c := f.ws.Calls(); c[0].Args[0] != "cinderline/CSR-2" || c[0].Args[1] != "main" || c[1].Args[0] != "/runs/CSR-2" {
		t.Errorf("workspace calls = %+v", c)
	}
	if tr := turns(f.cl); len(tr) != 1 || tr[0].Dir != "/runs/CSR-2" || tr[0].Token != "manager-token-1234567890" || tr[0].Attachments != attachmentsDir(f.p.State, cfg, "CSR-2") {
		t.Errorf("turns = %+v", tr)
	}
	if n, ok := f.p.readNote("CSR-2"); !ok || !n.Manager || n.Session != "s-2" || n.Origin != "paused" || n.Client != "claude" {
		t.Errorf("note = %+v, %v", n, ok)
	}
}

// A manager run is never checked, published or merged: such a step ends the
// run with the runner's own stop, which does not push either.
func TestAManagerRunRefusesCheckPublishAndResolve(t *testing.T) {
	for _, step := range []string{
		`{"kind":"check","issue":{"ref":"CSR-2","title":"Plan"}}`,
		`{"kind":"publish"}`,
		`{"kind":"resolve","agent":"Project Manager","session":"new","prompt":"Merge."}`,
	} {
		f := newFixture(t)
		f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-2"}}
		f.ws.Removes = []error{nil}
		app := &fakeApp{onNext: stepsOf(step, `{"kind":"done","session":"paused"}`)}
		if _, err := f.p.run(context.Background(), app.client(t), proj, managerJob(), true); err != nil {
			t.Fatal(err)
		}
		l := app.lasts()
		if len(l) != 1 || l[0].Step != "stop" || l[0].Outcome != "not_pushed" || !strings.Contains(deref(l[0].Error), "Project Manager") {
			t.Errorf("%s: lasts = %+v", step, l)
		}
		if got := methods(f.ws); !slices.Equal(got, []string{"Prepare", "Remove"}) {
			t.Errorf("%s: workspace calls = %v", step, got)
		}
		if len(f.cl.Calls()) != 0 {
			t.Errorf("%s: a turn ran", step)
		}
	}
}

// A follow-up resumes the manager's kept session in a fresh clone of the
// base: the old clone need not be there, and nothing is fast-forwarded.
func TestAManagerRunResumesItsSessionInAFreshClone(t *testing.T) {
	f := newFixture(t)
	f.p.saveNote("CSR-2", Note{Dir: "/runs/CSR-2", Session: "s-1", Client: "claude", Origin: "kept", Manager: true})
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	f.cl.Runs = []client.RunReply{ok("", "Answered.")}
	resume := strings.Replace(managerRun, `"session":"new"`, `"session":"resume"`, 1)
	app := &fakeApp{onNext: stepsOf(resume)}
	if _, err := f.p.run(context.Background(), app.client(t), proj, managerJob(), true); err != nil {
		t.Fatal(err)
	}
	reqs := app.requests()
	if s := reqs[0].Sessions; len(s) != 1 || s[0] != (cinderline.Session{Issue: "CSR-2", Agent: "Project Manager", Origin: "kept"}) {
		t.Errorf("first request sessions = %+v", s)
	}
	if tr := turns(f.cl); len(tr) != 1 || tr[0].SessionID != "s-1" {
		t.Errorf("turns = %+v", tr)
	}
	if l := app.lasts(); len(l) != 1 || l[0].Outcome != "finished" {
		t.Errorf("lasts = %+v", l)
	}
	if got := methods(f.ws); !slices.Equal(got, []string{"Prepare", "Remove"}) {
		t.Errorf("workspace calls = %v, want Prepare and Remove only", got)
	}
	if n, ok := f.p.readNote("CSR-2"); !ok || !n.Manager || n.Session != "s-1" || n.Origin != "kept" {
		t.Errorf("note = %+v, %v", n, ok)
	}
}

// Tidying never pushes or commits a manager's clone, stranded or not: it
// only deletes it, with the run's attachments folder.
func TestTidyNeverPushesAManagerClone(t *testing.T) {
	f := newFixture(t)
	f.p.saveNote("CSR-2", Note{Dir: "/runs/CSR-2", Session: "s-1", Client: "claude", Origin: "kept", Manager: true})
	f.ws.Clones = []string{"/runs/CSR-2"}
	f.ws.Removes = []error{nil}
	att := attachmentsDir(f.p.State, cfg, "CSR-2")
	if err := os.MkdirAll(att, 0o755); err != nil {
		t.Fatal(err)
	}
	stranded := []workspace.Clone{{Dir: "/runs/CSR-2", Branch: "cinderline/CSR-2"}}
	f.p.tidy(context.Background(), []string{"CSR-2"}, []cinderline.HeldItem{{Ref: "CSR-2", Session: "drop"}}, stranded)
	got := methods(f.ws)
	if slices.Contains(got, "Push") || slices.Contains(got, "Commit") || !slices.Contains(got, "Remove") {
		t.Errorf("workspace calls = %v; want Remove, no Commit or Push", got)
	}
	if _, ok := f.p.readNote("CSR-2"); ok {
		t.Error("note kept")
	}
	if _, err := os.Stat(att); !os.IsNotExist(err) {
		t.Errorf("attachments folder kept: %v", err)
	}
}

// The poll's stop ends only its own run's turn when two runs go at once.
func TestAStopEndsOnlyItsOwnRunsTurn(t *testing.T) {
	f := newFixture(t)
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-1"}, {Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	g := newGate("/runs/CSR-1", "/runs/CSR-2")
	f.p.Clients["claude"] = g
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew, `{"kind":"done","session":"paused"}`}, "CSR-2": {managerRun, `{"kind":"done","session":"paused"}`}})}
	c := app.client(t)
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = f.p.run(context.Background(), c, proj, soloJob(), false) })
	<-g.started["/runs/CSR-1"]
	wg.Go(func() { _, _ = f.p.run(context.Background(), c, proj, managerJob(), true) })
	<-g.started["/runs/CSR-2"]
	f.p.stopTurn("CSR-2")
	// The manager's run reports its stopped turn while the team's turn goes on.
	for deadline := time.Now().Add(5 * time.Second); len(lastsOf(app, "CSR-2")) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the manager's turn did not stop")
		}
	}
	if len(lastsOf(app, "CSR-1")) != 0 {
		t.Error("the team's turn ended with the manager's")
	}
	close(g.release["/runs/CSR-1"])
	wg.Wait()
	if l := lastsOf(app, "CSR-2"); len(l) != 1 || l[0].Outcome != "stopped" {
		t.Errorf("manager lasts = %+v", l)
	}
	if l := lastsOf(app, "CSR-1"); len(l) != 1 || l[0].Outcome != "finished" {
		t.Errorf("team lasts = %+v", l)
	}
}

// Refused hosts are each run's own: a step reports only its issue's.
func TestRefusedHostsArePerRun(t *testing.T) {
	f := newFixture(t)
	f.p.AddRefused("CSR-1", "a.example:443")
	f.p.AddRefused("CSR-2", "b.example:443")
	f.p.AddRefused("CSR-1", "A.example:443")
	if got := f.p.takeRefused("CSR-1"); !slices.Equal(got, []string{"a.example:443"}) {
		t.Errorf("CSR-1 refused %q", got)
	}
	if got := f.p.takeRefused("CSR-1"); got != nil {
		t.Errorf("CSR-1 refused %q again", got)
	}
	if got := f.p.takeRefused("CSR-2"); !slices.Equal(got, []string{"b.example:443"}) {
		t.Errorf("CSR-2 refused %q", got)
	}
	if f.p.refused != 3 {
		t.Errorf("refused count %d, want 3", f.p.refused)
	}
}

// A plain stop with both runs in flight ends the loop: it waits for each run once.
func TestAPlainStopEndsBothRuns(t *testing.T) {
	f := newFixture(t)
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-1"}, {Dir: "/runs/CSR-2"}}
	f.ws.Removes = []error{nil}
	g := newGate("/runs/CSR-1", "/runs/CSR-2")
	g.linger = 50 * time.Millisecond // so the loop sees the stop while both runs are still in flight
	f.p.Clients["claude"] = g
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}, "CSR-2": {managerRun}})}
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case g.isStarted("/runs/CSR-1") && g.isStarted("/runs/CSR-2"):
			stop()
		case req.Idle && req.ManagerIdle:
			return answer(t, proj, jobJSON+","+managerJobJSON)
		}
		return answer(t, proj, "")
	})
}

// A manager run that does not start holds off the next manager job, not the team's.
func TestAManagerRunThatDoesNotStartHoldsOffOnlyItsSlot(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{ok("s", "Done.")}
	broken := strings.Replace(managerJobJSON, `"client":"claude"`, `"client":"nope"`, 1)
	var after *cinderline.PollRequest
	app := &fakeApp{onNext: stepsOf(runNew)}
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case after != nil:
			stop()
		case len(req.Report.Errors) > 0:
			after = &req
			return answer(t, proj, jobJSON)
		case req.ManagerIdle:
			return answer(t, proj, broken)
		}
		return answer(t, proj, "")
	})
	if after == nil || after.ManagerIdle || !after.Idle || after.Report.Errors[0].Issue != "CSR-2" {
		t.Errorf("poll after the manager run did not start: %+v", after)
	}
}

// Tidying a team issue deletes its run's attachments folder too.
func TestTidyDeletesATeamIssuesAttachments(t *testing.T) {
	f := newFixture(t)
	f.ws.Removes = []error{nil}
	att := attachmentsDir(f.p.State, cfg, "CSR-1")
	if err := os.MkdirAll(att, 0o755); err != nil {
		t.Fatal(err)
	}
	f.p.tidy(context.Background(), []string{"CSR-1"}, []cinderline.HeldItem{{Ref: "CSR-1", Session: "drop"}}, nil)
	if _, err := os.Stat(att); !os.IsNotExist(err) {
		t.Errorf("attachments folder kept: %v", err)
	}
}
