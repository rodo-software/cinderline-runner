package forge

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The GitLab forge against recorded answers instead of a network.

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

// parseRepository reads GitLab URLs with nested groups and
// the GitLab side of "parseRepository lets the forge argument win over the
// host": any host is GitLab once the project names it. Picking the forge
// from the project's forge or the host is the forge choice's, not this file's.
func TestGitLabRepositoryForms(t *testing.T) {
	for _, tt := range []struct{ repository, api string }{
		{"https://gitlab.com/group/sub/repo.git", "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"},
		{"https://GitLab.com:443/group/sub/repo/", "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"},
		{"https://gitlab.example.com:8443/g/r", "https://gitlab.example.com:8443/api/v4/projects/g%2Fr"},
		{"https://git.example.com/o/r", "https://git.example.com/api/v4/projects/o%2Fr"},
	} {
		g, reqs := recordedGitLab(t, tt.repository, reply{200, `{"default_branch":"main"}`})
		if _, err := g.DefaultBranch(context.Background()); err != nil {
			t.Errorf("%s: %v", tt.repository, err)
		}
		wantURLs(t, *reqs, "GET "+tt.api)
	}

	for _, bad := range []string{"ext::sh -c evil", "ssh://git@host.example/o/r", "git@host.example:o/r", "https://user:pw@host.example/o/r", "https://gitlab.com/repo", "http://gitlab.com/o/r", "https://gitlab.com/o/../r", ""} {
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

// OpenPullRequest opens a GitLab merge request: the merge
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
	want := map[string]any{"source_branch": "cinderline/EMB-3", "target_branch": "develop", "title": "T", "description": "B"}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body = %v, want %v", r.body, want)
	}
}

// OpenPullRequest finds the open GitLab merge request on a
// 409: GitLab answers 409 when the source branch has one open, with its
// message as a list.
func TestGitLabOpenPullRequestFindsEarlier(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{409, `{"message":["Another open merge request already exists for this source branch: !5"]}`},
		reply{200, `[{"iid":4,"web_url":"x","source_branch":"other"},{"iid":5,"web_url":"u","source_branch":"cinderline/EMB-3"}]`})

	pr, err := g.OpenPullRequest(context.Background(), input)
	if err != nil || pr != (PullRequest{Number: 5, URL: "u"}) {
		t.Errorf("OpenPullRequest = %+v, %v", pr, err)
	}
	wantURLs(t, *reqs, "POST "+mrs, "GET "+mrs+"?state=opened&source_branch=cinderline%2FEMB-3")
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
		reply{200, `[{"iid":5,"web_url":"u","source_branch":"cinderline/EMB-3"}]`},
		reply{200, `[]`})
	ctx := context.Background()

	if pr, found, err := g.FindPullRequest(ctx, "cinderline/EMB-3"); pr != (PullRequest{5, "u"}) || !found || err != nil {
		t.Errorf("FindPullRequest = %+v, %v, %v", pr, found, err)
	}
	if pr, found, err := g.FindPullRequest(ctx, "cinderline/EMB-3"); pr != (PullRequest{}) || found || err != nil {
		t.Errorf("FindPullRequest without one = %+v, %v, %v", pr, found, err)
	}
	wantURLs(t, (*reqs)[:1], "GET "+mrs+"?state=opened&source_branch=cinderline%2FEMB-3")
}

// RequestMerge asks GitLab to merge when the pipeline
// succeeds.
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

// RequestMerge reports GitLab refusing: no retry.
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

