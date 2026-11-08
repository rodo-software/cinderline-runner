package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
)

// events is an ordered, concurrency-safe log of what a server and the hooks saw.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

func (e *events) count(s string) int {
	n := 0
	for _, x := range e.all() {
		if x == s {
			n++
		}
	}
	return n
}

// app is a fake runner API: /work answers issue (JSON, or "null") with an ETag,
// and holds a repeated poll for hold before answering 304; claim answers
// claimStatus; GET /issues answers todo, a JSON array; PATCH and comments
// are recorded with the status or body they carry.
type app struct {
	issue       string
	todo        string
	claimStatus int
	hold        time.Duration
	ev          *events
	rewind      bool // a PATCH to To do changes the ETag, so the next /work answers the issue again (CSR-81)
}

func (a app) start(t *testing.T) *cinderline.Client {
	t.Helper()
	if a.ev == nil {
		t.Fatal("app needs events")
	}
	var version atomic.Int32
	etag := func() string { return fmt.Sprintf(`"v%d"`, version.Load()+1) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
		var in map[string]string // the request body, if any
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch {
		case r.Method == "GET" && path == "/work" && r.Header.Get("If-None-Match") == etag():
			a.ev.add("GET /work cached")
			select {
			case <-r.Context().Done():
			case <-time.After(a.hold):
			}
			w.WriteHeader(http.StatusNotModified)
		case r.Method == "GET" && path == "/work":
			a.ev.add("GET /work")
			w.Header().Set("ETag", etag())
			_, _ = io.WriteString(w, `{"issue":`+a.issue+`}`)
		case r.Method == "GET" && path == "/issues" && r.URL.Query().Get("status") == "todo":
			a.ev.add("GET /issues todo")
			_, _ = io.WriteString(w, `{"issues":`+a.todo+`}`)
		case r.Method == "POST" && strings.HasSuffix(path, "/claim"):
			a.ev.add("POST " + path + " " + in["expected_status"])
			w.WriteHeader(a.claimStatus)
			if a.claimStatus == http.StatusConflict {
				_, _ = io.WriteString(w, `{"message":"Status changed.","status":"in_progress"}`)
			} else {
				_, _ = io.WriteString(w, issue)
			}
		case r.Method == "POST" && strings.HasSuffix(path, "/comments"):
			a.ev.add("POST " + path + " " + in["body"])
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == "PATCH" && strings.HasPrefix(path, "/issues/"):
			a.ev.add("PATCH " + path + " " + in["status"] + " from " + in["expected_status"])
			if a.rewind && in["status"] == "todo" {
				version.Add(1)
			}
			_, _ = io.WriteString(w, issue)
		default:
			a.ev.add("unexpected " + r.Method + " " + path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := cinderline.New(srv.URL, "tok", "0.0.0")
	c.HTTP = srv.Client()
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

const issue = `{"ref":"CSR-9","title":"T","status":{"value":"todo","label":"To do"}}`

// logBuf is a bytes.Buffer safe to read while slots write to it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// sched returns a scheduler with short intervals that logs JSON into the returned buffer.
func sched() (*Scheduler, *logBuf) {
	buf := &logBuf{}
	return &Scheduler{
		Log:            slog.New(slog.NewJSONHandler(buf, nil)),
		ReviewInterval: 10 * time.Millisecond,
		MergeInterval:  10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		IdleWait:       time.Millisecond,
		MaxIdleWait:    10 * time.Millisecond,
	}, buf
}

// start runs s in the background; the returned stop cancels it and fails if Run
// takes more than within to return.
func start(t *testing.T, s *Scheduler, slots []Slot) (stop func(within time.Duration)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, slots); close(done) }()
	stopped := false
	stop = func(within time.Duration) {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(within):
			t.Fatalf("Run did not return within %v of cancel", within)
		}
	}
	t.Cleanup(func() { stop(2 * time.Second) })
	return stop
}

// waitFor fails the test if cond is not true within two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func record(ev *events, what string) func(context.Context, *cinderline.Client) error {
	return func(context.Context, *cinderline.Client) error { ev.add(what); return nil }
}

