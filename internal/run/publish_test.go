package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The cases of legacy lib.forge.test.mjs, lib.prompt.test.mjs and
// runner.test.mjs on publishing (decisions 2026-11-04-runner-waits-for-merge,
// 2026-11-04-merge-status, 2026-11-04-base-branch, 0114), and the branches of
// legacy runner.mjs publish() that no legacy test reaches. The forge's own
// requests (URLs, headers, per-forge answers) are the real forges' tests in
// M4; here the Forge fake stands for them.

var testPR = forge.PullRequest{Number: 1, URL: "https://git.example/o/r/pulls/1"}

// noSleep replaces the merge wait's sleep for the test and records its
// durations.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := sleep
	sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	t.Cleanup(func() { sleep = old })
	return &slept
}

// Ported from lib.forge.test.mjs "baseBranch returns the project base branch
// without asking git" and "baseBranch falls back to the remote's default
// branch", the remote asked through the Forge.
func TestBaseBranch(t *testing.T) {
	for _, tt := range []struct {
		name, project string
		replies       []forge.DefaultBranchReply
		want, err     string
		forgeCalls    int
	}{
		{"the project's base branch", "release", nil, "release", "", 0},
		{"the remote's default branch", "", []forge.DefaultBranchReply{{Branch: "develop"}}, "develop", "", 1},
		{"no default branch", "", []forge.DefaultBranchReply{{Err: errors.New("404")}}, "",
			"could not find the repository's default branch; set a base branch on the project in Cinderline (404)", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{routes: map[string]reply{"GET /project": {200, `{"key":"CSR","base_branch":"` + tt.project + `"}`}}}
			fg := &forge.Fake{DefaultBranches: tt.replies}
			got, err := BaseBranch(context.Background(), api.client(t), fg)
			if got != tt.want || (err == nil) != (tt.err == "") || err != nil && err.Error() != tt.err {
				t.Errorf("BaseBranch = %q, %v; want %q, %q", got, err, tt.want, tt.err)
			}
			if n := len(fg.Calls()); n != tt.forgeCalls {
				t.Errorf("forge calls = %d, want %d", n, tt.forgeCalls)
			}
		})
	}
}

// Ported from lib.forge.test.mjs "awaitMerge polls an open pull request
// until it merged", "... reports a pull request closed without merging",
// "... gives up once the limit has passed", "... retries a failed poll" and
// "... retries a 502" (one row: to the Forge both are an error). The limit
// is the real one: polls at 0, 15 s, ... 15 min.
func TestAwaitMerge(t *testing.T) {
	open := forge.MergeStateReply{State: forge.MergePending}
	failedChecks := forge.MergeStateReply{State: forge.MergeFailed}
	for _, tt := range []struct {
		name    string
		replies []forge.MergeStateReply
		want    forge.MergeState
		polls   int
	}{
		{"merges", []forge.MergeStateReply{open, open, {State: forge.MergeMerged}}, forge.MergeMerged, 3},
		{"closed", []forge.MergeStateReply{{State: forge.MergeClosed}}, forge.MergeClosed, 1},
		{"a failed poll is retried", []forge.MergeStateReply{{Err: errors.New("502")}, {State: forge.MergeMerged}}, forge.MergeMerged, 2},
		// Failed checks do not end the wait: a person may re-run them.
		{"failed checks wait on", []forge.MergeStateReply{failedChecks, {State: forge.MergeMerged}}, forge.MergeMerged, 2},
		{"gives up at the limit", nil, forge.MergePending, 61},
	} {
		t.Run(tt.name, func(t *testing.T) {
			slept := noSleep(t)
			fg := &forge.Fake{MergeStates: tt.replies}
			for len(fg.MergeStates) < tt.polls {
				fg.MergeStates = append(fg.MergeStates, open)
			}
			var log bytes.Buffer
			p := project(t)
			p.Log = slog.New(slog.NewTextHandler(&log, nil))
			got, err := p.awaitMerge(context.Background(), fg, "cinderline/CSR-1")
			if got != tt.want || err != nil {
				t.Errorf("awaitMerge = %q, %v; want %q", got, err, tt.want)
			}
			calls := fg.Calls()
			if len(calls) != tt.polls || calls[0].Args[0] != "cinderline/CSR-1" {
				t.Errorf("polls = %d (%+v), want %d of the branch", len(calls), calls[0], tt.polls)
			}
			if len(*slept) != tt.polls-1 || len(*slept) > 0 && (*slept)[0] != 15*time.Second {
				t.Errorf("slept %v, want %d times 15s", *slept, tt.polls-1)
			}
			if strings.Contains(tt.name, "failed poll") && !strings.Contains(log.String(), "502") {
				t.Errorf("failed poll not logged: %s", log.String())
			}
		})
	}
}

