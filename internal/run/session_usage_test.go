package run

import (
	"errors"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/client"
)

// totalReply is a turn of a client that reports its session's usage total
// so far, as Codex does (CLR-232).
func totalReply(session string, in, out int) client.RunReply {
	return client.RunReply{Result: client.Result{SessionID: session, Usage: &client.Usage{InputTokens: in, OutputTokens: out}, UsageTotal: true}}
}

// A resumed session's turn reports what it added to the session's total; a
// turn without usage keeps the total, and a new session starts at zero. The
// kept note holds the lead's total.
func TestASessionTotalIsSentAsTheTurnsDifference(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{
		totalReply("th-1", 1000, 10),
		totalReply("th-1", 3000, 30),
		{Result: client.Result{SessionID: "th-1"}, Err: errors.New("exit 1")},
		totalReply("th-1", 5000, 50),
		totalReply("th-2", 700, 7),
	}
	resume := strings.Replace(runNew, `"new"`, `"resume"`, 1)
	app := &fakeApp{onNext: stepsOf(runNew, resume, resume, resume, runNew)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	lasts := app.lasts()
	if len(lasts) != 5 {
		t.Fatalf("lasts = %+v", lasts)
	}
	want := []struct{ in, out int }{{1000, 10}, {2000, 20}, {-1, -1}, {2000, 20}, {700, 7}}
	for i, w := range want {
		u := lasts[i].Usage
		switch {
		case w.in < 0 && u != nil:
			t.Errorf("turn %d usage = %+v, want none", i+1, u)
		case w.in >= 0 && (u == nil || u.InputTokens != w.in || u.OutputTokens != w.out):
			t.Errorf("turn %d usage = %+v, want %d in, %d out", i+1, u, w.in, w.out)
		}
	}
	n, _ := f.p.readNote("CSR-1")
	if n.Session != "th-2" || n.Total == nil || *n.Total != (client.Usage{InputTokens: 700, OutputTokens: 7}) {
		t.Errorf("note = %+v (total %+v), want th-2's total", n, n.Total)
	}
}

// A kept session's total comes back from its note: the next run's first
// turn reports only what it added.
func TestAKeptSessionsTotalCarriesOver(t *testing.T) {
	f := newFixture(t)
	f.p.saveNote("CSR-1", Note{Dir: t.TempDir(), Session: "th-1", Client: "claude", Origin: "kept", Total: &client.Usage{InputTokens: 3000, OutputTokens: 30}})
	f.ws.FastForwards = []error{nil}
	f.cl.Runs = []client.RunReply{totalReply("th-1", 4000, 40)}
	app := &fakeApp{onNext: stepsOf(strings.Replace(runNew, `"new"`, `"resume"`, 1))}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	if u := app.lasts()[0].Usage; u == nil || u.InputTokens != 1000 || u.OutputTokens != 10 {
		t.Errorf("usage = %+v, want 1000 in, 10 out", u)
	}
	if n, _ := f.p.readNote("CSR-1"); n.Total == nil || n.Total.InputTokens != 4000 {
		t.Errorf("note total = %+v, want 4000 in", n.Total)
	}
}

// A team's other session keeps its total while it continues; another
// agent's session replaces it from zero, and the lead's keeps its own.
func TestHeldTotalsPerSession(t *testing.T) {
	h := &held{reports: map[string]string{}, agents: map[string]string{}, lead: session{issue: "CSR-1", agent: "Lead", id: "l", total: &client.Usage{InputTokens: 9}}}
	h.turned("CSR-1.1", "Worker", "w", "", &client.Usage{InputTokens: 5})
	h.turned("CSR-1.1", "Worker", "w", "", nil)
	if s := h.session("CSR-1.1", "Worker"); s.total == nil || s.total.InputTokens != 5 {
		t.Errorf("worker total = %+v, want 5 in", s.total)
	}
	h.turned("CSR-1.2", "Tester", "x", "", nil)
	if s := h.session("CSR-1.2", "Tester"); s.id != "x" || s.total != nil {
		t.Errorf("tester session = %+v, want x without a total", s)
	}
	if s := h.session("CSR-1", "Lead"); s.total == nil || s.total.InputTokens != 9 {
		t.Errorf("lead total = %+v, want 9 in", s.total)
	}
}
