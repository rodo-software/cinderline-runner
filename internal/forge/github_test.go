package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

// The GitHub cases of the app repository's runner/lib.forge.test.mjs at
// commit ecde51a (decision 2026-11-05-legacy-reference, amendments), run
// against recorded answers instead of a network.

const testToken = "secret-token"

type reply struct {
	status int
	body   string
}

type request struct {
	method, url string
	header      http.Header
	body        map[string]any
}

// recorded returns a GitHub Forge for repository whose requests are recorded
// and answered with replies in order, as legacy fakeFetch.
func recorded(t *testing.T, repository string, replies ...reply) (*GitHub, *[]request) {
	t.Helper()
	g, err := NewGitHub(repository, testToken)
	if err != nil {
		t.Fatal(err)
	}
	var reqs *[]request
	g.HTTP, reqs = recorder(t, replies)
	return g, reqs
}

// recorder is an HTTP client that records its requests and answers them
// with replies in order.
func recorder(t *testing.T, replies []reply) (*http.Client, *[]request) {
	var reqs []request
	c := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		req := request{method: r.Method, url: r.URL.String(), header: r.Header}
		if r.Body != nil {
			data, _ := io.ReadAll(r.Body)
			if len(data) > 0 {
				if err := json.Unmarshal(data, &req.body); err != nil {
					t.Errorf("request body %q: %v", data, err)
				}
			}
		}
		reqs = append(reqs, req)
		w := httptest.NewRecorder()
		if len(replies) == 0 {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
			return w.Result(), nil
		}
		w.WriteHeader(replies[0].status)
		io.WriteString(w, replies[0].body)
		replies = replies[1:]
		return w.Result(), nil
	})}
	t.Cleanup(func() {
		if len(replies) > 0 {
			t.Errorf("%d replies left unused", len(replies))
		}
	})
	return c, &reqs
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func urls(reqs []request) []string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.method+" "+r.url)
	}
	return out
}

func wantURLs(t *testing.T, reqs []request, want ...string) {
	t.Helper()
	if got := urls(reqs); !reflect.DeepEqual(got, want) {
		t.Errorf("requests =\n%q\nwant\n%q", got, want)
	}
}

var input = PullRequestInput{Branch: "cinderstack/EMB-3", Base: "main", Title: "T", Body: "B"}

// Ported from "parseRepository treats github.com as GitHub, in every URL
// form" and "parseRepository keeps an https port for the clone, the push and
// the API" (its GitHub Enterprise side). Telling GitHub from other forges by
// host is the forge choice's, not this file's.
func TestGitHubRepositoryForms(t *testing.T) {
	for _, tt := range []struct{ repository, api string }{
		{"https://github.com/willem/ember.git", "https://api.github.com/repos/willem/ember"},
		{"https://GitHub.com/willem/ember", "https://api.github.com/repos/willem/ember"},
		{"ssh://git@github.com/willem/ember.git", "https://api.github.com/repos/willem/ember"},
		{"git@github.com:willem/ember.git", "https://api.github.com/repos/willem/ember"},
		{"https://github.com:443/willem/ember/", "https://api.github.com/repos/willem/ember"},
		{"git@ghe.example.com:team/app.git", "https://ghe.example.com/api/v3/repos/team/app"},
		{"ssh://git@ghe.example.com:222/team/app.git", "https://ghe.example.com/api/v3/repos/team/app"},
		{"https://ghe.example.com:8443/team/app.git", "https://ghe.example.com:8443/api/v3/repos/team/app"},
	} {
		g, reqs := recorded(t, tt.repository, reply{200, `{"default_branch":"main"}`})
		if _, err := g.DefaultBranch(context.Background()); err != nil {
			t.Errorf("%s: %v", tt.repository, err)
		}
		wantURLs(t, *reqs, "GET "+tt.api)
	}

	for _, bad := range []string{"ext::sh -c evil", "https://github.com/group/sub/repo", "https://github.com/repo", "http://github.com/o/r", ""} {
		if _, err := NewGitHub(bad, testToken); err == nil {
			t.Errorf("NewGitHub(%q) succeeded", bad)
		}
	}
}

func TestGitHubDefaultBranch(t *testing.T) {
	g, _ := recorded(t, "https://github.com/willem/ember", reply{200, `{"default_branch":"develop"}`})
	if b, err := g.DefaultBranch(context.Background()); b != "develop" || err != nil {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}

	g, _ = recorded(t, "https://github.com/willem/ember", reply{404, `{"message":"Not Found"}`})
	if _, err := g.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "404: Not Found") {
		t.Errorf("DefaultBranch on a 404 = %v", err)
	}
}

