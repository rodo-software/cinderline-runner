package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// syncApp is a fake runner API that offers POST /sync (decision
// 2026-11-16-sync-request). A sync answers work, review and, for each
// watched ref, status; a claim answers claim, and when that is 200 the work
// is taken. It records every request but the handshake.
type syncApp struct {
	mu     sync.Mutex
	work   string                         // JSON, or "" for null
	todo   string                         // GET /issues?status=todo, a JSON array; "" for the work alone
	review string                         // a JSON array; "" for an empty one
	after  string                         // the Poll-After of sync answers
	down   bool                           // sync answers 503
	claim  int                            // 0: 200
	status func(ref string, n int) string // of a watched ref in its n-th sync, from 0; nil: in_progress
	seen   map[string]int                 // syncs that watched each ref
	calls  []syncCall
}

type syncCall struct {
	what  string // "METHOD path?query"
	watch []string
	at    time.Time
}

func (a *syncApp) set(change func(*syncApp)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(a)
}

// all returns the recorded requests, and those whose what starts with prefix.
func (a *syncApp) all(prefix string) (all []string, match []syncCall) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.calls {
		all = append(all, c.what)
		if strings.HasPrefix(c.what, prefix) {
			match = append(match, c)
		}
	}
	return all, match
}

func (a *syncApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
	if path == "/project" {
		_, _ = io.WriteString(w, `{"key":"CSR","sync":true}`)
		return
	}
	call := syncCall{what: r.Method + " " + path, at: time.Now()}
	if r.URL.RawQuery != "" {
		call.what += "?" + r.URL.RawQuery
	}
	or := func(s, none string) string {
		if s == "" {
			return none
		}
		return s
	}
	switch {
	case call.what == "POST /sync":
		var in struct {
			Watch []string `json:"watch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		call.watch = in.Watch
		if a.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"message":"Deploying."}`)
			break
		}
		var watched []string
		for _, ref := range in.Watch {
			status := "in_progress"
			if a.status != nil {
				status = a.status(ref, a.seen[ref])
			}
			if a.seen == nil {
				a.seen = map[string]int{}
			}
			a.seen[ref]++
			watched = append(watched, `{"ref":"`+ref+`","status":{"value":"`+status+`","label":"`+status+`"},"last_comment_by_agent":false}`)
		}
		if a.after != "" {
			w.Header().Set(name.Name+"-Poll-After", a.after)
		}
		_, _ = io.WriteString(w, `{"work":`+or(a.work, "null")+`,"watched":[`+strings.Join(watched, ",")+`],"review":`+or(a.review, "[]")+`}`)
	case call.what == "GET /issues?status=todo":
		_, _ = io.WriteString(w, `{"issues":`+or(a.todo, "["+or(a.work, "")+"]")+`}`)
	case r.Method == "POST" && strings.HasSuffix(path, "/claim"):
		if a.claim != 0 {
			w.WriteHeader(a.claim)
			_, _ = io.WriteString(w, `{"message":"Status changed.","status":"in_progress"}`)
			break
		}
		a.work = ""
		_, _ = io.WriteString(w, issue)
	case call.what == "PUT /report":
		w.WriteHeader(http.StatusNoContent)
	default: // GET /work, the heartbeat, the Review lists and the rest: not in sync mode
		call.what = "unexpected " + call.what
		w.WriteHeader(http.StatusNotFound)
	}
	a.calls = append(a.calls, call)
}

// handshake is the Recover hook of the sync tests: the real one's GET
// /project, which tells the client that the app offers sync.
func handshake(ctx context.Context, c *cinderline.Client) error {
	_, err := c.GetProject(ctx)
	return err
}

