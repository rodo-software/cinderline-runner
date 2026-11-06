package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
	"github.com/rodo-software/cinderstack-runner/internal/client"
	"github.com/rodo-software/cinderstack-runner/internal/environment"
	"github.com/rodo-software/cinderstack-runner/internal/workspace"
)

// The cases of legacy runner.test.mjs, lib.test.mjs and lib.prompt.test.mjs
// on questions, pauses and comments during a run. The legacy runner tests
// drive a whole process with fake CLIs; these drive the same branches of
// attempts(), takePaused() and dropChangedKind() against the fakes.

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
	cs := []cinderstack.Comment{{ID: 3}, {ID: 5, Author: cinderstack.CommentAuthor{Agent: &agent}}, {ID: 7}}
	if got := newestCommentID(cs); got != 7 {
		t.Errorf("newestCommentID = %d, want 7", got)
	}
	if got := newestCommentID(nil); got != 0 {
		t.Errorf("newestCommentID(nil) = %d, want 0", got)
	}
	for _, tt := range []struct {
		cs     []cinderstack.Comment
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

// Ported from runner.test.mjs "an answered issue resumes its session and can
// pause again": the resumed run asks again, so the issue is Blocked after it;
// the note names the same session and nothing is commented or moved.
func TestAnsweredIssueResumesAndCanPauseAgain(t *testing.T) {
	p := project(t)
	note := pausedNote(t, p, Paused{Session: "the-session", Client: "claude", Kind: KindCode})
	got, ok := p.TakePaused("CSR-7", "claude")
	if !ok || got != note {
		t.Fatalf("TakePaused = %+v, %v; want %+v", got, ok, note)
	}

	api := &fakeAPI{routes: map[string]reply{
		"GET /issues/CSR-7":          {200, issueJSON("CSR-7", "blocked")},
		"GET /issues/CSR-7/comments": comments(comment(5, false)),
	}}
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "the-session"}}}}
	j := job
	j.Turn.Dir, j.Resume = got.Dir, got.Session
	out, err := p.Attempts(context.Background(), api.client(t), cl, &environment.Fake{}, j)
	if err != nil || !out.Paused {
		t.Fatalf("Attempts = %+v, %v; want paused", out, err)
	}

	want := client.Turn{Dir: note.Dir, Prompt: ResumePrompt("CSR-7"), SessionID: "the-session", MaxTurns: first.MaxTurns}
	if ts := turns(cl); len(ts) != 1 || ts[0] != want {
		t.Fatalf("turns = %+v, want %+v", ts, want)
	}
	if w := api.writes(); len(w) != 0 {
		t.Fatalf("writes = %q, want none", w)
	}
	again, ok := p.TakePaused("CSR-7", "claude")
	if !ok || again != note {
		t.Fatalf("note after the pause = %+v, %v; want %+v", again, ok, note)
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
			out, err := p.Attempts(context.Background(), api.client(t), cl, &environment.Fake{}, j)
			if err != nil || !out.Paused || len(cl.Calls()) != 1 {
				t.Fatalf("Attempts = %+v, %v after %d runs; want paused after 1", out, err, len(cl.Calls()))
			}
			got, ok := p.TakePaused("CSR-7", "claude")
			if want := (Paused{Dir: j.Turn.Dir, Session: "s1", Client: "claude", Kind: KindCode}); !ok || got != want {
				t.Fatalf("note = %+v, %v; want %+v", got, ok, want)
			}
		})
	}
}

// A finished run leaves no note, so the next claim starts fresh.
func TestAFinishedRunDoesNotPause(t *testing.T) {
	p := project(t)
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1"}}}}
	out, err := p.Attempts(context.Background(), working().client(t), cl, &environment.Fake{}, job)
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
			got, ok := p.TakePaused("CSR-7", tt.client)
			if ok != tt.ok || ok && got != note {
				t.Fatalf("TakePaused = %+v, %v; want %+v, %v", got, ok, note, tt.ok)
			}
			if _, err := os.Stat(noteFile(p)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("note after TakePaused: %v, want removed", err)
			}
		})
	}
	if _, ok := project(t).TakePaused("CSR-7", "claude"); ok {
		t.Fatal("TakePaused found a note that was never written")
	}
}