func recordIssue(ev *events, what string) func(context.Context, *cinderline.Client, cinderline.Issue) error {
	return func(_ context.Context, _ *cinderline.Client, i cinderline.Issue) error {
		ev.add(what + " " + i.Ref)
		return nil
	}
}

func TestOneSlotPerProject(t *testing.T) {
	s, _ := sched()
	var evs [3]*events
	var slots []Slot
	for i, p := range []string{"A", "B", "C"} {
		evs[i] = &events{}
		c := app{issue: "null", hold: 20 * time.Millisecond, ev: evs[i]}.start(t)
		slots = append(slots, Slot{Project: p, Client: c})
	}
	stop := start(t, s, slots)
	for i, ev := range evs {
		waitFor(t, "polls of slot "+slots[i].Project, func() bool {
			return ev.count("GET /work") == 1 && ev.count("GET /work cached") >= 1
		})
	}
	stop(time.Second)
}

func TestHappyPathOrder(t *testing.T) {
	s, _ := sched()
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Recover: record(ev, "recover"),
		Run:     recordIssue(ev, "run"),
		Publish: recordIssue(ev, "publish"),
	}}})
	want := []string{"recover", "GET /work", "POST /issues/CSR-9/claim todo", "run CSR-9", "publish CSR-9", "GET /work cached"}
	waitFor(t, "the sequence", func() bool { return len(ev.all()) >= len(want) })
	if got := ev.all()[:len(want)]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

func TestRecoverBeforeFirstWork(t *testing.T) {
	s, _ := sched()
	ev := &events{}
	c := app{issue: "null", hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{Recover: record(ev, "recover")}}})
	waitFor(t, "first poll", func() bool { return ev.count("GET /work") == 1 })
	if got := ev.all(); got[0] != "recover" || got[1] != "GET /work" {
		t.Fatalf("events = %q, want recover before the first GET /work", got)
	}
}

func TestClaimConflictMovesOn(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: issue, todo: "[" + issue + "]", claimStatus: http.StatusConflict, hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Recover: record(ev, "recover"),
		Run:     recordIssue(ev, "run"),
	}}})
	waitFor(t, "the next poll", func() bool { return ev.count("GET /work cached") == 1 })
	want := []string{"recover", "GET /work", "POST /issues/CSR-9/claim todo", "GET /issues todo", "GET /work cached"}
	if got := ev.all(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("a conflict was logged as a failure:\n%s", buf)
	}
}

func TestSkipWithoutRunStep(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: 20 * time.Millisecond, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c}})
	waitFor(t, "two cached polls", func() bool { return ev.count("GET /work cached") >= 2 })
	for _, e := range ev.all() {
		if strings.HasPrefix(e, "POST") || strings.HasPrefix(e, "unexpected") {
			t.Fatalf("got %q without a run step; events %q", e, ev.all())
		}
	}
	if l := buf.String(); !strings.Contains(l, `"msg":"no run step yet, skipping","project":"CSR","issue":"CSR-9"`) {
		t.Errorf("no skip line in log:\n%s", l)
	}
}

