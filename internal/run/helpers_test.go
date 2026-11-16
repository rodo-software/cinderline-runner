package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// TestMain makes every wait of the package instant.
func TestMain(m *testing.M) {
	sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	waitFor = func(time.Duration) time.Duration { return time.Millisecond }
	os.Exit(m.Run())
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type reply struct {
	status int
	body   string
}

type nextCall struct {
	ref string
	req cinderline.NextRequest
}

// fakeApp is the app's runner API: it records every poll and next request
// and answers them with onPoll and onNext (n counts from 0); without one a
// 404. Attachments are served from files, by "REF/ID".
type fakeApp struct {
	mu     sync.Mutex
	polls  []cinderline.PollRequest
	nexts  []nextCall
	onPoll func(n int, req cinderline.PollRequest) reply
	onNext func(n int, ref string, req cinderline.NextRequest) reply
	files  map[string]string
}

func (a *fakeApp) client(t *testing.T) *cinderline.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/runner/v2")
		rep := reply{404, `{"message":"Not found."}`}
		a.mu.Lock()
		switch {
		case r.Method == "POST" && path == "/poll":
			var req cinderline.PollRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			a.polls = append(a.polls, req)
			if a.onPoll != nil {
				rep = a.onPoll(len(a.polls)-1, req)
			}
		case r.Method == "POST" && strings.HasPrefix(path, "/runs/") && strings.HasSuffix(path, "/next"):
			var req cinderline.NextRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			ref := strings.TrimSuffix(strings.TrimPrefix(path, "/runs/"), "/next")
			a.nexts = append(a.nexts, nextCall{ref, req})
			if a.onNext != nil {
				rep = a.onNext(len(a.nexts)-1, ref, req)
			}
		case r.Method == "GET" && strings.HasPrefix(path, "/issues/"):
			parts := strings.Split(strings.TrimPrefix(path, "/issues/"), "/attachments/")
			if body, ok := a.files[strings.Join(parts, "/")]; ok {
				rep = reply{200, body}
			}
		}
		a.mu.Unlock()
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	c := cinderline.New(srv.URL, "runner-token", "0.0.0-test")
	c.HTTP, c.Log = srv.Client(), discard()
	return c
}

// requests returns the next requests so far.
func (a *fakeApp) requests() []cinderline.NextRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []cinderline.NextRequest
	for _, n := range a.nexts {
		out = append(out, n.req)
	}
	return out
}

func (a *fakeApp) pollsSoFar() []cinderline.PollRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]cinderline.PollRequest{}, a.polls...)
}

// lasts are the last facts of the next requests, nil ones left out.
func (a *fakeApp) lasts() []cinderline.LastStep {
	var out []cinderline.LastStep
	for _, r := range a.requests() {
		if r.Last != nil {
			out = append(out, *r.Last)
		}
	}
	return out
}

// stepsOf answers next with the steps in order, then with done kept.
func stepsOf(steps ...string) func(int, string, cinderline.NextRequest) reply {
	return func(n int, _ string, _ cinderline.NextRequest) reply {
		step := `{"kind":"done","session":"kept"}`
		if n < len(steps) {
			step = steps[n]
		}
		return reply{200, fmt.Sprintf(`{"state":"s%d","step":%s}`, n+1, step)}
	}
}

var cfg = config.Project{URL: "https://cs.example.com", Key: "CSR", Workspace: config.DefaultWorkspace, Repository: "https://git.example/o/r"}

// proj is the project as the app answers it.
var proj = cinderline.Project{Key: "CSR", Name: "Runner", RepositoryURL: cfg.Repository, Forge: "forgejo", BaseBranch: "main", Environment: "default", CheckCommand: "make test", IdleMinutes: 20}

// soloJob is a job of a solo team.
func soloJob() cinderline.Job {
	return cinderline.Job{
		Issue: cinderline.JobIssue{Ref: "CSR-1", Title: "Fix it", Body: "Please."},
		Team:  cinderline.Team{Name: "Dev", MaxMinutes: 10, MaxTurns: 50, Agents: []cinderline.TeamAgent{{Name: "Coder", Client: "claude"}}},
	}
}

func fill[T any](n int, v T) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// okWorkspace answers every call that changes nothing; IsAncestor, Behind and
// Stranded are left to each test (Stranded: none).
func okWorkspace() *workspace.Fake {
	return &workspace.Fake{
		Prepares:   fill(5, workspace.PrepareReply{Dir: "/runs/CSR-1"}),
		Commits:    fill(20, workspace.CommitReply{Committed: true}),
		MergeBases: fill(20, workspace.MergeBaseReply{}),
		Pushes:     fill(20, error(nil)),
		Heads:      fill(20, workspace.HeadReply{Commit: "abc123"}),
		Strandeds:  fill(5, workspace.StrandedReply{}),
	}
}

type fixture struct {
	p   *Project
	ws  *workspace.Fake
	fg  *forge.Fake
	cl  *client.Fake
	env *environment.Fake
}

// newFixture is a project on the fakes with a Claude Code client.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	keyring.MockInit()
	state := t.TempDir()
	f := &fixture{ws: okWorkspace(), fg: &forge.Fake{}, cl: &client.Fake{}, env: &environment.Fake{}}
	f.p = &Project{
		Config: cfg, Secrets: secrets.New(state), State: state, Env: f.env, Log: discard(),
		Clients:   map[string]client.Client{"claude": f.cl},
		Workspace: f.ws,
		Forge:     func(string) (forge.Forge, error) { return f.fg, nil },
	}
	return f
}

// methods are the workspace's calls, by name.
func methods(ws *workspace.Fake) []string {
	var out []string
	for _, c := range ws.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func turns(cl *client.Fake) []client.Turn {
	var out []client.Turn
	for _, c := range cl.Calls() {
		out = append(out, c.Args[0].(client.Turn))
	}
	return out
}

func ok(session, report string) client.RunReply {
	return client.RunReply{Result: client.Result{SessionID: session, Report: report, Usage: &client.Usage{InputTokens: 3, OutputTokens: 2}}}
}