// Ported from runner.test.mjs "a coding run paused before its team became a
// writer starts over", with legacy dropChangedKind's comment naming the
// paused coding run's last commit. A writer's paused note has no commit to
// name; a note of the same kind stays.
func TestDropChangedKind(t *testing.T) {
	for _, tt := range []struct {
		name, noteKind, kind string
		comment              string // empty: the note stays and nothing is said
	}{
		{"code to write", KindCode, KindWrite, "The team changed while paused, so this run starts over. The paused coding run's last commit was abc123."},
		{"write to code", KindWrite, KindCode, "The team changed while paused, so this run starts over."},
		{"same kind", KindCode, KindCode, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := project(t)
			note := pausedNote(t, p, Paused{Session: "the-session", Client: "claude", Kind: tt.noteKind})
			api := &fakeAPI{routes: map[string]reply{"POST /issues/CSR-7/comments": {201, comment(9, false)}}}
			ws := &workspace.Fake{Heads: []workspace.HeadReply{{Commit: "abc123"}}}
			if err := p.DropChangedKind(context.Background(), api.client(t), ws, "CSR-7", tt.kind); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(noteFile(p))
			if tt.comment == "" {
				if statErr != nil || len(api.writes()) != 0 {
					t.Fatalf("note: %v, writes %q; want the note kept and nothing said", statErr, api.writes())
				}
				return
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("note: %v, want dropped", statErr)
			}
			body, _ := json.Marshal(map[string]string{"body": tt.comment})
			if w := api.writes(); !slices.Equal(w, []string{"POST /issues/CSR-7/comments " + string(body)}) {
				t.Fatalf("writes = %q, want the comment %q", w, tt.comment)
			}
			if c := ws.Calls(); tt.noteKind == KindCode && (len(c) != 1 || c[0].Args[0] != note.Dir) {
				t.Fatalf("workspace calls = %v, want Head(%s)", c, note.Dir)
			}
		})
	}
}

// A last commit git cannot read is left out, as in legacy; the run still
// starts over.
func TestDropChangedKindWithoutHead(t *testing.T) {
	p := project(t)
	pausedNote(t, p, Paused{Session: "s", Client: "claude", Kind: KindCode})
	api := &fakeAPI{routes: map[string]reply{"POST /issues/CSR-7/comments": {201, comment(9, false)}}}
	ws := &workspace.Fake{Heads: []workspace.HeadReply{{Err: errors.New("not a git repository")}}}
	if err := p.DropChangedKind(context.Background(), api.client(t), ws, "CSR-7", KindWrite); err != nil {
		t.Fatal(err)
	}
	if w := api.writes(); len(w) != 1 || !strings.Contains(w[0], `starts over."}`) {
		t.Fatalf("writes = %q", w)
	}
}

// Ported from runner.test.mjs "a comment by <a person|an agent> during a run
// <resumes the session before the pull request|does not resume it>": 5
// answered a question before the run and is never delivered twice; 7 comes
// during the run.
func TestACommentDuringARun(t *testing.T) {
	for _, by := range []string{"a person", "an agent"} {
		t.Run(by, func(t *testing.T) {
			p := project(t)
			api := working()
			api.queues = map[string][]reply{"GET /issues/CSR-7/comments": {
				comments(comment(5, false), comment(6, true)),
				comments(comment(5, false), comment(6, true), comment(7, by == "an agent")),
			}}
			cl := &client.Fake{Runs: []client.RunReply{
				{Result: client.Result{SessionID: "the-session", Report: "built"}},
				{Result: client.Result{SessionID: "the-session", Report: "read them"}},
			}}
			out, err := p.Attempts(context.Background(), api.client(t), cl, &environment.Fake{}, job)
			if err != nil || out.Paused || out.Unread {
				t.Fatalf("Attempts = %+v, %v", out, err)
			}
			ts := turns(cl)
			if by == "an agent" {
				if len(ts) != 1 || out.Report != "built" {
					t.Fatalf("turns = %+v, report %q; want one run", ts, out.Report)
				}
				return
			}
			// Resumed once in the same session; the second check finds nothing new.
			want := client.Turn{Dir: first.Dir, Prompt: CommentsPrompt("CSR-7"), SessionID: "the-session", MaxTurns: first.MaxTurns}
			if len(ts) != 2 || ts[1] != want || out.Report != "read them" {
				t.Fatalf("turns = %+v, report %q; want second %+v", ts, out.Report, want)
			}
			if w := api.writes(); !slices.Equal(w, []string{`POST /issues/CSR-7/progress {"body":"Reading new comments from a person"}`}) {
				t.Fatalf("writes = %q", w)
			}
		})
	}
}