// A stop during the wait ends it with the context's error.
func TestAwaitMergeStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fg := &forge.Fake{MergeStates: []forge.MergeStateReply{{State: forge.MergePending}}}
	if _, err := project(t).awaitMerge(ctx, fg, "cinderline/CSR-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// Ported from lib.forge.test.mjs "MERGE_STATUS maps the publish wait to a
// merge status": an open pull request, or the wait's limit, writes null.
func TestMergeStatusOf(t *testing.T) {
	for state, want := range map[forge.MergeState]string{
		forge.MergeMerged: "success", forge.MergeClosed: "issue", forge.MergeFailed: "issue", forge.MergePending: "", "": "",
	} {
		if got := mergeStatusOf[state]; got != want {
			t.Errorf("mergeStatusOf[%q] = %q, want %q", state, got, want)
		}
	}
}

// Ported from lib.prompt.test.mjs "finishReport reports the pull request and
// its merge", every assertion.
func TestFinishReport(t *testing.T) {
	r := &Published{URL: "U", Base: "main"}
	with := func(f func(*Published)) *Published { c := *r; f(&c); return &c }
	for _, tt := range []struct {
		pub     *Published
		trusted bool
		forge   string
		want    string
	}{
		{with(func(p *Published) { p.Merge = forge.MergeMerged }), true, "forgejo", "Merged U."},
		{with(func(p *Published) { p.Merge = forge.MergePending }), true, "github",
			"Opened U. GitHub merges it when CI is green; it had not merged after 15 minutes, so the runner moved on."},
		{with(func(p *Published) { p.Merge = forge.MergeClosed }), true, "forgejo", "Opened U, but it was closed without merging."},
		{r, false, "forgejo", "Opened U. Review it and merge it; then move this issue to done."},
		{with(func(p *Published) { p.Conflicts = []string{"a", "b"} }), true, "forgejo",
			"Opened U. It conflicts with main in a, b; resolve the conflict by hand, then let CI merge it."},
		{with(func(p *Published) { p.Conflicts = []string{"a"} }), false, "forgejo",
			"Opened U. It conflicts with main in a; resolve the conflict by hand, then merge it."},
		{nil, true, "forgejo", "The run finished without changing any files, so there is no pull request."},
	} {
		if got := FinishReport(tt.pub, tt.trusted, tt.forge); got != tt.want {
			t.Errorf("FinishReport(%+v, %v, %s) =\n%q, want\n%q", tt.pub, tt.trusted, tt.forge, got, tt.want)
		}
	}
	for f, want := range map[string]string{"gitlab": "GitLab", "bitbucket": "Bitbucket", "forgejo": "Forgejo", "": "Forgejo"} {
		got := FinishReport(&Published{URL: "U", Merge: forge.MergePending}, true, f)
		if !strings.Contains(got, " "+want+" merges it") {
			t.Errorf("forge %q: %q", f, got)
		}
	}
}

// pubRun is one Publish against the fakes, by a team that is or is not
// trusted.
type pubRun struct {
	api   *fakeAPI
	ws    *workspace.Fake
	forge *forge.Fake
	pub   Pub
	log   bytes.Buffer
}

func newPubRun(trusted bool) *pubRun {
	team := devTeam
	team.Trusted = trusted
	return &pubRun{
		api: &fakeAPI{routes: map[string]reply{
			"POST /issues/CSR-1/comments": {201, `{"id":9,"body":"x"}`},
			"POST /issues/CSR-1/progress": {204, ""},
			"PATCH /issues/CSR-1":         {200, issueJSON("CSR-1", "review")},
			"GET /issues/CSR-1":           {200, issueJSON("CSR-1", "in_progress")}, // the budget check before a Merge Resolver run
		}},
		ws: &workspace.Fake{
			Commits:    []workspace.CommitReply{{Committed: true}},
			MergeBases: []workspace.MergeBaseReply{{}},
			Ancestors:  []workspace.IsAncestorReply{{}}, // HEAD is not in the base: there is work
			Pushes:     []error{nil},
		},
		forge: &forge.Fake{PullRequests: []forge.PullRequestReply{{PullRequest: testPR}}, Merges: []error{nil}},
		pub:   Pub{Ref: "CSR-1", Title: "T", Body: "Do it.", Base: "main", Dir: "/clone", MaxTurns: 30, Team: team, Forge: "forgejo"},
	}
}

func (r *pubRun) run(t *testing.T) error {
	t.Helper()
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&r.log, nil))
	return p.Publish(context.Background(), r.api.client(t), map[string]client.Client{}, r.ws, r.forge, r.pub)
}