// pullRequestOutcome on GitLab and "awaitMerge reads GitLab
// merge requests" (the merge wait reads MergeState by branch, decision 0091).
// CLR-244: the head commit's statuses are read too, for external CI.
func TestGitLabMergeState(t *testing.T) {
	const list = "GET " + mrs + "?source_branch=cinderline%2FEMBER-1&state=all"
	const one = "GET " + mrs + "/5"
	const statuses = "GET https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo/repository/commits/abc/statuses?per_page=100"
	const jobs = "GET https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo/pipelines/9/jobs?scope%5B%5D=failed&scope%5B%5D=canceled&per_page=100"
	mr := func(state string) reply { return reply{200, `[{"iid":5,"state":"` + state + `"}]`} }
	pipeline := func(p string) reply { return reply{200, `{"sha":"abc","head_pipeline":` + p + `}`} }
	none := reply{200, `[]`}
	job := `[{"id":70,"name":"test","failure_reason":"script_failure","allow_failure":false}]`
	for _, tt := range []struct {
		name    string
		replies []reply
		want    MergeState
		running bool
		urls    []string
	}{
		{"merged", []reply{mr("merged")}, MergeMerged, false, []string{list}},
		{"closed", []reply{mr("closed")}, MergeClosed, false, []string{list}},
		{"failed pipeline", []reply{mr("opened"), pipeline(`{"id":9,"status":"failed"}`), none, {200, job}}, MergeFailed, false, []string{list, one, statuses, jobs}},
		{"canceled pipeline", []reply{mr("opened"), pipeline(`{"id":9,"status":"canceled"}`), none, {200, job}}, MergeFailed, false, []string{list, one, statuses, jobs}},
		{"running pipeline", []reply{mr("opened"), pipeline(`{"id":9,"status":"running"}`), none}, MergePending, true, []string{list, one, statuses}},
		{"pending pipeline", []reply{mr("opened"), pipeline(`{"id":9,"status":"pending"}`), none}, MergePending, true, []string{list, one, statuses}},
		{"created pipeline", []reply{mr("opened"), pipeline(`{"id":9,"status":"created"}`), none}, MergePending, true, []string{list, one, statuses}},
		{"preparing external status", []reply{mr("opened"), pipeline(`null`), {200, `[{"name":"jenkins","status":"preparing"}]`}}, MergePending, true, []string{list, one, statuses}},
		{"passed pipeline", []reply{mr("opened"), pipeline(`{"id":9,"status":"success"}`), none}, MergePending, false, []string{list, one, statuses}},
		{"no pipeline", []reply{mr("opened"), pipeline(`null`), none}, MergePending, false, []string{list, one, statuses}},
		{"failed external status", []reply{mr("opened"), pipeline(`null`), {200, `[{"name":"jenkins","status":"failed","allow_failure":false,"target_url":"https://ci.example.com/7"}]`}},
			MergeFailed, false, []string{list, one, statuses}},
		{"allowed external failure", []reply{mr("opened"), pipeline(`null`), {200, `[{"name":"jenkins","status":"failed","allow_failure":true}]`}},
			MergePending, false, []string{list, one, statuses}},
		{"running external status", []reply{mr("opened"), pipeline(`{"id":9,"status":"success"}`), {200, `[{"name":"jenkins","status":"running"},{"name":"other","status":"failed"}]`}},
			MergeFailed, true, []string{list, one, statuses}},
		{"none", []reply{{200, `[]`}}, "", false, []string{list}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", tt.replies...)
			s, err := g.MergeState(context.Background(), "cinderline/EMBER-1")
			if s.State != tt.want || s.Running != tt.running || err != nil {
				t.Errorf("MergeState = %q running %v, %v; want %q running %v", s.State, s.Running, err, tt.want, tt.running)
			}
			wantURLs(t, *reqs, tt.urls...)
			if (*reqs)[0].header.Get("Private-Token") != testToken {
				t.Errorf("Private-Token = %q", (*reqs)[0].header.Get("Private-Token"))
			}
		})
	}
}

// pullRequestOutcome throws on an error answer (GitLab).
func TestGitLabMergeStateError(t *testing.T) {
	g, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{401, `{"message":"no"}`})
	if _, err := g.MergeState(context.Background(), "cinderline/EMBER-1"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("MergeState = %v, want the 401", err)
	}

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{502, `<html>Bad Gateway</html>`})
	if _, err := g.MergeState(context.Background(), "cinderline/EMBER-1"); err == nil || !strings.Contains(err.Error(), "502: <html>Bad Gateway</html>") {
		t.Errorf("MergeState = %v, want the 502", err)
	}
}

// CSR-81 point 1: GitLab keeps a merged request's source_branch after the
// branch is removed, and the list by source_branch still answers it, so the
// merge status poll sees the merge.
func TestGitLabMergeStateAfterBranchDeletion(t *testing.T) {
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{200, `[{"iid":53,"source_branch":"cinderline/EMBER-1","state":"merged"}]`})
	s, err := g.MergeState(context.Background(), "cinderline/EMBER-1")
	if s.State != MergeMerged || err != nil {
		t.Errorf("MergeState = %q, %v; want merged", s.State, err)
	}
	wantURLs(t, *reqs, "GET "+mrs+"?source_branch=cinderline%2FEMBER-1&state=all")
}

