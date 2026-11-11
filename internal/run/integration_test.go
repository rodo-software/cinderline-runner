//go:build integration

package run

// The Linux integration test of plan M4: one issue end to end against real
// Docker, a throwaway Forgejo and an httptest Cinderline, with a scripted
// agent in place of the model. It needs Docker and the network (the tools
// volume downloads the pinned CLIs), so it sits behind the integration tag:
//
//	go test -race -tags integration -run Integration -timeout 30m ./internal/run/
//
// See docs/ci/integration.yml for the CI job.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// forgejoImage is the throwaway Forgejo, pinned by digest (17.0.1,
// multi-arch index).
const forgejoImage = "codeberg.org/forgejo/forgejo:17.0.1@sha256:fcd19ff4ebc85fb098a67e8e1d0d5a84d07dd6e8a23edcfdfd5b71a6178953be"

const itCredential = "sk-ant-api-integration"

// scripted is the default Environment with the agent's command swapped for
// agentScript, so everything around it (image, network, MySQL, mounts,
// environment, the Claude Client's argv and parsing) is the real thing.
type scripted struct {
	*environment.Docker
	specs []environment.Spec
}

func (s *scripted) Run(ctx context.Context, spec environment.Spec) (string, error) {
	s.specs = append(s.specs, spec)
	spec.Cmd = []string{"sh", "-c", agentScript(spec.Issue), "sh"}
	return s.Docker.Run(ctx, spec)
}

// agentScript checks what a real agent relies on, then does the work: the
// tools volume's Claude Code runs on the image's libc, the prompt arrives on
// stdin, the credential arrives by name, MySQL is on the private network and
// the clone is on the issue's branch. It ends with Claude Code's JSON result.
func agentScript(ref string) string {
	return `set -eu
` + tools.Path + `/bin/claude --version
grep '# Your task: ` + ref + `' >/dev/null
test "$ANTHROPIC_API_KEY" = ` + itCredential + `
getent hosts mysql
test "$(git rev-parse --abbrev-ref HEAD)" = ` + name.Name + `/` + ref + `
echo 'written by the scripted agent' > hello.txt
echo '{"type":"result","subtype":"success","result":"Added hello.txt."}'`
}