// syncSlot runs a scheduler and the client's heartbeat loop, as the daemon
// does, until the test ends.
func syncSlot(t *testing.T, s *Scheduler, c *cinderline.Client, h Hooks) {
	t.Helper()
	if h.Recover == nil {
		h.Recover = handshake
	}
	go c.RunHeartbeats(t.Context(), cinderline.Report{Clients: []cinderline.ReportClient{}})
	go s.Run(t.Context(), []Slot{{Project: "CSR", Client: c, Hooks: h}})
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// With sync offered the work loop sends no GET /work: it claims and runs the
// work of a sync answer as it does GET /work's, the sync after a run goes
// out at once, and between answers nothing is sent at all.
func TestSyncWorkIsClaimedAndRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{}
		ev := &events{}
		var ended time.Time
		s := &Scheduler{Log: quiet(), ReviewInterval: time.Hour, MergeInterval: time.Hour}
		start := time.Now()
		syncSlot(t, s, memClient(a.ServeHTTP), Hooks{
			Pin: record(ev, "pin"),
			Run: func(_ context.Context, _ *cinderline.Client, i cinderline.Issue) error {
				ev.add("run " + i.Ref)
				time.Sleep(100 * time.Second)
				ended = time.Now()
				return nil
			},
			Publish: recordIssue(ev, "publish"),
		})
		time.Sleep(150 * time.Second)
		if got := ev.all(); len(got) != 0 {
			t.Fatalf("events while idle = %v", got)
		}
		a.set(func(a *syncApp) { a.work = issue })
		time.Sleep(450 * time.Second)

		if got, want := ev.all(), []string{"pin", "run CSR-9", "publish CSR-9"}; !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		all, syncs := a.all("POST /sync")
		_, claims := a.all("POST /issues/CSR-9/claim")
		if len(claims) != 1 {
			t.Fatalf("claims = %d, want 1; requests = %v", len(claims), all)
		}
		// The claim follows the first answer with the work at once; that
		// answer came on the interval, at most 60s plus jitter after the
		// work arrived.
		claimed := claims[0].at
		if d := claimed.Sub(start); d < 150*time.Second || d > 213*time.Second {
			t.Errorf("claimed %v after the start, want on the first sync after 150s", d)
		}
		if !slices.ContainsFunc(syncs, func(c syncCall) bool { return c.at.Equal(claimed) }) {
			t.Errorf("no sync at the claim's time %v", claimed.Sub(start))
		}
		if ended.IsZero() || !slices.ContainsFunc(syncs, func(c syncCall) bool { return c.at.Equal(ended) }) {
			t.Errorf("no sync at once when the run ended, %v", ended.Sub(start))
		}
		// 600s: one per minute idle, two a minute never (nothing is watched
		// here), and the extra ones at the start and after the run.
		if n := len(syncs); n < 9 || n > 13 {
			t.Errorf("syncs = %d in ten minutes, want about one a minute", n)
		}
		for _, c := range all {
			if c != "POST /sync" && c != "PUT /report" && c != "POST /issues/CSR-9/claim" {
				t.Errorf("request %s", c)
			}
		}
		if st := s.Status()["CSR"]; !st.Connected || st.LastPoll.IsZero() {
			t.Errorf("status = %+v, want connected", st)
		}
	})
}

// Work that cannot be run is tried once per sync answer and never again
// before the next: a skipped or lost issue walks To do once, and the loop
// then waits for the next answer instead of spinning on the one it has.
func TestSyncAnswerIsUsedOnce(t *testing.T) {
	for what, tt := range map[string]struct {
		check error
		claim int
	}{
		"skipped by the check": {check: context.DeadlineExceeded},
		"claimed by another":   {claim: http.StatusConflict},
		"no run step":          {},
	} {
		t.Run(what, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := &syncApp{work: issue, claim: tt.claim}
				h := Hooks{Check: func(context.Context, *cinderline.Client, cinderline.Issue) error { return tt.check }}
				if what != "no run step" {
					h.Run = func(context.Context, *cinderline.Client, cinderline.Issue) error {
						t.Error("the issue ran")
						return nil
					}
				}
				s := &Scheduler{Log: quiet(), ReviewInterval: time.Hour, MergeInterval: time.Hour}
				syncSlot(t, s, memClient(a.ServeHTTP), h)
				time.Sleep(10 * time.Minute)

				all, syncs := a.all("POST /sync")
				if n := len(syncs); n < 10 || n > 12 {
					t.Errorf("syncs = %d in ten minutes, want one a minute and one more at the start", n)
				}
				_, walks := a.all("GET /issues?status=todo")
				_, claims := a.all("POST /issues/CSR-9/claim")
				switch {
				case what == "no run step" && len(walks)+len(claims) != 0:
					t.Errorf("walks = %d, claims = %d without a run step", len(walks), len(claims))
				case what != "no run step" && len(walks) != len(syncs):
					t.Errorf("walks of To do = %d for %d sync answers, want one each", len(walks), len(syncs))
				case tt.claim != 0 && len(claims) != len(syncs):
					t.Errorf("claims = %d for %d sync answers, want one each", len(claims), len(syncs))
				}
				for _, c := range all {
					if strings.HasPrefix(c, "unexpected") {
						t.Errorf("request %s", c)
					}
				}
			})
		})
	}
}