// Ported from runner.test.mjs "a comment resume that fails twice still
// publishes the finished work, with a note": both resumes run in the same
// session with the comments prompt, nothing starts over, and the outcome
// says to add UnreadComments to the final comment.
func TestACommentResumeThatFailsTwice(t *testing.T) {
	p := project(t)
	api := working()
	api.queues = map[string][]reply{"GET /issues/CSR-7/comments": {comments(), comments(comment(7, false))}}
	cl := &client.Fake{Runs: []client.RunReply{
		{Result: client.Result{SessionID: "the-session", Report: "built"}},
		{Result: client.Result{SessionID: "the-session"}, Err: errFailed},
		{Result: client.Result{SessionID: "the-session"}, Err: errFailed},
	}}
	env := &environment.Fake{}
	out, err := p.Attempts(context.Background(), api.client(t), cl, env, job)
	if err != nil || !out.Unread || out.Paused || out.Report != "built" {
		t.Fatalf("Attempts = %+v, %v; want the finished work with Unread", out, err)
	}
	ts := turns(cl)
	want := client.Turn{Dir: first.Dir, Prompt: CommentsPrompt("CSR-7"), SessionID: "the-session", MaxTurns: first.MaxTurns}
	if len(ts) != 3 || ts[1] != want || ts[2] != want {
		t.Fatalf("turns = %+v, want two resumes %+v", ts, want)
	}
	if len(env.Calls()) != 0 {
		t.Fatalf("environment calls = %v, want no tail", env.Calls())
	}
	if !strings.Contains(UnreadComments, "resuming the agent to read those comments failed twice") {
		t.Fatalf("UnreadComments = %q", UnreadComments)
	}
}

// A comment resume that hits the turn limit continues with the continue
// prompt, as legacy does; then a success ends it without the note.
func TestACommentResumeContinuesAfterTheTurnLimit(t *testing.T) {
	p := project(t)
	api := working()
	api.queues = map[string][]reply{"GET /issues/CSR-7/comments": {comments(), comments(comment(7, false))}}
	cl := &client.Fake{Runs: []client.RunReply{
		{Result: client.Result{SessionID: "s1"}},
		{Result: client.Result{SessionID: "s1", HitTurnLimit: true}, Err: errFailed},
		{Result: client.Result{SessionID: "s1", Report: "done"}},
	}}
	out, err := p.Attempts(context.Background(), api.client(t), cl, &environment.Fake{}, job)
	if err != nil || out.Unread || out.Report != "done" {
		t.Fatalf("Attempts = %+v, %v", out, err)
	}
	if ts := turns(cl); len(ts) != 3 || ts[2].Prompt != ContinuePrompt("CSR-7") || ts[2].SessionID != "s1" {
		t.Fatalf("turns = %+v", ts)
	}
}

// A resumed run that fails for another reason starts over fresh, with the
// first prompt and no session (legacy: paused and resume cleared).
func TestAFailedResumeStartsOver(t *testing.T) {
	p := project(t)
	cl := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "the-session"}, Err: errFailed}, {}}}
	j := job
	j.Resume = "the-session"
	if _, err := p.Attempts(context.Background(), working().client(t), cl, &environment.Fake{}, j); err != nil {
		t.Fatal(err)
	}
	if ts := turns(cl); len(ts) != 2 || ts[1] != first {
		t.Fatalf("turns = %+v, want second %+v", ts, first)
	}
}

// A status or comment list CinderStack cannot give ends the run with the
// error, which the scheduler comments before Review.
func TestAttemptsFailsWhenCinderStackDoes(t *testing.T) {
	for _, route := range []string{"GET /issues/CSR-7", "GET /issues/CSR-7/comments"} {
		t.Run(route, func(t *testing.T) {
			api := working()
			api.routes[route] = reply{500, `{"message":"Server error."}`}
			cl := &client.Fake{Runs: []client.RunReply{{}}}
			if _, err := project(t).Attempts(context.Background(), api.client(t), cl, &environment.Fake{}, job); err == nil {
				t.Fatal("Attempts succeeded without CinderStack")
			}
		})
	}
}
