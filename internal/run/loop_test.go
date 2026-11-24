package run

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// answer is a poll answer with the project and extra, a JSON fragment of more fields.
func answer(t *testing.T, project cinderline.Project, extra string) reply {
	t.Helper()
	b, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		extra = "," + extra
	}
	return reply{200, `{"poll_after":30,"review_after":300,"project":` + string(b) + extra + `}`}
}

const jobJSON = `"job":{"issue":{"ref":"CSR-1","title":"Fix it","body":"Please."},"team":{"name":"Dev","max_minutes":10,"max_turns":50,"agents":[{"name":"Coder","client":"claude"}]}}`

// runLoop runs the project's loop against app, whose polls answerOf
// answers; stop ends the loop. It fails the test when the loop does not
// end within ten seconds.
func runLoop(t *testing.T, f *fixture, app *fakeApp, c *cinderline.Client, drain <-chan struct{}, answerOf func(n int, req cinderline.PollRequest, stop func()) reply) []cinderline.PollRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	app.onPoll = func(n int, req cinderline.PollRequest) reply { return answerOf(n, req, cancel) }
	if c == nil {
		c = app.client(t)
	}
	f.p.Loop(ctx, c, drain)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("the loop did not end")
	}
	return app.pollsSoFar()
}

// stopAt ends the loop at the nth poll and answers every poll with r.
func stopAt(n int, r reply) func(int, cinderline.PollRequest, func()) reply {
	return func(i int, _ cinderline.PollRequest, stop func()) reply {
		if i == n {
			stop()
		}
		return r
	}
}

// The poll carries the whole report, what the runner holds and, when due,
// held and review; an empty review the first time. Held issues are tidied.
func TestThePollRequest(t *testing.T) {
	f := newFixture(t)
	f.p.clients = []cinderline.ReportClient{{Name: "claude", State: "ready"}}
	f.p.AddRefused("CSR-9", "evil.example:443")
	f.p.StrictHosts = true
	f.p.saveNote("CSR-9", Note{Dir: "/runs/CSR-9", Session: "s", Client: "claude", Origin: "kept"})
	f.ws.Strandeds = []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-8", Branch: "cinderline/CSR-8"}}}}
	f.ws.Clones = []string{"/runs/CSR-8"}
	app := &fakeApp{}
	polls := runLoop(t, f, app, nil, nil, stopAt(2, answer(t, proj, `"held":[{"ref":"CSR-9","session":"keep"},{"ref":"CSR-8","session":"keep"}],"review":[]`)))
	first := polls[0]
	r := first.Report
	if r.RunnerVersion != "0.0.0-test" || len(r.Clients) != 1 || !slices.Equal(r.Environments, Environments) || r.Errors == nil || len(r.Errors) != 0 || r.RefusedConnections != 1 || !r.StrictHosts ||
		r.ModelProviders == nil || len(r.ModelProviders) != 0 {
		t.Errorf("report = %+v", r)
	}
	// Not idle while it asks for the review list: the pass comes first.
	if first.Running == nil || len(first.Running) != 0 || first.Idle || !polls[1].Idle {
		t.Errorf("running %v, idle %v then %v", first.Running, first.Idle, polls[1].Idle)
	}
	if !slices.Equal(first.Held, []string{"CSR-8", "CSR-9"}) || first.Review == nil || len(first.Review) != 0 {
		t.Errorf("held %v, review %v; want both, review empty", first.Held, first.Review)
	}
	if second := polls[1]; second.Held != nil || second.Review != nil {
		t.Errorf("second poll held %v, review %v; want neither so soon", second.Held, second.Review)
	}
	if !slices.Contains(methods(f.ws), "Push") {
		t.Error("the stranded clone was not pushed")
	}
}