func TestFailingSlotDoesNotStopOthers(t *testing.T) {
	fails := map[string]func(context.Context, *cinderline.Client) error{
		"panic": func(context.Context, *cinderline.Client) error { panic("boom") },
		"error": func(context.Context, *cinderline.Client) error { return errors.New("boom") },
	}
	for n, fail := range fails {
		t.Run(n, func(t *testing.T) {
			s, buf := sched()
			evA, evB := &events{}, &events{}
			a := app{issue: issue, claimStatus: 200, hold: 20 * time.Millisecond, ev: evA}.start(t)
			b := app{issue: issue, claimStatus: 200, hold: 20 * time.Millisecond, ev: evB}.start(t)
			start(t, s, []Slot{
				{Project: "A", Client: a, Hooks: Hooks{Recover: func(ctx context.Context, c *cinderline.Client) error {
					evA.add("recover")
					return fail(ctx, c)
				}}},
				{Project: "B", Client: b, Hooks: Hooks{Recover: record(evB, "recover"), Run: recordIssue(evB, "run")}},
			})
			waitFor(t, "slot A to restart", func() bool { return evA.count("recover") >= 2 })
			waitFor(t, "slot B to run and poll on", func() bool {
				return evB.count("run CSR-9") == 1 && evB.count("GET /work cached") >= 2
			})
			if evB.count("recover") != 1 {
				t.Errorf("slot B restarted: %q", evB.all())
			}
			l := buf.String()
			if !strings.Contains(l, `"level":"ERROR","msg":"slot failed","project":"A","err":"`) ||
				!strings.Contains(l, "boom") {
				t.Errorf("no error line with the project:\n%s", l)
			}
		})
	}
}

func TestTimersFire(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: "null", hold: 10 * time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Recover: record(ev, "recover"),
		ReviewBranches: func(context.Context, *cinderline.Client) error {
			ev.add("review")
			return errors.New("review broke")
		},
		MergeCheck: record(ev, "merge"),
	}}})
	waitFor(t, "the long poll", func() bool { return ev.count("GET /work cached") == 1 })
	waitFor(t, "timers while the poll is held", func() bool {
		return ev.count("review") >= 3 && ev.count("merge") >= 3
	})
	if ev.count("recover") != 1 || ev.count("GET /work cached") != 1 {
		t.Errorf("a timer error disturbed the work loop: %q", ev.all())
	}
	if l := buf.String(); !strings.Contains(l, `"level":"WARN","msg":"review branches failed","project":"CSR","err":"review broke"`) {
		t.Errorf("no warn line for the timer error:\n%s", l)
	}
}

func TestStopAtOnce(t *testing.T) {
	t.Run("long poll", func(t *testing.T) {
		s, buf := sched()
		ev := &events{}
		c := app{issue: "null", hold: 10 * time.Second, ev: ev}.start(t)
		stop := start(t, s, []Slot{{Project: "CSR", Client: c}})
		waitFor(t, "the long poll", func() bool { return ev.count("GET /work cached") == 1 })
		stop(300 * time.Millisecond)
		if l := buf.String(); strings.Contains(l, `"level":"ERROR"`) {
			t.Errorf("cancel was logged as a failure:\n%s", l)
		}
	})
	t.Run("backoff", func(t *testing.T) {
		s, _ := sched()
		s.MaxBackoff = 10 * time.Second // first wait is 1s
		ev := &events{}
		c := app{issue: "null", hold: 10 * time.Second, ev: ev}.start(t)
		stop := start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
			Recover: func(context.Context, *cinderline.Client) error { ev.add("recover"); return errors.New("down") },
		}}})
		waitFor(t, "the first failure", func() bool { return ev.count("recover") == 1 })
		time.Sleep(20 * time.Millisecond) // into the backoff
		stop(300 * time.Millisecond)
		if ev.count("recover") != 1 {
			t.Errorf("slot restarted before its backoff: %q", ev.all())
		}
	})
}

func TestStatus(t *testing.T) {
	s, _ := sched()
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev}.start(t)
	running, release := make(chan struct{}), make(chan struct{})
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Run: func(context.Context, *cinderline.Client, cinderline.Issue) error {
			running <- struct{}{}
			<-release
			return errors.New("boom")
		},
	}}})
	<-running
	st := s.Status()["CSR"]
	if !st.Connected || st.LastPoll.IsZero() || st.Issue != "CSR-9" || st.LastError != "" {
		t.Fatalf("during the run: %+v, want connected, a last poll, issue CSR-9 and no error", st)
	}
	close(release)
	waitFor(t, "the move to Review", func() bool { return ev.count("PATCH /issues/CSR-9 review from in_progress") == 1 })
	waitFor(t, "the issue to leave the status", func() bool { return s.Status()["CSR"].Issue == "" })
	if st = s.Status()["CSR"]; !st.Connected || st.LastError != "" {
		t.Fatalf("after a failed run: %+v, want connected without an error: the run's failure is the issue's, not the slot's", st)
	}
}