func TestIntegrationOneIssueToForgejo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(t.Output(), nil))
	project := "IT" + strconv.Itoa(os.Getpid()) // runs on one Docker never share names
	ref := project + "-1"

	// Everything this test creates carries the project's label, also
	// Forgejo, so the Environment's own Cleanup removes it.
	env := &scripted{Docker: &environment.Docker{Project: project, Log: log}}
	label := "label=" + workspace.ProjectLabel(project)
	cleanup := func() {
		ctx := context.WithoutCancel(ctx)
		if err := env.Cleanup(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if vols := docker(t, ctx, "volume", "ls", "--quiet", "--filter", label); vols != "" {
			docker(t, ctx, append([]string{"volume", "rm", "--force"}, strings.Fields(vols)...)...)
		}
	}
	t.Cleanup(cleanup)

	forgejoURL, token := startForgejo(t, ctx, project)
	repo := forgejoURL + "/it/repo"
	createRepo(t, ctx, forgejoURL, token)

	// The Forgejo Forge takes https only (its token must not travel in
	// clear); the throwaway one speaks http, so its requests are sent there.
	fg, err := forge.NewForgejo(strings.Replace(repo, "http://", "https://", 1), token)
	if err != nil {
		t.Fatal(err)
	}
	fg.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: toHTTP{}}

	status := "in_progress"
	api := &fakeAPI{handle: func(key, body string) (reply, bool) {
		switch key {
		case "GET /project":
			return reply{200, `{"key":"` + project + `","repository_url":"` + repo + `","forge":"forgejo","base_branch":""}`}, true
		case "GET /issues/" + ref:
			return reply{200, issueJSON(ref, status)}, true
		case "GET /issues/" + ref + "/comments":
			return reply{200, `{"comments":[]}`}, true
		case "POST /issues/" + ref + "/comments":
			return reply{201, `{"id":1,"body":"x","author":{"name":"runner"}}`}, true
		case "POST /issues/" + ref + "/progress":
			return reply{204, ""}, true
		case "PATCH /issues/" + ref:
			var r cinderline.UpdateIssueRequest
			_ = json.Unmarshal([]byte(body), &r)
			if r.Status != "" {
				status = r.Status
			}
			return reply{200, issueJSON(ref, status)}, true
		}
		return reply{}, false
	}}
	c := api.client(t)

	p := Project{Config: config.Project{URL: "http://cinderline.invalid", Key: project, Repository: repo}, State: t.TempDir(), Env: env, Log: log}
	ws := &workspace.Docker{Project: project, Repository: repo + ".git", Token: token}
	team := cinderline.Team{ID: 1, Name: "Dev", MaxMinutes: 10, Agents: []cinderline.TeamAgent{{Name: "Coder", Role: "coder", Client: "claude"}}}
	branch := name.Name + "/" + ref

	base, err := BaseBranch(ctx, c, fg)
	if err != nil || base != "main" {
		t.Fatalf("BaseBranch = %q, %v; want main", base, err)
	}
	dir, err := ws.Prepare(ctx, branch, base)
	if err != nil {
		t.Fatal(err)
	}
	image, err := env.Image(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := (&tools.Tools{State: p.State}).Mount(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	cl := &client.Claude{Project: project, Env: env, Credential: itCredential, MCPURL: "http://host.docker.internal/mcp", Tools: mount}

	out, err := p.Attempts(ctx, c, cl, Job{Ref: ref, Client: "claude", Turn: client.Turn{
		Dir: dir, Prompt: CodingPrompt(team, ref, "Add hello.txt", "Add a file.", branch, project, nil, nil), MaxTurns: 10, MaxMinutes: team.MaxMinutes,
	}})
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if out.Report != "Added hello.txt." || out.Paused {
		t.Fatalf("Attempts = %+v, want the scripted agent's report", out)
	}
	if got := env.specs[0].Cmd[0]; got != tools.Path+"/bin/claude" {
		t.Errorf("the Claude Client ran %q, want the tools volume's claude", got)
	}

	if err := p.Publish(ctx, c, map[string]client.Client{"claude": cl}, ws, fg, Pub{
		Ref: ref, Title: "Add hello.txt", Body: "Add a file.", Base: base, Dir: dir, MaxTurns: 10, Team: team, Forge: "forgejo",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	pr, found, err := fg.FindPullRequest(ctx, branch)
	if err != nil || !found {
		t.Fatalf("FindPullRequest = %+v, %v, %v; want the run's pull request", pr, found, err)
	}
	if got := get(t, ctx, forgejoURL+"/api/v1/repos/it/repo/raw/hello.txt?ref="+branch, token); got != "written by the scripted agent\n" {
		t.Errorf("hello.txt on %s = %q", branch, got)
	}
	api.mu.Lock()
	final := status
	api.mu.Unlock()
	if final != "review" {
		t.Errorf("issue status = %q, want review", final)
	}
	var report string
	for _, w := range api.writes() {
		if strings.HasPrefix(w, "POST /issues/"+ref+"/comments") {
			report = w
		}
	}
	if !strings.Contains(report, pr.URL) {
		t.Errorf("finish report %q lacks the pull request %s", report, pr.URL)
	}

	// Review item 9: nothing labelled is left behind.
	cleanup()
	for _, kind := range []string{"container", "network", "volume"} {
		args := []string{kind, "ls", "--quiet", "--filter", label}
		if kind == "container" {
			args = append(args, "--all")
		}
		if left := docker(t, ctx, args...); left != "" {
			t.Errorf("%ss left after cleanup: %s", kind, left)
		}
	}
}

// startForgejo starts a Forgejo on Docker's default bridge with an admin
// "it", and returns its URL, at the container's address so the test and the
// git helper containers both reach it, and the admin's token.
func startForgejo(t *testing.T, ctx context.Context, project string) (string, string) {
	t.Helper()
	ctr := name.Name + "-" + project + "-forgejo"
	docker(t, ctx, append(append([]string{"run", "--detach"}, workspace.Labels(project, "")...), "--name", ctr,
		"-e", "FORGEJO__security__INSTALL_LOCK=true",
		"-e", "FORGEJO__database__DB_TYPE=sqlite3",
		"-e", "FORGEJO__server__OFFLINE_MODE=true",
		forgejoImage)...)
	ip := docker(t, ctx, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", ctr)
	u := "http://" + ip + ":3000"
	for deadline := time.Now().Add(2 * time.Minute); ; {
		resp, err := http.Get(u + "/api/v1/version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Forgejo at %s not up: %v\n%s", u, err, docker(t, ctx, "logs", "--tail", "50", ctr))
		}
		time.Sleep(time.Second)
	}
	docker(t, ctx, "exec", "--user", "git", ctr, "forgejo", "admin", "user", "create", "--admin",
		"--username", "it", "--password", "integration-test", "--email", "it@example.invalid", "--must-change-password=false")
	token := docker(t, ctx, "exec", "--user", "git", ctr, "forgejo", "admin", "user", "generate-access-token",
		"--username", "it", "--token-name", "it", "--scopes", "all", "--raw")
	return u, token
}

// createRepo makes it/repo with one commit on main.
func createRepo(t *testing.T, ctx context.Context, forgejoURL, token string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, forgejoURL+"/api/v1/user/repos",
		strings.NewReader(`{"name":"repo","auto_init":true,"readme":"Default","default_branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create repository: %s %s", resp.Status, body)
	}
}

// get returns the body of a 200 answer to an authorised GET.
func get(t *testing.T, ctx context.Context, u, token string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s %s", u, resp.Status, body)
	}
	return string(body)
}

// docker runs the docker command and returns its trimmed output.
func docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := workspace.RunCommand(ctx, nil, append([]string{"docker"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// toHTTP sends the Forge's https requests to the throwaway Forgejo's http.
type toHTTP struct{}

func (toHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = "http"
	return http.DefaultTransport.RoundTrip(r)
}
