package forge

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Forgejo forge against recorded answers instead of a network. Gitea has the same API.

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

const pulls = "https://git.rodo.nl/api/v1/repos/willem/cinderline/pulls"

// parseRepository takes https URLs and ports, and
// clones over https and "parseRepository keeps an https port for the
// clone, the push and the API" (its API side; the clone URL is
// internal/workspace's, the repository check internal/config's).
func TestForgejoRepositoryForms(t *testing.T) {
	for _, tt := range []struct{ repository, api string }{
		{"https://git.rodo.nl/willem/cinderline.git", "https://git.rodo.nl/api/v1/repos/willem/cinderline"},
		{"https://git.rodo.nl:443/willem/cinderline", "https://git.rodo.nl/api/v1/repos/willem/cinderline"},
		{"https://Git.Rodo.nl/willem/cinderline/", "https://git.rodo.nl/api/v1/repos/willem/cinderline"},
		{"https://git.rodo.nl:3000/willem/cinderline.git", "https://git.rodo.nl:3000/api/v1/repos/willem/cinderline"},
		{"https://gitea.com/o/r", "https://gitea.com/api/v1/repos/o/r"},
	} {
		f, reqs, _ := recordedForgejo(t, tt.repository, reply{200, `{"default_branch":"main"}`})
		if _, err := f.DefaultBranch(context.Background()); err != nil {
			t.Errorf("%s: %v", tt.repository, err)
		}
		wantURLs(t, *reqs, "GET "+tt.api)
	}

	for _, bad := range []string{"ext::sh -c evil", "ssh://git@host.example/o/r", "git@host.example:o/r", "https://user:pw@host.example/o/r", "https://git.rodo.nl/repo", "https://git.rodo.nl/o/sub/r", "http://git.rodo.nl/o/r", "https://git.rodo.nl/o/..", ""} {
		if _, err := NewForgejo(bad, testToken); err == nil {
			t.Errorf("NewForgejo(%q) succeeded", bad)
		}
	}
}

func TestForgejoDefaultBranch(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline", reply{200, `{"default_branch":"develop"}`})
	if b, err := f.DefaultBranch(context.Background()); b != "develop" || err != nil {
		t.Errorf("DefaultBranch = %q, %v", b, err)
	}

	f, _, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline", reply{404, `{"message":"The target couldn't be found."}`})
	if _, err := f.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "404: The target couldn't be found.") {
		t.Errorf("DefaultBranch on a 404 = %v", err)
	}
}

// OpenPullRequest opens a Forgejo pull request with a token
// header and "openPullRequest targets the given base branch".
func TestForgejoOpenPullRequest(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{201, `{"number":7,"html_url":"u"}`})
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
	want := map[string]any{"head": "cinderline/EMB-3", "base": "develop", "title": "T", "body": "B"}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// OpenPullRequest finds the pull request an earlier run
// opened, its Forgejo half: a 409, then the open pull requests, matched by
// head ref.
func TestForgejoOpenPullRequestFindsEarlier(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{409, `{"message":"exists"}`},
		reply{200, `[{"number":1,"head":{"label":"cinderline/EMB-1","ref":"cinderline/EMB-1"}},{"number":2,"html_url":"u","head":{"label":"cinderline/EMB-3","ref":"cinderline/EMB-3"}}]`})

	pr, err := f.OpenPullRequest(context.Background(), input)
	if err != nil || pr != (PullRequest{Number: 2, URL: "u"}) {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs, "POST "+pulls, "GET "+pulls+"?state=open&sort=recentupdate&limit=50")
}

// OpenPullRequest reports a refusal with the forge's answer,
// on Forgejo: a 409 with none found, and any other refusal, end in the
// first answer, without the token.
func TestForgejoOpenPullRequestRefused(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{409, `{"message":"exists"}`},
		reply{200, `[]`})
	_, err := f.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "Forgejo answered POST "+pulls+" with 409: exists") {
		t.Errorf("OpenPullRequest = %v, want the 409 and its message", err)
	}

	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{422, `{"message":"no commits between main and cinderline/EMB-3"}`})
	_, err = f.OpenPullRequest(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "422: no commits") || strings.Contains(err.Error(), testToken) {
		t.Errorf("OpenPullRequest = %v, want the 422 without the token", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1: only a 409 looks for an open one", len(*reqs))
	}
}