func (r *pubRun) wsMethods() []string {
	var out []string
	for _, c := range r.ws.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func (r *pubRun) forgeMethods() []string {
	var out []string
	for _, c := range r.forge.Calls() {
		out = append(out, c.Method)
	}
	return out
}

// comment is the body of the last comment Publish wrote.
func (r *pubRun) comment() string {
	var body string
	for _, w := range r.api.writes() {
		if rest, ok := strings.CutPrefix(w, "POST /issues/CSR-1/comments "); ok {
			var in struct{ Body string }
			_ = json.Unmarshal([]byte(rest), &in)
			body = in.Body
		}
	}
	return body
}

// patches are the PATCH bodies Publish wrote, in order.
func (r *pubRun) patches() []string {
	var out []string
	for _, w := range r.api.writes() {
		if rest, ok := strings.CutPrefix(w, "PATCH /issues/CSR-1 "); ok {
			out = append(out, rest)
		}
	}
	return out
}

const toReview = `{"status":"review","expected_status":"in_progress"}`

// Ported from runner.test.mjs "the base branch moved during a run cleanly:
// it is merged in before the push" and "... with a conflict: the branch is
// pushed as it is", the publish part: what was left is committed as
// "<REF>: <title>", the base merged in, and the branch pushed as
// cinderline/<REF>, never the base, before the pull request is opened, with
// the progress line "Opening the pull request". As in the legacy test, the
// forge fails here, so the run ends in the error after the push.
func TestPublishPushesTheIssueBranch(t *testing.T) {
	noSleep(t)
	for _, conflicts := range [][]string{nil, {"work.txt"}} {
		r := newPubRun(false)
		// The forge failing is tried five times (CSR-81), each from the base merge.
		down := forge.PullRequestReply{Err: errors.New("forge unreachable")}
		r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: conflicts}, {Conflicts: conflicts}, {Conflicts: conflicts}, {Conflicts: conflicts}, {Conflicts: conflicts}}
		r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}, {}, {}, {}}
		r.ws.Pushes = []error{nil, nil, nil, nil, nil}
		r.ws.Heads = []workspace.HeadReply{{Commit: "0123456789abcdef0123456789abcdef01234567"}}
		r.forge.PullRequests = []forge.PullRequestReply{down, down, down, down, down}
		if err := r.run(t); err == nil || !strings.Contains(err.Error(), "forge unreachable") {
			t.Fatalf("Publish = %v, want the forge's error", err)
		}
		want := []workspace.Call{{Method: "Commit", Args: []any{"/clone", "CSR-1: T"}}}
		for range 5 {
			want = append(want,
				workspace.Call{Method: "MergeBase", Args: []any{"/clone", "main"}},
				workspace.Call{Method: "IsAncestor", Args: []any{"/clone", "HEAD", "main"}},
				workspace.Call{Method: "Push", Args: []any{"/clone", "cinderline/CSR-1"}})
		}
		want = append(want, workspace.Call{Method: "Head", Args: []any{"/clone"}})
		if got := r.ws.Calls(); !reflect.DeepEqual(got, want) {
			t.Errorf("conflicts %v: workspace calls =\n%+v, want\n%+v", conflicts, got, want)
		}
		if w := r.api.writes(); len(w) != 5 || w[0] != `POST /issues/CSR-1/progress {"body":"Opening the pull request"}` || w[4] != w[0] {
			t.Errorf("Cinderline writes = %v, want only the progress line, once per attempt", w)
		}
	}
}

// Ported from runner.test.mjs "a run resuming after a question on a branch
// main moved past merges main first": an untrusted team's pull request is
// opened for a person to merge, to the base, titled "<REF>: <title>" with
// the issue's text; the run ends in Review. No merge is requested and no
// merge_status written (0114).
func TestPublishUntrusted(t *testing.T) {
	r := newPubRun(false)
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	want := []forge.Call{{Method: "OpenPullRequest", Args: []any{forge.PullRequestInput{
		Branch: "cinderline/CSR-1", Base: "main", Title: "CSR-1: T", Body: "Resolves CSR-1 in Cinderline.\n\nDo it.",
	}}}}
	if got := r.forge.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("forge calls = %+v, want %+v", got, want)
	}
	if got := r.comment(); got != "Opened https://git.example/o/r/pulls/1. Review it and merge it; then move this issue to done." {
		t.Errorf("comment = %q", got)
	}
	if got := r.patches(); !reflect.DeepEqual(got, []string{toReview}) {
		t.Errorf("PATCHes = %v, want only the move to Review", got)
	}
}