// CLR-201: the failed checks of a failed pipeline are its failed and
// canceled jobs, except those allowed to fail; the head is the merge
// request's own commit. A job's log is its trace.
func TestGitLabFailedChecks(t *testing.T) {
	const project = "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{200, `[{"iid":5,"state":"opened"}]`},
		reply{200, `{"sha":"abc","head_pipeline":{"id":9,"sha":"merge-result","status":"failed"}}`},
		reply{200, `[]`},
		reply{200, `[{"id":70,"name":"test","failure_reason":"script_failure","web_url":"https://gitlab.com/group/sub/repo/-/jobs/70","allow_failure":false},` +
			`{"id":71,"name":"flaky","failure_reason":"script_failure","web_url":"u71","allow_failure":true},` +
			`{"id":72,"name":"deploy","failure_reason":null,"web_url":"u72","allow_failure":false}]`})
	s, err := g.MergeState(context.Background(), "cinderline/EMBER-1")
	want := PullState{State: MergeFailed, Head: "abc", Failed: []Check{
		{Name: "test", Description: "script_failure", URL: "https://gitlab.com/group/sub/repo/-/jobs/70", job: "70"},
		{Name: "deploy", URL: "u72", job: "72"},
	}}
	if err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("MergeState = %+v, %v; want %+v", s, err, want)
	}
	wantURLs(t, *reqs, "GET "+mrs+"?source_branch=cinderline%2FEMBER-1&state=all", "GET "+mrs+"/5", "GET "+project+"/repository/commits/abc/statuses?per_page=100",
		"GET "+project+"/pipelines/9/jobs?scope%5B%5D=failed&scope%5B%5D=canceled&per_page=100")

	g, reqs = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{200, "\x1b[31;1mERROR: Job failed: exit code 1\x1b[0;m"})
	if log, err := g.CheckLog(context.Background(), want.Failed[0]); !strings.Contains(log, "exit code 1") || err != nil {
		t.Errorf("CheckLog = %q, %v", log, err)
	}
	wantURLs(t, *reqs, "GET "+project+"/jobs/70/trace")
	if h := (*reqs)[0].header; h.Get("Private-Token") != testToken || h.Get("Range") == "" {
		t.Errorf("log request header = %v", h)
	}

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{403, `{"message":"403 Forbidden"}`})
	if _, err := g.CheckLog(context.Background(), want.Failed[0]); status(err) != 403 {
		t.Errorf("CheckLog on a 403 = %v", err)
	}
}

// CLR-237: a fork's merge request from a branch of the same name, listed
// first as the newest, is never found, merged or read as the state.
func TestGitLabSkipsForkMergeRequest(t *testing.T) {
	const fork = `{"iid":7,"web_url":"fork","state":"closed","source_branch":"cinderline/EMB-3","source_project_id":99,"target_project_id":1}`
	const own = `{"iid":5,"web_url":"own","state":"merged","source_branch":"cinderline/EMB-3","source_project_id":1,"target_project_id":1}`
	ctx := context.Background()

	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{200, `[` + fork + `,` + own + `]`}, reply{200, `{"iid":5}`})
	pr, found, err := g.FindPullRequest(ctx, "cinderline/EMB-3")
	if pr != (PullRequest{5, "own"}) || !found || err != nil {
		t.Fatalf("FindPullRequest = %+v, %v, %v; want the project's own !5", pr, found, err)
	}
	if err := g.RequestMerge(ctx, pr); err != nil {
		t.Fatal(err)
	}
	wantURLs(t, *reqs, "GET "+mrs+"?state=opened&source_branch=cinderline%2FEMB-3", "PUT "+mrs+"/5/merge")

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{200, `[` + fork + `]`})
	if pr, found, err := g.FindPullRequest(ctx, "cinderline/EMB-3"); found || err != nil {
		t.Errorf("FindPullRequest with only a fork's = %+v, %v, %v; want none", pr, found, err)
	}

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{200, `[` + fork + `,` + own + `]`})
	if s, err := g.MergeState(ctx, "cinderline/EMB-3"); s.State != MergeMerged || err != nil {
		t.Errorf("MergeState = %q, %v; want the project's own merged, not the fork's closed", s.State, err)
	}

	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{200, `[` + fork + `]`})
	if s, err := g.MergeState(ctx, "cinderline/EMB-3"); s.State != "" || err != nil {
		t.Errorf("MergeState with only a fork's = %q, %v; want none", s.State, err)
	}
}

