package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The cases of legacy runner.test.mjs, lib.test.mjs and lib.prompt.test.mjs
// on branch upkeep (decisions 2026-11-04-agent-resolves-merge-conflicts and
// 2026-11-04-runner-updates-branch-before-push), and the branches of legacy
// runner.mjs updateBranch(), catchUp() and updateReviewBranches() that no
// legacy test reaches. The legacy tests drive a whole process with real git
// and fake CLIs; these drive UpdateBranch, CatchUp and ReviewBranches
// against the fakes, so a "conflict" is what the Workspace fake reports.

// Ported from lib.test.mjs "reportsBlocked finds a line that starts with
// Blocked, after markdown".
func TestReportsBlocked(t *testing.T) {
	for _, m := range []string{"Blocked: no idea.", "Tried.\n**Blocked**: the tests fail.", "- blocked by a missing key", "## BLOCKED"} {
		if !reportsBlocked(m) {
			t.Errorf("reportsBlocked(%q) = false", m)
		}
	}
	for _, m := range []string{"Merged; nothing blocked it.", "Unblocked it.", ""} {
		if reportsBlocked(m) {
			t.Errorf("reportsBlocked(%q) = true", m)
		}
	}
}

// Ported from runner.mjs catchUp(): what a paused turn left is committed
// first, then the base merged in; a conflict is returned for the run's first
// request and an error only logs, so the run goes on and publish tries again.
func TestCatchUp(t *testing.T) {
	var log bytes.Buffer
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&log, nil))
	ws := &workspace.Fake{
		Commits:    []workspace.CommitReply{{}},
		MergeBases: []workspace.MergeBaseReply{{Conflicts: []string{"a.go", "b.go"}}},
		Heads:      []workspace.HeadReply{{Commit: "old"}},
		Logs:       []workspace.LogReply{{Log: "abc CSR-1: T"}},
	}
	cf := p.CatchUp(context.Background(), ws, "CSR-1", "/clone", "main", "T", devTeam)
	if cf == nil || cf.head != "old" || !reflect.DeepEqual(cf.last.Conflicts, []string{"a.go", "b.go"}) || deref(cf.last.Commits) != "abc CSR-1: T" {
		t.Fatalf("CatchUp = %+v, want the conflict", cf)
	}
	if got := ws.Calls(); got[0].Method != "Commit" || got[0].Args[1] != "Dev / Builder" || got[0].Args[2] != "CSR-1: T" || got[1].Method != "MergeBase" {
		t.Errorf("workspace calls = %+v, want the commit, then the merge", got)
	}
	if !strings.Contains(log.String(), "a.go") {
		t.Errorf("log = %s", log.String())
	}

	ws = &workspace.Fake{Commits: []workspace.CommitReply{{Err: errors.New("disk full")}}}
	if cf := p.CatchUp(context.Background(), ws, "CSR-1", "/clone", "main", "T", devTeam); cf != nil || !strings.Contains(log.String(), "disk full") {
		t.Errorf("CatchUp = %+v, log = %s", cf, log.String())
	}
	if got := wsMethods(ws); !reflect.DeepEqual(got, []string{"Commit"}) {
		t.Errorf("workspace calls = %v, want no merge after a failed commit", got)
	}

	ws = &workspace.Fake{Commits: []workspace.CommitReply{{}}, MergeBases: []workspace.MergeBaseReply{{Err: errors.New("fetch failed")}}}
	if cf := p.CatchUp(context.Background(), ws, "CSR-1", "/clone", "main", "T", devTeam); cf != nil || !strings.Contains(log.String(), "fetch failed") {
		t.Errorf("CatchUp = %+v, log = %s", cf, log.String())
	}
}

// reviewRun is one ReviewBranches call with its fakes. CSR-1 is a
// top-level Review issue; CSR-2 its sub-issue and CSR-3 an issue without a
// team are never touched.
type reviewRun struct {
	p     *Project // one per reviewRun, so its cache lasts across ticks
	api   *fakeAPI
	ws    *workspace.Fake
	forge *forge.Fake
	log   bytes.Buffer
}

