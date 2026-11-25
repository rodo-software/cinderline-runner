package forge

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The Bitbucket forge against recorded answers instead of a network.

// recordedBitbucket is recorded, for a Bitbucket Forge.
func recordedBitbucket(t *testing.T, replies ...reply) (*Bitbucket, *[]request) {
	t.Helper()
	b, err := NewBitbucket("https://bitbucket.org/ws/repo.git", testToken)
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
	bbPR    = `{"id":8,"state":"OPEN","source":{"branch":{"name":"cinderline/EMB-3"},"commit":{"hash":"abc123"}},"links":{"html":{"href":"https://bitbucket.org/ws/repo/pull-requests/8"}}}`
)

// parseRepository treats bitbucket.org as Bitbucket, in every https
// URL form: the API is always api.bitbucket.org, and only bitbucket.org is
// Bitbucket Cloud (decision 2026-11-04-bitbucket-cloud).
func TestBitbucketRepositoryForms(t *testing.T) {
	for _, repository := range []string{
		"https://bitbucket.org/ws/repo.git",
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
		"https://bitbucket.org.evil.example/o/r", // not Bitbucket
		"git@bitbucket.org:ws/repo.git",          // git signs in over https only
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

// OpenPullRequest opens a Bitbucket pull request with a bearer
// header: its id is the number, links.html.href the URL.
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
		"source":              map[string]any{"branch": map[string]any{"name": "cinderline/EMB-3"}},
		"destination":         map[string]any{"branch": map[string]any{"name": "develop"}},
		"close_source_branch": true,
	}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// OpenPullRequest finds the open Bitbucket pull request of the
// branch when it refuses a duplicate: Bitbucket answers a duplicate with a
// plain 400, so any 400 looks for the open one first.
func TestBitbucketOpenPullRequestFindsEarlier(t *testing.T) {
	b, reqs := recordedBitbucket(t,
		reply{400, `{"type":"error","error":{"message":"Only one pull request may be open for a given source and target branch"}}`},
		reply{200, `{"values":[{"id":2,"source":{"branch":{"name":"other"}},"links":{"html":{"href":"x"}}},` + bbPR + `]}`})

	pr, err := b.OpenPullRequest(context.Background(), input)
	if err != nil || pr.Number != 8 {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs, "POST "+bbPulls, "GET "+bbPulls+"?state=OPEN&q=source.branch.name%3D%22cinderline%2FEMB-3%22")
}

// OpenPullRequest reports a Bitbucket 400 that is not a
// duplicate, and the Bitbucket side of "openPullRequest reports a refusal
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

	if pr, found, err := b.FindPullRequest(ctx, "cinderline/EMB-3"); pr.Number != 8 || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := b.FindPullRequest(ctx, "cinderline/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, (*reqs)[:1], "GET "+bbPulls+"?state=OPEN&q=source.branch.name%3D%22cinderline%2FEMB-3%22")
}

// RequestMerge on Bitbucket asks a person to merge and never
// calls the API (decision 2026-11-04-bitbucket-cloud).
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

// The merge state reads the newest pull request of the branch, then the
// build statuses of its head commit, as the other forges.
func TestBitbucketMergeState(t *testing.T) {
	const list = "GET " + bbPulls + "?q=source.branch.name%3D%22cinderline%2FEMB-3%22&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED&sort=-created_on"
	const statuses = "GET " + bbRepo + "/commit/abc123/statuses?pagelen=100"
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
			s, err := b.MergeState(context.Background(), "cinderline/EMB-3")
			if s.State != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s.State, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
		})
	}
}

