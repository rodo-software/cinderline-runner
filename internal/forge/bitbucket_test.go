package forge

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The Bitbucket cases of the app repository's runner/lib.forge.test.mjs at
// commit ecde51a (decision 2026-11-05-legacy-reference, amendments), run
// against recorded answers instead of a network.

// recordedBitbucket is recorded, for a Bitbucket Forge.
func recordedBitbucket(t *testing.T, replies ...reply) (*Bitbucket, *[]request) {
	t.Helper()
	b, err := NewBitbucket("git@bitbucket.org:ws/repo.git", testToken)
	if err != nil {
		t.Fatal(err)
	}
	var reqs *[]request
	b.HTTP, reqs = recorder(t, replies)
	return b, reqs
}

const (
	bbRepo  = "https://api.bitbucket.org/2.0/repositories/ws/repo"
	bbPulls = bbRepo + "/pullrequests"
	bbPR    = `{"id":8,"state":"OPEN","source":{"branch":{"name":"cinderstack/EMB-3"},"commit":{"hash":"abc123"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/8"}}}`
)

// Ported from "parseRepository treats bitbucket.org as Bitbucket, in every
// URL form": the API is always api.bitbucket.org, and only bitbucket.org is
// Bitbucket Cloud (decision 2026-11-04-bitbucket-cloud).
func TestBitbucketRepositoryForms(t *testing.T) {
	for _, repository := range []string{
		"https://bitbucket.org/ws/repo.git",
		"ssh://git@bitbucket.org/ws/repo.git",
		"git@bitbucket.org:ws/repo.git",
		"https://BitBucket.org/ws/repo",
		"https://bitbucket.org:443/ws/repo/",
	} {
		b, err := NewBitbucket(repository, testToken)
		if err != nil {
			t.Errorf("NewBitbucket(%q): %v", repository, err)
			continue
		}
		var reqs *[]request
		b.HTTP, reqs = recorder(t, []reply{{200, `{"mainbranch":{"name":"main"}}`}})
		if _, err := b.DefaultBranch(context.Background()); err != nil {
			t.Errorf("%s: %v", repository, err)
		}
		wantURLs(t, *reqs, "GET "+bbRepo)
	}

	for _, bad := range []string{
		"https://bitbucket.org.evil.example/o/r", // legacy: not Bitbucket
		"https://bitbucket.example.com/ws/repo",  // Data Center is not supported
		"https://bitbucket.org:8443/ws/repo",
		"https://bitbucket.org/ws/sub/repo",
		"https://bitbucket.org/repo",
		"https://bitbucket.org/ws/..",
		"http://bitbucket.org/ws/repo",
		"ext::sh -c evil",
		"",
	} {
		if _, err := NewBitbucket(bad, testToken); err == nil {
			t.Errorf("NewBitbucket(%q) succeeded", bad)
		}
	}
}

func TestBitbucketDefaultBranch(t *testing.T) {
	b, reqs := recordedBitbucket(t, reply{200, `{"mainbranch":{"name":"develop"}}`})
	if got, err := b.DefaultBranch(context.Background()); got != "develop" || err != nil {
		t.Errorf("DefaultBranch = %q, %v", got, err)
	}
	if h := (*reqs)[0].header.Get("Authorization"); h != "Bearer "+testToken {
		t.Errorf("Authorization = %q", h)
	}

	b, _ = recordedBitbucket(t, reply{404, `{"type":"error","error":{"message":"Repository ws/repo not found"}}`})
	if _, err := b.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "404: Repository ws/repo not found") {
		t.Errorf("DefaultBranch on a 404 = %v", err)
	}
}