func TestForgejoFindPullRequest(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, `[{"number":2,"html_url":"u","head":{"label":"cinderline/EMB-3","ref":"cinderline/EMB-3"}}]`},
		reply{200, `[{"number":1,"head":{"label":"cinderline/EMB-1","ref":"cinderline/EMB-1"}}]`})
	ctx := context.Background()

	if pr, found, err := f.FindPullRequest(ctx, "cinderline/EMB-3"); pr != (PullRequest{2, "u"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := f.FindPullRequest(ctx, "cinderline/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, (*reqs)[:1], "GET "+pulls+"?state=open&sort=recentupdate&limit=50")
}

// RequestMerge asks Forgejo to squash-merge when the checks
// succeed: Forgejo answers the merge with an empty 200.
func TestForgejoRequestMerge(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{200, ``})

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

// RequestMerge retries a Forgejo 405 and then succeeds.
func TestForgejoRequestMergeRetries405(t *testing.T) {
	f, reqs, waits := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{405, tryAgain}, reply{405, tryAgain}, reply{200, `null`})

	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs, "POST "+pulls+"/7/merge", "POST "+pulls+"/7/merge", "POST "+pulls+"/7/merge")
	if want := []time.Duration{5 * time.Second, 5 * time.Second}; !reflect.DeepEqual(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

// RequestMerge gives up on Forgejo after twelve 405s.
func TestForgejoRequestMergeGivesUp(t *testing.T) {
	replies := make([]reply, 12)
	for i := range replies {
		replies[i] = reply{405, tryAgain}
	}
	f, reqs, waits := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", replies...)

	err := f.RequestMerge(context.Background(), PullRequest{Number: 7})
	if err == nil || !strings.Contains(err.Error(), "Forgejo answered POST") || !strings.Contains(err.Error(), "405: Please try again later") {
		t.Errorf("RequestMerge = %v, want Forgejo's 405", err)
	}
	if len(*reqs) != 12 || len(*waits) != 11 {
		t.Errorf("%d requests and %d waits, want 12 and 11", len(*reqs), len(*waits))
	}
}

// Other refusals are not retried, and a cancelled wait ends
// the retries with the context's error.
func TestForgejoRequestMergeStops(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{409, `{"message":"not mergeable"}`})
	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); err == nil || !strings.Contains(err.Error(), "409: not mergeable") {
		t.Errorf("RequestMerge = %v, want the 409", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1", len(*reqs))
	}

	f, reqs, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{405, tryAgain})
	f.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if err := f.RequestMerge(context.Background(), PullRequest{Number: 7}); !errors.Is(err, context.Canceled) {
		t.Errorf("RequestMerge = %v, want context.Canceled", err)
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests, want 1", len(*reqs))
	}
}

// pullRequestOutcome on Forgejo: the most recently updated
// pull request of the branch among the last 50, then the combined commit
// status of its head.
func TestForgejoMergeState(t *testing.T) {
	const list = "GET " + pulls + "?state=all&sort=recentupdate&limit=50"
	const status = "GET https://git.rodo.nl/api/v1/repos/willem/cinderline/commits/abc/status"
	pr := func(fields string) reply {
		return reply{200, `[{"head":{"label":"cinderline/OTHER","ref":"cinderline/OTHER","sha":"x"}},{"head":{"label":"cinderline/EMBER-1","ref":"cinderline/EMBER-1","sha":"abc"},` + fields + `}]`}
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
			f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", tt.replies...)
			s, err := f.MergeState(context.Background(), "cinderline/EMBER-1")
			if s.State != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s.State, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
			if got := (*reqs)[0].header.Get("Authorization"); got != "token "+testToken {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
}

// pullRequestOutcome throws on an error answer (Forgejo).
func TestForgejoMergeStateError(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{401, `{"message":"no"}`})
	if _, err := f.MergeState(context.Background(), "cinderline/EMBER-1"); err == nil || !strings.Contains(err.Error(), "401: no") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	f, _, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{502, `<html>Bad Gateway</html>`})
	if _, err := f.MergeState(context.Background(), "cinderline/EMBER-1"); err == nil || !strings.Contains(err.Error(), "502: <html>Bad Gateway</html>") || strings.Contains(err.Error(), testToken) {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}

// CSR-81 point 1 (decision 2026-11-07-publish-resilience): Forgejo keeps
// the pull request's own head branch in head.label; head.ref is rewritten
// to refs/pull/N/head once the branch is deleted after the merge (seen on
// git.rodo.nl, PR 53, and on codeberg.org). Matching on the label finds the
// merged pull request, so the merge status poll turns an earlier "issue"
// into "success".
func TestForgejoMergeStateAfterBranchDeletion(t *testing.T) {
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, `[{"number":54,"head":{"label":"cinderline/EMBER-116","ref":"cinderline/EMBER-116","sha":"y"},"state":"open"},` +
			`{"number":53,"head":{"label":"cinderline/EMBER-115","ref":"refs/pull/53/head","sha":"x"},"merged":true,"state":"closed"}]`})
	s, err := f.MergeState(context.Background(), "cinderline/EMBER-115")
	if s.State != MergeMerged || err != nil {
		t.Errorf("MergeState = %q, %v; want merged", s.State, err)
	}
	wantURLs(t, *reqs, "GET "+pulls+"?state=all&sort=recentupdate&limit=50")
}