// Ported from "openPullRequest opens a GitHub pull request with a bearer
// header", with the rest of legacy forgeRequest's GitHub headers.
func TestGitHubOpenPullRequest(t *testing.T) {
	g, reqs := recorded(t, "https://github.com/willem/ember.git",
		reply{201, `{"number":7,"html_url":"u","node_id":"PR_1"}`})

	pr, err := g.OpenPullRequest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PullRequest{Number: 7, URL: "u"}) {
		t.Errorf("pull request = %+v", pr)
	}
	wantURLs(t, *reqs, "POST https://api.github.com/repos/willem/ember/pulls")
	r := (*reqs)[0]
	for h, want := range map[string]string{
		"Authorization":        "Bearer " + testToken,
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": "2022-11-28",
		"User-Agent":           name.Name + "-runner",
		"Content-Type":         "application/json",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	if want := map[string]any{"head": "cinderstack/EMB-3", "base": "main", "title": "T", "body": "B"}; !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// Ported from the GitHub half of "openPullRequest finds the pull request an
// earlier run opened": GitHub answers 422 when one is open.
func TestGitHubOpenPullRequestFindsEarlier(t *testing.T) {
	g, reqs := recorded(t, "https://github.com/willem/ember.git",
		reply{422, `{"message":"exists"}`},
		reply{200, `[{"number":2,"html_url":"u","head":{"ref":"cinderstack/EMB-3"}}]`})

	pr, err := g.OpenPullRequest(context.Background(), input)
	if err != nil || pr != (PullRequest{Number: 2, URL: "u"}) {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs,
		"POST https://api.github.com/repos/willem/ember/pulls",
		"GET https://api.github.com/repos/willem/ember/pulls?state=open&head=willem%3Acinderstack%2FEMB-3")
}

// Ported from "openPullRequest reports a refusal with the forge's answer".
func TestGitHubOpenPullRequestRefused(t *testing.T) {
	g, _ := recorded(t, "https://github.com/willem/ember.git",
		reply{422, `{"message":"No commits between main and cinderstack/EMB-3"}`},
		reply{200, `[]`})

	_, err := g.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "422: No commits between main") {
		t.Errorf("OpenPullRequest = %v, want the 422 and its message", err)
	}

	g, _ = recorded(t, "https://github.com/willem/ember.git", reply{401, `{"message":"Bad credentials"}`})
	_, err = g.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "401: Bad credentials") || strings.Contains(err.Error(), testToken) {
		t.Errorf("OpenPullRequest = %v, want the 401 without the token", err)
	}
}

func TestGitHubFindPullRequest(t *testing.T) {
	g, reqs := recorded(t, "https://github.com/willem/ember.git",
		reply{200, `[{"number":2,"html_url":"u","head":{"ref":"cinderstack/EMB-3"}}]`},
		reply{200, `[]`})
	ctx := context.Background()

	if pr, found, err := g.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{2, "u"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := g.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	if (*reqs)[0].url != "https://api.github.com/repos/willem/ember/pulls?state=open&head=willem%3Acinderstack%2FEMB-3" {
		t.Errorf("url = %s", (*reqs)[0].url)
	}
}

// Ported from "requestMerge enables GitHub auto-merge over GraphQL". The
// pull request's node ID is read first: the Forge's PullRequest carries only
// its number.
func TestGitHubRequestMerge(t *testing.T) {
	g, reqs := recorded(t, "https://github.com/willem/ember.git",
		reply{200, `{"number":7,"node_id":"PR_1"}`},
		reply{200, `{"data":{"enablePullRequestAutoMerge":{"clientMutationId":null}}}`})

	if err := g.RequestMerge(context.Background(), PullRequest{Number: 7}); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs,
		"GET https://api.github.com/repos/willem/ember/pulls/7",
		"POST https://api.github.com/graphql")
	r := (*reqs)[1]
	if r.header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization = %q", r.header.Get("Authorization"))
	}
	q, _ := r.body["query"].(string)
	if !strings.Contains(q, "enablePullRequestAutoMerge") || !strings.Contains(q, "mergeMethod: SQUASH") {
		t.Errorf("query = %q", q)
	}
	if want := map[string]any{"id": "PR_1"}; !reflect.DeepEqual(r.body["variables"], want) {
		t.Errorf("variables = %v, want %v", r.body["variables"], want)
	}
}