func TestStatusAfterSlotFailure(t *testing.T) {
	s, _ := sched()
	ev := &events{}
	c := app{issue: "null", hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Recover: func(context.Context, *cinderline.Client) error { return errors.New("down") },
	}}})
	waitFor(t, "the failure in the status", func() bool { return s.Status()["CSR"].LastError != "" })
	st := s.Status()["CSR"]
	if st.Connected || st.Issue != "" || st.LastError != "recover: down" || st.LastErrorAt.IsZero() {
		t.Fatalf("after the failure: %+v, want not connected, no issue, error recover: down with its time", st)
	}
}

// Ported from legacy runner.test.mjs "an error after the claim ends in a
// comment and Review", for Run and Publish, as an error and as a panic.
func TestErrorAfterClaimEndsInCommentAndReview(t *testing.T) {
	fail := func(ctx context.Context, c *cinderline.Client, i cinderline.Issue) error {
		return errors.New("Issue not found.")
	}
	panics := func(context.Context, *cinderline.Client, cinderline.Issue) error { panic("Issue not found.") }
	for _, tt := range []struct {
		name  string
		hooks func(ev *events) Hooks
		says  string
	}{
		{"run error", func(*events) Hooks { return Hooks{Run: fail} }, "The run stopped with an error: Issue not found."},
		{"run panic", func(*events) Hooks { return Hooks{Run: panics} }, "The run stopped with an error: panic: Issue not found."},
		{"publish error", func(ev *events) Hooks { return Hooks{Run: recordIssue(ev, "run"), Publish: fail} }, "The run stopped with an error: Issue not found."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, buf := sched()
			ev := &events{}
			c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev}.start(t)
			start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: tt.hooks(ev)}})
			waitFor(t, "the next poll", func() bool { return ev.count("GET /work cached") == 1 })
			var got []string
			for _, e := range ev.all() {
				if strings.HasPrefix(e, "POST") || strings.HasPrefix(e, "PATCH") {
					got = append(got, e)
				}
			}
			want := []string{
				"POST /issues/CSR-9/claim todo",
				"POST /issues/CSR-9/comments " + tt.says,
				"PATCH /issues/CSR-9 review from in_progress",
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("writes = %q, want %q", got, want)
			}
			if l := buf.String(); !strings.Contains(l, `"level":"ERROR","msg":"run failed","project":"CSR","issue":"CSR-9","err":"`) {
				t.Errorf("no error line with project and issue:\n%s", l)
			}
			if st := s.Status()["CSR"]; st.LastError != "" {
				t.Errorf("a failed run became a slot failure: %+v", st)
			}
		})
	}
}

// A stop during a run leaves the issue In progress: the next start's
// recovery puts it back in To do (decision 2026-11-05-runner-rewrite-details, point 14).
func TestStopDuringRunLeavesIssueInProgress(t *testing.T) {
	s, _ := sched()
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev}.start(t)
	running := make(chan struct{})
	stop := start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Run: func(ctx context.Context, _ *cinderline.Client, _ cinderline.Issue) error {
			close(running)
			<-ctx.Done()
			return ctx.Err()
		},
	}}})
	<-running
	stop(300 * time.Millisecond)
	for _, e := range ev.all() {
		if strings.HasPrefix(e, "PATCH") || strings.Contains(e, "/comments") {
			t.Fatalf("a stop wrote %q; events %q", e, ev.all())
		}
	}
}