// CSR-81 point 1: the list is walked page by page, most recently updated
// first, until the branch's pull request is found, the list ends or three
// pages were read. A page shorter than 50 is the last one.
func TestForgejoMergeStatePagesUntilFound(t *testing.T) {
	others := make([]string, 50)
	for i := range others {
		others[i] = `{"number":` + strconv.Itoa(100+i) + `,"head":{"label":"other-` + strconv.Itoa(i) + `","ref":"other-` + strconv.Itoa(i) + `","sha":"o"},"state":"open"}`
	}
	full := reply{200, `[` + strings.Join(others, ",") + `]`}
	const page1 = "GET " + pulls + "?state=all&sort=recentupdate&limit=50"
	const page2 = "GET " + pulls + "?state=all&sort=recentupdate&limit=50&page=2"
	const page3 = "GET " + pulls + "?state=all&sort=recentupdate&limit=50&page=3"
	for _, tt := range []struct {
		name    string
		replies []reply
		want    MergeState
		urls    []string
	}{
		{"on page 2", []reply{full, {200, `[{"number":7,"head":{"label":"cinderline/EMBER-1","ref":"refs/pull/7/head","sha":"x"},"merged":true,"state":"closed"}]`}}, MergeMerged, []string{page1, page2}},
		{"on page 3", []reply{full, full, {200, `[{"number":7,"head":{"label":"cinderline/EMBER-1","ref":"cinderline/EMBER-1","sha":"x"},"merged":false,"state":"closed"}]`}}, MergeClosed, []string{page1, page2, page3}},
		{"none, list ends with an empty page", []reply{full, {200, `[]`}}, "", []string{page1, page2}},
		{"none in the pages it reads", []reply{full, full, full}, "", []string{page1, page2, page3}},
		{"none, list ends with a short page", []reply{{200, `[{"number":1,"head":{"label":"other","ref":"other","sha":"o"},"state":"open"}]`}}, "", []string{page1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", tt.replies...)
			s, err := f.MergeState(context.Background(), "cinderline/EMBER-1")
			if s.State != tt.want || err != nil {
				t.Errorf("MergeState = %q, %v; want %q", s.State, err, tt.want)
			}
			wantURLs(t, *reqs, tt.urls...)
		})
	}
}

// CSR-81 point 1: the open pull request of a branch is matched on its label
// too, and the open list is paged the same way.
func TestForgejoFindPullRequestByLabel(t *testing.T) {
	others := make([]string, 50)
	for i := range others {
		others[i] = `{"number":` + strconv.Itoa(100+i) + `,"head":{"label":"other-` + strconv.Itoa(i) + `","ref":"other-` + strconv.Itoa(i) + `"}}`
	}
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, `[` + strings.Join(others, ",") + `]`},
		reply{200, `[{"number":2,"html_url":"u","head":{"label":"cinderline/EMB-3","ref":"cinderline/EMB-3"}}]`})
	if pr, found, err := f.FindPullRequest(context.Background(), "cinderline/EMB-3"); pr != (PullRequest{2, "u"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, *reqs, "GET "+pulls+"?state=open&sort=recentupdate&limit=50", "GET "+pulls+"?state=open&sort=recentupdate&limit=50&page=2")
}

// CLR-201: the failed checks of the head are its commit statuses that
// failed or erred, the newest of each context; a pending one is running.
func TestForgejoFailedChecks(t *testing.T) {
	f, _, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, `[{"head":{"label":"cinderline/EMBER-1","ref":"cinderline/EMBER-1","sha":"abc"},"state":"open"}]`},
		reply{200, `{"state":"failure","statuses":[` +
			`{"status":"failure","context":"ci / test (push)","description":"Failing after 1m","target_url":"https://git.rodo.nl/willem/cinderline/actions/runs/7/jobs/1"},` +
			`{"status":"error","context":"lint","description":"","target_url":""},` +
			`{"status":"success","context":"ci / vet (push)"},{"status":"pending","context":"ci / race (push)"}]}`})
	s, err := f.MergeState(context.Background(), "cinderline/EMBER-1")
	want := PullState{State: MergeFailed, Head: "abc", Running: true, Failed: []Check{
		{Name: "ci / test (push)", Description: "Failing after 1m", URL: "https://git.rodo.nl/willem/cinderline/actions/runs/7/jobs/1", head: "abc"},
		{Name: "lint", head: "abc"},
	}}
	if err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("MergeState = %+v, %v; want %+v", s, err, want)
	}
}