// CLR-244: the head commit's failed statuses that are not a GitLab job
// (whose target is a /-/jobs/ page) are external CI's checks, after the
// pipeline's failed jobs; their log is their description. A failed
// pipeline without a failed job or status is one check with what GitLab
// says of it: its status and its YAML errors.
func TestGitLabExternalStatusesAndFailedWithoutJobs(t *testing.T) {
	const project = "https://gitlab.com/api/v4/projects/group%2Fsub%2Frepo"
	g, _ := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
		reply{200, `[{"iid":5,"state":"opened"}]`},
		reply{200, `{"sha":"abc","head_pipeline":{"id":9,"status":"failed","web_url":"https://gitlab.com/group/sub/repo/-/pipelines/9"}}`},
		reply{200, `[{"id":70,"name":"test","status":"failed","allow_failure":false,"target_url":"https://gitlab.com/group/sub/repo/-/jobs/70"},` +
			`{"id":80,"name":"jenkins","status":"failed","allow_failure":false,"description":"Build #12 failed: 3 tests","target_url":"https://ci.example.com/job/12"},` +
			`{"id":81,"name":"sonar","status":"canceled","allow_failure":false,"description":null,"target_url":null},` +
			`{"id":82,"name":"optional","status":"failed","allow_failure":true,"target_url":"https://ci.example.com/job/13"},` +
			`{"id":83,"name":"docs","status":"success","allow_failure":false,"target_url":"https://ci.example.com/job/14"}]`},
		reply{200, `[{"id":70,"name":"test","failure_reason":"script_failure","web_url":"https://gitlab.com/group/sub/repo/-/jobs/70","allow_failure":false}]`})
	s, err := g.MergeState(context.Background(), "cinderline/EMBER-1")
	want := PullState{State: MergeFailed, Head: "abc", Failed: []Check{
		{Name: "test", Description: "script_failure", URL: "https://gitlab.com/group/sub/repo/-/jobs/70", job: "70"},
		{Name: "jenkins", Description: "Build #12 failed: 3 tests", URL: "https://ci.example.com/job/12"},
		{Name: "sonar"},
	}}
	if err != nil || !reflect.DeepEqual(s, want) {
		t.Errorf("MergeState = %+v, %v; want %+v", s, err, want)
	}
	g, reqs := recordedGitLab(t, "https://gitlab.com/group/sub/repo.git")
	if log, err := Log(context.Background(), g, want.Failed[1]); err != nil || !strings.Contains(log, "with the CI that reported it") || !strings.HasSuffix(log, "Build #12 failed: 3 tests") || len(*reqs) != 0 {
		t.Errorf("Log of an external status = %q, %v after %d requests", log, err, len(*reqs))
	}

	// A job whose trace is empty, or cannot be read twice, gets its failure reason.
	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{200, ""})
	if log, err := Log(context.Background(), g, want.Failed[0]); err != nil || !strings.HasSuffix(log, "script_failure") {
		t.Errorf("Log of an empty trace = %q, %v", log, err)
	}
	g, _ = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git", reply{403, `{"message":"403 Forbidden"}`}, reply{403, `{"message":"403 Forbidden"}`})
	if log, err := Log(context.Background(), g, want.Failed[0]); status(err) != 403 || log != "Reading this check's log failed: GitLab answered 403: 403 Forbidden\n\nscript_failure" {
		t.Errorf("Log of a trace read twice refused = %q, %v", log, err)
	}

	for _, tc := range []struct {
		pipeline string
		want     Check
	}{
		{`{"id":9,"status":"failed","web_url":"https://gitlab.com/group/sub/repo/-/pipelines/9","yaml_errors":"jobs:test config contains unknown keys: scrpt"}`,
			Check{Name: "pipeline", Description: "pipeline failed", URL: "https://gitlab.com/group/sub/repo/-/pipelines/9",
				text: "The head pipeline's status is failed without a failed job.\n\nYAML errors: jobs:test config contains unknown keys: scrpt"}},
		{`{"id":9,"status":"canceled","web_url":"w"}`,
			Check{Name: "pipeline", Description: "pipeline canceled", URL: "w", text: "The head pipeline's status is canceled without a failed job."}},
	} {
		g, reqs = recordedGitLab(t, "https://gitlab.com/group/sub/repo.git",
			reply{200, `[{"iid":5,"state":"opened"}]`}, reply{200, `{"sha":"abc","head_pipeline":` + tc.pipeline + `}`},
			reply{200, `[{"id":70,"name":"test","status":"failed","allow_failure":true,"target_url":"https://gitlab.com/group/sub/repo/-/jobs/70"}]`},
			reply{200, `[{"id":70,"name":"test","failure_reason":"script_failure","allow_failure":true}]`})
		s, err := g.MergeState(context.Background(), "cinderline/EMBER-1")
		want := PullState{State: MergeFailed, Head: "abc", Failed: []Check{tc.want}}
		if err != nil || !reflect.DeepEqual(s, want) {
			t.Errorf("MergeState without failed jobs = %+v, %v; want %+v", s, err, want)
		}
		if len(*reqs) != 4 || !strings.HasPrefix(urls(*reqs)[3], "GET "+project+"/pipelines/9/jobs") {
			t.Errorf("requests = %q", urls(*reqs))
		}
	}
}
