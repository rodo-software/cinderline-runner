package cinderline

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// flaky is an instance that fails every request while down, and records
// the writes it took.
type flaky struct {
	mu     sync.Mutex
	down   bool
	status map[string]int // by "METHOD path", when up; default 201 or 200
	writes []string
}

func (f *flaky) client(t *testing.T, outbox string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/runner/v1")
		status := f.status[key]
		switch {
		case status != 0:
		case r.Method == "POST" && strings.HasSuffix(key, "/comments"):
			status = 201
		case r.Method == "POST":
			status = 204
		default:
			status = 200
		}
		if status < 300 {
			f.writes = append(f.writes, key+" "+string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"ref":"CSR-7","status":{"value":"review","label":"Review"}}`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok", "0.0.0")
	c.HTTP, c.Outbox, c.SendWaits = srv.Client(), outbox, []time.Duration{0, 0}
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

func (f *flaky) set(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *flaky) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

var (
	report = Write{Ref: "CSR-7", Comment: "Opened the pull request."}
	review = Write{Ref: "CSR-7", Move: &UpdateIssueRequest{Status: "review", ExpectedStatus: "in_progress"}}
	usage  = Write{Ref: "CSR-7", Usage: []UsageEntry{{Issue: "CSR-7", Agent: "Coder", OutputTokens: 5}}}
)

// A write that goes through is not kept.
func TestSendDelivers(t *testing.T) {
	f := &flaky{}
	c := f.client(t, filepath.Join(t.TempDir(), "outbox.json"))
	c.Send(context.Background(), report)
	if got := f.took(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /issues/CSR-7/comments") {
		t.Fatalf("writes = %q", got)
	}
	if c.Waiting("CSR-7") {
		t.Error("a delivered write is still waiting")
	}
}

// Writes that fail after the retries wait on disk, in order, and a new
// client on the same file (a restart) delivers them once Cinderline answers
// again: the issue is never left In progress silently (CSR-159).
func TestSendKeepsFailedWritesAcrossARestart(t *testing.T) {
	f := &flaky{down: true}
	file := filepath.Join(t.TempDir(), "outbox", "CSR.json")
	c := f.client(t, file)
	for _, w := range []Write{report, review, usage} {
		c.Send(context.Background(), w)
	}
	if !c.Waiting("CSR-7") || c.Waiting("CSR-8") {
		t.Fatal("Waiting: want CSR-7 only")
	}
	c.Replay(context.Background()) // still down
	if len(f.took()) != 0 {
		t.Fatalf("writes while down = %q", f.took())
	}

	f.set(false)
	restarted := f.client(t, file)
	if !restarted.Waiting("CSR-7") {
		t.Fatal("the restarted client lost the waiting writes")
	}
	restarted.Replay(context.Background())
	got := f.took()
	want := []string{"POST /issues/CSR-7/comments", "PATCH /issues/CSR-7", "POST /runs/CSR-7/usage"}
	if len(got) != len(want) {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("writes = %q, want %q", got, want)
		}
	}
	if restarted.Waiting("CSR-7") {
		t.Error("delivered writes are still waiting")
	}
	if again := f.client(t, file); again.Waiting("CSR-7") {
		t.Error("the file still holds delivered writes")
	}
}

// While an issue has a write waiting, a later one for it waits behind it,
// so a move never overtakes its comment; other issues are not held up.
func TestSendKeepsOrderPerIssue(t *testing.T) {
	f := &flaky{down: true}
	c := f.client(t, filepath.Join(t.TempDir(), "outbox.json"))
	c.Send(context.Background(), report)
	f.set(false)
	c.Send(context.Background(), review)
	c.Send(context.Background(), Write{Ref: "CSR-8", Comment: "other"})
	if got := f.took(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /issues/CSR-8/comments") {
		t.Fatalf("writes = %q, want only CSR-8's", got)
	}
	c.Replay(context.Background())
	got := f.took()
	if len(got) != 3 || !strings.HasPrefix(got[1], "POST /issues/CSR-7/comments") || !strings.HasPrefix(got[2], "PATCH /issues/CSR-7") {
		t.Fatalf("writes = %q", got)
	}
}

// A write Cinderline refuses for good (a move someone overtook: 409; any
// other 4xx) is dropped, not replayed forever.
func TestReplayDropsRefusedWrites(t *testing.T) {
	f := &flaky{down: true}
	c := f.client(t, filepath.Join(t.TempDir(), "outbox.json"))
	c.Send(context.Background(), review)
	c.Send(context.Background(), Write{Ref: "CSR-8", Comment: "x"})
	f.set(false)
	f.status = map[string]int{"PATCH /issues/CSR-7": 409, "POST /issues/CSR-8/comments": 422}
	c.Replay(context.Background())
	if c.Waiting("CSR-7") || c.Waiting("CSR-8") {
		t.Error("refused writes are still waiting")
	}
}

// Replay backs off while Cinderline stays down: a second call right after a
// failed one sends nothing.
func TestReplayBacksOff(t *testing.T) {
	f := &flaky{down: true}
	c := f.client(t, filepath.Join(t.TempDir(), "outbox.json"))
	c.Send(context.Background(), report)
	c.Replay(context.Background())
	f.set(false)
	c.Replay(context.Background())
	if len(f.took()) != 0 {
		t.Fatalf("replayed during the backoff: %q", f.took())
	}
	c.out.replayAt = time.Time{}
	c.Replay(context.Background())
	if len(f.took()) != 1 {
		t.Fatalf("writes = %q", f.took())
	}
}

// Secrets in a comment never reach the outbox file.
func TestSendRedactsBeforeKeeping(t *testing.T) {
	f := &flaky{down: true}
	file := filepath.Join(t.TempDir(), "outbox.json")
	c := f.client(t, file)
	redact.Add("runner token", runnerToken)
	c.Send(context.Background(), Write{Ref: "CSR-7", Comment: "token " + runnerToken})
	if b, err := os.ReadFile(file); err != nil || strings.Contains(string(b), runnerToken) {
		t.Errorf("outbox holds the token: %s", b)
	}
}
