package forge

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The GitLab cases of the app repository's runner/lib.forge.test.mjs at
// commit ecde51a (decision 2026-11-05-legacy-reference, amendments), run
// against recorded answers instead of a network.

// recordedGitLab is recorded, for a GitLab Forge.
func recordedGitLab(t *testing.T, repository string, replies ...reply) (*GitLab, *[]request) {
	t.Helper()
	g, err := NewGitLab(repository, testToken)
	if err != nil {
		t.Fatal(err)
	}
	var reqs *[]request
	g.HTTP, reqs = recorder(t, replies)
	return g, reqs
}

const mrs = "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo/merge_requests"

// Ported from "parseRepository reads GitLab URLs with nested groups" and
// the GitLab side of "parseRepository lets the forge argument win over the
// host": any host is GitLab once the project names it. Picking the forge
// from the project's forge or the host is the forge choice's, not this file's.
func TestGitLabRepositoryForms(t *testing.T) {
	for _, tt := range []struct{ repository, api string }{
		{"https://gitlab.com/group/sub/repo.git", "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"},
		{"git@gitlab.com:group/sub/repo.git", "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"},
		{"ssh://git@gitlab.com/group/sub/repo", "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"},
		{"https://GitLab.com:443/group/sub/repo/", "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"},
		{"https://gitlab.example.com:8443/g/r", "https://gitlab.example.com:8443/api/v4/projects/g%2Fr"},
		{"ssh://git@gitlab.example.com:2222/g/r.git", "https://gitlab.example.com/api/v4/projects/g%2Fr"},
		{"https://git.example.com/o/r", "https://git.example.com/api/v4/projects/o%2Fr"},
	} {
		g, reqs := recordedGitLab(t, tt.repository, reply{200, `{"default_branch":"main"}`})
		if _, err := g.DefaultBranch(context.Background()); err != nil {
			t.Errorf("%s: %v", tt.repository, err)
		}
		wantURLs(t, *reqs, "GET "+tt.api)
	}

	for _, bad := range []string{"ext::sh -c evil", "https://gitlab.com/repo", "http://gitlab.com/o/r", "https://gitlab.com/o/../r", ""} {
		if _, err := NewGitLab(bad, testToken); err == nil {
			t.Errorf("NewGitLab(%q) succeeded", bad)
		}
	}
}

func TestGitLabDefaultBranch(t *testing.T) {
	g, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo", reply{200, `{"default_branch":"develop"}`})
	if b, err := g.DefaultBranch(context.Background()); b != "develop" || err != nil {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo", reply{404, `{"message":"404 Project Not Found"}`})
	if _, err := g.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "404: 404 Project Not Found") {
		t.Errorf("DefaultBranch on a 404 = %v", err)
	}
}

// Ported from "openPullRequest opens a GitLab merge request": the merge
// request's iid is its number.
func TestGitLabOpenPullRequest(t *testing.T) {
	const web = "https://gitlab.com/group/sub/repo/-/merge_requests/5"
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{201, `{"iid":5,"web_url":"` + web + `","id":99}`})
	in := input
	in.Base = "develop"

	pr, err := g.OpenPullRequest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PullRequest{Number: 5, URL: web}) {
		t.Errorf("pull request = %+v", pr)
	}
	wantURLs(t, *reqs, "POST "+mrs)
	r := (*reqs)[0]
	for h, want := range map[string]string{
		"Private-Token": testToken,
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	want := map[string]any{"source_branch": "cinderstack/EMB-3", "target_branch": "develop", "title": "T", "description": "B"}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// Ported from "openPullRequest finds the open GitLab merge request on a
// 409": GitLab answers 409 when the source branch has one open, with its
// message as a list.
func TestGitLabOpenPullRequestFindsEarlier(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{409, `{"message":["Another open merge request already exists for this source branch: !5"]}`},
		reply{200, `[{"iid":4,"web_url":"x","source_branch":"other"},{"iid":5,"web_url":"u","source_branch":"cinderstack/EMB-3"}]`})

	pr, err := g.OpenPullRequest(context.Background(), input)
	if err != nil || pr != (PullRequest{Number: 5, URL: "u"}) {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs, "POST "+mrs, "GET "+mrs+"?state=opened&source_branch=cinderstack%2FEMB-3")
}

// The GitLab side of "openPullRequest reports a refusal with the forge's
// answer": a 409 with no open merge request found, and any other refusal,
// end in the first answer, without the token.
func TestGitLabOpenPullRequestRefused(t *testing.T) {
	g, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{409, `{"message":["Another open merge request already exists"]}`},
		reply{200, `[]`})
	_, err := g.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), `409: ["Another open merge request already exists"]`) {
		t.Errorf("OpenPullRequest = %v, want the 409 and its message", err)
	}

	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{401, `{"message":"401 Unauthorized"}`})
	_, err = g.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "401: 401 Unauthorized") || strings.Contains(err.Error(), testToken) {
		t.Errorf("OpenPullRequest = %v, want the 401 without the token", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1: only a 409 looks for an open one", len(*reqs))
	}
}