// A job starts a run that is running at once; the poll's stop ends its
// turn as stopped, and the app's steps end the run.
func TestAJobRunsAndAStopEndsItsTurn(t *testing.T) {
	f := newFixture(t)
	f.p.Clients["claude"] = &blocking{started: make(chan struct{})}
	app := &fakeApp{onNext: stepsOf(runNew, `{"kind":"stop"}`, `{"kind":"done","session":"paused"}`)}
	running, offered := 0, -1
	polls := runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case offered < 0 && req.Idle:
			offered = n
			return answer(t, proj, jobJSON)
		case slices.Contains(req.Running, "CSR-1"):
			running++
			return answer(t, proj, `"stop":["CSR-1"]`)
		case running > 0:
			stop() // the run ended
		}
		return answer(t, proj, "")
	})
	if after := polls[offered+1]; after.Idle || !slices.Equal(after.Running, []string{"CSR-1"}) {
		t.Errorf("poll after the job: running %v, idle %v", after.Running, after.Idle)
	}
	lasts := app.lasts()
	if len(lasts) != 2 || lasts[0].Outcome != "stopped" || lasts[1].Step != "stop" {
		t.Errorf("lasts = %+v", lasts)
	}
	if st := f.p.Status(); !st.Connected || st.Issue != "" || st.LastPoll.IsZero() {
		t.Errorf("status = %+v", st)
	}
}

// Drain takes no new job, lets the run in flight finish, and then the loop ends.
func TestDrainLetsTheRunFinish(t *testing.T) {
	f := newFixture(t)
	f.p.Clients["claude"] = &blocking{started: make(chan struct{})}
	app := &fakeApp{onNext: stepsOf(runNew)}
	drain := make(chan struct{})
	drained := false
	polls := runLoop(t, f, app, nil, drain, func(n int, req cinderline.PollRequest, _ func()) reply {
		if n > 0 && slices.Contains(req.Running, "CSR-1") {
			if !drained {
				close(drain)
				drained = true
			}
			return answer(t, proj, jobJSON+`,"stop":["CSR-1"]`)
		}
		return answer(t, proj, jobJSON) // offered again and again
	})
	runs := 0
	for _, r := range app.requests() {
		if r.State == nil {
			runs++
		}
	}
	if runs != 1 {
		t.Errorf("%d runs, want 1", runs)
	}
	if last := polls[len(polls)-1]; last.Idle {
		t.Error("a draining runner said it was idle")
	}
}

// A drain that comes while a poll is out leaves the job it answers: a job
// is an offer, and a run that starts after a reload must have the new
// config (decision 2026-11-24-rolling-reload).
func TestDrainDuringAPollLeavesItsJob(t *testing.T) {
	f := newFixture(t)
	f.p.Clients["claude"] = &blocking{started: make(chan struct{})}
	app := &fakeApp{onNext: stepsOf(runNew)}
	drain := make(chan struct{})
	runLoop(t, f, app, nil, drain, func(_ int, req cinderline.PollRequest, _ func()) reply {
		if req.Idle {
			close(drain)
		}
		return answer(t, proj, jobJSON)
	})
	if reqs := app.requests(); len(reqs) != 0 {
		t.Errorf("a draining loop started a run: %+v", reqs)
	}
}

// A 426 shows in the status and waits an hour.
func TestUpgradeRequired(t *testing.T) {
	f := newFixture(t)
	app := &fakeApp{}
	var waits []time.Duration
	old := waitFor
	t.Cleanup(func() { waitFor = old })
	waitFor = func(d time.Duration) time.Duration { waits = append(waits, d); return time.Millisecond }
	runLoop(t, f, app, nil, nil, stopAt(1, reply{426, `{"message":"too old","minimum_runner_version":"9.9.9"}`}))
	if len(waits) == 0 || waits[0] != upgradeWait {
		t.Errorf("waits = %v, want an hour first", waits)
	}
	if st := f.p.Status(); st.Connected || !strings.Contains(st.LastError, "9.9.9") {
		t.Errorf("status = %+v", st)
	}
}

// A failed poll backs off from a second and shows in the status.
func TestAFailedPollBacksOff(t *testing.T) {
	f := newFixture(t)
	app := &fakeApp{}
	var waits []time.Duration
	old := waitFor
	t.Cleanup(func() { waitFor = old })
	waitFor = func(d time.Duration) time.Duration { waits = append(waits, d); return time.Millisecond }
	runLoop(t, f, app, nil, nil, stopAt(3, reply{503, `{"message":"down"}`}))
	if !slices.Equal(waits[:3], []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Errorf("waits = %v, want 1s doubling", waits)
	}
	if st := f.p.Status(); st.Connected || !strings.Contains(st.LastError, "503") {
		t.Errorf("status = %+v", st)
	}
}

