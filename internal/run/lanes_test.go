package run

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLR-222, decision 2026-11-24-lanes in the app repository: a project with
// max_parallel_runs above 1 says so in its poll and runs each of the
// answer's jobs in its own team slot, clone and branch.

// laneJob is a solo job on ref as the answer's jobs list it.
func laneJob(ref string) string {
	return fmt.Sprintf(`{"issue":{"ref":%q,"title":"Fix it","body":"Please."},"team":{"name":"Dev","max_minutes":10,"max_turns":50,"agents":[{"name":"Coder","client":"claude"}]}}`, ref)
}

// jobsJSON is the answer's jobs field with a job per ref, and job null.
func jobsJSON(refs ...string) string {
	var jobs []string
	for _, ref := range refs {
		jobs = append(jobs, laneJob(ref))
	}
	return `"job":null,"jobs":[` + strings.Join(jobs, ",") + `]`
}

// runOn is the run step of the issue ref.
func runOn(ref string) string { return strings.Replace(runNew, "CSR-1", ref, 1) }

// laneFixture is a project with n team slots whose runs wait at the gate
// in the clones of refs, prepared in that order.
func laneFixture(t *testing.T, n int, refs ...string) (*fixture, *gate) {
	f := newFixture(t)
	f.p.Config.MaxParallelRuns = n
	f.ws.Prepares = nil
	var dirs []string
	for _, ref := range refs {
		f.ws.Prepares = append(f.ws.Prepares, workspace.PrepareReply{Dir: "/runs/" + ref})
		dirs = append(dirs, "/runs/"+ref)
	}
	g := newGate(dirs...)
	f.p.Clients["claude"] = g
	return f, g
}

// nextRefs are the refs that got a next request, once each.
func nextRefs(app *fakeApp) []string {
	app.mu.Lock()
	defer app.mu.Unlock()
	var refs []string
	for _, n := range app.nexts {
		if !slices.Contains(refs, n.ref) {
			refs = append(refs, n.ref)
		}
	}
	slices.Sort(refs)
	return refs
}

// A project with one slot, the default, sends no max_parallel, so the app
// answers with job as before.
func TestOneSlotSendsNoMaxParallel(t *testing.T) {
	f := newFixture(t)
	polls := runLoop(t, f, &fakeApp{}, nil, nil, stopAt(2, answer(t, proj, "")))
	for i, p := range polls {
		if p.MaxParallel != 0 {
			t.Errorf("poll %d: max_parallel %d, want none", i, p.MaxParallel)
		}
	}
}

// CLR-249, decision 2026-11-25-parallel-runs-default: a project without its
// own max_parallel_runs gets the runner-wide one; its own wins.
func TestRunnerWideMaxParallel(t *testing.T) {
	for _, tt := range []struct{ runner, own, want int }{{3, 0, 3}, {3, 2, 2}} {
		f := newFixture(t)
		f.p.MaxParallelRuns, f.p.Config.MaxParallelRuns = tt.runner, tt.own
		polls := runLoop(t, f, &fakeApp{}, nil, nil, stopAt(2, answer(t, proj, jobsJSON())))
		for i, p := range polls {
			if p.MaxParallel != tt.want {
				t.Errorf("runner-wide %d, own %d: poll %d: max_parallel %d, want %d", tt.runner, tt.own, i, p.MaxParallel, tt.want)
			}
		}
	}
}

// The answer's jobs start side by side, each in its own slot and clone on
// its own branch; the poll says how many slots there are, and is idle while
// one is free. No tidying or upkeep pass starts while they run.
func TestJobsRunSideBySide(t *testing.T) {
	f, g := laneFixture(t, 3, "CSR-1", "CSR-3")
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runOn("CSR-1")}, "CSR-3": {runOn("CSR-3")}})}
	var during []cinderline.PollRequest
	var status []string
	phase := 0
	polls := runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			return answer(t, proj, jobsJSON("CSR-1", "CSR-3"))
		case phase == 1 && g.isStarted("/runs/CSR-1") && g.isStarted("/runs/CSR-3"):
			during = append(during, req)
			if len(during) == 2 {
				phase, status = 2, f.p.Status().Issues
				close(g.release["/runs/CSR-1"])
				close(g.release["/runs/CSR-3"])
			}
		case phase == 2 && len(req.Running) == 0:
			stop()
		}
		return answer(t, proj, jobsJSON())
	})
	if phase != 2 {
		t.Fatalf("stopped in phase %d", phase)
	}
	for i, p := range polls {
		if p.MaxParallel != 3 {
			t.Errorf("poll %d: max_parallel %d, want 3", i, p.MaxParallel)
		}
	}
	for _, req := range during {
		if r := slices.Sorted(slices.Values(req.Running)); !slices.Equal(r, []string{"CSR-1", "CSR-3"}) || !req.Idle || req.Held != nil || req.Review != nil {
			t.Errorf("poll during both runs: running %v, idle %v, held %v, review %v; want both running, the third slot idle, no held or review", req.Running, req.Idle, req.Held, req.Review)
		}
	}
	if s := slices.Sorted(slices.Values(status)); !slices.Equal(s, []string{"CSR-1", "CSR-3"}) {
		t.Errorf("status during both runs: issues %v", status)
	}
	if len(f.p.Status().Issues) != 0 {
		t.Errorf("status after both runs: issues %v", f.p.Status().Issues)
	}
	var branches []string
	for _, c := range f.ws.Calls() {
		if c.Method == "Prepare" {
			branches = append(branches, c.Args[0].(string))
		}
	}
	if slices.Sort(branches); !slices.Equal(branches, []string{"cinderline/CSR-1", "cinderline/CSR-3"}) {
		t.Errorf("prepared branches = %v, want one per run", branches)
	}
	for _, ref := range []string{"CSR-1", "CSR-3"} {
		if l := lastsOf(app, ref); len(l) != 1 || l[0].Outcome != "finished" || l[0].Issue != ref {
			t.Errorf("%s lasts = %+v", ref, l)
		}
	}
}

