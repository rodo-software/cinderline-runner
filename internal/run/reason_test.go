package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// publishOnce runs one publish step on a fixture set up by setup and
// returns its report.
func publishOnce(t *testing.T, step string, setup func(*fixture)) cinderline.LastStep {
	t.Helper()
	f := newFixture(t)
	f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "https://git.example/r/pulls/7"}}}
	setup(f)
	app := &fakeApp{onNext: stepsOf(step)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	lasts := app.lasts()
	if len(lasts) != 1 {
		t.Fatalf("lasts = %+v", lasts)
	}
	return lasts[0]
}

// CLR-245: a merge the forge refused reports the forge's words as the
// reason, without a URL; an error that is not the forge's answer keeps the
// error alone, as before.
func TestAMergeTheForgeRefusedHasAReason(t *testing.T) {
	refused := forge.RefusedError("Forgejo", 405, "not mergeable")
	for _, tc := range []struct {
		err  error
		want string
	}{
		{refused, "The forge refused to merge the pull request: Forgejo answered 405: not mergeable"},
		{errors.New("dial tcp: connection refused"), ""},
	} {
		last := publishOnce(t, `{"kind":"publish","merge":true}`, func(f *fixture) { f.fg.Merges = []error{tc.err} })
		if deref(last.Merge) != "failed" || deref(last.Reason) != tc.want || !strings.Contains(deref(last.Error), tc.err.Error()) {
			t.Errorf("%v: merge %q, reason %q, error %q; want failed, %q", tc.err, deref(last.Merge), deref(last.Reason), deref(last.Error), tc.want)
		}
		if strings.Contains(deref(last.Reason), "http") {
			t.Errorf("reason %q carries a URL", deref(last.Reason))
		}
	}
}

// CLR-245: a pull request with conflicts gets a reason only when the runner
// was asked to merge it; otherwise a person merges it.
func TestConflictsWithoutAMergeHaveNoReason(t *testing.T) {
	last := publishOnce(t, `{"kind":"publish"}`, func(f *fixture) {
		f.ws.MergeBases = append([]workspace.MergeBaseReply{{}, {Conflicts: []string{"a.go"}}}, f.ws.MergeBases...)
	})
	if last.Outcome != "published" || last.Merge != nil || last.Reason != nil || !slices.Equal(last.Conflicts, []string{"a.go"}) {
		t.Errorf("last = %s; want published with the conflicts and no merge or reason", show(last))
	}
}

// CLR-245: when the merge agent could not resolve the conflict, the publish
// that follows names the files and the agent's "Blocked:" line.
func TestAnUnresolvedConflictHasTheMergeAgentsBlockedLine(t *testing.T) {
	f := newFixture(t)
	f.ws.MergeBases = append([]workspace.MergeBaseReply{{}, {Conflicts: []string{"a.go", "b.go"}}, {Conflicts: []string{"a.go", "b.go"}}}, f.ws.MergeBases...)
	f.ws.Logs = []workspace.LogReply{{Log: "abc fix"}}
	f.ws.Ancestors = []workspace.IsAncestorReply{{Yes: false}}
	f.ws.Resets = []error{nil}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u"}}}
	f.cl.Runs = []client.RunReply{ok("r1", "\n**Blocked:** both sides rewrote parse()\n\nI ran git merge --abort.")}
	app := &fakeApp{onNext: stepsOf(
		`{"kind":"publish","resolve_conflicts":true,"merge":true}`,
		`{"kind":"resolve","agent":"Coder","session":"new","token":null,"prompt":"Resolve."}`,
		`{"kind":"publish","merge":true}`,
	)}
	if _, err := f.run(t, app); err != nil {
		t.Fatal(err)
	}
	lasts := app.lasts()
	if len(lasts) != 3 || lasts[0].Step != "merge" || lasts[1].Step != "resolve" || lasts[2].Step != "publish" {
		t.Fatalf("lasts = %+v", lasts)
	}
	if lasts[0].Reason != nil || lasts[1].Reason != nil {
		t.Errorf("the merge or resolve step has a reason: %q, %q", deref(lasts[0].Reason), deref(lasts[1].Reason))
	}
	want := "Merging the base branch main into the branch conflicts in: a.go, b.go\nThe merge agent: **Blocked:** both sides rewrote parse()"
	if p := lasts[2]; deref(p.Merge) != "failed" || deref(p.Reason) != want {
		t.Errorf("publish merge %q, reason %q; want failed, %q", deref(p.Merge), deref(p.Reason), want)
	}
}

