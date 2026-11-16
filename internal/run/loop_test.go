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
	f.p.AddRefused()
	f.p.saveNote("CSR-9", Note{Dir: "/runs/CSR-9", Session: "s", Client: "claude", Origin: "kept"})
	f.ws.Strandeds = []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-8", Branch: "cinderline/CSR-8"}}}}
	app := &fakeApp{}
	polls := runLoop(t, f, app, nil, nil, stopAt(2, answer(t, proj, `"held":[{"ref":"CSR-9","session":"keep"},{"ref":"CSR-8","session":"keep"}],"review":[]`)))
	first := polls[0]
	r := first.Report
	if r.RunnerVersion != "0.0.0-test" || len(r.Clients) != 1 || !slices.Equal(r.Environments, Environments) || r.Errors == nil || len(r.Errors) != 0 || r.RefusedConnections != 1 {
		t.Errorf("report = %+v", r)
	}
	if first.Running == nil || len(first.Running) != 0 || !first.Idle {
		t.Errorf("running %v, idle %v", first.Running, first.Idle)
	}
	if !slices.Equal(first.Held, []string{"CSR-9", "CSR-8"}) || first.Review == nil || len(first.Review) != 0 {
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
	running := 0
	polls := runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case n == 0:
			return answer(t, proj, jobJSON)
		case slices.Contains(req.Running, "CSR-1"):
			running++
			return answer(t, proj, `"stop":["CSR-1"]`)
		case running > 0:
			stop() // the run ended
		}
		return answer(t, proj, "")
	})
	if polls[1].Idle || !slices.Equal(polls[1].Running, []string{"CSR-1"}) {
		t.Errorf("poll after the job: running %v, idle %v", polls[1].Running, polls[1].Idle)
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