// A failed sync is the slot's failed poll: it backs off as after a failed
// GET /work, asks again by itself, and is connected again once the app
// answers. The syncs stay few: the backoff's, not a loop's.
func TestSyncFailureBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{down: true}
		s := &Scheduler{Log: quiet(), ReviewInterval: time.Hour, MergeInterval: time.Hour}
		syncSlot(t, s, memClient(a.ServeHTTP), Hooks{})
		time.Sleep(60 * time.Second)
		if st := s.Status()["CSR"]; st.Connected || !strings.Contains(st.LastError, "503") {
			t.Errorf("status = %+v, want the failed sync", st)
		}
		// The slot's waits double from 1s: 0, 1, 3, 7, 15, 31; the loop's own minute never comes.
		if _, syncs := a.all("POST /sync"); len(syncs) < 5 || len(syncs) > 8 {
			t.Errorf("syncs = %d in a minute of outage, want the backoff's few", len(syncs))
		}
		a.set(func(a *syncApp) { a.down = false })
		time.Sleep(60 * time.Second)
		if st := s.Status()["CSR"]; !st.Connected || st.LastError != "" {
			t.Errorf("status = %+v, want connected again", st)
		}
		all, _ := a.all("")
		for _, c := range all {
			if strings.HasPrefix(c, "unexpected") {
				t.Errorf("request %s", c)
			}
		}
	})
}

// With sync offered the review-branch and merge checks keep their own
// intervals, whatever interval the app sets for the sync, and read the
// Review list of the latest sync answer without a request of their own.
func TestSyncTimersKeepTheirCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{after: "20", review: `[{"ref":"CSR-3","title":"T","status":{"value":"review","label":"Review"},"team":{"id":1,"name":"Dev","trusted":true}}]`}
		at := map[cinderline.Poll][]time.Time{}
		var mu sync.Mutex
		list := func(p cinderline.Poll) func(context.Context, *cinderline.Client) error {
			return func(ctx context.Context, c *cinderline.Client) error {
				issues, err := c.ListReview(ctx, p)
				if err != nil || len(issues) != 1 || issues[0].Ref != "CSR-3" || issues[0].Team.Trusted == nil || !*issues[0].Team.Trusted {
					t.Errorf("ListReview(%s) = %+v, %v; want the sync answer's CSR-3 of a trusted team", p, issues, err)
				}
				mu.Lock()
				defer mu.Unlock()
				at[p] = append(at[p], time.Now())
				return nil
			}
		}
		s := &Scheduler{Log: quiet(), ReviewInterval: 300 * time.Second, MergeInterval: 60 * time.Second}
		start := time.Now()
		syncSlot(t, s, memClient(a.ServeHTTP), Hooks{MergeCheck: list(cinderline.PollMerge), ReviewBranches: list(cinderline.PollReview)})
		time.Sleep(11 * time.Minute)
		mu.Lock()
		defer mu.Unlock()
		checkGaps(t, "merge", start, at[cinderline.PollMerge], []time.Duration{60, 60, 60, 60})
		checkGaps(t, "review", start, at[cinderline.PollReview], []time.Duration{300, 300})
		all, syncs := a.all("POST /sync")
		if n := len(syncs); n < 28 || n > 36 {
			t.Errorf("syncs = %d in eleven minutes, want one per 20s", n)
		}
		for _, c := range all {
			if c != "POST /sync" && c != "PUT /report" {
				t.Errorf("request %s", c)
			}
		}
	})
}