func TestGitLabFindPullRequest(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{200, `[{"iid":5,"web_url":"u","source_branch":"cinderstack/EMB-3"}]`},
		reply{200, `[]`})
	ctx := context.Background()

	if pr, found, err := g.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{5, "u"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := g.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, (*reqs)[:1], "GET "+mrs+"?state=opened&source_branch=cinderstack%2FEMB-3")
}

// Ported from "requestMerge asks GitLab to merge when the pipeline
// succeeds".
func TestGitLabRequestMerge(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{200, `{"iid":5}`})

	if err := g.RequestMerge(context.Background(), PullRequest{Number: 5}); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs, "PUT "+mrs+"/5/merge")
	want := map[string]any{
		"auto_merge":                   true,
		"merge_when_pipeline_succeeds": true,
		"squash":                       true,
		"should_remove_source_branch":  true,
	}
	if r := (*reqs)[0]; !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// Ported from "requestMerge reports GitLab refusing": no retry.
func TestGitLabRequestMergeRefused(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{405, `{"message":"405 Method Not Allowed"}`})
	err := g.RequestMerge(context.Background(), PullRequest{Number: 5})
	if err == nil || !strings.Contains(err.Error(), "GitLab answered PUT") || !strings.Contains(err.Error(), "405: 405 Method Not Allowed") {
		t.Errorf("RequestMerge = %v, want GitLab's 405", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1: no retry", len(*reqs))
	}
}

// Ported from "pullRequestOutcome on GitLab" and "awaitMerge reads GitLab
// merge requests" (the merge wait reads MergeState by branch, decision 0091).
func TestGitLabMergeState(t *testing.T) {
	const list = "GET " + mrs + "?source_branch=cinderstack%2FEMBER-1&state=all"
	const one = "GET " + mrs + "/5"
	mr := func(state string) reply { return reply{200, `[{"iid":5,"state":"` + state + `"}]`} }
	for _, tt := range []struct {
		name    string
		replies []reply
		want    MergeState
		urls    []string
	}{
		{"merged", []reply{mr("merged")}, MergeMerged, []string{list}},
		{"closed", []reply{mr("closed")}, MergeClosed, []string{list}},
		{"failed pipeline", []reply{mr("opened"), {200, `{"head_pipeline":{"status":"failed"}}`}}, MergeFailed, []string{list, one}},
		{"canceled pipeline", []reply{mr("opened"), {200, `{"head_pipeline":{"status":"canceled"}}`}}, MergeFailed, []string{list, one}},
		{"running pipeline", []reply{mr("opened"), {200, `{"head_pipeline":{"status":"running"}}`}}, MergePending, []string{list, one}},
		{"no pipeline", []reply{mr("opened"), {200, `{"head_pipeline":null}`}}, MergePending, []string{list, one}},
		{"none", []reply{{200, `[]`}}, "", []string{list}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", tt.replies...)
			s, err := g.MergeState(context.Background(), "cinderstack/EMBER-1")
			if s != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
			if (*reqs)[0].header.Get("Private-Token") != testToken {
				t.Errorf("Private-Token = %q", (*reqs)[0].header.Get("Private-Token"))
			}
		})
	}
}

// Ported from "pullRequestOutcome throws on an error answer" (GitLab).
func TestGitLabMergeStateError(t *testing.T) {
	g, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{401, `{"message":"no"}`})
	if _, err := g.MergeState(context.Background(), "cinderstack/EMBER-1"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{502, `<html>Bad Gateway</html>`})
	if _, err := g.MergeState(context.Background(), "cinderstack/EMBER-1"); err == nil || !strings.Contains(err.Error(), "502: <html>Bad Gateway</html>") {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}

// CSR-81 point 1: GitLab keeps a merged request's source_branch after the
// branch is removed, and the list by source_branch still answers it, so the
// merge status poll sees the merge.
func TestGitLabMergeStateAfterBranchDeletion(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{200, `[{"iid":53,"source_branch":"cinderstack/EMBER-1","state":"merged"}]`})
	s, err := g.MergeState(context.Background(), "cinderstack/EMBER-1")
	if s != MergeMerged || err != nil {
		t.Errorf("MergeState = %q, %v; want merged", s, err)
	}
	wantURLs(t, *reqs, "GET "+mrs+"?source_branch=cinderstack%2FEMBER-1&state=all")
}
