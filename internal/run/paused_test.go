package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The cases of legacy runner.test.mjs, lib.test.mjs and lib.prompt.test.mjs
// on questions, pauses and comments during a run. The legacy runner tests
// drive a whole process with fake CLIs; these drive the same branches of
// attempts() and takePaused() against the fakes.

// pausedNote writes a note for CSR-7 whose clone exists, as pausedRun does.
func pausedNote(t *testing.T, p Project, n Paused) Paused {
	t.Helper()
	if n.Dir == "" {
		n.Dir = t.TempDir()
	}
	if err := p.SavePaused("CSR-7", n); err != nil {
		t.Fatal(err)
	}
	return n
}

// take is TakePaused for a paused (not kept) note, which needs no API.
func take(p Project, ref, client string) (Paused, bool) {
	return p.TakePaused(context.Background(), nil, &workspace.Fake{}, ref, "cinderline/"+ref, client, false)
}

func noteFile(p Project) string {
	return filepath.Join(PausedDir(p.State, p.Config), "CSR-7.json")
}

func comment(id int, agent bool) string {
	a := "null"
	if agent {
		a = `"Boss"`
	}
	b, _ := json.Marshal(id)
	return `{"id":` + string(b) + `,"body":"x","author":{"name":"W","agent":` + a + `},"created_at":"2026-11-06T12:00:00Z"}`
}

func comments(c ...string) reply {
	return reply{200, `{"comments":[` + strings.Join(c, ",") + `]}`}
}

// commentRange is comments from..to, by an agent or a person.
func commentRange(from, to int, agent bool) []string {
	var out []string
	for id := from; id <= to; id++ {
		out = append(out, comment(id, agent))
	}
	return out
}

// commentPage answers GET .../comments[?after=N] as the contract does
// (decision 2026-11-08-comment-cursor): with after, the oldest 200 newer than
// it; without, the newest 200; oldest first either way.
func commentPage(path string, cs []string) reply {
	after := 0
	if _, q, ok := strings.Cut(path, "?after="); ok {
		after, _ = strconv.Atoi(q)
	}
	var out []string
	for _, c := range cs {
		var v struct{ ID int }
		_ = json.Unmarshal([]byte(c), &v)
		if v.ID > after {
			out = append(out, c)
		}
	}
	if after > 0 {
		out = out[:min(len(out), 200)]
	} else {
		out = out[max(0, len(out)-200):]
	}
	return comments(out...)
}

// Ported from lib.prompt.test.mjs "resumePrompt tells the agent to reply to
// a question in the answer first".
func TestResumePrompt(t *testing.T) {
	got := ResumePrompt("CSR-3")
	for _, want := range []string{
		"list_comments on CSR-3",
		"If the person's answer also asks you something, reply with reply_person on CSR-3 first, then continue",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ResumePrompt = %q, want %q in it", got, want)
		}
	}
}

// Ported from lib.test.mjs "only a person's comments after the cursor count".
func TestCommentCursor(t *testing.T) {
	agent := "Boss"
	cs := []cinderline.Comment{{ID: 3}, {ID: 5, Author: cinderline.CommentAuthor{Agent: &agent}}, {ID: 7}}
	if got := newestCommentID(cs); got != 7 {
		t.Errorf("newestCommentID = %d, want 7", got)
	}
	if got := newestCommentID(nil); got != 0 {
		t.Errorf("newestCommentID(nil) = %d, want 0", got)
	}
	for _, tt := range []struct {
		cs     []cinderline.Comment
		cursor int
		want   bool
	}{
		{cs, 3, true}, {cs, 7, false}, {cs[:2], 0, true}, {cs[1:2], 0, false},
	} {
		if got := personCommentedAfter(tt.cs, tt.cursor); got != tt.want {
			t.Errorf("personCommentedAfter(%v, %d) = %v, want %v", tt.cs, tt.cursor, got, tt.want)
		}
	}
	if got := CommentsPrompt("CSR-3"); !strings.Contains(got, "a person commented on CSR-3. Read the latest comments with list_comments on CSR-3") {
		t.Errorf("CommentsPrompt = %q", got)
	}
}

