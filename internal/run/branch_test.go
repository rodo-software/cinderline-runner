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
	"github.com/rodo-software/cinderline-runner/internal/client"
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

var resolverAgent = cinderline.TeamAgent{Name: "Merge Resolver", Role: "merge resolver", Client: "opencode", Model: ptr("openrouter/qwen"), Instructions: ptr("Resolve merges.")}

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

// Ported from lib.prompt.test.mjs "mergePrompt has the resolver merge and
// commit, with the conflicts as data last", with the Cinderline renames.
func TestMergePrompt(t *testing.T) {
	got := MergePrompt("Resolve well.", "CSR-3", "main", "cinderline/CSR-3", []string{"app/a.php", "b.md"}, "abc1234 CSR-4: Part\n")
	if !strings.HasPrefix(got, "Resolve well.\n\n---\n") {
		t.Errorf("prompt does not start with the instructions:\n%s", got)
	}
	for _, want := range []string{
		"Run `git merge origin/main`",
		"run the project's checks as the repository's own instructions describe",
		"`git commit --no-edit`, and leave the worktree clean",
		"no access to Cinderline",
		`start your final message with "Blocked:"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	end := "## Merge main into cinderline/CSR-3\n\nBase branch: `main`\n\nConflicting files:\n\n- `app/a.php`\n- `b.md`\n\nCommits on this branch that are not on main (`git log --oneline origin/main..HEAD`):\n\n```\nabc1234 CSR-4: Part\n```\n"
	if !strings.HasSuffix(got, end) {
		t.Errorf("prompt does not end with the data:\n%s", got)
	}
	if got := MergePrompt("R.", "CSR-3", "main", "b", []string{"a"}, " \n"); !strings.Contains(got, "```\n(none)\n```") {
		t.Errorf("an empty log is not (none):\n%s", got)
	}
}

// upkeepRun is one UpdateBranch or CatchUp call with its fakes, on CSR-1.
type upkeepRun struct {
	api      *fakeAPI
	opencode *client.Fake
	ws       *workspace.Fake
	u        Upkeep
	log      bytes.Buffer
}

func newUpkeepRun(team cinderline.Team) *upkeepRun {
	tr := newTree("in_progress")
	return &upkeepRun{
		api:      tr.api(),
		opencode: &client.Fake{},
		ws:       &workspace.Fake{},
		u:        Upkeep{Ref: "CSR-1", Branch: "cinderline/CSR-1", Base: "main", Dir: "/clone", MaxTurns: 30, Team: team},
	}
}

func (r *upkeepRun) project(t *testing.T) Project {
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&r.log, nil))
	return p
}

func (r *upkeepRun) update(t *testing.T) ([]string, error) {
	t.Helper()
	return r.project(t).UpdateBranch(context.Background(), r.api.client(t), map[string]client.Client{"opencode": r.opencode}, r.ws, r.u)
}

func (r *upkeepRun) methods() []string {
	var out []string
	for _, c := range r.ws.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func (r *upkeepRun) body(prefix string) []string {
	var out []string
	for _, w := range r.api.writes() {
		if rest, ok := strings.CutPrefix(w, prefix+" "); ok {
			var in struct{ Body string }
			_ = json.Unmarshal([]byte(rest), &in)
			out = append(out, in.Body)
		}
	}
	return out
}

// Ported from runner.test.mjs "the base branch moved during a run cleanly:
// it is merged in before the push": the merge part; the push is publish's
// (CSR-26).
func TestUpdateBranchMergesCleanly(t *testing.T) {
	r := newUpkeepRun(cinderline.Team{Agents: []cinderline.TeamAgent{devTeam.Agents[0], resolverAgent}})
	r.ws.MergeBases = []workspace.MergeBaseReply{{}}
	conflicts, err := r.update(t)
	if conflicts != nil || err != nil {
		t.Fatalf("UpdateBranch = %v, %v", conflicts, err)
	}
	if got := r.ws.Calls(); !reflect.DeepEqual(got, []workspace.Call{{Method: "MergeBase", Args: []any{"/clone", "main"}}}) {
		t.Errorf("workspace calls = %+v", got)
	}
	if len(r.opencode.Calls()) != 0 || len(r.api.writes()) != 0 {
		t.Errorf("the resolver ran or Cinderline was written: %v", r.api.writes())
	}
}

// Ported from runner.test.mjs "the base branch moved during a run with a
// conflict: the branch is pushed as it is": a team without a Merge Resolver
// keeps its branch as it was (the aborted merge) and reports the conflicts.
func TestUpdateBranchConflictWithoutResolver(t *testing.T) {
	r := newUpkeepRun(devTeam)
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	conflicts, err := r.update(t)
	if !reflect.DeepEqual(conflicts, []string{"work.txt"}) || err != nil {
		t.Fatalf("UpdateBranch = %v, %v", conflicts, err)
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"MergeBase"}) {
		t.Errorf("workspace calls = %v, want only the merge", got)
	}
	if len(r.api.writes()) != 0 {
		t.Errorf("Cinderline written: %v", r.api.writes())
	}
}

// A merge that fails without conflicts is an error, as legacy mergeBase
// throws.
func TestUpdateBranchMergeError(t *testing.T) {
	r := newUpkeepRun(devTeam)
	r.ws.MergeBases = []workspace.MergeBaseReply{{Err: errors.New("fetch failed")}}
	if _, err := r.update(t); err == nil || !strings.Contains(err.Error(), "fetch failed") {
		t.Errorf("err = %v, want the merge's error", err)
	}
}

// Ported from runner.test.mjs "a branch that conflicts with main at the
// push, and a Merge Resolver that <way>", one row per way, plus the
// branches of updateBranch() no legacy test reaches: a failed run, the
// branch's own work dropped, and a check that errs. The publish part (push,
// merge request, finish comment, merge_status issue) is CSR-26's.
func TestUpdateBranchResolver(t *testing.T) {
	ok := client.RunReply{Result: client.Result{SessionID: "s", Report: "Resolved."}}
	yes, no := workspace.BoolReply{Yes: true}, workspace.BoolReply{}
	anc := func(base, head bool) []workspace.IsAncestorReply {
		return []workspace.IsAncestorReply{{Yes: base}, {Yes: head}}
	}
	tests := []struct {
		way      string
		run      client.RunReply
		cleans   []workspace.BoolReply
		ancs     []workspace.IsAncestorReply
		markers  []workspace.BoolReply
		resolved bool
		checks   []string // the workspace calls after the run, before a reset
	}{
		{"resolves", ok, []workspace.BoolReply{yes}, anc(true, true), []workspace.BoolReply{no}, true,
			[]string{"Clean", "IsAncestor", "IsAncestor", "ConflictMarkers"}},
		{"commits the markers", ok, []workspace.BoolReply{yes}, anc(true, true), []workspace.BoolReply{yes}, false,
			[]string{"Clean", "IsAncestor", "IsAncestor", "ConflictMarkers"}},
		{"leaves the worktree unclean", ok, []workspace.BoolReply{no}, nil, nil, false,
			[]string{"Clean"}},
		{"does not merge the base", ok, []workspace.BoolReply{yes}, anc(false, true), nil, false,
			[]string{"Clean", "IsAncestor"}},
		{"reports Blocked", client.RunReply{Result: client.Result{Report: "**Blocked**: unsure."}}, nil, nil, nil, false,
			nil},
		{"fails", client.RunReply{Err: errors.New("exit 1")}, nil, nil, nil, false,
			nil},
		{"drops the branch's work", ok, []workspace.BoolReply{yes}, anc(true, false), nil, false,
			[]string{"Clean", "IsAncestor", "IsAncestor"}},
		{"leaves a check failing", ok, []workspace.BoolReply{{Err: errors.New("git broke")}}, nil, nil, false,
			[]string{"Clean"}},
	}
	for _, tt := range tests {
		t.Run(tt.way, func(t *testing.T) {
			r := newUpkeepRun(cinderline.Team{Name: "Dev", MaxMinutes: 45, Ponytail: true, Agents: []cinderline.TeamAgent{devTeam.Agents[0], devTeam.Agents[1], resolverAgent}})
			r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
			r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
			r.ws.Logs = []workspace.LogReply{{Log: "abc CSR-1: T"}}
			r.ws.Cleans, r.ws.Ancestors, r.ws.Markers = tt.cleans, tt.ancs, tt.markers
			r.ws.Resets = []error{nil}
			r.opencode.Runs = []client.RunReply{tt.run}

			conflicts, err := r.update(t)
			if err != nil {
				t.Fatalf("UpdateBranch err = %v", err)
			}
			if tt.resolved != (conflicts == nil) {
				t.Errorf("conflicts = %v, resolved %v", conflicts, tt.resolved)
			}
			if !tt.resolved && !reflect.DeepEqual(conflicts, []string{"work.txt"}) {
				t.Errorf("conflicts = %v, want work.txt", conflicts)
			}

			want := append([]string{"MergeBase", "Head", "Log"}, tt.checks...)
			if !tt.resolved {
				want = append(want, "Reset")
			}
			if got := r.methods(); !reflect.DeepEqual(got, want) {
				t.Errorf("workspace calls = %v, want %v", got, want)
			}
			for _, c := range r.ws.Calls() {
				switch c.Method {
				case "IsAncestor":
					if a := c.Args[1]; a != "main" && a != "old" || c.Args[2] != "HEAD" {
						t.Errorf("IsAncestor %v: want main, then the old head, in HEAD", c.Args)
					}
				case "ConflictMarkers":
					if !reflect.DeepEqual(c.Args[1], []string{"work.txt"}) {
						t.Errorf("ConflictMarkers looked at %v", c.Args[1])
					}
				case "Reset":
					if c.Args[1] != "old" {
						t.Errorf("Reset to %v, want the recorded head", c.Args[1])
					}
				}
			}

			runs := r.opencode.Calls()
			if len(runs) != 1 {
				t.Fatalf("resolver runs = %d, want 1 (never a retry)", len(runs))
			}
			turn := runs[0].Args[0].(client.Turn)
			if turn.Token != "" || turn.SessionID != "" || turn.Dir != "/clone" || turn.Model != "openrouter/qwen" || turn.MaxTurns != 30 || turn.MaxMinutes != 45 || !turn.Ponytail {
				t.Errorf("resolver turn = %+v, want a fresh session in the clone without a token (0122)", turn)
			}
			if !strings.HasPrefix(turn.Prompt, "Resolve merges.") || !strings.Contains(turn.Prompt, "# Your task: merge main into cinderline/CSR-1") ||
				!strings.Contains(turn.Prompt, "- `work.txt`") || !strings.Contains(turn.Prompt, "abc CSR-1: T") {
				t.Errorf("resolver prompt:\n%s", turn.Prompt)
			}

			if got := r.body("POST /issues/CSR-1/progress"); !reflect.DeepEqual(got, []string{"Resolving the merge with main"}) {
				t.Errorf("progress = %v", got)
			}
			cs := r.body("POST /issues/CSR-1/comments")
			if len(cs) != 1 {
				t.Fatalf("comments = %v, want the resolver's report", cs)
			}
			if tt.resolved {
				if cs[0] != "Resolved." {
					t.Errorf("comment = %q, want the report", cs[0])
				}
				return
			}
			if !strings.HasPrefix(cs[0], "**Failed**: the merge with main was not resolved") || !strings.Contains(cs[0], "so the runner undid it") {
				t.Errorf("comment = %q", cs[0])
			}
			if tt.run.Result.Report != "" && !strings.Contains(cs[0], tt.run.Result.Report) {
				t.Errorf("comment %q lacks the report", cs[0])
			}
		})
	}
}

// A reset that fails after a failed attempt is an error: the clone may still
// hold the resolver's half merge, so it must not be pushed.
func TestUpdateBranchResetFails(t *testing.T) {
	r := newUpkeepRun(cinderline.Team{Agents: []cinderline.TeamAgent{devTeam.Agents[0], resolverAgent}})
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
	r.ws.Logs = []workspace.LogReply{{}}
	r.ws.Cleans = []workspace.BoolReply{{}}
	r.ws.Resets = []error{errors.New("disk full")}
	r.opencode.Runs = []client.RunReply{{Result: client.Result{Report: "Done."}}}
	if _, err := r.update(t); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want the reset's error", err)
	}
}

// Ported from runner.test.mjs "a run resuming after a question on a branch
// main moved past merges main first", both rows: what the paused turn left
// is committed, then the base is merged in (by the Merge Resolver on a
// conflict), all before the orchestrator's turn. Driven through Rounds,
// which resumes from the paused note.
func TestResumedRoundsCatchUpFirst(t *testing.T) {
	for _, conflicts := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanly", true: "with the Merge Resolver"}[conflicts], func(t *testing.T) {
			tr := newTree("in_progress")
			r := newTeamRun(tr)
			p := project(t)
			clone := t.TempDir()
			if err := p.SavePaused("CSR-1", Paused{Dir: clone, Session: "the-session", Client: "claude"}); err != nil {
				t.Fatal(err)
			}
			r.job.Team.Agents = append(slices.Clone(devTeam.Agents), resolverAgent)
			order := []string{}
			r.claude.during = func(client.Turn) { order = append(order, "orchestrator") }
			r.opencode.during = func(client.Turn) { order = append(order, "resolver") }
			r.claude.Runs = []client.RunReply{ok("the-session", "Done.")}
			if conflicts {
				r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
				r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
				r.ws.Logs = []workspace.LogReply{{}}
				r.ws.Cleans = []workspace.BoolReply{{Yes: true}}
				r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}, {Yes: true}}
				r.ws.Markers = []workspace.BoolReply{{}}
				r.opencode.Runs = []client.RunReply{ok("r", "Resolved.")}
			} else {
				r.ws.MergeBases = []workspace.MergeBaseReply{{}}
			}

			if _, err := r.run(t, context.Background(), p); err != nil {
				t.Fatalf("Rounds = %v", err)
			}
			calls := r.ws.Calls()[1:] // after Exists
			if len(calls) < 2 || calls[0].Method != "Commit" || calls[0].Args[1] != "CSR-1: Parent" || calls[1].Method != "MergeBase" {
				t.Errorf("workspace calls = %+v, want the commit, then the merge first", calls)
			}
			want := []string{"orchestrator"}
			if conflicts {
				want = []string{"resolver", "orchestrator"}
			}
			if !reflect.DeepEqual(order, want) {
				t.Errorf("runs = %v, want %v", order, want)
			}
			if turn := r.claude.Calls()[0].Args[0].(client.Turn); turn.SessionID != "the-session" {
				t.Errorf("orchestrator session = %q, want the resumed one", turn.SessionID)
			}
		})
	}
}

// Ported from runner.mjs catchUp(): a conflict left or an error only logs,
// and the run goes on; publish tries again.
func TestCatchUpOnlyLogs(t *testing.T) {
	r := newUpkeepRun(devTeam)
	r.ws.Commits = []workspace.CommitReply{{}}
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"a.go", "b.go"}}}
	r.project(t).CatchUp(context.Background(), r.api.client(t), nil, r.ws, r.u, "T")
	if !strings.Contains(r.log.String(), "a.go") || !strings.Contains(r.log.String(), "base not merged in") {
		t.Errorf("log = %s", r.log.String())
	}

	r = newUpkeepRun(devTeam)
	r.ws.Commits = []workspace.CommitReply{{Err: errors.New("disk full")}}
	r.project(t).CatchUp(context.Background(), r.api.client(t), nil, r.ws, r.u, "T")
	if !strings.Contains(r.log.String(), "disk full") {
		t.Errorf("log = %s", r.log.String())
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"Commit"}) {
		t.Errorf("workspace calls = %v, want no merge after a failed commit", got)
	}
}

// reviewRun is one ReviewBranches call with its fakes. CSR-1 is a
// top-level Review issue; CSR-2 its sub-issue and CSR-3 an issue without a
// team are never touched.
type reviewRun struct {
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
			"GET /issues?status=review": {200, issues},
			"GET /teams/1":              {200, string(team)},
			"PATCH /issues/CSR-1":       {200, issueJSON("CSR-1", "review")},
		}},
		ws:    &workspace.Fake{Prepares: []workspace.PrepareReply{{Dir: "/review"}}},
		forge: &forge.Fake{},
	}
}

func (r *reviewRun) run(t *testing.T) error {
	t.Helper()
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&r.log, nil))
	return p.ReviewBranches(context.Background(), r.api.client(t), r.ws, r.forge, "main")
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
// parent's. Git only: no agent runs here.
func TestReviewBranches(t *testing.T) {
	pr := forge.PullRequest{Number: 1, URL: "https://git.example/o/r/pulls/1"}
	tests := []struct {
		state   string
		trusted bool
		ancs    []workspace.IsAncestorReply // HEAD in main, then main in HEAD
		merge   []workspace.MergeBaseReply
		ws      []string
		forge   []string
		status  bool // merge_status issue written
		log     string
	}{
		{"behind main, merging cleanly", true, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{}},
			[]string{"Prepare", "IsAncestor", "IsAncestor", "MergeBase", "Push"}, []string{"FindPullRequest", "RequestMerge"}, false, "base merged in"},
		{"behind main, merging cleanly", false, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{}},
			[]string{"Prepare", "IsAncestor", "IsAncestor", "MergeBase", "Push"}, nil, false, "base merged in"},
		{"behind main, with a conflict", true, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}},
			[]string{"Prepare", "IsAncestor", "IsAncestor", "MergeBase"}, nil, true, "work.txt"},
		{"behind main, with a conflict", false, []workspace.IsAncestorReply{{}, {}}, []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}},
			[]string{"Prepare", "IsAncestor", "IsAncestor", "MergeBase"}, nil, false, "work.txt"},
		{"already merged", true, []workspace.IsAncestorReply{{Yes: true}}, nil,
			[]string{"Prepare", "IsAncestor"}, nil, false, ""},
		{"already current", true, []workspace.IsAncestorReply{{}, {Yes: true}}, nil,
			[]string{"Prepare", "IsAncestor", "IsAncestor"}, nil, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.state+map[bool]string{true: ", trusted", false: ", untrusted"}[tt.trusted], func(t *testing.T) {
			r := newReviewRun(tt.trusted, "")
			r.ws.Ancestors, r.ws.MergeBases, r.ws.Pushes = tt.ancs, tt.merge, []error{nil}
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
			want := []string(nil)
			if tt.status {
				want = []string{`PATCH /issues/CSR-1 {"merge_status":"issue"}`}
			}
			if !reflect.DeepEqual(writes, want) {
				t.Errorf("Cinderline writes = %v, want %v", writes, want)
			}
			if !strings.Contains(r.log.String(), tt.log) {
				t.Errorf("log lacks %q:\n%s", tt.log, r.log.String())
			}
		})
	}
}

// Ported from runner.mjs updateReviewBranches(): a trusted issue whose
// merge_status is issue already is not written again.
func TestReviewBranchesKeepsAnIssueStatus(t *testing.T) {
	r := newReviewRun(true, "issue")
	r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}}
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if w := r.api.writes(); len(w) != 0 {
		t.Errorf("Cinderline writes = %v", w)
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
	r.api.routes["GET /issues?status=review"] = reply{200, `{"issues":[` + issueJSON("CSR-1", "review") + `,` + issueJSON("CSR-4", "review") + `]}`}
	r.ws.Prepares = []workspace.PrepareReply{{Err: errors.New("clone failed")}, {Dir: "/review"}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}}
	if err := r.run(t); err != nil {
		t.Fatalf("ReviewBranches = %v", err)
	}
	if !strings.Contains(r.log.String(), "clone failed") || !strings.Contains(r.log.String(), "CSR-1") {
		t.Errorf("log = %s", r.log.String())
	}
	if got := r.ws.Calls(); len(got) != 3 || got[1].Args[0] != "cinderline/CSR-4" {
		t.Errorf("workspace calls = %+v, want CSR-4 checked after CSR-1 failed", got)
	}

	r = newReviewRun(false, "")
	r.api.routes["GET /issues?status=review"] = reply{500, `{"message":"down"}`}
	if err := r.run(t); err == nil {
		t.Error("a failed list is no error")
	}
}

// soloRun is an UpdateBranch on CSR-1 by a solo team (only the
// orchestrator, on claude) that has just worked in session "s1" with its
// scoped token "tok" (decision 2026-11-07-solo-team-resolves-merge-conflicts).
func soloRun(t *testing.T, team cinderline.Team, run client.RunReply, verified bool) (*upkeepRun, *client.Fake, []string, error) {
	t.Helper()
	r := newUpkeepRun(team)
	r.u.Session, r.u.Token = "s1", "tok"
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
	r.ws.Logs = []workspace.LogReply{{Log: "abc CSR-1: T"}}
	r.ws.Cleans = []workspace.BoolReply{{Yes: verified}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}, {Yes: true}}
	r.ws.Markers = []workspace.BoolReply{{}}
	r.ws.Resets = []error{nil}
	claude := &client.Fake{Runs: []client.RunReply{run}}
	conflicts, err := r.project(t).UpdateBranch(context.Background(), r.api.client(t), map[string]client.Client{"claude": claude, "opencode": r.opencode}, r.ws, r.u)
	return r, claude, conflicts, err
}

var soloDev = cinderline.Team{Name: "Solo", MaxMinutes: 45, Agents: devTeam.Agents[:1]}

// A solo team's conflict gets one attempt by its orchestrator, in the
// session it just worked in, with its scoped token and the resolver prompt;
// a verified attempt leaves no conflict.
func TestUpdateBranchSoloOrchestratorResolves(t *testing.T) {
	r, claude, conflicts, err := soloRun(t, soloDev, client.RunReply{Result: client.Result{SessionID: "s1", Report: "Resolved."}}, true)
	if conflicts != nil || err != nil {
		t.Fatalf("UpdateBranch = %v, %v, want resolved", conflicts, err)
	}
	runs := claude.Calls()
	if len(runs) != 1 {
		t.Fatalf("orchestrator runs = %d, want 1 (never a retry)", len(runs))
	}
	turn := runs[0].Args[0].(client.Turn)
	if turn.SessionID != "s1" || turn.Token != "tok" || turn.Dir != "/clone" || turn.Model != "m" || turn.MaxTurns != 30 || turn.MaxMinutes != 45 {
		t.Errorf("orchestrator turn = %+v, want session s1 with token tok in the clone", turn)
	}
	if !strings.Contains(turn.Prompt, "# Your task: merge main into cinderline/CSR-1") || !strings.Contains(turn.Prompt, "- `work.txt`") || !strings.Contains(turn.Prompt, "abc CSR-1: T") {
		t.Errorf("orchestrator prompt is not the resolver prompt:\n%s", turn.Prompt)
	}
	if len(r.opencode.Calls()) != 0 {
		t.Error("another agent ran")
	}
	if got := r.methods(); !reflect.DeepEqual(got, []string{"MergeBase", "Head", "Log", "Clean", "IsAncestor", "IsAncestor", "ConflictMarkers"}) {
		t.Errorf("workspace calls = %v, want the merge, the attempt's checks and no reset", got)
	}
	if got := r.body("POST /issues/CSR-1/comments"); !reflect.DeepEqual(got, []string{"Resolved."}) {
		t.Errorf("comments = %v, want the report", got)
	}
}

// A solo orchestrator's attempt that fails a check, or reports Blocked, is
// undone and the conflict returned, as for the Merge Resolver.
func TestUpdateBranchSoloOrchestratorFails(t *testing.T) {
	for _, tt := range []struct {
		way      string
		run      client.RunReply
		verified bool
	}{
		{"leaves the worktree unclean", client.RunReply{Result: client.Result{Report: "Done."}}, false},
		{"reports Blocked", client.RunReply{Result: client.Result{Report: "Blocked: unsure."}}, true},
	} {
		t.Run(tt.way, func(t *testing.T) {
			r, claude, conflicts, err := soloRun(t, soloDev, tt.run, tt.verified)
			if !reflect.DeepEqual(conflicts, []string{"work.txt"}) || err != nil {
				t.Fatalf("UpdateBranch = %v, %v, want work.txt", conflicts, err)
			}
			if len(claude.Calls()) != 1 {
				t.Errorf("orchestrator runs = %d, want 1", len(claude.Calls()))
			}
			ms := r.methods()
			if ms[len(ms)-1] != "Reset" || r.ws.Calls()[len(ms)-1].Args[1] != "old" {
				t.Errorf("workspace calls = %v, want a reset to the old head last", ms)
			}
			if cs := r.body("POST /issues/CSR-1/comments"); len(cs) != 1 || !strings.HasPrefix(cs[0], "**Failed**: the merge with main was not resolved") {
				t.Errorf("comments = %v", cs)
			}
		})
	}
}

// Without the session it just worked in (CatchUp before a solo team's first
// turn), or for a team with workers but no Merge Resolver, nobody runs.
func TestUpdateBranchNoAttemptWithoutSoloSession(t *testing.T) {
	for _, tt := range []struct {
		way     string
		team    cinderline.Team
		session string
	}{
		{"solo without a session", soloDev, ""},
		{"workers without a resolver", devTeam, "s1"},
	} {
		t.Run(tt.way, func(t *testing.T) {
			r := newUpkeepRun(tt.team)
			r.u.Session, r.u.Token = tt.session, "tok"
			r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
			claude := &client.Fake{}
			conflicts, err := r.project(t).UpdateBranch(context.Background(), r.api.client(t), map[string]client.Client{"claude": claude}, r.ws, r.u)
			if !reflect.DeepEqual(conflicts, []string{"work.txt"}) || err != nil {
				t.Fatalf("UpdateBranch = %v, %v", conflicts, err)
			}
			if len(claude.Calls()) != 0 || !reflect.DeepEqual(r.methods(), []string{"MergeBase"}) {
				t.Errorf("an agent ran: %v", r.methods())
			}
		})
	}
}

// A team with a Merge Resolver is unchanged: the resolver runs in a fresh
// session without a token, even when the orchestrator's session is known.
func TestUpdateBranchResolverIgnoresSession(t *testing.T) {
	r := newUpkeepRun(cinderline.Team{Agents: []cinderline.TeamAgent{devTeam.Agents[0], resolverAgent}})
	r.u.Session, r.u.Token = "s1", "tok"
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
	r.ws.Logs = []workspace.LogReply{{}}
	r.ws.Cleans = []workspace.BoolReply{{Yes: true}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}, {Yes: true}}
	r.ws.Markers = []workspace.BoolReply{{}}
	r.opencode.Runs = []client.RunReply{{Result: client.Result{Report: "Resolved."}}}
	claude := &client.Fake{}
	if _, err := r.project(t).UpdateBranch(context.Background(), r.api.client(t), map[string]client.Client{"claude": claude, "opencode": r.opencode}, r.ws, r.u); err != nil {
		t.Fatal(err)
	}
	if len(claude.Calls()) != 0 || len(r.opencode.Calls()) != 1 {
		t.Fatalf("orchestrator runs %d, resolver runs %d, want 0 and 1", len(claude.Calls()), len(r.opencode.Calls()))
	}
	if turn := r.opencode.Calls()[0].Args[0].(client.Turn); turn.SessionID != "" || turn.Token != "" {
		t.Errorf("resolver turn = %+v, want a fresh session without a token (0122)", turn)
	}
}