// The Bitbucket side of "pullRequestOutcome throws on an error answer".
func TestBitbucketMergeStateError(t *testing.T) {
	b, _ := recordedBitbucket(t, reply{401, `{"type":"error","error":{"message":"no"}}`})
	if _, err := b.MergeState(context.Background(), "cinderline/EMB-3"); err == nil || !strings.Contains(err.Error(), "401: no") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	b, _ = recordedBitbucket(t, reply{200, `{"values":[` + bbPR + `]}`}, reply{502, `<html>Bad Gateway</html>`})
	if _, err := b.MergeState(context.Background(), "cinderline/EMB-3"); err == nil || !strings.Contains(err.Error(), "502: <html>Bad Gateway</html>") {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}

// CLR-201: the failed checks are the head's FAILED and STOPPED build
// statuses, named by their name, else their key; one in progress is
// running. CLR-244: a Bitbucket Pipelines build, whose status links its
// results page by build number, has a log; another CI's has its
// description.
func TestBitbucketFailedChecks(t *testing.T) {
	b, _ := recordedBitbucket(t, reply{200, `{"values":[` + bbPR + `]}`},
		reply{200, `{"values":[{"state":"FAILED","key":"k1","name":"Pipeline #12 for cinderline/EMB-3","description":"1 of 2 steps failed","url":"https://bitbucket.org/ws/repo/pipelines/results/12"},` +
			`{"state":"STOPPED","key":"jenkins","name":"","description":"","url":"https://ci.example.com/1"},` +
			`{"state":"FAILED","key":"k4","name":"Pipeline #13","description":"","url":"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/13"},` +
			`{"state":"INPROGRESS","key":"k3","name":"lint"}]}`})
	s, err := b.MergeState(context.Background(), "cinderline/EMB-3")
	want := PullState{State: MergeFailed, Head: "abc123", Running: true, Failed: []Check{
		{Name: "Pipeline #12 for cinderline/EMB-3", Description: "1 of 2 steps failed", URL: "https://bitbucket.org/ws/repo/pipelines/results/12", job: "12"},
		{Name: "jenkins", URL: "https://ci.example.com/1"},
		{Name: "Pipeline #13", URL: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/13", job: "13"},
	}}
	if err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("MergeState = %+v, %v; want %+v", s, err, want)
	}

	// The log of a Pipelines build is the log of its first failed step.
	const pipeline = "GET " + bbRepo + "/pipelines/%7Bp-uuid%7D"
	b, reqs := recordedBitbucket(t,
		reply{200, `{"uuid":"{p-uuid}","build_number":12}`},
		reply{200, `{"values":[{"uuid":"{s1}","name":"build","state":{"name":"COMPLETED","result":{"name":"SUCCESSFUL"}}},` +
			`{"uuid":"{s2}","name":"test","state":{"name":"COMPLETED","result":{"name":"FAILED"}}},` +
			`{"uuid":"{s3}","name":"deploy","state":{"name":"COMPLETED","result":{"name":"ERROR"}}}]}`},
		reply{206, "+ go test ./...\n--- FAIL: TestX"})
	if log, err := b.CheckLog(context.Background(), want.Failed[0]); log != "+ go test ./...\n--- FAIL: TestX" || err != nil {
		t.Errorf("CheckLog = %q, %v", log, err)
	}
	wantURLs(t, *reqs, "GET "+bbRepo+"/pipelines/12", pipeline+"/steps?pagelen=100", pipeline+"/steps/%7Bs2%7D/log")
	if len(*reqs) != 3 {
		t.FailNow()
	}
	if h := (*reqs)[2].header; h.Get("Authorization") != "Bearer "+testToken || h.Get("Range") == "" {
		t.Errorf("log request header = %v", h)
	}

	// A pipeline without a failed step has no log; another CI's build reads
	// nothing; both send their text.
	b, reqs = recordedBitbucket(t, reply{200, `{"uuid":"{p-uuid}"}`},
		reply{200, `{"values":[{"uuid":"{s1}","state":{"name":"COMPLETED","result":{"name":"STOPPED"}}}]}`})
	if log, err := Log(context.Background(), b, want.Failed[2]); err != nil || !strings.HasSuffix(log, "The forge says only that this check failed.") || len(*reqs) != 2 {
		t.Errorf("Log without a failed step = %q, %v after %d requests", log, err, len(*reqs))
	}
	b, reqs = recordedBitbucket(t)
	jenkins := Check{Name: "jenkins", Description: "Build #7 failed", URL: "https://ci.example.com/1"}
	if log, err := Log(context.Background(), b, jenkins); err != nil || !strings.Contains(log, "with the CI that reported it") || !strings.HasSuffix(log, "Build #7 failed") || len(*reqs) != 0 {
		t.Errorf("Log of another CI's build = %q, %v after %d requests", log, err, len(*reqs))
	}

	// A step log read refused twice sends the error and the description.
	b, _ = recordedBitbucket(t, reply{404, `{"type":"error","error":{"message":"Pipeline not found"}}`}, reply{404, `{"type":"error","error":{"message":"Pipeline not found"}}`})
	if log, err := Log(context.Background(), b, want.Failed[0]); status(err) != 404 || log != "Reading this check's log failed: Bitbucket answered 404: Pipeline not found\n\n1 of 2 steps failed" {
		t.Errorf("Log of a refused read = %q, %v", log, err)
	}
}

// CLR-244: build statuses are read 100 to a page, following Bitbucket's
// next link while it stays in the repository's API, never elsewhere with
// the token.
func TestBitbucketPagesBuildStatuses(t *testing.T) {
	b, reqs := recordedBitbucket(t, reply{200, `{"values":[` + bbPR + `]}`},
		reply{200, `{"values":[{"state":"SUCCESSFUL","key":"k1"}],"next":"` + bbRepo + `/commit/abc123/statuses?pagelen=100&page=2"}`},
		reply{200, `{"values":[{"state":"FAILED","key":"k2","name":"late"}],"next":"https://evil.example.com/steal?page=3"}`})
	s, err := b.MergeState(context.Background(), "cinderline/EMB-3")
	want := PullState{State: MergeFailed, Head: "abc123", Failed: []Check{{Name: "late"}}}
	if err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("MergeState = %+v, %v; want %+v", s, err, want)
	}
	if got := urls(*reqs); len(got) != 3 || got[1] != "GET "+bbRepo+"/commit/abc123/statuses?pagelen=100" || got[2] != "GET "+bbRepo+"/commit/abc123/statuses?pagelen=100&page=2" {
		t.Errorf("requests = %q", got)
	}
}