// Ported from runner.test.mjs "a branch that conflicts with main at the
// push, and a Merge Resolver that resolves: merged and published": a trusted
// team without conflicts requests the merge once and waits; merged is
// merge_status success and the comment "Merged <url>.".
func TestPublishTrustedMerges(t *testing.T) {
	slept := noSleep(t)
	r := newPubRun(true)
	r.forge.MergeStates = []forge.MergeStateReply{{State: forge.MergePending}, {State: forge.MergeMerged}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.forgeMethods(); !reflect.DeepEqual(got, []string{"OpenPullRequest", "RequestMerge", "MergeState", "MergeState"}) {
		t.Errorf("forge calls = %v", got)
	}
	if c := r.forge.Calls(); c[1].Args[0] != testPR || c[2].Args[0] != "cinderline/CSR-1" {
		t.Errorf("merge of %v, waited on %v", c[1].Args, c[2].Args)
	}
	if len(*slept) != 1 {
		t.Errorf("slept %v", *slept)
	}
	if got := r.comment(); got != "Merged https://git.example/o/r/pulls/1." {
		t.Errorf("comment = %q", got)
	}
	if got := r.patches(); !reflect.DeepEqual(got, []string{`{"merge_status":"success"}`, toReview}) {
		t.Errorf("PATCHes = %v", got)
	}
}

// Ported from runner.mjs publish(), whose other wait outcomes no legacy
// runner test reaches: closed is merge_status issue; the limit clears an
// earlier issue (null), as MERGE_STATUS.timeout.
func TestPublishTrustedWaitOutcomes(t *testing.T) {
	for _, tt := range []struct {
		state   forge.MergeState
		status  string
		comment string
	}{
		{forge.MergeClosed, `{"merge_status":"issue"}`, "Opened https://git.example/o/r/pulls/1, but it was closed without merging."},
		{forge.MergePending, `{"merge_status":null}`, "Opened https://git.example/o/r/pulls/1. Forgejo merges it when CI is green; it had not merged after 15 minutes, so the runner moved on."},
	} {
		noSleep(t)
		r := newPubRun(true)
		for range 61 {
			r.forge.MergeStates = append(r.forge.MergeStates, forge.MergeStateReply{State: tt.state})
		}
		if err := r.run(t); err != nil {
			t.Fatal(err)
		}
		if got := r.comment(); got != tt.comment {
			t.Errorf("%s: comment = %q", tt.state, got)
		}
		if got := r.patches(); !reflect.DeepEqual(got, []string{tt.status, toReview}) {
			t.Errorf("%s: PATCHes = %v", tt.state, got)
		}
	}
}

// Ported from runner.test.mjs "a branch that conflicts with main at the
// push, and a Merge Resolver that <does not resolve it>: undone, pushed as
// it is": a trusted team's conflict is pushed and opened, but not merged; it
// is merge_status issue and the comment names the files.
func TestPublishTrustedConflict(t *testing.T) {
	r := newPubRun(true)
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.forgeMethods(); !reflect.DeepEqual(got, []string{"OpenPullRequest"}) {
		t.Errorf("forge calls = %v, want no merge", got)
	}
	if got := r.comment(); got != "Opened https://git.example/o/r/pulls/1. It conflicts with main in work.txt; resolve the conflict by hand, then let CI merge it." {
		t.Errorf("comment = %q", got)
	}
	if got := r.patches(); !reflect.DeepEqual(got, []string{`{"merge_status":"issue"}`, toReview}) {
		t.Errorf("PATCHes = %v", got)
	}
}

// Ported from runner.mjs publish(): a merge the forge refuses is
// merge_status issue and the run's error, which names the open pull
// request; the scheduler comments it and moves the issue to Review.
func TestPublishMergeRefused(t *testing.T) {
	r := newPubRun(true)
	r.forge.Merges = []error{errors.New("405: not allowed")}
	err := r.run(t)
	if err == nil || err.Error() != "pull request https://git.example/o/r/pulls/1 is open, but asking Forgejo to merge it failed: 405: not allowed" {
		t.Fatalf("Publish = %v", err)
	}
	if got := r.patches(); !reflect.DeepEqual(got, []string{`{"merge_status":"issue"}`}) {
		t.Errorf("PATCHes = %v", got)
	}
}

// Ported from runner.test.mjs "a comment resume that fails twice still
// publishes the finished work, with a note": a run that changed nothing
// pushes nothing and opens no pull request; the report gets the unread note.
func TestPublishNothingChanged(t *testing.T) {
	r := newPubRun(true)
	r.ws.Commits = []workspace.CommitReply{{}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{Yes: true}}
	r.pub.Unread = true
	if err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.wsMethods(); !reflect.DeepEqual(got, []string{"Commit", "MergeBase", "IsAncestor"}) {
		t.Errorf("workspace calls = %v, want no push", got)
	}
	if len(r.forge.Calls()) != 0 {
		t.Errorf("forge calls = %v", r.forge.Calls())
	}
	want := "The run finished without changing any files, so there is no pull request." + UnreadComments
	if got := r.comment(); got != want {
		t.Errorf("comment = %q", got)
	}
	if got := r.patches(); !reflect.DeepEqual(got, []string{toReview}) {
		t.Errorf("PATCHes = %v", got)
	}
}

// Ported from runner.mjs runIssue(): a move to Review that is refused, as
// after a question (0116), only logs; so does a failed merge_status write.
// A failed commit, push or comment is the run's error.
func TestPublishFailures(t *testing.T) {
	noSleep(t)
	r := newPubRun(false)
	r.api.routes["PATCH /issues/CSR-1"] = reply{409, `{"message":"Conflict.","status":"blocked"}`}
	if err := r.run(t); err != nil {
		t.Fatalf("a refused move = %v", err)
	}
	if !strings.Contains(r.log.String(), "not moved to Review") {
		t.Errorf("log = %s", r.log.String())
	}

	r = newPubRun(true)
	r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"a"}}}
	r.api.routes["PATCH /issues/CSR-1"] = reply{500, `{"message":"down"}`}
	if err := r.run(t); err != nil {
		t.Fatalf("a failed merge_status write = %v", err)
	}
	if !strings.Contains(r.log.String(), "merge status not set") {
		t.Errorf("log = %s", r.log.String())
	}

	for name, set := range map[string]func(*pubRun){
		"commit": func(r *pubRun) { r.ws.Commits = []workspace.CommitReply{{Err: errors.New("boom")}} },
		"push": func(r *pubRun) { // five attempts (CSR-81)
			r.ws.MergeBases = []workspace.MergeBaseReply{{}, {}, {}, {}, {}}
			r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}, {}, {}, {}}
			r.ws.Pushes = []error{errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom")}
			r.ws.Heads = []workspace.HeadReply{{Commit: "0123456789abcdef0123456789abcdef01234567"}}
		},
		"comment": func(r *pubRun) { r.api.routes["POST /issues/CSR-1/comments"] = reply{500, `{"message":"boom"}`} },
	} {
		r := newPubRun(false)
		set(r)
		if err := r.run(t); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s failing: Publish = %v", name, err)
		}
		if got := r.patches(); len(got) != 0 {
			t.Errorf("%s failing: PATCHes = %v, want the scheduler's move only", name, got)
		}
	}
}