// A run that does not start is reported with its issue, and no job is
// taken for a while.
func TestARunThatDoesNotStartIsReported(t *testing.T) {
	f := newFixture(t)
	delete(f.p.Clients, "claude")
	app := &fakeApp{}
	polls := runLoop(t, f, app, nil, nil, stopAt(2, answer(t, proj, jobJSON)))
	errs := polls[2].Report.Errors
	if len(errs) != 1 || errs[0].Kind != cinderline.KindNotStarted || errs[0].Issue != "CSR-1" || polls[2].Idle {
		t.Errorf("report errors %+v, idle %v", errs, polls[2].Idle)
	}
	if len(app.requests()) != 0 {
		t.Error("a run that did not start called next")
	}
}

// Another repository than the configured one is reported, and no job is taken.
func TestARepositoryMismatchTakesNoJob(t *testing.T) {
	f := newFixture(t)
	other := proj
	other.RepositoryURL = "https://evil.example/o/r"
	app := &fakeApp{}
	polls := runLoop(t, f, app, nil, nil, stopAt(1, answer(t, other, jobJSON)))
	errs := polls[1].Report.Errors
	if len(errs) != 1 || errs[0].Kind != cinderline.KindRepositoryMismatch || polls[1].Idle {
		t.Errorf("report errors %+v, idle %v", errs, polls[1].Idle)
	}
	if len(app.requests()) != 0 {
		t.Error("a mismatched project ran a job")
	}
}

// A queued report is sent before the poll; until it is delivered its run is running.
func TestTheOutboxIsReplayedBeforeEachPoll(t *testing.T) {
	f := newFixture(t)
	down := true
	app := &fakeApp{onNext: func(int, string, cinderline.NextRequest) reply {
		if down {
			return reply{503, ""}
		}
		return reply{200, `{"state":"x","step":{"kind":"done","session":"kept"}}`}
	}}
	c := app.client(t)
	c.Outbox = filepath.Join(t.TempDir(), "outbox.json")
	c.Queue("CSR-5", cinderline.NextRequest{ID: "id-5", Sessions: []cinderline.Session{}})
	polls := runLoop(t, f, app, c, nil, func(n int, _ cinderline.PollRequest, stop func()) reply {
		if n == 0 {
			down = false
		} else {
			stop()
		}
		return answer(t, proj, "")
	})
	if !slices.Equal(polls[0].Running, []string{"CSR-5"}) || len(polls[1].Running) != 0 {
		t.Errorf("running %v then %v", polls[0].Running, polls[1].Running)
	}
	if reqs := app.requests(); len(reqs) != 2 || reqs[1].ID != "id-5" {
		t.Errorf("next requests %+v", reqs)
	}
}

// The runner is not idle before client detection has a result, so the app
// never judges its clients by an empty report.
func TestNotIdleBeforeDetection(t *testing.T) {
	f := newFixture(t)
	f.p.clients = nil
	f.p.detect = func(context.Context) ([]cinderline.ReportClient, bool) {
		return []cinderline.ReportClient{{Name: "claude", State: "ready"}}, false
	}
	polls := runLoop(t, f, &fakeApp{}, nil, nil, func(n int, _ cinderline.PollRequest, stop func()) reply {
		switch n {
		case 1:
			f.p.Detect(context.Background())
		case 2:
			stop()
		}
		return answer(t, proj, "")
	})
	if polls[1].Idle || len(polls[1].Report.Clients) != 0 || !polls[2].Idle || len(polls[2].Report.Clients) != 1 {
		t.Errorf("idle %v with %v, then %v with %v", polls[1].Idle, polls[1].Report.Clients, polls[2].Idle, polls[2].Report.Clients)
	}
}

// blockingForge holds the upkeep pass in its first forge call until release.
type blockingForge struct {
	*forge.Fake
	started, release chan struct{}
}

func (b blockingForge) MergeState(ctx context.Context, branch string) (forge.PullState, error) {
	close(b.started)
	<-b.release
	return forge.PullState{}, nil
}