// CLR-245: at the wait's limit, a pull request still open of which the
// forge has no merge scheduled any more reports that the forge dropped it;
// one the forge still means to merge stays pending without a reason.
func TestADroppedMergeHasAReasonAtTheLimit(t *testing.T) {
	polls := int(mergeLimit/mergeEvery) + 1
	for _, tc := range []struct {
		unscheduled, want string
	}{
		{"GitLab has no merge of it scheduled; its merge status is not_approved",
			"The forge dropped the scheduled merge without merging: GitLab has no merge of it scheduled; its merge status is not_approved"},
		{"", ""},
	} {
		last := publishOnce(t, `{"kind":"publish","merge":true}`, func(f *fixture) {
			f.fg.Merges = []error{nil}
			f.fg.MergeStates = fill(polls, forge.MergeStateReply{State: forge.MergePending, Head: "abc123", Unscheduled: tc.unscheduled})
		})
		if deref(last.Merge) != "pending" || deref(last.Reason) != tc.want {
			t.Errorf("unscheduled %q: merge %q, reason %q; want pending, %q", tc.unscheduled, deref(last.Merge), deref(last.Reason), tc.want)
		}
	}
}

// CLR-245: in the Review upkeep, a merge the forge refused after the base
// was merged in is the fact's reason; a failure that is not the forge's
// answer gives none.
func TestUpkeepReportsARefusedMerge(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{forge.RefusedError("GitHub", 200, "Pull request is not mergeable"), "The forge refused to merge the pull request: GitHub answered 200: Pull request is not mergeable"},
		{errors.New("timeout"), ""},
		{nil, ""},
	} {
		f := newFixture(t)
		f.ws.Behinds = []workspace.BehindReply{{Branches: []string{"cinderline/CSR-7"}}}
		f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-7"}}
		f.ws.Ancestors = fill(2, workspace.IsAncestorReply{})
		f.fg.MergeStates = []forge.MergeStateReply{{State: forge.MergePending}}
		f.fg.Finds = []forge.FindReply{{PullRequest: forge.PullRequest{URL: "u7"}, Found: true}}
		f.fg.Merges = []error{tc.err}
		facts := f.p.upkeep(context.Background(), proj, []cinderline.ReviewItem{{Ref: "CSR-7", Title: "Seven", Merge: true}})
		if len(facts) != 1 || facts[0].PullRequest != "open" || deref(facts[0].Reason) != tc.want {
			t.Errorf("%v: facts = %+v; want open with reason %q", tc.err, facts, tc.want)
		}
	}
}

// CLR-245 with CLR-244: failed CI is sent as its checks, never also as a
// reason, so the app does not get it twice.
func TestFailedChecksHaveNoReason(t *testing.T) {
	last := publishOnce(t, `{"kind":"publish","merge":true}`, func(f *fixture) {
		f.fg.Merges = []error{nil}
		f.fg.MergeStates = []forge.MergeStateReply{{State: forge.MergeFailed, Head: "abc123", Failed: []forge.Check{{Name: "combined status", Description: "The head's combined commit status is failure without a failed status."}}}}
		f.fg.CheckLogs = []forge.CheckLogReply{{}, {}}
	})
	if deref(last.Merge) != "failed" || len(last.Checks) != 1 || last.Reason != nil {
		t.Errorf("last = %s; want failed with its check and no reason", show(last))
	}
}