// Ported from legacy runner.test.mjs "<x> is skipped before the claim", the
// repository cases ("a repository other than the configured one", "a project
// without a repository"): Pin refuses, so nothing is claimed or commented,
// and the slot waits out its backoff instead of looping.
func TestPinRefusalClaimsNothing(t *testing.T) {
	s, buf := sched()
	s.MaxBackoff = 10 * time.Second // first wait is 1s
	ev := &events{}
	c := app{issue: issue, todo: "[" + issue + "]", claimStatus: 200, hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Pin: func(context.Context, *cinderline.Client) error {
			ev.add("pin")
			return errors.New("Cinderline names another repository")
		},
		Check: func(context.Context, *cinderline.Client, cinderline.Issue) error { ev.add("check"); return nil },
		Run:   recordIssue(ev, "run"),
	}}})
	waitFor(t, "the pin", func() bool { return ev.count("pin") == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := ev.all(); strings.Join(got, "|") != "GET /work|pin" {
		t.Fatalf("events = %q, want one poll and one pin, nothing else", got)
	}
	if st := s.Status()["CSR"]; !strings.Contains(st.LastError, "another repository") {
		t.Errorf("status %+v does not show the refusal", st)
	}
	if l := buf.String(); !strings.Contains(l, `"msg":"slot failed","project":"CSR","err":"repository: Cinderline names another repository"`) {
		t.Errorf("no failure line:\n%s", l)
	}
}

// Ported from legacy runner.test.mjs "<x> is skipped before the claim", the
// team cases ("an orchestrator on another client", "an OpenCode worker
// without an OpenRouter key"): Check refuses, so nothing is claimed or
// commented, the skip is logged and the slot waits for the next poll.
func TestCheckRefusalSkipsBeforeClaim(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: issue, todo: "[" + issue + "]", claimStatus: 200, hold: 200 * time.Millisecond, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Check: func(_ context.Context, _ *cinderline.Client, i cinderline.Issue) error {
			ev.add("check " + i.Ref)
			return errors.New("agent Ada runs on codex")
		},
		Run: recordIssue(ev, "run"),
	}}})
	waitFor(t, "the first check", func() bool { return ev.count("check CSR-9") == 1 })
	time.Sleep(100 * time.Millisecond) // within the held poll
	want := []string{"GET /work", "check CSR-9", "GET /issues todo", "GET /work cached"}
	if got := ev.all(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %q, want %q", got, want)
	}
	// The next poll checks the board again: the skipped issue still blocks Work's answer.
	waitFor(t, "the second check", func() bool { return ev.count("check CSR-9") == 2 })
	if l := buf.String(); !strings.Contains(l, `"level":"WARN","msg":"skipped before the claim","project":"CSR","issue":"CSR-9","err":"agent Ada runs on codex"`) {
		t.Errorf("no skip line:\n%s", l)
	}
	if st := s.Status()["CSR"]; st.LastError != "" || !st.Connected {
		t.Errorf("a skip became a slot failure: %+v", st)
	}
}

// Ported from legacy runner.test.mjs "an issue that fails before its claim
// does not block the next".
func TestSkippedIssueDoesNotBlockNext(t *testing.T) {
	s, _ := sched()
	ev := &events{}
	next := `{"ref":"CSR-10","title":"U","status":{"value":"todo","label":"To do"},"team":{"id":2,"name":"B"}}`
	c := app{issue: issue, todo: "[" + issue + "," + next + "]", claimStatus: 200, hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Check: func(_ context.Context, _ *cinderline.Client, i cinderline.Issue) error {
			if i.Ref == "CSR-9" {
				return errors.New("Unknown team.")
			}
			return nil
		},
		Run: recordIssue(ev, "run"),
	}}})
	waitFor(t, "the run of the next issue", func() bool { return ev.count("run CSR-10") == 1 })
	if ev.count("POST /issues/CSR-9/claim todo") != 0 || ev.count("POST /issues/CSR-10/claim todo") != 1 {
		t.Fatalf("events = %q, want only CSR-10 claimed", ev.all())
	}
}

