package cinderline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// syncApp is an app that offers POST /sync (decision 2026-11-16-sync-request).
// It records every request but the handshake and answers sync n (from 0) with
// answer, or with an empty answer when answer is nil.
type syncApp struct {
	mu     sync.Mutex
	offer  string // what GET /project adds to the project: `,"sync":true`
	calls  []string
	at     []time.Time // of each sync
	watch  []string    // the watch of each sync, as its JSON
	answer func(n int) (status int, after, body string)
}

const emptySync = `{"work":null,"watched":[],"review":[]}`

func (a *syncApp) handle(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
	if path == "/project" {
		_, _ = io.WriteString(w, `{"key":"CSR"`+a.offer+`}`)
		return
	}
	a.calls = append(a.calls, r.Method+" "+path)
	switch r.Method + " " + path {
	case "POST /sync":
		var in struct {
			Watch json.RawMessage `json:"watch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		status, after, body := 200, "", emptySync
		if a.answer != nil {
			status, after, body = a.answer(len(a.at))
		}
		a.at = append(a.at, time.Now())
		a.watch = append(a.watch, string(in.Watch))
		if after != "" {
			w.Header().Set(name.Name+"-Poll-After", after)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	case "GET /work":
		_, _ = io.WriteString(w, `{"issue":null}`)
	case "GET /issues":
		_, _ = io.WriteString(w, `{"issues":[]}`)
	default: // the report and the heartbeat
		w.WriteHeader(204)
	}
}

func (a *syncApp) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

func (a *syncApp) syncs() ([]time.Time, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.at), slices.Clone(a.watch)
}

// started returns a client that shook hands with a, and runs its heartbeat
// loop until the test ends.
func (a *syncApp) started(t *testing.T) *Client {
	t.Helper()
	c := memClient(a.handle)
	if _, err := c.GetProject(t.Context()); err != nil {
		t.Fatal(err)
	}
	go c.RunHeartbeats(t.Context(), Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}})
	return c
}

// gaps fails for each wait between at (from start) that is not want's
// seconds plus Jitter's at most a quarter, at most 3s.
func gaps(t *testing.T, start time.Time, at []time.Time, want []time.Duration) {
	t.Helper()
	if len(at) < len(want) {
		t.Fatalf("%d syncs, want at least %d", len(at), len(want))
	}
	prev := start
	for i, w := range want {
		gap, lo := at[i].Sub(prev), w*time.Second
		if gap < lo || gap > lo+min(lo/4, 3*time.Second) {
			t.Errorf("wait before sync %d = %v, want %v plus jitter", i+1, gap, lo)
		}
		prev = at[i]
	}
}

// POST /sync sends the watched refs, an empty list when there are none, and
// decodes the work, the watched issues and the Review list, whose teams say
// whether they are trusted.
func TestSyncRequest(t *testing.T) {
	review := strings.Replace(issueJSON, `"team":{"id":2,"name":"Dev"}`, `"team":{"id":2,"name":"Dev","trusted":false}`, 1)
	body := `{"work":` + issueJSON + `,
		"watched":[{"ref":"CSR-12","status":{"value":"blocked","label":"Blocked"},"last_comment_by_agent":true}],
		"review":[` + review + `]}`
	c, rec := server(t, 200, body, nil)
	got, err := c.Sync(context.Background(), []string{"CSR-12"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != "POST" || rec.path != "/api/runner/v1/sync" || !sameJSON(t, rec.body, `{"watch":["CSR-12"]}`) {
		t.Errorf("request = %s %s %s", rec.method, rec.path, rec.body)
	}
	if b, _ := json.Marshal(got); !sameJSON(t, string(b), body) {
		t.Errorf("answer = %s\nwant %s", b, body)
	}
	if got.Work.Team.Trusted != nil {
		t.Error("a team without trusted is trusted or not; want unknown")
	}
	if tr := got.Review[0].Team.Trusted; tr == nil || *tr {
		t.Errorf("review team trusted = %v, want false", tr)
	}
	if w := got.Watched[0]; w.Ref != "CSR-12" || w.Status.Label != "Blocked" || !w.LastCommentByAgent {
		t.Errorf("watched = %+v", w)
	}

	c, rec = server(t, 200, emptySync, nil)
	if _, err := c.Sync(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, rec.body, `{"watch":[]}`) {
		t.Errorf("idle body = %s, want an empty list", rec.body)
	}
}

// The handshake's sync says whether the app offers POST /sync; absent, from
// an older app, means it does not.
func TestProjectSync(t *testing.T) {
	for offer, want := range map[string]bool{`,"sync":true`: true, `,"sync":false`: false, ``: false} {
		a := &syncApp{offer: offer}
		c := memClient(a.handle)
		if c.Syncing() {
			t.Error("syncing before the handshake")
		}
		p, err := c.GetProject(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if p.Sync != want || c.Syncing() != want {
			t.Errorf("%q: Sync = %v, Syncing = %v, want %v", offer, p.Sync, c.Syncing(), want)
		}
	}
}

// With sync offered, one POST /sync per wait replaces the heartbeat, and the
// report follows it as it follows a heartbeat. The first goes at once; the
// wait is the interval the last successful answer sent, clamped, else 60s
// idle, with jitter; a failed sync is logged and keeps the interval before it.
func TestSyncReplacesTheHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		answers := []struct {
			status int
			after  string
		}{{200, ""}, {200, "10"}, {500, "999"}, {200, "0"}, {200, ""}, {200, ""}}
		a := &syncApp{offer: `,"sync":true`}
		a.answer = func(n int) (int, string, string) {
			x := answers[min(n, len(answers)-1)]
			return x.status, x.after, emptySync
		}
		var buf bytes.Buffer
		c := memClient(a.handle)
		c.Log = slog.New(slog.NewTextHandler(&buf, nil))
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := c.GetProject(ctx); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		go c.RunHeartbeats(ctx, Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}})
		time.Sleep(250 * time.Second)
		cancel()
		synctest.Wait()

		at, watch := a.syncs()
		// At once, none sent (60s), 10s, 10s again after the failure, 0
		// clamped to 1s, none (60s).
		gaps(t, start, at, []time.Duration{0, 60, 10, 10, 1, 60})
		if watch[0] != "[]" {
			t.Errorf("idle watch = %s, want an empty list", watch[0])
		}
		calls := a.seen()
		if n := len(at); len(calls) != n+1 || calls[0] != "POST /sync" || calls[1] != "PUT /report" || slices.Contains(calls, "POST /heartbeat") {
			t.Errorf("requests = %v, want %d syncs and the report after the first", calls, n)
		}
		if got := strings.Count(buf.String(), "level=WARN"); got != 1 || !strings.Contains(buf.String(), "sync failed") {
			t.Errorf("warn lines = %d, want 1 for the failed sync:\n%s", got, buf.String())
		}
		if d, ok := c.PollAfter(PollSync); ok {
			t.Errorf("PollAfter(sync) = %v after an answer without one", d)
		}
	})
}

// While an issue is watched the syncs carry its ref and, when the app sends
// no interval, come every 30s; a ref is sent once however many watch it, in
// order, at most 20 of them.
func TestSyncWatchesTheRunningTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{offer: `,"sync":true`}
		c := memClient(a.handle)
		_, stop12 := c.Watch("CSR-12")
		_, stop12b := c.Watch("CSR-12")
		_, stop3 := c.Watch("CSR-3")
		start := time.Now()
		c = a.startedWith(t, c)
		time.Sleep(70 * time.Second)
		at, watch := a.syncs()
		gaps(t, start, at, []time.Duration{0, 30, 30})
		if watch[0] != `["CSR-12","CSR-3"]` {
			t.Errorf("watch = %s", watch[0])
		}
		stop12()
		stop3()
		time.Sleep(30 * time.Second) // the sync at 90s, still watching CSR-12
		stop12b()
		time.Sleep(100 * time.Second)
		at, watch = a.syncs()
		gaps(t, start, at, []time.Duration{0, 30, 30, 30, 30, 60})
		if watch[3] != `["CSR-12"]` || watch[4] != `[]` {
			t.Errorf("watch after the stops = %s, %s", watch[3], watch[4])
		}

		for i := range 25 {
			c.Watch(fmt.Sprintf("CSR-%03d", i))
		}
		time.Sleep(60 * time.Second)
		_, watch = a.syncs()
		var refs []string
		_ = json.Unmarshal([]byte(watch[len(watch)-1]), &refs)
		if len(refs) != 20 || refs[0] != "CSR-000" {
			t.Errorf("watch = %v, want the first 20", refs)
		}
	})
}

// startedWith is started for a client made before.
func (a *syncApp) startedWith(t *testing.T, c *Client) *Client {
	t.Helper()
	if _, err := c.GetProject(t.Context()); err != nil {
		t.Fatal(err)
	}
	go c.RunHeartbeats(t.Context(), Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}})
	return c
}

// A watcher gets the entry of each answer that has one for its ref, a newer
// one replacing one it has not read; an answer that leaves the ref out, a
// failed sync and a stopped watch bring nothing.
func TestSyncAnswersReachTheWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entry := func(ref, status string, agent bool) string {
			return fmt.Sprintf(`{"work":null,"watched":[{"ref":%q,"status":{"value":%q,"label":"L"},"last_comment_by_agent":%v}],"review":[]}`, ref, status, agent)
		}
		a := &syncApp{offer: `,"sync":true`}
		a.answer = func(n int) (int, string, string) {
			switch n {
			case 0:
				return 200, "", entry("CSR-12", "in_progress", false)
			case 1:
				return 200, "", entry("CSR-99", "blocked", false) // another issue's
			case 2:
				return 500, "", `{"message":"down"}`
			case 3:
				return 200, "", entry("CSR-12", "review", false)
			}
			return 200, "", entry("CSR-12", "blocked", true)
		}
		c := memClient(a.handle)
		checks, stop := c.Watch("CSR-12")
		a.startedWith(t, c)
		synctest.Wait()
		if w := <-checks; w.Status.Value != "in_progress" || w.LastCommentByAgent {
			t.Errorf("first check = %+v", w)
		}
		time.Sleep(70 * time.Second) // syncs 1 and 2
		synctest.Wait()
		select {
		case w := <-checks:
			t.Errorf("check = %+v from an answer without the ref or a failed sync", w)
		default:
		}
		time.Sleep(70 * time.Second) // syncs 3 and 4: the newer replaces the unread one
		synctest.Wait()
		if w := <-checks; w.Status.Value != "blocked" || !w.LastCommentByAgent {
			t.Errorf("check = %+v, want the latest", w)
		}
		stop()
		time.Sleep(70 * time.Second)
		synctest.Wait()
		select {
		case w := <-checks:
			t.Errorf("check = %+v after the stop", w)
		default:
		}
	})
}

// With sync offered Work sends nothing: it returns the work of each sync
// answer once, waits for the next answer after that, and returns a failed
// sync's error as its own. SyncNow gets it an answer at once, and never an
// answer from before.
func TestSyncAnswersWorkOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{offer: `,"sync":true`}
		a.answer = func(n int) (int, string, string) {
			switch n {
			case 1, 5:
				return 200, "", `{"work":` + issueJSON + `,"watched":[],"review":[]}`
			case 2:
				return 503, "", `{"message":"down"}`
			}
			return 200, "", emptySync
		}
		c := a.started(t)
		ctx := context.Background()
		start := time.Now()
		var at []time.Time
		work := func() (*Issue, error) {
			issue, err := c.Work(ctx)
			at = append(at, time.Now())
			return issue, err
		}
		if issue, err := work(); issue != nil || err != nil {
			t.Fatalf("first Work = %v, %v; want no work", issue, err)
		}
		if issue, err := work(); err != nil || issue == nil || issue.Ref != "CSR-9" {
			t.Fatalf("second Work = %v, %v; want CSR-9", issue, err)
		}
		var api *APIError
		if _, err := work(); !errors.As(err, &api) || api.StatusCode != 503 || !Unavailable(err) {
			t.Fatalf("third Work = %v, want the sync's 503", err)
		}
		if issue, err := work(); issue != nil || err != nil {
			t.Fatalf("fourth Work = %v, %v; want no work", issue, err)
		}
		// Each Work waited for its own answer: at once, then a minute apart.
		gaps(t, start, at, []time.Duration{0, 60, 60, 60})

		// Sync 4 comes while nobody asks; SyncNow drops it for a new one.
		time.Sleep(90 * time.Second)
		before := time.Now()
		c.SyncNow()
		if issue, err := work(); err != nil || issue == nil {
			t.Fatalf("Work after SyncNow = %v, %v; want sync 5's work, not sync 4's none", issue, err)
		}
		if d := time.Since(before); d != 0 {
			t.Errorf("Work after SyncNow took %v, want none", d)
		}
		syncs, _ := a.syncs()
		if len(syncs) != 6 {
			t.Errorf("syncs = %d, want 6", len(syncs))
		}
		if calls := a.seen(); slices.Contains(calls, "GET /work") || slices.Contains(calls, "POST /heartbeat") {
			t.Errorf("requests = %v, want no work poll and no heartbeat", calls)
		}

		// A Work that waits ends with its context.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := c.Work(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Work = %v, want the context's end", err)
		}
	})
}

// With sync offered ListReview sends nothing: it returns the Review list of
// the latest sync answer for either purpose, and an error while there is no
// answer or the latest sync failed.
func TestSyncAnswersListReview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{offer: `,"sync":true`}
		a.answer = func(n int) (int, string, string) {
			switch n {
			case 0:
				return 200, "", `{"work":null,"watched":[],"review":[` + issueJSON + `]}`
			case 1:
				return 500, "", `{"message":"down"}`
			}
			return 200, "", emptySync
		}
		c := memClient(a.handle)
		ctx := context.Background()
		if _, err := c.GetProject(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ListReview(ctx, PollMerge); err == nil {
			t.Error("ListReview before the first answer = nil error")
		}
		a.startedWith(t, c)
		synctest.Wait()
		for _, p := range []Poll{PollMerge, PollReview} {
			if got, err := c.ListReview(ctx, p); err != nil || len(got) != 1 || got[0].Ref != "CSR-9" {
				t.Errorf("ListReview(%s) = %v, %v; want CSR-9", p, got, err)
			}
		}
		time.Sleep(70 * time.Second)
		var api *APIError
		if _, err := c.ListReview(ctx, PollMerge); !errors.As(err, &api) || api.StatusCode != 500 {
			t.Errorf("ListReview after a failed sync = %v, want its error", err)
		}
		time.Sleep(70 * time.Second)
		if got, err := c.ListReview(ctx, PollMerge); err != nil || len(got) != 0 {
			t.Errorf("ListReview = %v, %v; want the latest, empty list", got, err)
		}
		if calls := a.seen(); slices.Contains(calls, "GET /issues") {
			t.Errorf("requests = %v, want no list", calls)
		}
	})
}

// A handshake that withdraws sync brings the separate requests back: a Work
// that waited for a sync answer polls GET /work, the loop sends heartbeats
// at their own interval, and ListReview fetches again. A handshake that
// offers it again syncs at once.
func TestSyncWithdrawnAndOfferedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{offer: `,"sync":true`}
		a.answer = func(int) (int, string, string) { return 200, "5", emptySync }
		c := a.started(t)
		ctx := context.Background()
		if _, err := c.Work(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := c.Work(ctx); done <- err }()
		synctest.Wait()

		a.mu.Lock()
		a.offer = ""
		a.mu.Unlock()
		if _, err := c.GetProject(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("Work after sync was withdrawn = %v", err)
		}
		if _, ok := c.PollAfter(PollSync); ok {
			t.Error("the sync interval outlived sync")
		}
		before := len(a.seen())
		time.Sleep(70 * time.Second)
		if _, err := c.ListReview(ctx, PollMerge); err != nil {
			t.Fatal(err)
		}
		if got, want := a.seen()[before-1:], []string{"GET /work", "POST /heartbeat", "GET /issues"}; !reflect.DeepEqual(got, want) {
			t.Errorf("requests without sync = %v, want %v", got, want)
		}

		a.mu.Lock()
		a.offer = `,"sync":true`
		a.mu.Unlock()
		if _, err := c.GetProject(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if calls := a.seen(); calls[len(calls)-1] != "POST /sync" {
			t.Errorf("requests = %v, want a sync at once", calls)
		}
	})
}

// Without sync in the handshake nothing changes: Work polls, ListReview
// fetches, the loop sends heartbeats, SyncNow does nothing and a watch gets
// nothing.
func TestWithoutSyncNothingChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &syncApp{}
		c := a.started(t)
		checks, stop := c.Watch("CSR-12")
		defer stop()
		ctx := context.Background()
		c.SyncNow()
		if _, err := c.Work(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ListReview(ctx, PollReview); err != nil {
			t.Fatal(err)
		}
		time.Sleep(130 * time.Second)
		synctest.Wait()
		want := []string{"GET /work", "GET /issues", "POST /heartbeat", "PUT /report", "POST /heartbeat"}
		if got := a.seen(); !reflect.DeepEqual(got, want) {
			t.Errorf("requests = %v, want %v", got, want)
		}
		select {
		case w := <-checks:
			t.Errorf("check = %+v without sync", w)
		default:
		}
	})
}