// mergeCheck runs CheckMerges over issues, the teams 1 trusted and 2 not,
// with the forge's states in order.
func mergeCheck(t *testing.T, issues []string, states []forge.MergeStateReply) (*fakeAPI, *forge.Fake, string) {
	t.Helper()
	trusted, _ := json.Marshal(cinderline.Team{ID: 1, Name: "Trusted", Trusted: true})
	untrusted, _ := json.Marshal(cinderline.Team{ID: 2, Name: "Untrusted"})
	api := &fakeAPI{routes: map[string]reply{
		"GET /issues?status=review": {200, `{"issues":[` + strings.Join(issues, ",") + `]}`},
		"GET /teams/1":              {200, string(trusted)},
		"GET /teams/2":              {200, string(untrusted)},
	}}
	api.handle = func(key, _ string) (reply, bool) {
		if strings.HasPrefix(key, "PATCH /issues/") {
			return reply{200, issueJSON("CSR-1", "review")}, true
		}
		return reply{}, false
	}
	fg := &forge.Fake{MergeStates: states}
	var log bytes.Buffer
	p := project(t)
	p.Log = slog.New(slog.NewTextHandler(&log, nil))
	if err := p.CheckMerges(context.Background(), api.client(t), fg); err != nil {
		t.Fatalf("CheckMerges = %v", err)
	}
	return api, fg, log.String()
}