// CLR-201: the log of a failed status is the log of the Actions job it
// links: the head's run whose page starts the link, then that run's job
// with the linked page, else the job at the link's index. The shapes are
// those of codeberg.org's API description (runs in workflow_runs, jobs a
// bare list). A status no run links has none; Forgejo 15, as on
// git.rodo.nl, lists runs but answers 404 for their jobs.
func TestForgejoCheckLog(t *testing.T) {
	const repo = "https://git.rodo.nl/api/v1/repos/willem/cinderline"
	const runs = `{"workflow_runs":[{"id":40,"index_in_repo":6,"html_url":"https://git.rodo.nl/willem/cinderline/actions/runs/6"},` +
		`{"id":41,"index_in_repo":7,"html_url":"https://git.rodo.nl/willem/cinderline/actions/runs/7"}],"total_count":2}`
	check := Check{Name: "ci / test (push)", URL: "https://git.rodo.nl/willem/cinderline/actions/runs/7/jobs/1", head: "abc"}
	f, reqs, _ := recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, runs},
		reply{200, `[{"id":91,"name":"vet","html_url":"https://git.rodo.nl/willem/cinderline/actions/runs/7/jobs/0"},{"id":92,"name":"test","html_url":"https://git.rodo.nl/willem/cinderline/actions/runs/7/jobs/1"}]`},
		reply{206, "--- FAIL: TestX"})
	if log, err := f.CheckLog(context.Background(), check); log != "--- FAIL: TestX" || err != nil {
		t.Errorf("CheckLog = %q, %v", log, err)
	}
	wantURLs(t, *reqs, "GET "+repo+"/actions/runs?head_sha=abc", "GET "+repo+"/actions/runs/41/jobs", "GET "+repo+"/actions/jobs/92/logs")
	if h := (*reqs)[2].header; h.Get("Authorization") != "token "+testToken || h.Get("Range") == "" {
		t.Errorf("log request header = %v", h)
	}

	// Jobs whose pages are not the status's link: the one at its index.
	f, reqs, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git",
		reply{200, runs}, reply{200, `[{"id":91,"html_url":"https://git.rodo.nl/x/91"},{"id":92,"html_url":""}]`}, reply{200, "log"})
	if log, err := f.CheckLog(context.Background(), check); log != "log" || err != nil {
		t.Errorf("CheckLog by index = %q, %v", log, err)
	}
	wantURLs(t, *reqs, "GET "+repo+"/actions/runs?head_sha=abc", "GET "+repo+"/actions/runs/41/jobs", "GET "+repo+"/actions/jobs/92/logs")

	f, _, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{200, runs}, reply{200, `[{"id":91,"html_url":"https://git.rodo.nl/x/91"}]`})
	if log, err := f.CheckLog(context.Background(), check); log != "" || err != nil {
		t.Errorf("CheckLog of a job the run does not have = %q, %v", log, err)
	}

	other := Check{Name: "woodpecker", URL: "https://ci.example.com/repos/1/pipeline/5", head: "abc"}
	f, reqs, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{200, runs})
	if log, err := f.CheckLog(context.Background(), other); log != "" || err != nil || len(*reqs) != 1 {
		t.Errorf("CheckLog of another CI's status = %q, %v after %d requests", log, err, len(*reqs))
	}

	f, _, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git", reply{200, runs}, reply{404, `{"message":"The target couldn't be found."}`})
	if _, err := f.CheckLog(context.Background(), check); status(err) != 404 {
		t.Errorf("CheckLog on Forgejo 15 = %v, want the 404", err)
	}

	f, reqs, _ = recordedForgejo(t, "https://git.rodo.nl/willem/cinderline.git")
	if log, err := f.CheckLog(context.Background(), Check{Name: "lint", head: "abc"}); log != "" || err != nil || len(*reqs) != 0 {
		t.Errorf("CheckLog of a status without a URL = %q, %v after %d requests", log, err, len(*reqs))
	}
}
