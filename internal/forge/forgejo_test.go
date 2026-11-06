package forge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The Forgejo cases of the app repository's runner/lib.forge.test.mjs at
// commit ecde51a (decision 2026-11-05-legacy-reference, amendments), run
// against recorded answers instead of a network. Gitea has the same API.

// recordedForgejo is recorded, for a Forgejo Forge whose merge retries
// record their waits instead of sleeping.
func recordedForgejo(t *testing.T, repository string, replies ...reply) (*Forgejo, *[]request, *[]time.Duration) {
	t.Helper()
	f, err := NewForgejo(repository, testToken)
	if err != nil {
		t.Fatal(err)
	}
	var reqs *[]request
	f.HTTP, reqs = recorder(t, replies)
	var waits []time.Duration
	f.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	return f, reqs, &waits
}

const pulls = "https://git.rodo.nl/api/v1/repos/willem/ember/pulls"

// Ported from "parseRepository takes the three URL forms and ports, and
// clones over https" and "parseRepository keeps an https port for the
// clone, the push and the API" (its API side; the clone URL is
// internal/workspace's, the repository check internal/config's).
func TestForgejoRepositoryForms(t *testing.T) {
	for _, tt := range []struct{ repository, api string }{
		{"https://git.rodo.nl/willem/ember.git", "https://git.rodo.nl/api/v1/repos/willem/ember"},
		{"https://git.rodo.nl:443/willem/ember", "https://git.rodo.nl/api/v1/repos/willem/ember"},
		{"ssh://git@git.rodo.nl:222/willem/ember.git", "https://git.rodo.nl/api/v1/repos/willem/ember"},
		{"git@git.rodo.nl:willem/ember.git", "https://git.rodo.nl/api/v1/repos/willem/ember"},
		{"https://Git.Rodo.nl/willem/ember/", "https://git.rodo.nl/api/v1/repos/willem/ember"},
		{"https://git.rodo.nl:3000/willem/ember.git", "https://git.rodo.nl:3000/api/v1/repos/willem/ember"},
		{"https://gitea.com/o/r", "https://gitea.com/api/v1/repos/o/r"},
	} {
		f, reqs, _ := recordedForgejo(t, tt.repository, reply{200, `{"default_branch":"main"}`})
		if _, err := f.DefaultBranch(context.Background()); err != nil {
			t.Errorf("%s: %v", tt.repository, err)
		}
		wantURLs(t, *reqs, "GET "+tt.api)
	}

	for _, bad := range []string{"ext::sh -c evil", "https://git.rodo.nl/repo", "https://git.rodo.nl/o/sub/r", "http://git.rodo.nl/o/r", "https://git.rodo.nl/o/..", ""} {
		if _, err := NewForgejo(bad, testToken); err == nil {
			t.Errorf("NewForgejo(%q) succeeded", bad)
		}
	}
}

func TestForgejoDefaultBranch(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember", reply{200, `{"default_branch":"develop"}`})
	if b, err := f.DefaultBranch(context.Background()); b != "develop" || err != nil {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}

	f, _, _ = recordedForgejo(t, "https://git.rodo.nl/willem/ember", reply{404, `{"message":"The target couldn't be found."}`})
	if _, err := f.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "404: The target couldn't be found.") {
		t.Errorf("DefaultBranch on a 404 = %v", err)
	}
}

// Ported from "openPullRequest opens a Forgejo pull request with a token
// header" and "openPullRequest targets the given base branch".
func TestForgejoOpenPullRequest(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{201, `{"number":7,"html_url":"u"}`})
	in := input
	in.Base = "develop"

	pr, err := f.OpenPullRequest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PullRequest{Number: 7, URL: "u"}) {
		t.Errorf("pull request = %+v", pr)
	}
	wantURLs(t, *reqs, "POST "+pulls)
	r := (*reqs)[0]
	for h, want := range map[string]string{
		"Authorization": "token " + testToken,
		"Accept":        "application/json",
		"Content-Type":  "application/json",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	want := map[string]any{"head": "cinderstack/EMB-3", "base": "develop", "title": "T", "body": "B"}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// Ported from "openPullRequest finds the pull request an earlier run
// opened", its Forgejo half: a 409, then the open pull requests, matched by
// head ref.
func TestForgejoOpenPullRequestFindsEarlier(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git",
		reply{409, `{"message":"exists"}`},
		reply{200, `[{"number":1,"head":{"ref":"cinderstack/EMB-1"}},{"number":2,"html_url":"u","head":{"ref":"cinderstack/EMB-3"}}]`})

	pr, err := f.OpenPullRequest(context.Background(), input)
	if err != nil || pr != (PullRequest{Number: 2, URL: "u"}) {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs, "POST "+pulls, "GET "+pulls+"?state=open&limit=50")
}

// Ported from "openPullRequest reports a refusal with the forge's answer",
// on Forgejo: a 409 with none found, and any other refusal, end in the
// first answer, without the token.
func TestForgejoOpenPullRequestRefused(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git",
		reply{409, `{"message":"exists"}`},
		reply{200, `[]`})
	_, err := f.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "Forgejo answered POST "+pulls+" with 409: exists") {
		t.Errorf("OpenPullRequest = %v, want the 409 and its message", err)
	}

	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{422, `{"message":"no commits between main and cinderstack/EMB-3"}`})
	_, err = f.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "422: no commits") || strings.Contains(err.Error(), testToken) {
		t.Errorf("OpenPullRequest = %v, want the 422 without the token", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1: only a 409 looks for an open one", len(*reqs))
	}
}