// An issue without a pull request is not looked up again on the next tick:
// finding none walks the forge's whole list (decision 2026-11-11-audit-fixes).
func TestCheckMergesLeavesAnIssueWithoutPullRequestAlone(t *testing.T) {
	trusted, _ := json.Marshal(cinderline.Team{ID: 1, Name: "Trusted", Trusted: true})
	api := &fakeAPI{routes: map[string]reply{
		"GET /issues?status=review": {200, `{"issues":[` + reviewJSON("CSR-1", 1, false, "") + `,` + reviewJSON("CSR-2", 1, false, "") + `]}`},
		"GET /teams/1":              {200, string(trusted)},
	}}
	fg := &forge.Fake{MergeStates: []forge.MergeStateReply{{}, {State: forge.MergePending}, {State: forge.MergePending}}}
	p := project(t)
	p.run = &runState{}
	for range 2 {
		if err := p.CheckMerges(context.Background(), api.client(t), fg); err != nil {
			t.Fatalf("CheckMerges = %v", err)
		}
	}
	if got, want := branches(fg), []any{"cinderline/CSR-1", "cinderline/CSR-2", "cinderline/CSR-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("looked up %v, want %v", got, want)
	}
}

// reviewJSON is a Review issue of team (0: none), with a parent and a
// merge_status when given.
func reviewJSON(ref string, team int, parent bool, status string) string {
	s := `{"ref":"` + ref + `","title":"T","status":{"value":"review","label":"Review"}`
	if team > 0 {
		s += `,"team":{"id":` + strconv.Itoa(team) + `,"name":"x"}`
	}
	if parent {
		s += `,"parent":{"ref":"CSR-9","title":"Parent"}`
	}
	if status != "" {
		s += `,"merge_status":"` + status + `"`
	}
	return s + "}"
}

func branches(fg *forge.Fake) []any {
	var out []any
	for _, c := range fg.Calls() {
		out = append(out, c.Args[0])
	}
	return out
}

// Ported from lib.forge.test.mjs "checkMerges writes the outcome of trusted
// Review issues": the team is asked once, an earlier issue is overwritten.
func TestCheckMergesWritesOutcomes(t *testing.T) {
	api, _, _ := mergeCheck(t,
		[]string{reviewJSON("CSR-1", 1, false, ""), reviewJSON("CSR-2", 1, false, ""), reviewJSON("CSR-3", 1, false, "issue")},
		[]forge.MergeStateReply{{State: forge.MergeMerged}, {State: forge.MergeFailed}, {State: forge.MergeMerged}})
	want := []string{
		"GET /issues?status=review",
		"GET /teams/1",
		`PATCH /issues/CSR-1 {"merge_status":"success"}`,
		`PATCH /issues/CSR-2 {"merge_status":"issue"}`,
		`PATCH /issues/CSR-3 {"merge_status":"success"}`,
	}
	if !reflect.DeepEqual(api.calls, want) {
		t.Errorf("calls =\n%v, want\n%v", api.calls, want)
	}
}

