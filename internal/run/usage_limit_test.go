package run

import (
	"errors"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

func TestUsageLimitReportedAndSessionPaused(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, knownReset := range []bool{false, true} {
			f := newFixture(t)
			step := runNew
			session := "s1"
			if resumed {
				dir := t.TempDir()
				f.p.saveNote("CSR-1", Note{Dir: dir, Session: "s1", Client: "claude", Origin: "paused"})
				step = `{"kind":"run","issue":{"ref":"CSR-1"},"agent":"Coder","session":"resume","prompt":"Continue."}`
				session = ""
			}
			res := client.Result{SessionID: session, HitUsageLimit: true, Report: "limit", Tail: "output"}
			if knownReset {
				res.ResetsAt = time.Unix(1795084800, 0).UTC()
			}
			f.cl.Runs = []client.RunReply{{Result: res, Err: errors.New("exit 1")}}
			app := &fakeApp{onNext: stepsOf(step, `{"kind":"stop"}`, `{"kind":"done","session":"paused"}`)}
			if _, err := f.run(t, app); err != nil {
				t.Fatal(err)
			}
			last := app.lasts()[0]
			if last.Outcome != "usage_limit" || deref(last.Error) != "exit 1" || deref(last.Tail) != "output" || deref(last.Report) != "limit" {
				t.Fatalf("last = %+v", last)
			}
			if knownReset {
				if deref(last.ResetsAt) != "2026-11-19T10:40:00Z" {
					t.Errorf("reset = %v", last.ResetsAt)
				}
			} else if last.ResetsAt != nil {
				t.Errorf("unexpected reset = %v", last.ResetsAt)
			}
			sessions := app.requests()[1].Sessions
			if len(sessions) != 1 || sessions[0] != (cinderline.Session{Issue: "CSR-1", Agent: "Coder", Origin: "turn"}) {
				t.Errorf("sessions = %+v", sessions)
			}
			if note, ok := f.p.readNote("CSR-1"); !ok || note.Session != "s1" || note.Origin != "paused" {
				t.Errorf("note = %+v, %v", note, ok)
			}
		}
	}
}

// The copied app contract does not accept usage_limit on resolve steps.
func TestUsageLimitDuringResolveStaysFailed(t *testing.T) {
	f := newFixture(t)
	f.cl.Runs = []client.RunReply{{Result: client.Result{SessionID: "s1", HitUsageLimit: true, ResetsAt: time.Now(), Tail: "limit"}, Err: errors.New("exit 1")}}
	app := &fakeApp{onNext: stepsOf(`{"kind":"publish","resolve_conflicts":true}`, `{"kind":"resolve","agent":"Coder","session":"new","prompt":"Resolve."}`)}
	f.ws.MergeBases = []workspace.MergeBaseReply{{}, {Conflicts: []string{"a.go"}}}
	f.ws.Resets = []error{nil}
	f.ws.Logs = []workspace.LogReply{{Log: "conflict"}}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	lasts := app.lasts()
	if len(lasts) != 2 || lasts[1].Outcome != "failed" || lasts[1].ResetsAt != nil || deref(lasts[1].Tail) != "limit" {
		t.Fatalf("lasts = %+v", lasts)
	}
}