// A fresh run that asks a person pauses too, and a quick answer that moved
// the issue back to To do already is a pause as well (decision 0116).
func TestAQuestionPausesTheRun(t *testing.T) {
	for _, status := range []string{"blocked", "todo"} {
		t.Run(status, func(t *testing.T) {
			p := project(t)
			api := &fakeAPI{routes: map[string]reply{
				"GET /issues/CSR-7":          {200, issueJSON("CSR-7", status)},
				"GET /issues/CSR-7/comments": comments(),
			}}
			// A failed run that asked is not retried: the person answers first.
			cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1"}, Err: errFailed}}}
			j := job
			j.Turn.Dir = t.TempDir()
			out, err := p.Attempts(context.Background(), api.client(t), cl, j)
			if err != nil || !out.Paused || len(cl.Calls()) != 1 {
				t.Fatalf("Attempts = %+v, %v after %d runs; want paused after 1", out, err, len(cl.Calls()))
			}
			got, ok := take(p, "CSR-7", "claude")
			if want := (Paused{Dir: j.Turn.Dir, Session: "s1", Client: "claude"}); !ok || got != want {
				t.Fatalf("note = %+v, %v; want %+v", got, ok, want)
			}
		})
	}
}

// Attempts writes no note for a finished run: its caller keeps the session
// for a follow-up (CSR-145).
func TestAFinishedRunDoesNotPause(t *testing.T) {
	p := project(t)
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1"}}}}
	out, err := p.Attempts(context.Background(), working().client(t), cl, job)
	if err != nil || out.Paused || out.Unread {
		t.Fatalf("Attempts = %+v, %v", out, err)
	}
	if _, err := os.Stat(noteFile(p)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("note: %v, want none", err)
	}
}

// Ported from runner.test.mjs "an answered issue of an OpenCode orchestrator
// resumes in OpenCode", with legacy readPaused's other refusals: a note
// resumes only in the client that made it, with a session and its clone.
// Taking a note removes it, so a crash during the resumed run starts fresh.
func TestTakePaused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		note   Paused
		client string
		ok     bool
	}{
		{"an OpenCode note in OpenCode", Paused{Session: "ses_paused", Client: "opencode"}, "opencode", true},
		{"an OpenCode note in Claude", Paused{Session: "ses_paused", Client: "opencode"}, "claude", false},
		{"no session", Paused{Client: "claude"}, "claude", false},
		{"a clone that is gone", Paused{Session: "s", Client: "claude", Dir: "/gone/CSR-7"}, "claude", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := project(t)
			note := pausedNote(t, p, tt.note)
			got, ok := take(p, "CSR-7", tt.client)
			if ok != tt.ok || ok && got != note {
				t.Fatalf("TakePaused = %+v, %v; want %+v, %v", got, ok, note, tt.ok)
			}
			if _, err := os.Stat(noteFile(p)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("note after TakePaused: %v, want removed", err)
			}
		})
	}
	if _, ok := take(project(t), "CSR-7", "claude"); ok {
		t.Fatal("TakePaused found a note that was never written")
	}
}

// A status Cinderline cannot give ends a team's turn with the error, which
// the scheduler comments before Blocked.
func TestAttemptsFailsWhenCinderlineDoes(t *testing.T) {
	for _, route := range []string{"GET /issues/CSR-7"} {
		t.Run(route, func(t *testing.T) {
			api := working()
			api.routes[route] = reply{500, `{"message":"Server error."}`}
			cl := &client.Fake{Runs: []client.RunReply{{}}}
			if _, err := project(t).Attempts(context.Background(), api.client(t), cl, job); err == nil {
				t.Fatal("Attempts succeeded without Cinderline")
			}
		})
	}
}