// Ported from legacy runner.test.mjs "a claim that keeps failing waits for
// the next poll".
func TestFailingClaimWaitsForNextPoll(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: issue, todo: "[" + issue + "]", claimStatus: http.StatusForbidden, hold: 300 * time.Millisecond, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{Run: recordIssue(ev, "run")}}})
	waitFor(t, "the held poll", func() bool { return ev.count("GET /work cached") == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := ev.count("POST /issues/CSR-9/claim todo"); n != 1 {
		t.Fatalf("%d claims within one poll, want 1: %q", n, ev.all())
	}
	if l := buf.String(); !strings.Contains(l, `"level":"WARN","msg":"not claimed","project":"CSR","issue":"CSR-9"`) {
		t.Errorf("no not-claimed line:\n%s", l)
	}
	if st := s.Status()["CSR"]; st.LastError != "" {
		t.Errorf("a refused claim became a slot failure: %+v", st)
	}
}

// writes are the POSTs and PATCHes the app saw, in order.
func writes(ev *events) []string {
	var got []string
	for _, e := range ev.all() {
		if strings.HasPrefix(e, "POST") || strings.HasPrefix(e, "PATCH") {
			got = append(got, e)
		}
	}
	return got
}

// CSR-81 point 4: a Run error wrapping ErrNotStarted (a failure before the
// first agent turn: fetch, worktree, token) puts the issue back in To do
// with no comment, logs it, and the slot waits StartBackoff before its next
// claim; the next poll claims and runs it. It is not a slot failure.
func TestNotStartedGoesBackToTodoWithoutComment(t *testing.T) {
	s, buf := sched()
	s.StartBackoff = time.Millisecond
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev, rewind: true}.start(t)
	var runs atomic.Int32
	run := func(_ context.Context, _ *cinderline.Client, i cinderline.Issue) error {
		if runs.Add(1) == 1 {
			return fmt.Errorf("%w: git fetch: Forgejo unreachable", ErrNotStarted)
		}
		ev.add("run " + i.Ref)
		return nil
	}
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{Run: run, Publish: recordIssue(ev, "publish")}}})
	waitFor(t, "the second run", func() bool { return ev.count("publish CSR-9") == 1 })
	want := []string{
		"POST /issues/CSR-9/claim todo",
		"PATCH /issues/CSR-9 todo from in_progress",
		"POST /issues/CSR-9/claim todo",
	}
	if got := writes(ev); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	if l := buf.String(); !strings.Contains(l, `"level":"WARN","msg":"not started, back in To do","project":"CSR","issue":"CSR-9","err":"the run did not start: git fetch: Forgejo unreachable","retry_in":`) {
		t.Errorf("no warning with project, issue and wait:\n%s", l)
	}
	if st := s.Status()["CSR"]; st.LastError != "" {
		t.Errorf("a run that did not start became a slot failure: %+v", st)
	}
}

// CSR-81 point 4: the same issue failing to start three times in a row gets
// one comment, so a persistent misconfiguration is visible; it still goes
// back to To do, and later failures add no comment.
func TestNotStartedThriceComments(t *testing.T) {
	s, _ := sched()
	s.StartBackoff = time.Millisecond
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev, rewind: true}.start(t)
	run := func(context.Context, *cinderline.Client, cinderline.Issue) error {
		return fmt.Errorf("%w: no git token", ErrNotStarted)
	}
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{Run: run}}})
	waitFor(t, "five failures", func() bool { return ev.count("PATCH /issues/CSR-9 todo from in_progress") >= 5 })
	var comments []string
	for _, w := range writes(ev) {
		if strings.HasPrefix(w, "POST /issues/CSR-9/comments ") {
			comments = append(comments, w)
		}
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "three times") || !strings.Contains(comments[0], "no git token") {
		t.Fatalf("comments = %q, want one after the third failure naming the error", comments)
	}
	got := writes(ev)
	if i := slices.Index(got, comments[0]); i < 5 || got[i-1] != "POST /issues/CSR-9/claim todo" || !strings.HasPrefix(got[i+1], "PATCH /issues/CSR-9 todo") {
		t.Errorf("the comment is not between the third claim and its move back:\n%s", strings.Join(got, "\n"))
	}
}