func newReviewRun(trusted bool, mergeStatus string) *reviewRun {
	ms := "null"
	if mergeStatus != "" {
		ms = `"` + mergeStatus + `"`
	}
	issues := `{"issues":[` +
		`{"ref":"CSR-1","title":"T","status":{"value":"review","label":"x"},"merge_status":` + ms + `,"team":{"id":1,"name":"Dev"}},` +
		`{"ref":"CSR-2","title":"S","status":{"value":"review","label":"x"},"team":{"id":1,"name":"Dev"},"parent":{"ref":"CSR-1","title":"T"}},` +
		`{"ref":"CSR-3","title":"N","status":{"value":"review","label":"x"},"team":null}]}`
	team, _ := json.Marshal(cinderline.Team{ID: 1, Name: "Dev", Trusted: trusted, Agents: devTeam.Agents})
	return &reviewRun{
		api: &fakeAPI{routes: map[string]reply{
			"GET /issues?purpose=review&status=review": {200, issues},
			"GET /teams/1":          {200, string(team)},
			"POST /runs/CSR-1/next": {200, `{"state":"s","step":{"kind":"none"}}`},
		}},
		ws: &workspace.Fake{
			Prepares:  []workspace.PrepareReply{{Dir: "/review"}},
			Strandeds: []workspace.StrandedReply{{}},
			Behinds:   []workspace.BehindReply{{Branches: []string{"cinderline/CSR-1", "cinderline/CSR-4"}}},
		},
		forge: &forge.Fake{},
	}
}

func (r *reviewRun) run(t *testing.T) error {
	t.Helper()
	if r.p == nil {
		p := project(t)
		p.Log = slog.New(slog.NewTextHandler(&r.log, nil))
		p.Hooks()
		r.p = &p
	}
	return r.p.ReviewBranches(context.Background(), r.api.client(t), r.ws, r.forge, "main")
}

func (r *reviewRun) methods() []string {
	var out []string
	for _, c := range r.ws.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func (r *reviewRun) forgeMethods() []string {
	var out []string
	for _, c := range r.forge.Calls() {
		out = append(out, c.Method)
	}
	return out
}

// Ported from runner.test.mjs "a poll with no claim keeps a <trusted or
// untrusted> Review branch current: <state>", one row per legacy case. Only
// the top-level issue with a team is updated: a sub-issue's branch is its
// parent's. Git only: no agent runs here. A conflict goes to next, which
// answers none and sets a trusted issue's merge_status itself.
func TestReviewBranches(t *testing.T) {
	pr := forge.PullRequest{Number: 1, URL: "https://git.example/o/r/pulls/1"}
	tests := []struct {
		state   string
		trusted bool
		ancs    []workspace.IsAncestorReply // HEAD in main, then main in HEAD
		merge   []workspace.MergeBaseReply
		ws      []string
		forge   []string
		next    bool // the conflict reported to next
		log     string
	}{
		{"behind main, merging cleanly", true, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{}},
			[]string{"Stranded", "Behind", "Prepare", "IsAncestor", "IsAncestor", "MergeBase", "Push"}, []string{"FindPullRequest", "RequestMerge"}, false, "base merged in"},
		{"behind main, merging cleanly", false, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{}},
			[]string{"Stranded", "Behind", "Prepare", "IsAncestor", "IsAncestor", "MergeBase", "Push"}, nil, false, "base merged in"},
		{"behind main, with a conflict", true, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}},
			[]string{"Stranded", "Behind", "Prepare", "IsAncestor", "IsAncestor", "MergeBase", "Head", "Log"}, nil, true, "work.txt"},
		{"behind main, with a conflict", false, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}},
			[]string{"Stranded", "Behind", "Prepare", "IsAncestor", "IsAncestor", "MergeBase", "Head", "Log"}, nil, true, "work.txt"},
		{"already merged", true, []workspace.IsAncestorReply{{Yes: true}}, nil,
			[]string{"Stranded", "Behind", "Prepare", "IsAncestor"}, nil, false, ""},
		{"already current", true, []workspace.IsAncestorReply{{}, {Yes: true}}, nil,
			[]string{"Stranded", "Behind", "Prepare", "IsAncestor", "IsAncestor"}, nil, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.state+map[bool]string{true: ", trusted", false: ", untrusted"}[tt.trusted], func(t *testing.T) {
			r := newReviewRun(tt.trusted, "")
			r.ws.Ancestors, r.ws.MergeBases, r.ws.Pushes = tt.ancs, tt.merge, []error{nil}
			r.ws.Heads, r.ws.Logs = []workspace.HeadReply{{Commit: "old"}}, []workspace.LogReply{{Log: "abc CSR-1: T"}}
			r.forge.Finds = []forge.FindReply{{PullRequest: pr, Found: true}}
			r.forge.Merges = []error{nil}
			if err := r.run(t); err != nil {
				t.Fatalf("ReviewBranches = %v", err)
			}
			if got := r.methods(); !reflect.DeepEqual(got, tt.ws) {
				t.Errorf("workspace calls = %v, want %v", got, tt.ws)
			}
			for _, c := range r.ws.Calls() {
				switch c.Method {
				case "Prepare":
					if !reflect.DeepEqual(c.Args, []any{"cinderline/CSR-1", "main"}) {
						t.Errorf("Prepare %v", c.Args)
					}
				case "MergeBase":
					if c.Args[2] != "Dev / Builder" {
						t.Errorf("MergeBase %v: want the lead agent's identity", c.Args)
					}
				case "Push":
					if !reflect.DeepEqual(c.Args, []any{"/review", "cinderline/CSR-1"}) {
						t.Errorf("Push %v: never the base", c.Args)
					}
				}
			}
			if got := r.forgeMethods(); !reflect.DeepEqual(got, tt.forge) {
				t.Errorf("forge calls = %v, want %v", got, tt.forge)
			}
			if tt.forge != nil {
				calls := r.forge.Calls()
				if calls[0].Args[0] != "cinderline/CSR-1" || calls[1].Args[0] != pr {
					t.Errorf("forge calls = %+v", calls)
				}
			}
			writes := r.api.writes()
			if !tt.next && len(writes) != 0 || tt.next && (len(writes) != 1 || !strings.HasPrefix(writes[0], "POST /runs/CSR-1/next ")) {
				t.Errorf("Cinderline writes = %v, want the next request %v", writes, tt.next)
			}
			if tt.next {
				req := nextRequests(t, r.api)[0]
				want := cinderline.NextRequest{Context: "review", Sessions: []cinderline.ResumableSession{}, Workspace: cinderline.RunWorkspace{
					Branch: "cinderline/CSR-1", Base: "main", ExtraRepositories: []cinderline.WorkspaceRepository{}, LinkedProjects: []cinderline.WorkspaceLink{},
				}}
				l := req.Last
				if req.State != nil || l == nil || l.Step != "merge" || l.Issue != "CSR-1" || l.Agent != nil || l.Outcome != "conflict" ||
					!reflect.DeepEqual(l.Conflicts, []string{"work.txt"}) || deref(l.Commits) != "abc CSR-1: T" {
					t.Errorf("last = %+v", l)
				}
				if req.Last = nil; !reflect.DeepEqual(req, want) {
					t.Errorf("request = %+v, want %+v", req, want)
				}
			}
			if !strings.Contains(r.log.String(), tt.log) {
				t.Errorf("log lacks %q:\n%s", tt.log, r.log.String())
			}
		})
	}
}

