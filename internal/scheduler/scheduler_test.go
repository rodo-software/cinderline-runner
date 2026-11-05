package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
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
// and holds a repeated poll for hold before answering 304; claim answers claimStatus.
type app struct {
	issue       string
	claimStatus int
	hold        time.Duration
	ev          *events
}

func (a app) start(t *testing.T) *cinderstack.Client {
	t.Helper()
	if a.ev == nil {
		t.Fatal("app needs events")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
		switch {
		case r.Method == "GET" && path == "/work" && r.Header.Get("If-None-Match") == `"v1"`:
			a.ev.add("GET /work cached")
			select {
			case <-r.Context().Done():
			case <-time.After(a.hold):
			}
			w.WriteHeader(http.StatusNotModified)
		case r.Method == "GET" && path == "/work":
			a.ev.add("GET /work")
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, `{"issue":`+a.issue+`}`)
		case r.Method == "POST" && strings.HasSuffix(path, "/claim"):
			a.ev.add("POST " + path)
			w.WriteHeader(a.claimStatus)
			if a.claimStatus == http.StatusConflict {
				_, _ = io.WriteString(w, `{"message":"Status changed.","status":"in_progress"}`)
			} else {
				_, _ = io.WriteString(w, issue)
			}
		default:
			a.ev.add("unexpected " + r.Method + " " + path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := cinderstack.New(srv.URL, "tok", "0.0.0")
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

func record(ev *events, what string) func(context.Context, *cinderstack.Client) error {
	return func(context.Context, *cinderstack.Client) error { ev.add(what); return nil }
}

func recordIssue(ev *events, what string) func(context.Context, *cinderstack.Client, cinderstack.Issue) error {
	return func(_ context.Context, _ *cinderstack.Client, i cinderstack.Issue) error {
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
	want := []string{"recover", "GET /work", "POST /issues/CSR-9/claim", "run CSR-9", "publish CSR-9", "GET /work cached"}
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
	c := app{issue: issue, claimStatus: http.StatusConflict, hold: time.Second, ev: ev}.start(t)
	start(t, s, []Slot{{Project: "CSR", Client: c, Hooks: Hooks{
		Recover: record(ev, "recover"),
		Run:     recordIssue(ev, "run"),
	}}})
	waitFor(t, "the next poll", func() bool { return ev.count("GET /work cached") == 1 })
	want := []string{"recover", "GET /work", "POST /issues/CSR-9/claim", "GET /work cached"}
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
	fails := map[string]func(context.Context, *cinderstack.Client, cinderstack.Issue) error{
		"panic": func(context.Context, *cinderstack.Client, cinderstack.Issue) error { panic("boom") },
		"error": func(context.Context, *cinderstack.Client, cinderstack.Issue) error { return errors.New("boom") },
	}
	for n, fail := range fails {
		t.Run(n, func(t *testing.T) {
			s, buf := sched()
			evA, evB := &events{}, &events{}
			a := app{issue: issue, claimStatus: 200, hold: 20 * time.Millisecond, ev: evA}.start(t)
			b := app{issue: issue, claimStatus: 200, hold: 20 * time.Millisecond, ev: evB}.start(t)
			start(t, s, []Slot{
				{Project: "A", Client: a, Hooks: Hooks{Recover: record(evA, "recover"), Run: fail}},
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
			if !strings.Contains(l, `"level":"ERROR","msg":"slot failed","project":"A","issue":"CSR-9","err":"`) ||
				!strings.Contains(l, "boom") {
				t.Errorf("no error line with project and issue:\n%s", l)
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
		ReviewBranches: func(context.Context, *cinderstack.Client) error {
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
			Recover: func(context.Context, *cinderstack.Client) error { ev.add("recover"); return errors.New("down") },
		}}})
		waitFor(t, "the first failure", func() bool { return ev.count("recover") == 1 })
		time.Sleep(20 * time.Millisecond) // into the backoff
		stop(300 * time.Millisecond)
		if ev.count("recover") != 1 {
			t.Errorf("slot restarted before its backoff: %q", ev.all())
		}
	})
}
