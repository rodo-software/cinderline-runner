package forge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Acceptance tests of CSR-47: each forge's CI secrets check against recorded
// answers, for safe, exposed and unknown.

// routes is an HTTP client answering GET requests by path (decoded or
// escaped, query ignored); any other request fails the test.
func routes(t *testing.T, replies map[string]reply) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if r.Header.Get("Authorization") == "" && r.Header.Get("PRIVATE-TOKEN") == "" { // GitLab uses PRIVATE-TOKEN
			t.Errorf("%s %s without Authorization", r.Method, r.URL)
		}
		rep, ok := replies[r.URL.Path]
		if !ok {
			rep, ok = replies[r.URL.EscapedPath()]
		}
		if r.Method != http.MethodGet || !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
			return w.Result(), nil
		}
		w.WriteHeader(rep.status)
		io.WriteString(w, rep.body)
		return w.Result(), nil
	})}
}

func checkSafe(t *testing.T, f Forge) {
	t.Helper()
	exposed, err := f.CISecrets(context.Background())
	if err != nil || len(exposed) != 0 {
		t.Fatalf("CISecrets = %q, %v; want none, nil", exposed, err)
	}
}

func checkExposed(t *testing.T, f Forge, want ...string) {
	t.Helper()
	exposed, err := f.CISecrets(context.Background())
	if err != nil {
		t.Fatalf("CISecrets error %v", err)
	}
	slices.Sort(exposed)
	slices.Sort(want)
	if !reflect.DeepEqual(exposed, want) {
		t.Fatalf("CISecrets = %q, want %q", exposed, want)
	}
}

func checkUnknown(t *testing.T, f Forge, wantInErr string) {
	t.Helper()
	exposed, err := f.CISecrets(context.Background())
	if err == nil {
		t.Fatalf("CISecrets = %q, nil; want an error", exposed)
	}
	if len(exposed) != 0 {
		t.Errorf("CISecrets returned %q with its error", exposed)
	}
	if !strings.Contains(err.Error(), wantInErr) {
		t.Errorf("error %q lacks %q", err, wantInErr)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error %q contains the token", err)
	}
}

// GitHub: repository and organization secrets reach every branch; an
// environment's secrets do unless it limits deployment branches or needs a
// reviewer.

const ghRepo = "/repos/o/r"

func githubCI(t *testing.T, replies map[string]reply) *GitHub {
	t.Helper()
	g, err := NewGitHub("https://github.com/o/r", testToken)
	if err != nil {
		t.Fatal(err)
	}
	g.HTTP = routes(t, replies)
	return g
}

const ghEnvironments = `{"total_count":3,"environments":[
 {"name":"production","deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false},"protection_rules":[]},
 {"name":"review","deployment_branch_policy":null,"protection_rules":[{"id":1,"type":"required_reviewers"}]},
 {"name":"staging","deployment_branch_policy":null,"protection_rules":[{"id":2,"type":"wait_timer"}]}]}`

func TestGitHubCISecretsSafe(t *testing.T) {
	checkSafe(t, githubCI(t, map[string]reply{
		ghRepo + "/actions/secrets":              {200, `{"total_count":0,"secrets":[]}`},
		ghRepo + "/actions/organization-secrets": {200, `{"total_count":0,"secrets":[]}`},
		ghRepo + "/environments":                 {200, ghEnvironments},
		ghRepo + "/environments/staging/secrets": {200, `{"total_count":0,"secrets":[]}`},
	}))
}

func TestGitHubCISecretsExposed(t *testing.T) {
	checkExposed(t, githubCI(t, map[string]reply{
		ghRepo + "/actions/secrets":              {200, `{"total_count":1,"secrets":[{"name":"DEPLOY_KEY"}]}`},
		ghRepo + "/actions/organization-secrets": {200, `{"total_count":1,"secrets":[{"name":"NPM_TOKEN"}]}`},
		ghRepo + "/environments":                 {200, ghEnvironments},
		ghRepo + "/environments/staging/secrets": {200, `{"total_count":1,"secrets":[{"name":"DB_PASSWORD"}]}`},
	}), "repository secret DEPLOY_KEY", "organization secret NPM_TOKEN", "environment staging secret DB_PASSWORD")
}

func TestGitHubCISecretsUnknown(t *testing.T) {
	checkUnknown(t, githubCI(t, map[string]reply{
		ghRepo + "/actions/secrets":              {403, `{"message":"Resource not accessible by personal access token"}`},
		ghRepo + "/actions/organization-secrets": {403, `{"message":"Resource not accessible by personal access token"}`},
		ghRepo + "/environments":                 {403, `{"message":"Resource not accessible by personal access token"}`},
	}), "403")
}

// GitLab: an unprotected project or group variable reaches every branch.

const glProject = "/api/v4/projects/grp%2Fapp"