// CLR-237: a fork's pull request from a branch of the same name, listed
// first as the newest, is never found or read as the state.
func TestBitbucketSkipsForkPullRequest(t *testing.T) {
	const fork = `{"id":9,"state":"DECLINED","source":{"branch":{"name":"cinderline/EMB-3"},"repository":{"full_name":"someone/repo"}},` +
		`"destination":{"repository":{"full_name":"ws/repo"}},"links":{"html":{"href":"fork"}}}`
	const own = `{"id":8,"state":"MERGED","source":{"branch":{"name":"cinderline/EMB-3"},"repository":{"full_name":"ws/repo"}},` +
		`"destination":{"repository":{"full_name":"ws/repo"}},"links":{"html":{"href":"own"}}}`
	ctx := context.Background()

	b, _ := recordedBitbucket(t, reply{200, `{"values":[` + fork + `,` + own + `]}`}, reply{200, `{"values":[` + fork + `]}`})
	if pr, found, err := b.FindPullRequest(ctx, "cinderline/EMB-3"); pr != (PullRequest{8, "own"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v; want the repository's own #8", pr, found, err)
	}
	if pr, found, err := b.FindPullRequest(ctx, "cinderline/EMB-3"); found || err != nil {
		t.Errorf("FindPullRequest with only a fork's = %+v, %v, %v; want none", pr, found, err)
	}

	b, _ = recordedBitbucket(t, reply{200, `{"values":[` + fork + `,` + own + `]}`}, reply{200, `{"values":[` + fork + `]}`})
	if s, err := b.MergeState(ctx, "cinderline/EMB-3"); s.State != MergeMerged || err != nil {
		t.Errorf("MergeState = %q, %v; want the repository's own merged, not the fork's declined", s.State, err)
	}
	if s, err := b.MergeState(ctx, "cinderline/EMB-3"); s.State != "" || err != nil {
		t.Errorf("MergeState with only a fork's = %q, %v; want none", s.State, err)
	}
}