// End to end against an app that speaks sync: idle, work arrives, it runs
// with its issue watched, a person stops it, and the slot is idle again.
// The only request the runner repeats on a timer is POST /sync.
func TestSyncEndToEnd(t *testing.T) {
	a := &syncApp{review: `[{"ref":"CSR-3","title":"T","status":{"value":"review","label":"Review"},"team":{"id":1,"name":"Dev","trusted":true}}]`}
	a.status = func(_ string, n int) string {
		if n < 2 {
			return "in_progress"
		}
		return "blocked"
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	c := cinderline.New(srv.URL, "tok", "0.0.0")
	c.HTTP, c.Log = srv.Client(), quiet()
	c.SyncWait, c.SyncWatchWait = 40*time.Millisecond, 20*time.Millisecond

	ev := &events{}
	lists := func(p cinderline.Poll) func(context.Context, *cinderline.Client) error {
		return func(ctx context.Context, c *cinderline.Client) error {
			if issues, err := c.ListReview(ctx, p); err == nil && len(issues) == 1 {
				ev.add("list " + string(p))
			}
			return nil
		}
	}
	s, _ := sched()
	began := time.Now()
	syncSlot(t, s, c, Hooks{
		Pin: handshake, // as the real Pin: a GET /project before the claims
		Run: func(ctx context.Context, c *cinderline.Client, i cinderline.Issue) error {
			// As a turn's watcher: the ref is watched until a stop status.
			checks, stop := c.Watch(i.Ref)
			defer stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case w := <-checks:
					ev.add("check " + w.Ref + " " + w.Status.Value)
					if w.Status.Value == "blocked" {
						return nil
					}
				}
			}
		},
		Publish:        recordIssue(ev, "publish"),
		MergeCheck:     lists(cinderline.PollMerge),
		ReviewBranches: lists(cinderline.PollReview),
	})

	idle := func(n int) func() bool {
		return func() bool {
			_, syncs := a.all("POST /sync")
			return len(syncs) >= n && ev.count("list merge") > 0 && ev.count("list review") > 0
		}
	}
	waitFor(t, "idle syncs", idle(3))
	_, before := a.all("POST /sync")
	a.set(func(a *syncApp) { a.work = issue })
	waitFor(t, "the run's end", func() bool { return ev.count("publish CSR-9") == 1 })
	_, during := a.all("POST /sync")
	waitFor(t, "idle syncs after the run", idle(len(during)+3))
	elapsed := time.Since(began)

	var checks []string
	for _, e := range ev.all() {
		if strings.HasPrefix(e, "check ") {
			checks = append(checks, e)
		}
	}
	if want := []string{"check CSR-9 in_progress", "check CSR-9 in_progress", "check CSR-9 blocked"}; !slices.Equal(checks, want) {
		t.Errorf("checks = %v, want %v", checks, want)
	}
	all, syncs := a.all("POST /sync")
	for i, c := range syncs {
		watching := i >= len(before) && i < len(during)
		switch {
		case len(c.watch) > 1 || len(c.watch) == 1 && c.watch[0] != "CSR-9":
			t.Errorf("sync %d watched %v", i, c.watch)
		case len(c.watch) == 1 && !watching:
			t.Errorf("sync %d watched %v outside the run", i, c.watch)
		}
	}
	if !slices.ContainsFunc(syncs[len(before):len(during)], func(c syncCall) bool { return len(c.watch) == 1 }) {
		t.Error("no sync watched CSR-9 during the run")
	}
	// Timed requests: only the sync. The claim and the report follow events.
	timed := map[string]bool{}
	for _, c := range all {
		if c != "POST /issues/CSR-9/claim" && c != "PUT /report" {
			timed[c] = true
		}
	}
	if len(timed) != 1 || !timed["POST /sync"] {
		t.Errorf("timed requests = %v, want only POST /sync", timed)
	}
	// Bounded: the watching interval at its fastest, and the syncs asked for at once.
	if max := int(elapsed/(20*time.Millisecond)) + 6; len(syncs) > max {
		t.Errorf("syncs = %d in %v, want at most %d", len(syncs), elapsed, max)
	}
}