func gitlabCI(t *testing.T, replies map[string]reply) *GitLab {
	t.Helper()
	g, err := NewGitLab("https://gitlab.example.com/grp/app", testToken)
	if err != nil {
		t.Fatal(err)
	}
	g.HTTP = routes(t, replies)
	return g
}

func TestGitLabCISecretsSafe(t *testing.T) {
	checkSafe(t, gitlabCI(t, map[string]reply{
		glProject + "/variables":     {200, `[{"key":"DEPLOY_KEY","protected":true,"environment_scope":"*"}]`},
		glProject:                    {200, `{"id":3,"namespace":{"id":7,"kind":"group","full_path":"grp"}}`},
		"/api/v4/groups/7/variables": {200, `[{"key":"NPM_TOKEN","protected":true,"environment_scope":"*"}]`},
	}))
}

func TestGitLabCISecretsUserNamespaceSkipsGroup(t *testing.T) {
	checkSafe(t, gitlabCI(t, map[string]reply{
		glProject + "/variables": {200, `[]`},
		glProject:                {200, `{"id":3,"namespace":{"id":9,"kind":"user","full_path":"grp"}}`},
	}))
}

func TestGitLabCISecretsExposed(t *testing.T) {
	checkExposed(t, gitlabCI(t, map[string]reply{
		glProject + "/variables":     {200, `[{"key":"DEPLOY_KEY","protected":false,"environment_scope":"*"},{"key":"SAFE","protected":true,"environment_scope":"*"}]`},
		glProject:                    {200, `{"id":3,"namespace":{"id":7,"kind":"group","full_path":"grp"}}`},
		"/api/v4/groups/7/variables": {200, `[{"key":"NPM_TOKEN","protected":false,"environment_scope":"*"}]`},
	}), "project variable DEPLOY_KEY", "group variable NPM_TOKEN")
}

func TestGitLabCISecretsUnknown(t *testing.T) {
	checkUnknown(t, gitlabCI(t, map[string]reply{
		glProject + "/variables":     {200, `[]`},
		glProject:                    {200, `{"id":3,"namespace":{"id":7,"kind":"group","full_path":"grp"}}`},
		"/api/v4/groups/7/variables": {403, `{"message":"403 Forbidden"}`},
	}), "403")
}

// Forgejo and Gitea: Actions secrets have no protection; any repository or
// organization secret reaches every branch.

const fjRepo = "/api/v1/repos/o/r"

func forgejoCI(t *testing.T, replies map[string]reply) *Forgejo {
	t.Helper()
	f, err := NewForgejo("https://code.example.com/o/r", testToken)
	if err != nil {
		t.Fatal(err)
	}
	f.HTTP = routes(t, replies)
	return f
}

func TestForgejoCISecretsSafe(t *testing.T) {
	checkSafe(t, forgejoCI(t, map[string]reply{
		fjRepo + "/actions/secrets":      {200, `[]`},
		"/api/v1/orgs/o/actions/secrets": {200, `[]`},
	}))
}

func TestForgejoCISecretsUserOwnerSkipsOrg(t *testing.T) {
	checkSafe(t, forgejoCI(t, map[string]reply{
		fjRepo + "/actions/secrets":      {200, `[]`},
		"/api/v1/orgs/o/actions/secrets": {404, `{"message":"GetOrgByName"}`},
	}))
}

func TestForgejoCISecretsExposed(t *testing.T) {
	checkExposed(t, forgejoCI(t, map[string]reply{
		fjRepo + "/actions/secrets":      {200, `[{"name":"DEPLOY_KEY","created_at":"2026-01-01T00:00:00Z"}]`},
		"/api/v1/orgs/o/actions/secrets": {200, `[{"name":"NPM_TOKEN","created_at":"2026-01-01T00:00:00Z"}]`},
	}), "repository secret DEPLOY_KEY", "organization secret NPM_TOKEN")
}

// A 403 on the organization call (CSR-147): Forgejo checks the token's
// scopes before it resolves the owner, so a user owner answers 403 to a token
// without read:organization. The repository findings stay, with an error.
func TestForgejoCISecretsOrgForbiddenKeepsRepository(t *testing.T) {
	f := forgejoCI(t, map[string]reply{
		fjRepo + "/actions/secrets":      {200, `[{"name":"DEPLOY_KEY"}]`},
		"/api/v1/orgs/o/actions/secrets": {403, `{"message":"token does not have at least one of required scope(s): [read:organization]"}`},
	})
	exposed, err := f.CISecrets(context.Background())
	if want := []string{"repository secret DEPLOY_KEY"}; !reflect.DeepEqual(exposed, want) {
		t.Errorf("CISecrets = %q, want %q", exposed, want)
	}
	if err == nil || !strings.Contains(err.Error(), "organization secrets not checked") || !strings.Contains(err.Error(), "read:organization") {
		t.Errorf("error %v, want organization secrets not checked for read:organization", err)
	}
}