// Ported from "GitHub Enterprise opens the pull request under /api/v3 and
// auto-merges over /api/graphql".
func TestGitHubEnterprise(t *testing.T) {
	g, reqs := recorded(t, "git@ghe.example.com:team/app.git",
		reply{201, `{"number":7,"html_url":"u","node_id":"PR_1"}`},
		reply{200, `{"number":7,"node_id":"PR_1"}`},
		reply{200, `{"data":{"enablePullRequestAutoMerge":null}}`})
	ctx := context.Background()
	in := input
	in.Base = "develop"

	pr, err := g.OpenPullRequest(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.RequestMerge(ctx, pr); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs,
		"POST https://ghe.example.com/api/v3/repos/team/app/pulls",
		"GET https://ghe.example.com/api/v3/repos/team/app/pulls/7",
		"POST https://ghe.example.com/api/graphql")
	if (*reqs)[0].body["base"] != "develop" {
		t.Errorf("base = %v", (*reqs)[0].body["base"])
	}
	if (*reqs)[2].header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization = %q", (*reqs)[2].header.Get("Authorization"))
	}
}

// Ported from "requestMerge reports a refusal, also as GraphQL errors in a
// 200" and "requestMerge does not retry a 405 on GitHub".
func TestGitHubRequestMergeRefused(t *testing.T) {
	pr := reply{200, `{"number":7,"node_id":"x"}`}
	for _, tt := range []struct {
		answer reply
		want   string
	}{
		{reply{200, `{"errors":[{"message":"Auto merge is not allowed for this repository"}]}`}, "200: Auto merge is not allowed"},
		{reply{405, `{"message":"Please try again later"}`}, "405: Please try again later"},
	} {
		g, reqs := recorded(t, "https://github.com/willem/ember.git", pr, tt.answer)
		err := g.RequestMerge(context.Background(), PullRequest{Number: 7})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("RequestMerge = %v, want %q", err, tt.want)
		}
		if len(*reqs) != 2 {
			t.Errorf("%d requests, want 2: no retry", len(*reqs))
		}
	}

	g, reqs := recorded(t, "https://github.com/willem/ember.git", reply{404, `{"message":"Not Found"}`})
	if err := g.RequestMerge(context.Background(), PullRequest{Number: 7}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("RequestMerge of a missing pull request = %v", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1: no GraphQL call without a node ID", len(*reqs))
	}
}

// Ported from "pullRequestOutcome on GitHub" and "awaitMerge reads GitHub
// pull requests" (the merge wait reads MergeState by branch, decision 0091).
func TestGitHubMergeState(t *testing.T) {
	const list = "GET https://api.github.com/repos/willem/ember/pulls?state=all&head=willem%3Acinderstack%2FEMBER-1"
	const checks = "GET https://api.github.com/repos/willem/ember/commits/abc/check-runs"
	pr := func(fields string) reply {
		return reply{200, `[{"head":{"sha":"abc"},"merged_at":null,` + fields + `}]`}
	}
	for _, tt := range []struct {
		name    string
		replies []reply
		want    MergeState
		urls    []string
	}{
		{"merged", []reply{pr(`"merged_at":"2026-11-04T10:00:00Z","state":"closed"`)}, MergeMerged, []string{list}},
		{"closed", []reply{pr(`"state":"closed"`)}, MergeClosed, []string{list}},
		{"failed checks", []reply{pr(`"state":"open"`),
			{200, `{"check_runs":[{"conclusion":"success"},{"conclusion":"timed_out"}]}`}}, MergeFailed, []string{list, checks}},
		{"cancelled checks", []reply{pr(`"state":"open"`),
			{200, `{"check_runs":[{"conclusion":"cancelled"}]}`}}, MergeFailed, []string{list, checks}},
		{"running checks", []reply{pr(`"state":"open"`),
			{200, `{"check_runs":[{"conclusion":null}]}`}}, MergePending, []string{list, checks}},
		{"none", []reply{{200, `[]`}}, "", []string{list}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, reqs := recorded(t, "https://github.com/willem/ember.git", tt.replies...)
			s, err := g.MergeState(context.Background(), "cinderstack/EMBER-1")
			if s != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
			if (*reqs)[0].header.Get("Authorization") != "Bearer "+testToken {
				t.Errorf("Authorization = %q", (*reqs)[0].header.Get("Authorization"))
			}
		})
	}
}

// Ported from "pullRequestOutcome throws on an error answer" (GitHub).
func TestGitHubMergeStateError(t *testing.T) {
	g, _ := recorded(t, "https://github.com/willem/ember.git", reply{401, `{"message":"no"}`})
	if _, err := g.MergeState(context.Background(), "cinderstack/EMBER-1"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	g, _ = recorded(t, "https://github.com/willem/ember.git", reply{502, `<html>Bad Gateway</html>`})
	if _, err := g.MergeState(context.Background(), "cinderstack/EMBER-1"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}