// A Review branch's conflict is reported to next once: the next tick with
// the same head and files asks nothing, a new conflict asks again, and a
// call that fails is asked again the next tick. In Review the runner does
// only none: another step is logged and nothing runs.
func TestAReviewConflictIsAskedOnce(t *testing.T) {
	r := newReviewRun(true, "issue")
	tick := func(head string, files ...string) {
		t.Helper()
		r.ws.Prepares, r.ws.Strandeds = []workspace.PrepareReply{{Dir: "/review"}}, []workspace.StrandedReply{{}}
		r.ws.Behinds = []workspace.BehindReply{{Branches: []string{"cinderline/CSR-1"}}}
		r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}}
		r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: files}}
		r.ws.Heads, r.ws.Logs = []workspace.HeadReply{{Commit: head}}, []workspace.LogReply{{}}
		if err := r.run(t); err != nil {
			t.Fatal(err)
		}
	}
	asks := func() int { return len(nextRequests(t, r.api)) }

	tick("old", "work.txt")
	tick("old", "work.txt")
	if n := asks(); n != 1 {
		t.Errorf("requests after the same conflict twice = %d, want 1", n)
	}
	tick("new", "work.txt")
	tick("new", "work.txt", "go.mod")
	if n := asks(); n != 3 {
		t.Errorf("requests after a new head and new files = %d, want 3", n)
	}

	r.api.routes["POST /runs/CSR-1/next"] = reply{502, `{}`}
	tick("other", "work.txt")
	if !strings.Contains(r.log.String(), "branch not updated") {
		t.Errorf("log = %s, want the failed call logged", r.log.String())
	}
	r.api.routes["POST /runs/CSR-1/next"] = reply{200, `{"state":"s","step":{"kind":"resolve","agent":"Merge Resolver","session":"new","token":"none","prompt":"Merge.","progress":null}}`}
	tick("other", "work.txt")
	if n := asks(); n != 5 {
		t.Errorf("requests = %d, want the failed one asked again", n)
	}
	if !strings.Contains(r.log.String(), "step not done in Review") {
		t.Errorf("log = %s, want the resolve step logged", r.log.String())
	}
	if w := r.api.writes(); slices.ContainsFunc(w, func(s string) bool { return !strings.Contains(s, "/next ") }) {
		t.Errorf("Cinderline writes = %v, want only next requests", w)
	}
	if got := r.methods(); slices.Contains(got, "Push") || slices.Contains(got, "Reset") {
		t.Errorf("workspace = %v, want the branch left as it is", got)
	}
}