// While an upkeep pass runs the runner is not idle, so no job starts on the
// clones it works on, and no tidying or second pass starts either.
func TestAnUpkeepPassExcludesJobsAndTidying(t *testing.T) {
	f := newFixture(t)
	f.ws.Behinds = []workspace.BehindReply{{}}
	fg := blockingForge{&forge.Fake{}, make(chan struct{}), make(chan struct{})}
	f.p.Forge = func(string) (forge.Forge, error) { return fg, nil }
	polls := runLoop(t, f, &fakeApp{}, nil, nil, func(n int, _ cinderline.PollRequest, stop func()) reply {
		switch n {
		case 0:
			return answer(t, proj, `"review":[{"ref":"CSR-7","title":"T","merge":false}]`)
		case 1:
			<-fg.started
		case 3:
			close(fg.release)
			stop()
		}
		return answer(t, proj, jobJSON+`,"review":[]`)
	})
	for _, p := range polls[1:3] {
		if p.Idle || p.Held != nil || p.Review != nil {
			t.Errorf("poll during the pass: idle %v, held %v, review %v", p.Idle, p.Held, p.Review)
		}
	}
	if len(f.cl.Calls()) != 0 {
		t.Error("a job ran during the pass")
	}
}

// While a run is in flight no upkeep pass and no tidying start.
func TestARunExcludesUpkeepAndTidying(t *testing.T) {
	f := newFixture(t)
	app := &fakeApp{onPoll: func(int, cinderline.PollRequest) reply { return answer(t, proj, "") }}
	l := &loop{p: f.p, c: app.client(t), ctx: context.Background(), runDone: make(chan ended, 1), upkeepDone: make(chan []cinderline.ReviewFact, 1), review: time.Minute, running: "CSR-1"}
	l.poll()
	if p := app.pollsSoFar()[0]; p.Idle || p.Held != nil || p.Review != nil || l.upkeep {
		t.Errorf("poll during a run: idle %v, held %v, review %v", p.Idle, p.Held, p.Review)
	}
}

// The first detection's result brings the next poll forward, so a job need
// not wait a whole interval after a start.
func TestTheFirstDetectionPollsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.p.clients, f.p.detected = nil, make(chan struct{})
	f.p.detect = func(context.Context) ([]cinderline.ReportClient, bool) { return nil, false }
	old := waitFor
	t.Cleanup(func() { waitFor = old })
	waitFor = func(time.Duration) time.Duration { return time.Hour }
	polls := runLoop(t, f, &fakeApp{}, nil, nil, func(n int, _ cinderline.PollRequest, stop func()) reply {
		if n == 0 {
			go f.p.Detect(context.Background())
		} else {
			stop()
		}
		return answer(t, proj, "")
	})
	if len(polls) != 2 || polls[0].Idle || !polls[1].Idle {
		t.Errorf("%d polls; want a second, idle one at once", len(polls))
	}
}

// Decision 2026-11-17-local-model-providers point 4: the report names the
// providers OpenCode agents can use: openrouter when detection found its key
// usable, then each of the runner's, by name only.
func TestTheReportNamesModelProviders(t *testing.T) {
	f := newFixture(t)
	f.p.Providers = []config.Provider{{Name: "cerebellum", BaseURL: "http://localhost:8080/v1"}, {Name: "lan", BaseURL: "http://provider.example:8080/v1"}}
	f.p.openRouter = true
	l := &loop{p: f.p, c: cinderline.New("http://x", "t", "0")}
	if got := l.report().ModelProviders; !slices.Equal(got, []string{"openrouter", "cerebellum", "lan"}) {
		t.Errorf("model_providers = %q", got)
	}
	f.p.openRouter = false
	if got := l.report().ModelProviders; !slices.Equal(got, []string{"cerebellum", "lan"}) {
		t.Errorf("model_providers = %q", got)
	}
}

func TestPollReportsConfiguredFreeProviders(t *testing.T) {
	f := newFixture(t)
	f.p.Providers = []config.Provider{
		{Name: "local", BaseURL: "http://localhost:8080/v1", Free: true},
		{Name: "paid", BaseURL: "https://provider.example/v1"},
		{Name: "lan", BaseURL: "http://lan.example:8080/v1", Free: true},
	}
	f.p.openRouter = true
	polls := runLoop(t, f, &fakeApp{}, nil, nil, stopAt(0, answer(t, proj, "")))
	got := polls[0].Report
	if !slices.Equal(got.ModelProviders, []string{"openrouter", "local", "paid", "lan"}) ||
		!slices.Equal(got.FreeProviders, []string{"local", "lan"}) {
		t.Errorf("model_providers = %q, free_providers = %q", got.ModelProviders, got.FreeProviders)
	}
}
