package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// soloRunAPI answers what hookRun asks for a solo team's CSR-7, In progress,
// with simpleNext as the app.
func soloRunAPI() *fakeAPI {
	issue := `{"ref":"CSR-7","title":"Add hello","body":"Add a file.","status":{"value":"in_progress","label":"x"},"team":{"id":1,"name":"Dev"},"sub_issues":[]}`
	return &fakeAPI{handle: simpleNext("Coder"), routes: map[string]reply{
		"GET /project":                   {200, `{"key":"CSR","name":"CSR","repository_url":"` + cfg.Repository + `","forge":"forgejo","base_branch":"main","environment":"default"}`},
		"GET /teams/1":                   {200, `{"id":1,"name":"Dev","max_minutes":30,"agents":[{"name":"Coder","role":"coder","client":"claude"}]}`},
		"GET /issues/CSR-7":              {200, issue},
		"POST /issues/CSR-7/comments":    {201, `{"id":1,"body":"x","author":{"name":"runner"}}`},
		"POST /issues/CSR-7/progress":    {204, ""},
		"POST /runs/CSR-7/agent-token":   {201, `{"token":"scoped","expires_at":"2030-01-01T00:00:00Z"}`},
		"DELETE /runs/CSR-7/agent-token": {204, ""},
	}}
}

// A finished solo run keeps its session; when the issue is back in To do,
// the next run fast-forwards the clone and offers that session to next as
// kept, and resumes it when the step says so (CSR-145; the follow-up prompt
// is the app's, EMBER-215). A clone that cannot be fast-forwarded, or a
// session pruning deleted, is not offered, with a comment that says so.
func TestAFollowUpResumesTheKeptSession(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ff      error  // FastForward's answer
		pruned  bool   // pruning deleted the session between the runs
		comment string // the second run's comment; empty means it resumes
	}{
		{"resumes", nil, false, ""},
		{"a branch that cannot fast-forward", errors.New("not possible to fast-forward"), false, "could not be brought up to date with the remote branch cinderline/CSR-7"},
		{"a pruned session", nil, true, "was deleted because it was not used for longer than keep_sessions_for"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			p := project(t)
			if _, err := p.Secrets.Set(cfg, secrets.AnthropicCredential, "sk-ant"); err != nil {
				t.Fatal(err)
			}
			clone := t.TempDir()
			var deleted []string
			p.deleteSession = func(_ context.Context, ref, session string) error {
				deleted = append(deleted, ref+" "+session)
				return nil
			}
			cl := &client.Fake{Runs: []client.RunReply{
				{Result: client.Result{SessionID: "s1", Report: "Added hello."}},
				{Result: client.Result{SessionID: "s1", Report: "Answered."}},
			}}
			ws := &workspace.Fake{
				Prepares:     []workspace.PrepareReply{{Dir: clone}, {Dir: clone}},
				Commits:      make([]workspace.CommitReply, 4), // the catch-up's and publish's of two runs
				MergeBases:   make([]workspace.MergeBaseReply, 4),
				FastForwards: []error{tt.ff},
			}
			p.Clients = map[string]client.Client{"claude": cl}
			p.Workspace = ws
			p.Forge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
			h := p.Hooks()
			api := soloRunAPI()
			c := api.client(t)
			issue := cinderline.Issue{Ref: "CSR-7", Team: &cinderline.IssueTeam{ID: 1}}

			if err := h.Run(ctx, c, issue); err != nil {
				t.Fatal(err)
			}
			p.takeHandoff()
			n, ok := p.readPaused("CSR-7")
			if !ok || n.Session != "s1" || n.Dir != clone || n.Client != "claude" || n.Kept.IsZero() {
				t.Fatalf("note after the finished run = %+v, %v; want s1 kept", n, ok)
			}
			if tt.pruned {
				if err := p.SavePaused("CSR-7", Paused{Pruned: prunedAge}); err != nil {
					t.Fatal(err)
				}
			}

			if err := h.Run(ctx, c, issue); err != nil {
				t.Fatal(err)
			}
			second := turns(cl)[1]
			var commented string
			for _, w := range api.writes() {
				if strings.HasPrefix(w, "POST /issues/CSR-7/comments") {
					commented += w
				}
			}
			reqs := nextRequests(t, api)
			offered := reqs[len(reqs)-2].Sessions
			if tt.comment == "" {
				if len(offered) != 1 || offered[0] != (cinderline.ResumableSession{Issue: "CSR-7", Agent: "Coder", Origin: "kept"}) {
					t.Fatalf("sessions offered = %+v, want s1 as kept", offered)
				}
				want := client.Turn{Dir: clone, Prompt: "Do CSR-7.", SessionID: "s1", MaxTurns: 50, MaxMinutes: 30, Idle: defaultIdle, Author: "Dev / Coder", Token: "scoped"}
				if second != want {
					t.Fatalf("second turn = %+v, want %+v", second, want)
				}
				if commented != "" || len(ws.Prepares) != 1 {
					t.Fatalf("comments %q, %d prepares left; want none and the kept clone", commented, len(ws.Prepares))
				}
			} else {
				if second.SessionID != "" || len(offered) != 0 {
					t.Fatalf("second turn = %+v, sessions offered %+v; want a fresh session", second, offered)
				}
				if !strings.Contains(commented, tt.comment) {
					t.Fatalf("comments %q, want %q", commented, tt.comment)
				}
				if tt.ff != nil && (len(deleted) != 1 || deleted[0] != "CSR-7 s1") {
					t.Fatalf("deleted sessions %q, want CSR-7 s1", deleted)
				}
			}
			if n, _ := p.readPaused("CSR-7"); n.Session != "s1" || n.Kept.IsZero() {
				t.Fatalf("note after the follow-up = %+v, want its session kept", n)
			}
		})
	}
}

var time1 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// A team with workers offers its orchestrator's kept session too, and
// resumes it when the step says so.
func TestAFollowUpResumesTheOrchestrator(t *testing.T) {
	r := newTeamStepRun(t, teamStep("CSR-1", "Parent", "Builder", "resume", "orchestrator"), failStep)
	clone := t.TempDir()
	if err := r.p.SavePaused("CSR-1", Paused{Dir: clone, Session: "the-session", Client: "claude", Kept: time1}); err != nil {
		t.Fatal(err)
	}
	r.ws.FastForwards = []error{nil}
	r.cl.Runs = []client.RunReply{ok("the-session", "Answered.")}
	_ = r.run(t)
	if s := r.reqs(t)[0].Sessions; len(s) != 1 || s[0] != offered("CSR-1", "Builder", "kept") {
		t.Errorf("sessions = %+v, want the kept one", s)
	}
	ts := turns(r.cl)
	if len(ts) != 1 || ts[0].SessionID != "the-session" || ts[0].Dir != clone {
		t.Fatalf("turns = %+v, want the follow-up in the-session", ts)
	}
	if n, _ := r.p.readPaused("CSR-1"); n.Session != "the-session" || !n.Kept.After(time1) {
		t.Fatalf("note = %+v, want kept again with a later time", n)
	}
}