func TestForgejoCISecretsUnknown(t *testing.T) {
	checkUnknown(t, forgejoCI(t, map[string]reply{
		fjRepo + "/actions/secrets":      {200, `[{"name":"DEPLOY_KEY"}]`},
		"/api/v1/orgs/o/actions/secrets": {500, `{"message":"internal error"}`},
	}), "500")
	checkUnknown(t, forgejoCI(t, map[string]reply{
		fjRepo + "/actions/secrets": {403, `{"message":"token does not have at least one of required scope(s): [read:repository]"}`},
	}), "403")
}

// Bitbucket Cloud: repository and workspace variables reach every branch;
// whether a deployment environment's variables do depends on deployment
// permissions the API does not show.

const bbCIRepo = "/2.0/repositories/ws/repo"

func bitbucketCI(t *testing.T, replies map[string]reply) *Bitbucket {
	t.Helper()
	b, err := NewBitbucket("https://bitbucket.org/ws/repo", testToken)
	if err != nil {
		t.Fatal(err)
	}
	b.HTTP = routes(t, replies)
	return b
}

const bbEnvironments = `{"values":[{"uuid":"{e1}","name":"Production"}]}`

func TestBitbucketCISecretsSafe(t *testing.T) {
	checkSafe(t, bitbucketCI(t, map[string]reply{
		bbCIRepo + "/pipelines_config/variables":                     {200, `{"values":[]}`},
		"/2.0/workspaces/ws/pipelines-config/variables":              {200, `{"values":[]}`},
		bbCIRepo + "/environments":                                   {200, bbEnvironments},
		bbCIRepo + "/deployments_config/environments/{e1}/variables": {200, `{"values":[]}`},
	}))
}

func TestBitbucketCISecretsExposed(t *testing.T) {
	checkExposed(t, bitbucketCI(t, map[string]reply{
		bbCIRepo + "/pipelines_config/variables":                     {200, `{"values":[{"key":"DEPLOY_KEY","secured":true}]}`},
		"/2.0/workspaces/ws/pipelines-config/variables":              {200, `{"values":[{"key":"NPM_TOKEN","secured":true}]}`},
		bbCIRepo + "/environments":                                   {200, bbEnvironments},
		bbCIRepo + "/deployments_config/environments/{e1}/variables": {200, `{"values":[{"key":"DB_PASSWORD","secured":true}]}`},
	}), "repository variable DEPLOY_KEY", "workspace variable NPM_TOKEN")
}

func TestBitbucketCISecretsUnknownDeploymentVariables(t *testing.T) {
	checkUnknown(t, bitbucketCI(t, map[string]reply{
		bbCIRepo + "/pipelines_config/variables":                     {200, `{"values":[]}`},
		"/2.0/workspaces/ws/pipelines-config/variables":              {200, `{"values":[]}`},
		bbCIRepo + "/environments":                                   {200, bbEnvironments},
		bbCIRepo + "/deployments_config/environments/{e1}/variables": {200, `{"values":[{"key":"DB_PASSWORD","secured":true}]}`},
	}), "Production")
}

func TestBitbucketCISecretsUnknownForbidden(t *testing.T) {
	checkUnknown(t, bitbucketCI(t, map[string]reply{
		bbCIRepo + "/pipelines_config/variables":        {200, `{"values":[]}`},
		"/2.0/workspaces/ws/pipelines-config/variables": {403, `{"type":"error","error":{"message":"Access denied"}}`},
		bbCIRepo + "/environments":                      {200, `{"values":[]}`},
	}), "403")
}

// Fake: scripted like the other methods.

func TestFakeCISecrets(t *testing.T) {
	f := &Fake{CISecretsReplies: []CISecretsReply{{Exposed: []string{"repository secret X"}}}}
	checkExposed(t, f, "repository secret X")
	if _, err := f.CISecrets(context.Background()); err == nil {
		t.Error("empty queue gave no error")
	}
	if got := f.Calls(); len(got) != 2 || got[0].Method != "CISecrets" {
		t.Errorf("calls %+v", got)
	}
}

// New picks the forge by the project's kind.

func TestNew(t *testing.T) {
	for kind, repo := range map[string]string{
		"github": "https://github.com/o/r", "gitlab": "https://gitlab.com/g/r",
		"forgejo": "https://code.example.com/o/r", "bitbucket": "https://bitbucket.org/w/r",
	} {
		if f, err := New(kind, repo, testToken); err != nil || f == nil {
			t.Errorf("New(%q) = %v, %v", kind, f, err)
		}
	}
	if _, err := New("svn", "https://x.example.com/o/r", testToken); err == nil {
		t.Error("New accepted an unknown kind")
	}
}
