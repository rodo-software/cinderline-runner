package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// CLR-245: a merge request the forge refused gives the forge's own words,
// without the request's URL; an error that is not the forge's answer, or a
// 5xx, gives none, so the agent is not sent back for it.
func TestRefusalPerForge(t *testing.T) {
	ctx := context.Background()
	gh, _ := recorded(t, "https://github.com/willem/cinderline.git", reply{200, `{"number":7,"node_id":"x"}`},
		reply{200, `{"errors":[{"message":"Pull request Protected branch rules not configured for this branch"}]}`})
	gl, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{405, `{"message":"405 Method Not Allowed"}`})
	fj, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{409, `{"message":"required approvals missing"}`})
	bb, _ := recordedBitbucket(t)
	for _, tt := range []struct {
		forge string
		err   error
		want  string // "": no reason
	}{
		{"GitHub", gh.RequestMerge(ctx, PullRequest{Number: 7}), "GitHub answered 200: Pull request Protected branch rules not configured for this branch"},
		{"GitLab", gl.RequestMerge(ctx, PullRequest{Number: 5}), "GitLab answered 405: 405 Method Not Allowed"},
		{"Forgejo", fj.RequestMerge(ctx, PullRequest{Number: 7}), "Forgejo answered 409: required approvals missing"},
		// The runner never asks Bitbucket; the agent cannot change that.
		{"Bitbucket", bb.RequestMerge(ctx, PullRequest{Number: 7}), ""},
		{"server error", RefusedError("GitLab", http.StatusBadGateway, "bad gateway"), ""},
		{"no answer", errors.New("dial tcp: connection refused"), ""},
		{"cancelled", context.Canceled, ""},
		{"none", nil, ""},
		{"helper", RefusedError("Forgejo", 405, "not mergeable"), "Forgejo answered 405: not mergeable"},
	} {
		got := Refusal(tt.err)
		if got != tt.want {
			t.Errorf("%s: Refusal(%v) = %q, want %q", tt.forge, tt.err, got, tt.want)
		}
		if strings.Contains(got, "http") {
			t.Errorf("%s: Refusal = %q carries a URL", tt.forge, got)
		}
	}
}

// CLR-245: an open pull request whose checks all finished without a
// failure, and of which the forge has no merge scheduled, says so in
// Unscheduled; one with a merge scheduled, a running check or a failure does
// not. GitHub reads auto_merge from the list, GitLab merge_when_pipeline_succeeds
// and detailed_merge_status from the single merge request.
func TestUnscheduledPerForge(t *testing.T) {
	ctx := context.Background()
	ghPull := func(autoMerge string) reply {
		return reply{200, `[{"head":{"sha":"abc"},"merged_at":null,"state":"open","auto_merge":` + autoMerge + `}]`}
	}
	noRuns, none := reply{200, `{"check_runs":[]}`}, reply{200, `{"state":"pending","statuses":[]}`}
	running := reply{200, `{"check_runs":[{"status":"in_progress","conclusion":null}]}`}
	for _, tt := range []struct {
		name    string
		replies []reply
		want    string
	}{
		{"dropped", []reply{ghPull(`null`), noRuns, none}, "GitHub has no auto-merge enabled on it"},
		{"scheduled", []reply{ghPull(`{"merge_method":"squash"}`), noRuns, none}, ""},
		{"running", []reply{ghPull(`null`), running, none}, ""},
	} {
		g, _ := recorded(t, "https://github.com/willem/cinderline.git", tt.replies...)
		s, err := g.MergeState(ctx, "cinderline/EMBER-1")
		if err != nil || s.Unscheduled != tt.want {
			t.Errorf("GitHub %s: Unscheduled = %q, %v; want %q", tt.name, s.Unscheduled, err, tt.want)
		}
	}

	mr := reply{200, `[{"iid":5,"state":"opened"}]`}
	one := func(fields string) reply { return reply{200, `{"sha":"abc",` + fields + `}`} }
	noStatuses := reply{200, `[]`}
	for _, tt := range []struct {
		name    string
		replies []reply
		want    string
	}{
		{"dropped", []reply{mr, one(`"head_pipeline":{"id":9,"status":"success"},"merge_when_pipeline_succeeds":false,"detailed_merge_status":"not_approved"`), noStatuses},
			"GitLab has no merge of it scheduled; its merge status is not_approved"},
		{"dropped without a status", []reply{mr, one(`"head_pipeline":null,"merge_when_pipeline_succeeds":false`), noStatuses},
			"GitLab has no merge of it scheduled"},
		{"scheduled", []reply{mr, one(`"head_pipeline":{"id":9,"status":"success"},"merge_when_pipeline_succeeds":true,"detailed_merge_status":"not_approved"`), noStatuses}, ""},
		{"running", []reply{mr, one(`"head_pipeline":{"id":9,"status":"running"},"merge_when_pipeline_succeeds":false`), noStatuses}, ""},
		{"failed", []reply{mr, one(`"head_pipeline":null,"merge_when_pipeline_succeeds":false`), {200, `[{"name":"jenkins","status":"failed"}]`}}, ""},
	} {
		g, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", tt.replies...)
		s, err := g.MergeState(ctx, "cinderline/EMBER-1")
		if err != nil || s.Unscheduled != tt.want {
			t.Errorf("GitLab %s: Unscheduled = %q, %v; want %q", tt.name, s.Unscheduled, err, tt.want)
		}
	}

	// Forgejo's API does not say whether a merge is scheduled.
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, `[{"number":7,"state":"open","head":{"ref":"cinderline/EMBER-1","sha":"abc","label":"cinderline/EMBER-1"}}]`},
		reply{200, `{"state":"success","statuses":[]}`})
	if s, err := f.MergeState(ctx, "cinderline/EMBER-1"); err != nil || s.State != MergePending || s.Unscheduled != "" {
		t.Errorf("Forgejo: MergeState = %+v, %v; want pending without Unscheduled", s, err)
	}
}

// The fake passes Unscheduled through.
func TestFakeUnscheduled(t *testing.T) {
	f := &Fake{MergeStates: []MergeStateReply{{State: MergePending, Unscheduled: "dropped"}}}
	if s, _ := f.MergeState(context.Background(), "b"); s.Unscheduled != "dropped" {
		t.Errorf("Unscheduled = %q", s.Unscheduled)
	}
}