// Jobs past the free slots, and a job already running, are left for a
// later poll.
func TestJobsPastTheFreeSlotsAreLeft(t *testing.T) {
	f, g := laneFixture(t, 2, "CSR-1", "CSR-3")
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runOn("CSR-1")}, "CSR-3": {runOn("CSR-3")}})}
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			return answer(t, proj, jobsJSON("CSR-1", "CSR-1", "CSR-3", "CSR-4"))
		case phase == 1 && g.isStarted("/runs/CSR-1") && g.isStarted("/runs/CSR-3"):
			if req.Idle {
				t.Errorf("poll with both slots busy said idle")
			}
			phase = 2
			close(g.release["/runs/CSR-1"])
			close(g.release["/runs/CSR-3"])
			return answer(t, proj, jobsJSON("CSR-4"))
		case phase == 2 && len(req.Running) == 0:
			stop()
		}
		return answer(t, proj, jobsJSON())
	})
	if phase != 2 {
		t.Fatalf("stopped in phase %d", phase)
	}
	if got := nextRefs(app); !slices.Equal(got, []string{"CSR-1", "CSR-3"}) {
		t.Errorf("runs = %v, want CSR-1 and CSR-3 only", got)
	}
}

// An app before lanes ignores max_parallel and answers with job, one run at
// a time: its job starts only while no team run is in flight.
func TestAnOldAppsJobRunsOneAtATime(t *testing.T) {
	f, g := laneFixture(t, 2, "CSR-1")
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	second := strings.Replace(jobJSON, "CSR-1", "CSR-3", 1)
	phase, while := 0, 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			return answer(t, proj, jobJSON)
		case phase == 1 && g.isStarted("/runs/CSR-1"):
			if !req.Idle {
				t.Errorf("poll with a free slot not idle")
			}
			if while++; while == 3 {
				phase = 2
				close(g.release["/runs/CSR-1"])
			}
			return answer(t, proj, second)
		case phase == 2 && len(req.Running) == 0:
			stop()
		}
		return answer(t, proj, "")
	})
	if phase != 2 {
		t.Fatalf("stopped in phase %d", phase)
	}
	if got := nextRefs(app); !slices.Equal(got, []string{"CSR-1"}) {
		t.Errorf("runs = %v, want CSR-1 only", got)
	}
}

// Each slot reserves a run's memory: where only one run fits, one job
// starts and the other is left.
func TestOnlyTheSlotsThatFitTakeAJob(t *testing.T) {
	b := budgetOf(6 << 30)
	f, g := laneFixture(t, 2, "CSR-1", "CSR-3")
	f.p.Budget = b
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runOn("CSR-1")}, "CSR-3": {runOn("CSR-3")}})}
	var offeredAt int64
	phase, while := 0, 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase, offeredAt = 1, reserved(b)
			return answer(t, proj, jobsJSON("CSR-1", "CSR-3"))
		case phase == 1 && g.isStarted("/runs/CSR-1"):
			if req.Idle {
				t.Errorf("poll offered the slot that does not fit")
			}
			if while++; while == 3 {
				phase = 2
				close(g.release["/runs/CSR-1"])
			}
		case phase == 2 && len(req.Running) == 0:
			stop()
		}
		return answer(t, proj, jobsJSON())
	})
	if phase != 2 {
		t.Fatalf("stopped in phase %d", phase)
	}
	if offeredAt != 4<<30 {
		t.Errorf("reserved when offered = %d, want one run's 4 GiB", offeredAt)
	}
	if got := nextRefs(app); !slices.Equal(got, []string{"CSR-1"}) {
		t.Errorf("runs = %v, want CSR-1 only", got)
	}
	if got := reserved(b); got != 0 {
		t.Errorf("reserved after the loop = %d, want 0", got)
	}
}

// A job that does not start holds off every team slot, and a run of
// another slot that began before it and ends does not end the hold.
func TestANotStartedJobHoldsEveryTeamSlot(t *testing.T) {
	f, g := laneFixture(t, 3, "CSR-1")
	app := &fakeApp{onNext: perRef(map[string][]string{"CSR-1": {runNew}})}
	var held, after []cinderline.PollRequest
	phase := 0
	runLoop(t, f, app, nil, nil, func(n int, req cinderline.PollRequest, stop func()) reply {
		switch {
		case phase == 0 && req.Idle:
			phase = 1
			// The second job's ref is not an issue reference, so it does not start.
			return answer(t, proj, jobsJSON("CSR-1", "not a ref"))
		case phase == 1 && g.isStarted("/runs/CSR-1") && len(req.Report.Errors) > 0:
			if held = append(held, req); len(held) == 2 {
				phase = 2
				close(g.release["/runs/CSR-1"])
			}
		case phase == 2 && len(req.Running) == 0:
			if after = append(after, req); len(after) == 2 {
				stop()
			}
		}
		return answer(t, proj, jobsJSON())
	})
	if phase != 2 {
		t.Fatalf("stopped in phase %d", phase)
	}
	for _, req := range append(held, after...) {
		errs := req.Report.Errors
		if req.Idle || len(errs) != 1 || errs[0].Kind != cinderline.KindNotStarted || errs[0].Issue != "not a ref" {
			t.Errorf("poll after the job that did not start: idle %v, errors %+v; want not idle, its not_started error", req.Idle, errs)
		}
	}
}