func TestForgejoFindPullRequest(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git",
		reply{200, `[{"number":2,"html_url":"u","head":{"ref":"cinderstack/EMB-3"}}]`},
		reply{200, `[{"number":1,"head":{"ref":"cinderstack/EMB-1"}}]`})
	ctx := context.Background()

	if pr, found, err := f.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{2, "u"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := f.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, (*reqs)[:1], "GET "+pulls+"?state=open&limit=50")
}

// Ported from "requestMerge asks Forgejo to squash-merge when the checks
// succeed": Forgejo answers the merge with an empty 200.
func TestForgejoRequestMerge(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{200, ``})

	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs, "POST "+pulls+"/7/merge")
	want := map[string]any{"Do": "squash", "merge_when_checks_succeed": true, "delete_branch_after_merge": true}
	if r := (*reqs)[0]; !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

const tryAgain = `{"message":"Please try again later"}`

// Ported from "requestMerge retries a Forgejo 405 and then succeeds".
func TestForgejoRequestMergeRetries405(t *testing.T) {
	f, reqs, waits := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git",
		reply{405, tryAgain}, reply{405, tryAgain}, reply{200, `null`})

	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs, "POST "+pulls+"/7/merge", "POST "+pulls+"/7/merge", "POST "+pulls+"/7/merge")
	if want := []time.Duration{5 * time.Second, 5 * time.Second}; !reflect.DeepEqual(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

// Ported from "requestMerge gives up on Forgejo after twelve 405s".
func TestForgejoRequestMergeGivesUp(t *testing.T) {
	replies := make([]reply, 12)
	for i := range replies {
		replies[i] = reply{405, tryAgain}
	}
	f, reqs, waits := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", replies...)

	err := f.RequestMerge(context.Background(), PullRequest{Number: 7})
	if err == nil || !strings.Contains(err.Error(), "Forgejo answered POST") || !strings.Contains(err.Error(), "405: Please try again later") {
		t.Errorf("RequestMerge = %v, want Forgejo's 405", err)
	}
	if len(*reqs) != 12 || len(*waits) != 11 {
		t.Errorf("%d requests and %d waits, want 12 and 11", len(*reqs), len(*waits))
	}
}

// Not in legacy: other refusals are not retried, and a cancelled wait ends
// the retries with the context's error.
func TestForgejoRequestMergeStops(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{409, `{"message":"not mergeable"}`})
	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); err == nil || !strings.Contains(err.Error(), "409: not mergeable") {
		t.Errorf("RequestMerge = %v, want the 409", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1", len(*reqs))
	}

	f, reqs, _ = recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{405, tryAgain})
	f.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); !errors.Is(err, context.Canceled) {
		t.Errorf("RequestMerge = %v, want context.Canceled", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1", len(*reqs))
	}
}

// Ported from "pullRequestOutcome on Forgejo": the most recently updated
// pull request of the branch among the last 50, then the combined commit
// status of its head.
func TestForgejoMergeState(t *testing.T) {
	const list = "GET " + pulls + "?state=all&sort=recentupdate&limit=50"
	const status = "GET https://git.rodo.nl/api/v1/repos/willem/ember/commits/abc/status"
	pr := func(fields string) reply {
		return reply{200, `[{"head":{"ref":"cinderstack/OTHER","sha":"x"}},{"head":{"ref":"cinderstack/EMBER-1","sha":"abc"},` + fields + `}]`}
	}
	for _, tt := range []struct {
		name    string
		replies []reply
		want    MergeState
		urls    []string
	}{
		{"merged", []reply{pr(`"merged":true,"state":"closed"`)}, MergeMerged, []string{list}},
		{"closed", []reply{pr(`"merged":false,"state":"closed"`)}, MergeClosed, []string{list}},
		{"failure", []reply{pr(`"merged":false,"state":"open"`), {200, `{"state":"failure"}`}}, MergeFailed, []string{list, status}},
		{"error", []reply{pr(`"state":"open"`), {200, `{"state":"error"}`}}, MergeFailed, []string{list, status}},
		{"pending", []reply{pr(`"state":"open"`), {200, `{"state":"pending"}`}}, MergePending, []string{list, status}},
		{"none", []reply{{200, `[]`}}, "", []string{list}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", tt.replies...)
			s, err := f.MergeState(context.Background(), "cinderstack/EMBER-1")
			if s != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
			if got := (*reqs)[0].header.Get("Authorization"); got != "token "+testToken {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
}

// Ported from "pullRequestOutcome throws on an error answer" (Forgejo).
func TestForgejoMergeStateError(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{401, `{"message":"no"}`})
	if _, err := f.MergeState(context.Background(), "cinderstack/EMBER-1"); err == nil || !strings.Contains(err.Error(), "401: no") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	f, _, _ = recordedForgejo(t, "https://git.rodo.nl/willem/ember.git", reply{502, `<html>Bad Gateway</html>`})
	if _, err := f.MergeState(context.Background(), "cinderstack/EMBER-1"); err == nil || !strings.Contains(err.Error(), "502: <html>Bad Gateway</html>") || strings.Contains(err.Error(), testToken) {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}