// Ported from "openPullRequest opens a Bitbucket pull request with a bearer
// header": its id is the number, links.html.href the URL.
func TestBitbucketOpenPullRequest(t *testing.T) {
	b, reqs := recordedBitbucket(t, reply{201, bbPR})
	in := input
	in.Base = "develop"

	pr, err := b.OpenPullRequest(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PullRequest{Number: 8, URL: "https://bitbucket.org/ws/repo/pull-requests/8"}) {
		t.Errorf("pull request = %+v", pr)
	}
	wantURLs(t, *reqs, "POST "+bbPulls)
	r := (*reqs)[0]
	for h, want := range map[string]string{
		"Authorization": "Bearer " + testToken,
		"Accept":        "application/json",
		"Content-Type":  "application/json",
	} {
		if got := r.header.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	want := map[string]any{
		"title":               "T",
		"description":         "B",
		"source":              map[string]any{"branch": map[string]any{"name": "cinderstack/EMB-3"}},
		"destination":         map[string]any{"branch": map[string]any{"name": "develop"}},
		"close_source_branch": true,
	}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// Ported from "openPullRequest finds the open Bitbucket pull request of the
// branch when it refuses a duplicate": Bitbucket answers a duplicate with a
// plain 400, so any 400 looks for the open one first.
func TestBitbucketOpenPullRequestFindsEarlier(t *testing.T) {
	b, reqs := recordedBitbucket(t,
		reply{400, `{"type":"error","error":{"message":"Only one pull request may be open for a given source and target branch"}}`},
		reply{200, `{"values":[{"id":2,"source":{"branch":{"name":"other"}},"links":{"html":{"href":"x"}}},` + bbPR + `]}`})

	pr, err := b.OpenPullRequest(context.Background(), input)
	if err != nil || pr.Number != 8 {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs, "POST "+bbPulls, "GET "+bbPulls+"?state=OPEN&q=source.branch.name%3D%22cinderstack%2FEMB-3%22")
}

// Ported from "openPullRequest reports a Bitbucket 400 that is not a
// duplicate", and the Bitbucket side of "openPullRequest reports a refusal
// with the forge's answer": the first answer, without the token.
func TestBitbucketOpenPullRequestRefused(t *testing.T) {
	b, _ := recordedBitbucket(t, reply{400, `{"error":{"message":"bad"}}`}, reply{200, `{"values":[]}`})
	_, err := b.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "Bitbucket answered POST") || !strings.Contains(err.Error(), "400: bad") {
		t.Errorf("OpenPullRequest = %v, want the 400 and its message", err)
	}

	b, reqs := recordedBitbucket(t, reply{401, `Unauthorized`})
	_, err = b.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "401: Unauthorized") || strings.Contains(err.Error(), testToken) {
		t.Errorf("OpenPullRequest = %v, want the 401 without the token", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1: only a 400 looks for an open one", len(*reqs))
	}
}

func TestBitbucketFindPullRequest(t *testing.T) {
	b, reqs := recordedBitbucket(t, reply{200, `{"values":[` + bbPR + `]}`}, reply{200, `{"values":[]}`})
	ctx := context.Background()

	if pr, found, err := b.FindPullRequest(ctx, "cinderstack/EMB-3"); pr.Number != 8 || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := b.FindPullRequest(ctx, "cinderstack/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, (*reqs)[:1], "GET "+bbPulls+"?state=OPEN&q=source.branch.name%3D%22cinderstack%2FEMB-3%22")
}

// Ported from "requestMerge on Bitbucket asks a person to merge and never
// calls the API" (decision 2026-11-04-bitbucket-cloud).
func TestBitbucketRequestMerge(t *testing.T) {
	b, reqs := recordedBitbucket(t)
	err := b.RequestMerge(context.Background(), PullRequest{Number: 8})
	if err == nil || !strings.Contains(err.Error(), "Bitbucket cannot merge") || !strings.Contains(err.Error(), "; merge it yourself") {
		t.Errorf("RequestMerge = %v", err)
	}
	if len(*reqs) != 0 {
		t.Errorf("%d requests, want none", len(*reqs))
	}
}

// Legacy has no Bitbucket pullRequestOutcome: it asked api.bitbucket.org a
// Forgejo route and always failed. This reads the newest pull request of
// the branch, then the build statuses of its head commit, as the other forges.
func TestBitbucketMergeState(t *testing.T) {
	const list = "GET " + bbPulls + "?q=source.branch.name%3D%22cinderstack%2FEMB-3%22&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED&sort=-created_on"
	const statuses = "GET " + bbRepo + "/commit/abc123/statuses"
	pr := func(state string) reply {
		return reply{200, `{"values":[` + strings.Replace(bbPR, `"OPEN"`, `"`+state+`"`, 1) + `]}`}
	}
	builds := func(states ...string) reply {
		var vs []string
		for _, s := range states {
			vs = append(vs, `{"state":"`+s+`"}`)
		}
		return reply{200, `{"values":[` + strings.Join(vs, ",") + `]}`}
	}
	for _, tt := range []struct {
		name    string
		replies []reply
		want    MergeState
		urls    []string
	}{
		{"merged", []reply{pr("MERGED")}, MergeMerged, []string{list}},
		{"declined", []reply{pr("DECLINED")}, MergeClosed, []string{list}},
		{"superseded", []reply{pr("SUPERSEDED")}, MergeClosed, []string{list}},
		{"failed build", []reply{pr("OPEN"), builds("SUCCESSFUL", "FAILED")}, MergeFailed, []string{list, statuses}},
		{"stopped build", []reply{pr("OPEN"), builds("STOPPED")}, MergeFailed, []string{list, statuses}},
		{"running build", []reply{pr("OPEN"), builds("INPROGRESS", "SUCCESSFUL")}, MergePending, []string{list, statuses}},
		{"no builds", []reply{pr("OPEN"), builds()}, MergePending, []string{list, statuses}},
		{"none", []reply{{200, `{"values":[]}`}}, "", []string{list}},
		{"other branch only", []reply{{200, `{"values":[{"id":2,"state":"MERGED","source":{"branch":{"name":"other"}}}]}`}}, "", []string{list}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, reqs := recordedBitbucket(t, tt.replies...)
			s, err := b.MergeState(context.Background(), "cinderstack/EMB-3")
			if s != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
		})
	}
}

// The Bitbucket side of "pullRequestOutcome throws on an error answer".
func TestBitbucketMergeStateError(t *testing.T) {
	b, _ := recordedBitbucket(t, reply{401, `{"type":"error","error":{"message":"no"}}`})
	if _, err := b.MergeState(context.Background(), "cinderstack/EMB-3"); err == nil || !strings.Contains(err.Error(), "401: no") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	b, _ = recordedBitbucket(t, reply{200, `{"values":[` + bbPR + `]}`}, reply{502, `<html>Bad Gateway</html>`})
	if _, err := b.MergeState(context.Background(), "cinderstack/EMB-3"); err == nil || !strings.Contains(err.Error(), "502: <html>Bad Gateway</html>") {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}