// Ported from lib.forge.test.mjs "checkMerges writes nothing for an open PR,
// a timeout or an unchanged status"; the Forge has no timeout, so that row
// is a forge error here, and "no pull request" is the empty state.
func TestCheckMergesWritesNothingNew(t *testing.T) {
	api, fg, _ := mergeCheck(t,
		[]string{reviewJSON("CSR-1", 1, false, ""), reviewJSON("CSR-2", 1, false, ""), reviewJSON("CSR-3", 1, false, "issue"), reviewJSON("CSR-4", 1, false, "")},
		[]forge.MergeStateReply{{State: forge.MergePending}, {Err: errors.New("timeout")}, {State: forge.MergeClosed}, {}})
	if len(fg.Calls()) != 4 {
		t.Errorf("asked %v", branches(fg))
	}
	if w := api.writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

// Ported from lib.forge.test.mjs "checkMerges skips untrusted teams,
// sub-issues, issues without a team and merged ones".
func TestCheckMergesSkips(t *testing.T) {
	api, fg, _ := mergeCheck(t, []string{
		reviewJSON("CSR-1", 2, false, ""),
		reviewJSON("CSR-2", 1, true, ""),
		reviewJSON("CSR-3", 0, false, ""),
		reviewJSON("CSR-4", 1, false, "success"),
		reviewJSON("CSR-5", 2, false, ""),
	}, nil)
	if len(fg.Calls()) != 0 {
		t.Errorf("asked %v", branches(fg))
	}
	if want := []string{"GET /issues?status=review", "GET /teams/2"}; !reflect.DeepEqual(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

// Ported from lib.forge.test.mjs "checkMerges logs a failing issue and
// checks the next"; the branch is cinderline/<REF>. A failed list is the
// hook's error.
func TestCheckMergesFailures(t *testing.T) {
	api, fg, log := mergeCheck(t, []string{reviewJSON("CSR-1", 1, false, ""), reviewJSON("CSR-2", 1, false, "")},
		[]forge.MergeStateReply{{Err: errors.New("forge down")}, {State: forge.MergeMerged}})
	if !strings.Contains(log, "merge status not checked") || !strings.Contains(log, "CSR-1") || !strings.Contains(log, "forge down") {
		t.Errorf("log = %s", log)
	}
	if got := branches(fg); !reflect.DeepEqual(got, []any{"cinderline/CSR-1", "cinderline/CSR-2"}) {
		t.Errorf("asked %v", got)
	}
	if w := api.writes(); !reflect.DeepEqual(w, []string{`PATCH /issues/CSR-2 {"merge_status":"success"}`}) {
		t.Errorf("writes = %v", w)
	}

	api = &fakeAPI{routes: map[string]reply{"GET /issues?status=review": {500, `{"message":"down"}`}}}
	if err := project(t).CheckMerges(context.Background(), api.client(t), &forge.Fake{}); err == nil {
		t.Error("a failed list is no error")
	}
}

// A trusted solo team's conflict at publish: the orchestrator's verified
// attempt is pushed and the merge requested; a failed one is undone and the
// branch pushed with the conflict as today, merge_status issue
// (decision 2026-11-07-solo-team-resolves-merge-conflicts).
func TestPublishSoloOrchestratorAttempt(t *testing.T) {
	for _, tt := range []struct {
		way     string
		report  string
		forge   []string
		patches []string
		ancs    []workspace.IsAncestorReply // the checks of the attempt, then HEAD not in the base
	}{
		{"resolved", "Resolved.", []string{"OpenPullRequest", "RequestMerge", "MergeState"}, []string{`{"merge_status":"success"}`, toReview}, []workspace.IsAncestorReply{{Yes: true}, {Yes: true}, {}}},
		{"blocked", "Blocked: unsure.", []string{"OpenPullRequest"}, []string{`{"merge_status":"issue"}`, toReview}, []workspace.IsAncestorReply{{}}},
	} {
		t.Run(tt.way, func(t *testing.T) {
			noSleep(t)
			r := newPubRun(true)
			r.pub.Team = cinderline.Team{Name: "Solo", Trusted: true, Agents: devTeam.Agents[:1]}
			r.pub.Session, r.pub.Token = "s1", "tok"
			r.ws.MergeBases = []workspace.MergeBaseReply{{Conflicts: []string{"work.txt"}}}
			r.ws.Heads = []workspace.HeadReply{{Commit: "old"}}
			r.ws.Logs = []workspace.LogReply{{}}
			r.ws.Cleans = []workspace.BoolReply{{Yes: true}}
			r.ws.Ancestors = tt.ancs
			r.ws.Markers = []workspace.BoolReply{{}}
			r.ws.Resets = []error{nil}
			r.forge.MergeStates = []forge.MergeStateReply{{State: forge.MergeMerged}}
			claude := &client.Fake{Runs: []client.RunReply{{Result: client.Result{SessionID: "s1", Report: tt.report}}}}
			p := project(t)
			p.Log = slog.New(slog.NewTextHandler(&r.log, nil))
			if err := p.Publish(context.Background(), r.api.client(t), map[string]client.Client{"claude": claude}, r.ws, r.forge, r.pub); err != nil {
				t.Fatal(err)
			}
			if n := len(claude.Calls()); n != 1 {
				t.Fatalf("orchestrator runs = %d, want 1", n)
			}
			if turn := claude.Calls()[0].Args[0].(client.Turn); turn.SessionID != "s1" || turn.Token != "tok" {
				t.Errorf("turn = %+v, want session s1 with token tok", turn)
			}
			ms := r.wsMethods()
			if !reflect.DeepEqual(ms[len(ms)-1:], []string{"Push"}) {
				t.Errorf("workspace calls = %v, want the push last", ms)
			}
			if reset := reflect.DeepEqual(ms[len(ms)-3:len(ms)-2], []string{"Reset"}); reset != (tt.report != "Resolved.") {
				t.Errorf("workspace calls = %v: reset %v", ms, reset)
			}
			if got := r.forgeMethods(); !reflect.DeepEqual(got, tt.forge) {
				t.Errorf("forge calls = %v, want %v", got, tt.forge)
			}
			if got := r.patches(); !reflect.DeepEqual(got, tt.patches) {
				t.Errorf("PATCHes = %v, want %v", got, tt.patches)
			}
		})
	}
}

// CSR-81 point 2: once the agent has finished, a failure in the fetch, base
// merge, push or pull request steps is tried again with backoff, in memory
// inside the run (decision 0091): the issue's status does not change until
// it reaches Review once, with a pull request and no error comment. Every
// attempt starts again at the base merge, so a base that moved during the
// wait is merged in (decision 0121).
func TestPublishRetriesTransientFailures(t *testing.T) {
	slept := noSleep(t)
	r := newPubRun(false)
	r.ws.MergeBases = []workspace.MergeBaseReply{{}, {}, {}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}, {}}
	r.ws.Pushes = []error{errors.New("Forgejo unreachable"), errors.New("Forgejo unreachable"), nil}
	if err := r.run(t); err != nil {
		t.Fatalf("Publish = %v", err)
	}
	want := []string{"Commit", "MergeBase", "IsAncestor", "Push", "MergeBase", "IsAncestor", "Push", "MergeBase", "IsAncestor", "Push"}
	if got := r.wsMethods(); !reflect.DeepEqual(got, want) {
		t.Errorf("workspace calls = %v, want %v", got, want)
	}
	if got := r.forgeMethods(); !reflect.DeepEqual(got, []string{"OpenPullRequest"}) {
		t.Errorf("forge calls = %v, want the pull request once", got)
	}
	if got := r.patches(); !reflect.DeepEqual(got, []string{toReview}) {
		t.Errorf("PATCHes = %v, want Review once", got)
	}
	if c := r.comment(); !strings.HasPrefix(c, "Opened "+testPR.URL) {
		t.Errorf("comment = %q, want the opened pull request, no error", c)
	}
	if len(*slept) != 2 || (*slept)[0] <= 0 || (*slept)[1] <= (*slept)[0] {
		t.Errorf("waits = %v, want two, growing", *slept)
	}
}

// CSR-81 point 2: after the last of five attempts the error names the
// unpushed commit, so a person can recover it; nothing is commented or
// moved here (the scheduler does that), and the five waits stay within ten
// minutes.
func TestPublishGivesUpAfterFiveAttempts(t *testing.T) {
	slept := noSleep(t)
	r := newPubRun(false)
	boom := errors.New("Forgejo unreachable")
	r.ws.MergeBases = []workspace.MergeBaseReply{{}, {}, {}, {}, {}}
	r.ws.Ancestors = []workspace.IsAncestorReply{{}, {}, {}, {}, {}}
	r.ws.Pushes = []error{boom, boom, boom, boom, boom}
	r.ws.Heads = []workspace.HeadReply{{Commit: "0123456789abcdef0123456789abcdef01234567"}}
	err := r.run(t)
	if err == nil || !strings.Contains(err.Error(), "Forgejo unreachable") || !strings.Contains(err.Error(), "0123456789abcdef0123456789abcdef01234567") || !strings.Contains(err.Error(), "cinderline/CSR-1") {
		t.Fatalf("Publish = %v, want the last error with the unpushed commit and branch", err)
	}
	if n := strings.Count(strings.Join(r.wsMethods(), " "), "Push"); n != 5 {
		t.Errorf("pushed %d times, want 5", n)
	}
	if got := r.patches(); len(got) != 0 {
		t.Errorf("PATCHes = %v, want none", got)
	}
	if c := r.comment(); c != "" {
		t.Errorf("comment = %q, want none", c)
	}
	var total time.Duration
	for i, d := range *slept {
		total += d
		if d <= 0 || (i > 0 && d <= (*slept)[i-1]) {
			t.Errorf("waits = %v, want growing", *slept)
		}
	}
	if len(*slept) != 4 || total > 10*time.Minute {
		t.Errorf("waits = %v (total %v), want four within ten minutes", *slept, total)
	}
}

// TestPullRequestTextRedacted: a known secret in the issue's title or body
// never reaches the pull request (CSR-157).
func TestPullRequestTextRedacted(t *testing.T) {
	const v = "sk-or-v1-prtextredaction-0123456789"
	redact.Add("openrouter key", v)
	if got := pullRequestTitle("CSR-9", "leak "+v); got != "CSR-9: leak [redacted openrouter key]" {
		t.Errorf("title %q", got)
	}
	if got := pullRequestBody("CSR-9", "key "+v); strings.Contains(got, v) {
		t.Errorf("body %q", got)
	}
}
