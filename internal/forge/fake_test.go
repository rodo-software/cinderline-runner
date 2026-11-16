package forge

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestFake(t *testing.T) {
	boom := errors.New("boom")
	pr := PullRequest{Number: 7, URL: "https://git.example.com/acme/web/pulls/7"}
	in := PullRequestInput{Branch: "x/WEB-1", Base: "main", Title: "WEB-1: t", Body: "b"}
	f := &Fake{
		DefaultBranches: []DefaultBranchReply{{Branch: "main"}},
		PullRequests:    []PullRequestReply{{PullRequest: pr}},
		Finds:           []FindReply{{PullRequest: pr, Found: true}},
		Merges:          []error{nil, boom},
		MergeStates:     []MergeStateReply{{State: MergePending}, {State: MergeMerged}},
	}
	ctx := context.Background()

	if b, err := f.DefaultBranch(ctx); b != "main" || err != nil {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}
	if got, err := f.OpenPullRequest(ctx, in); got != pr || err != nil {
		t.Errorf("OpenPullRequest = %+v, %v", got, err)
	}
	if got, found, err := f.FindPullRequest(ctx, "x/WEB-1"); got != pr || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", got, found, err)
	}
	if err := f.RequestMerge(ctx, pr); err != nil {
		t.Errorf("first RequestMerge = %v", err)
	}
	if err := f.RequestMerge(ctx, pr); !errors.Is(err, boom) {
		t.Errorf("second RequestMerge = %v, want boom", err)
	}
	for _, want := range []MergeState{MergePending, MergeMerged} {
		if s, err := f.MergeState(ctx, "x/WEB-1"); s != want || err != nil {
			t.Errorf("MergeState = %q, %v; want %q", s, err, want)
		}
	}

	want := []Call{
		{"DefaultBranch", nil},
		{"OpenPullRequest", []any{in}},
		{"FindPullRequest", []any{"x/WEB-1"}},
		{"RequestMerge", []any{pr}},
		{"RequestMerge", []any{pr}},
		{"MergeState", []any{"x/WEB-1"}},
		{"MergeState", []any{"x/WEB-1"}},
	}
	if calls := f.Calls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("Calls() = %+v, want %+v", calls, want)
	}
}

func TestFakeEmptyQueues(t *testing.T) {
	f := &Fake{}
	ctx := context.Background()
	tests := []struct {
		method string
		call   func() (zero bool, err error)
	}{
		{"DefaultBranch", func() (bool, error) { b, err := f.DefaultBranch(ctx); return b == "", err }},
		{"OpenPullRequest", func() (bool, error) {
			p, err := f.OpenPullRequest(ctx, PullRequestInput{})
			return p == PullRequest{}, err
		}},
		{"FindPullRequest", func() (bool, error) {
			p, found, err := f.FindPullRequest(ctx, "b")
			return p == PullRequest{} && !found, err
		}},
		{"RequestMerge", func() (bool, error) { return true, f.RequestMerge(ctx, PullRequest{}) }},
		{"MergeState", func() (bool, error) { s, err := f.MergeState(ctx, "b"); return s == "", err }},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			zero, err := tt.call()
			if !zero || !errors.Is(err, ErrNotScripted) {
				t.Errorf("zero = %v, err = %v; want zero value and ErrNotScripted", zero, err)
			}
		})
	}
}