// Ported from runner.mjs updateReviewBranches(): only an open pull request
// is asked to merge, so one a person closed is never opened again, and a
// refused merge request only logs.
func TestReviewBranchesTrustedPullRequest(t *testing.T) {
	for _, tt := range []struct {
		name  string
		find  forge.FindReply
		merge []error
		calls []string
		log   string
	}{
		{"closed by a person", forge.FindReply{}, nil, []string{"FindPullRequest"}, ""},
		{"merge refused", forge.FindReply{Found: true}, []error{errors.New("405")}, []string{"FindPullRequest", "RequestMerge"}, "405"},
		{"forge down", forge.FindReply{Err: errors.New("502")}, nil, []string{"FindPullRequest"}, "502"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newReviewRun(true, "")
			r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}}
			r.ws.MergeBases, r.ws.Pushes = []workspace.MergeBaseReply{{}}, []error{nil}
			r.forge.Finds, r.forge.Merges = []forge.FindReply{tt.find}, tt.merge
			if err := r.run(t); err != nil {
				t.Fatalf("ReviewBranches = %v", err)
			}
			if got := r.forgeMethods(); !reflect.DeepEqual(got, tt.calls) {
				t.Errorf("forge calls = %v, want %v", got, tt.calls)
			}
			if !strings.Contains(r.log.String(), tt.log) {
				t.Errorf("log lacks %q:\n%s", tt.log, r.log.String())
			}
		})
	}
}

// Ported from runner.mjs updateReviewBranches(): an issue that fails is
// logged and the next one checked; the list failing is the hook's error.
func TestReviewBranchesFailures(t *testing.T) {
	r := newReviewRun(false, "")
	r.api.routes["GET /issues?purpose=review&status=review"] = reply{200, `{"issues":[` + issueJSON("CSR-1", "review") + `,` + issueJSON("CSR-4", "review") + `]}`}
	r.ws.Prepares = []workspace.PrepareReply{{Err: errors.New("clone failed")}, {Dir: "/review"}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}}
	if err := r.run(t); err != nil {
		t.Fatalf("ReviewBranches = %v", err)
	}
	if !strings.Contains(r.log.String(), "clone failed") || !strings.Contains(r.log.String(), "CSR-1") {
		t.Errorf("log = %s", r.log.String())
	}
	if got := r.ws.Calls(); len(got) != 5 || got[3].Args[0] != "cinderline/CSR-4" {
		t.Errorf("workspace calls = %+v, want CSR-4 checked after CSR-1 failed", got)
	}

	r = newReviewRun(false, "")
	r.api.routes["GET /issues?purpose=review&status=review"] = reply{500, `{"message":"down"}`}
	if err := r.run(t); err == nil {
		t.Error("a failed list is no error")
	}
}

// A branch that is not behind the base gets no clone: one fetch per tick,
// not one per Review issue (decision 2026-11-11-audit-fixes).
func TestReviewBranchesSkipsCurrentBranches(t *testing.T) {
	r := newReviewRun(true, "")
	r.ws.Behinds = []workspace.BehindReply{{}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"Stranded", "Behind"}) {
		t.Errorf("workspace calls = %v", got)
	}
}

// A Review issue's clone with commits the remote lacks (a failed run or
// publish) is pushed before a fresh clone replaces it, and its pull request
// opened; while the push fails the clone stays (decision 2026-11-11-audit-fixes).
func TestReviewBranchesNeverRemovesUnpushedWork(t *testing.T) {
	clone := []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-1", Branch: "cinderline/CSR-1"}}}}

	r := newReviewRun(false, "")
	r.ws.Strandeds, r.ws.Pushes = clone, []error{errors.New("forge down")}
	r.forge.MergeStates = []forge.MergeStateReply{{}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"Stranded", "Push", "Behind"}) {
		t.Errorf("after a failed push, workspace calls = %v; want no Prepare", got)
	}

	r = newReviewRun(false, "")
	r.ws.Strandeds, r.ws.Pushes = clone, []error{nil}
	r.ws.Ancestors = []workspace.IsAncestorReply{{}, {Yes: true}}
	r.forge.MergeStates = []forge.MergeStateReply{{}}
	r.forge.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{Number: 1, URL: "u"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"Stranded", "Push", "Behind", "Prepare", "IsAncestor", "IsAncestor"}) {
		t.Errorf("after the push, workspace calls = %v", got)
	}
	if got := r.forgeMethods(); !reflect.DeepEqual(got, []string{"MergeState", "OpenPullRequest"}) {
		t.Errorf("forge calls = %v", got)
	}

	// The list failing leaves every clone alone.
	r = newReviewRun(false, "")
	r.ws.Strandeds = []workspace.StrandedReply{{Err: errors.New("docker down")}}
	if err := r.run(t); err == nil || len(r.methods()) != 1 {
		t.Errorf("ReviewBranches = %v after %v; want an error and no clone", err, r.methods())
	}
}