// runDraining runs s with a Drain channel and returns the channel and one
// that is closed when Run returns. The context is only cancelled at cleanup.
func runDraining(t *testing.T, s *Scheduler, slots []Slot) (drain chan struct{}, done chan struct{}) {
	t.Helper()
	drain, done = make(chan struct{}), make(chan struct{})
	s.Drain = drain
	ctx, cancel := context.WithCancel(context.Background())
	go func() { s.Run(ctx, slots); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return drain, done
}

// CSR-44: a drain lets the run in flight finish and publish with its context
// intact, claims nothing more, and then Run returns on its own.
func TestDrainFinishesRunInFlight(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: issue, claimStatus: 200, hold: time.Second, ev: ev}.start(t)
	running, release := make(chan struct{}), make(chan struct{})
	var runCtxErr error
	drain, done := runDraining(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Run: func(ctx context.Context, _ *cinderline.Client, i cinderline.Issue) error {
			ev.add("run " + i.Ref)
			close(running)
			<-release
			runCtxErr = ctx.Err()
			return nil
		},
		Publish: recordIssue(ev, "publish"),
	}}})
	<-running
	close(drain)
	select {
	case <-done:
		t.Fatal("Run returned while a run was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	works := ev.count("GET /work") + ev.count("GET /work cached")
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after the run in flight finished; events %q", ev.all())
	}
	if runCtxErr != nil {
		t.Errorf("the run's context was cancelled by the drain: %v", runCtxErr)
	}
	if ev.count("publish CSR-9") != 1 {
		t.Errorf("the run in flight was not published; events %q", ev.all())
	}
	if n := ev.count("GET /work") + ev.count("GET /work cached"); n != works {
		t.Errorf("polled for new work after the drain; events %q", ev.all())
	}
	if ev.count("run CSR-9") != 1 {
		t.Errorf("runs = %d, want 1; events %q", ev.count("run CSR-9"), ev.all())
	}
	if l := buf.String(); strings.Contains(l, `"level":"ERROR"`) {
		t.Errorf("the drain was logged as a failure:\n%s", l)
	}
}

// CSR-44: an idle slot (in a long poll) ends at once on a drain.
func TestDrainWhileIdleReturnsAtOnce(t *testing.T) {
	s, buf := sched()
	ev := &events{}
	c := app{issue: "null", hold: 10 * time.Second, ev: ev}.start(t)
	drain, done := runDraining(t, s, []Slot{{Project: "CSR", Client: c}})
	waitFor(t, "the long poll", func() bool { return ev.count("GET /work cached") == 1 })
	close(drain)
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Run did not return within 300ms of a drain while idle")
	}
	if l := buf.String(); strings.Contains(l, `"level":"ERROR"`) {
		t.Errorf("the drain was logged as a failure:\n%s", l)
	}
}

// pacedWork is a runner API whose /work answers at once (CSR-113): answers(n)
// gives the body of the nth call, from 1, each with a new ETag. It records when
// each /work call came.
func pacedWork(t *testing.T, answers func(n int) string) (*cinderline.Client, func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runner/v1/work" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		w.Header().Set("ETag", fmt.Sprintf(`"v%d"`, n))
		_, _ = io.WriteString(w, `{"issue":`+answers(n)+`}`)
	}))
	t.Cleanup(srv.Close)
	c := cinderline.New(srv.URL, "tok", "0.0.0")
	c.HTTP = srv.Client()
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), at...)
	}
}

