package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// writeNote writes a note as of when, which saveNote does not allow.
func writeNote(t *testing.T, p *Project, ref string, n Note, when time.Time) {
	t.Helper()
	p.saveNote(ref, n)
	n.Time = when
	b, _ := json.Marshal(n)
	if err := os.WriteFile(p.noteFile(ref), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// drop deletes now, expire applies the age limit and then the disk limit,
// oldest first, keep leaves it; a clone that could not be pushed is never
// deleted, and a ref the runner did not ask about is never touched.
func TestTidy(t *testing.T) {
	f := newFixture(t)
	p := f.p
	p.KeepFor, p.MinFree = time.Hour, 100
	var forgotten []string
	p.deleteSession = func(_ context.Context, ref, _ string) error { forgotten = append(forgotten, ref); return nil }
	writeNote(t, p, "CSR-9", Note{Dir: "/runs/CSR-9", Session: "s9", Client: "claude", Origin: "kept"}, time.Now().Add(-2*time.Hour))
	writeNote(t, p, "CSR-10", Note{Dir: "/runs/CSR-10", Session: "s10", Client: "claude", Origin: "kept"}, time.Now())
	writeNote(t, p, "CSR-11", Note{Dir: "/runs/CSR-11", Session: "s11", Client: "claude", Origin: "paused"}, time.Now())
	writeNote(t, p, "CSR-13", Note{Dir: "/runs/CSR-13", Session: "s13", Client: "claude", Origin: "kept"}, time.Now().Add(-48*time.Hour))
	f.ws.Strandeds = []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-12", Branch: "cinderline/CSR-12"}}}}
	f.ws.Pushes = []error{errors.New("offline")}
	f.ws.Removes = fill(3, error(nil))
	f.ws.Frees = []workspace.FreeReply{{Bytes: 50}, {Bytes: 200}}

	asked, stranded, ok := p.held(context.Background())
	if !ok || !slices.Equal(asked, []string{"CSR-10", "CSR-11", "CSR-13", "CSR-9", "CSR-12"}) {
		t.Fatalf("held = %v, %v", asked, ok)
	}
	asked = slices.DeleteFunc(asked, func(r string) bool { return r == "CSR-13" })
	p.tidy(context.Background(), asked, []cinderline.HeldItem{
		{Ref: "CSR-9", Session: "expire"}, {Ref: "CSR-10", Session: "expire"}, {Ref: "CSR-11", Session: "drop"},
		{Ref: "CSR-12", Session: "drop"}, {Ref: "CSR-13", Session: "drop"}, {Ref: "../x", Session: "drop"},
	}, stranded)

	var removed []string
	for _, c := range f.ws.Calls() {
		if c.Method == "Remove" {
			removed = append(removed, c.Args[0].(string))
		}
	}
	if !slices.Equal(removed, []string{"/runs/CSR-9", "/runs/CSR-11", "/runs/CSR-10"}) {
		t.Errorf("removed %v", removed)
	}
	if !slices.Equal(forgotten, []string{"CSR-9", "CSR-11", "CSR-10"}) {
		t.Errorf("sessions deleted %v", forgotten)
	}
	if n, _ := p.readNote("CSR-9"); n.Pruned != prunedAge {
		t.Errorf("CSR-9 note = %+v; want why it was pruned", n)
	}
	if n, _ := p.readNote("CSR-10"); n.Pruned != prunedDisk {
		t.Errorf("CSR-10 note = %+v; want why it was pruned", n)
	}
	if _, ok := p.readNote("CSR-11"); ok {
		t.Error("the dropped issue kept its note")
	}
	if n, ok := p.readNote("CSR-13"); !ok || n.Session != "s13" {
		t.Error("an issue the runner did not ask about was touched")
	}
}

// One upkeep pass: a stranded clone is pushed and gets its pull request, a
// branch behind the base is updated and its merge asked for again, a
// conflict is recorded without a push, and every item's pull request state
// is a fact.
func TestUpkeep(t *testing.T) {
	f := newFixture(t)
	f.ws.Strandeds = []workspace.StrandedReply{{Clones: []workspace.Clone{{Dir: "/runs/CSR-7", Branch: "cinderline/CSR-7"}}}}
	f.ws.Behinds = []workspace.BehindReply{{Branches: []string{"cinderline/CSR-7", "cinderline/CSR-9"}}}
	f.ws.Prepares = []workspace.PrepareReply{{Dir: "/runs/CSR-7"}, {Dir: "/runs/CSR-9"}}
	f.ws.Ancestors = fill(4, workspace.IsAncestorReply{})
	f.ws.MergeBases = []workspace.MergeBaseReply{{}, {Conflicts: []string{"x.go"}}}
	f.ws.Logs = []workspace.LogReply{{}}
	f.fg.MergeStates = []forge.MergeStateReply{{State: ""}, {State: forge.MergeMerged}, {State: forge.MergePending}}
	f.fg.PullRequests = []forge.PullRequestReply{{PullRequest: forge.PullRequest{URL: "u7"}}}
	f.fg.Finds = []forge.FindReply{{PullRequest: forge.PullRequest{URL: "u7"}, Found: true}}
	f.fg.Merges = []error{nil}

	facts := f.p.upkeep(context.Background(), proj, []cinderline.ReviewItem{
		{Ref: "CSR-7", Title: "Seven", Merge: true}, {Ref: "CSR-8", Title: "Eight"}, {Ref: "CSR-9", Title: "Nine", Merge: true}, {Ref: "../x"},
	})
	want := []cinderline.ReviewFact{
		{Ref: "CSR-7", PullRequest: "open", Conflicts: []string{}},
		{Ref: "CSR-8", PullRequest: "merged", Conflicts: []string{}},
		{Ref: "CSR-9", PullRequest: "open", Conflicts: []string{"x.go"}},
	}
	if len(facts) != len(want) {
		t.Fatalf("facts = %+v", facts)
	}
	for i := range want {
		if facts[i].Ref != want[i].Ref || facts[i].PullRequest != want[i].PullRequest || !slices.Equal(facts[i].Conflicts, want[i].Conflicts) {
			t.Errorf("fact %d = %+v, want %+v", i, facts[i], want[i])
		}
	}
	var forgeCalls []string
	for _, c := range f.fg.Calls() {
		forgeCalls = append(forgeCalls, c.Method)
		if c.Method == "OpenPullRequest" {
			in := c.Args[0].(forge.PullRequestInput)
			if in.Title != "CSR-7: Seven" || in.Body != "Resolves CSR-7 in Cinderline." || in.Base != "main" {
				t.Errorf("pull request = %+v", in)
			}
		}
	}
	if !slices.Equal(forgeCalls, []string{"MergeState", "OpenPullRequest", "FindPullRequest", "RequestMerge", "MergeState", "MergeState"}) {
		t.Errorf("forge calls %v", forgeCalls)
	}
	var pushes []string
	for _, c := range f.ws.Calls() {
		if c.Method == "Push" {
			pushes = append(pushes, c.Args[0].(string))
		}
	}
	if !slices.Equal(pushes, []string{"/runs/CSR-7", "/runs/CSR-7"}) {
		t.Errorf("pushes %v; want the stranded clone and its update, not the conflict", pushes)
	}
}

// verifyMerge holds only for a clean clone that contains the base and the
// branch's own head, without conflict markers.
func TestVerifyMerge(t *testing.T) {
	for _, tc := range []struct {
		clean, base, head, markers, want bool
	}{
		{true, true, true, false, true},
		{false, true, true, false, false},
		{true, false, true, false, false},
		{true, true, false, false, false},
		{true, true, true, true, false},
	} {
		ws := &workspace.Fake{
			Cleans:    []workspace.BoolReply{{Yes: tc.clean}},
			Ancestors: []workspace.IsAncestorReply{{Yes: tc.base}, {Yes: tc.head}},
			Markers:   []workspace.BoolReply{{Yes: tc.markers}},
		}
		if got, _ := verifyMerge(context.Background(), ws, "/runs/CSR-1", "main", "abc", []string{"a"}); got != tc.want {
			t.Errorf("%+v: verifyMerge = %v", tc, got)
		}
	}
}