// A server that answers at once with no work gets one GET /work per
// IdleWait, not a tight loop (CSR-113).
func TestEmptyAnswersArePaced(t *testing.T) {
	s, _ := sched()
	s.IdleWait, s.MaxIdleWait = 50*time.Millisecond, 50*time.Millisecond
	c, calls := pacedWork(t, func(int) string { return "null" })
	stop := start(t, s, []Slot{{Project: "CSR", Client: c}})
	time.Sleep(300 * time.Millisecond)
	stop(time.Second)
	if n := len(calls()); n < 3 || n > 8 {
		t.Fatalf("%d GET /work in 300ms at one per 50ms, want about 6", n)
	}
}

// Unchanged answers (304) are paced too.
func TestUnchangedAnswersArePaced(t *testing.T) {
	s, _ := sched()
	s.IdleWait, s.MaxIdleWait = 50*time.Millisecond, 50*time.Millisecond
	ev := &events{}
	c := app{issue: "null", ev: ev}.start(t) // hold 0: the 304 comes at once
	stop := start(t, s, []Slot{{Project: "CSR", Client: c}})
	time.Sleep(300 * time.Millisecond)
	stop(time.Second)
	if n := ev.count("GET /work") + ev.count("GET /work cached"); n < 3 || n > 8 {
		t.Fatalf("%d GET /work in 300ms at one per 50ms, want about 6", n)
	}
}

// The wait doubles per empty answer up to MaxIdleWait.
func TestIdleWaitDoubles(t *testing.T) {
	s, _ := sched()
	s.IdleWait, s.MaxIdleWait = 20*time.Millisecond, 80*time.Millisecond
	c, calls := pacedWork(t, func(int) string { return "null" })
	stop := start(t, s, []Slot{{Project: "CSR", Client: c}})
	waitFor(t, "six calls", func() bool { return len(calls()) >= 6 })
	stop(time.Second)
	at := calls()
	// waits 20, 40, 80, 80, 80 ms
	for i, min := range []time.Duration{20, 40, 80, 80, 80} {
		if gap := at[i+1].Sub(at[i]); gap < min*time.Millisecond {
			t.Errorf("gap %d = %v, want at least %v", i+1, gap, min*time.Millisecond)
		}
	}
	if gap := at[5].Sub(at[4]); gap > 150*time.Millisecond {
		t.Errorf("gap 5 = %v, want about 80ms (the ceiling)", gap)
	}
}

// A 200 with an issue resets the wait to IdleWait.
func TestAnswerWithWorkResetsIdleWait(t *testing.T) {
	s, _ := sched()
	s.IdleWait, s.MaxIdleWait = 20*time.Millisecond, time.Second
	c, calls := pacedWork(t, func(n int) string {
		if n == 5 {
			return issue // skipped: no run step
		}
		return "null"
	})
	stop := start(t, s, []Slot{{Project: "CSR", Client: c}})
	waitFor(t, "six calls", func() bool { return len(calls()) >= 6 })
	stop(time.Second)
	at := calls()
	// before the issue the wait grew to 160ms; after it, back to 20ms
	if gap := at[5].Sub(at[4]); gap < 20*time.Millisecond || gap > 100*time.Millisecond {
		t.Fatalf("wait after the answer with an issue = %v, want 20ms (reset)", gap)
	}
}

// Stopping during an idle wait returns at once, not after the wait (CSR-113).
func TestStopDuringIdleWait(t *testing.T) {
	s, _ := sched()
	s.IdleWait, s.MaxIdleWait = 10*time.Second, 10*time.Second
	c, calls := pacedWork(t, func(int) string { return "null" })
	stop := start(t, s, []Slot{{Project: "CSR", Client: c}})
	waitFor(t, "the first call", func() bool { return len(calls()) == 1 })
	stop(500 * time.Millisecond)
}
