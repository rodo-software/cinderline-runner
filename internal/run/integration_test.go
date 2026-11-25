//go:build integration

package run

// The Linux integration test of plan M4: one issue end to end against real
// Docker, a throwaway Forgejo and an httptest Cinderline, with a scripted
// agent in place of the model. It needs Docker and the network (the tools
// volume downloads the pinned CLIs), so it sits behind the integration tag:
//
//	go test -race -tags integration -run Integration -timeout 30m ./internal/run/
//
// .forgejo/workflows/integration.yml runs it in CI.

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/rodo-software/cinderline-runner/internal/loopback"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// forgejoImage is the throwaway Forgejo, pinned by digest (17.0.1,
// multi-arch index).
const forgejoImage = "codeberg.org/forgejo/forgejo:17.0.1@sha256:fcd19ff4ebc85fb098a67e8e1d0d5a84d07dd6e8a23edcfdfd5b71a6178953be"

const itCredential = "sk-ant-api-integration"

// scripted is the default Environment with the agent's command swapped for
// agentScript, so everything around it (image, network, mounts,
// environment, the Claude Client's argv and parsing) is the real thing.
type scripted struct {
	*environment.Docker
	specs []environment.Spec
	forge string // the forge's host:port, which the agent must not see
}

func (s *scripted) Run(ctx context.Context, spec environment.Spec) (string, error) {
	s.specs = append(s.specs, spec)
	spec.Cmd = []string{"sh", "-c", agentScript(spec.Issue, s.forge), "sh"}
	return s.Docker.Run(ctx, spec)
}

// agentScript checks what a real agent relies on, then does the work: the
// tools volume's Claude Code runs on the image's libc, the prompt arrives on
// stdin, the credential arrives by name, the clone is on the issue's branch
// and is the only folder of the runs volume it sees (CLR-207), and nothing it
// can read names the forge's host:port, forge (CL decision
// 2026-11-25-forge-off-limits-for-agents). It commits hello.txt with the identity
// the runner set and leaves left.txt for the runner to commit (CSR-169). It
// ends with Claude Code's JSON result.
func agentScript(ref, forge string) string {
	return `set -eu
` + tools.Path + `/bin/claude --version
grep '# Your task: ` + ref + `' >/dev/null
test "$ANTHROPIC_API_KEY" = ` + itCredential + `
test "$(git rev-parse --abbrev-ref HEAD)" = ` + name.Name + `/` + ref + `
test "$(ls -A ` + workspace.RunsPath + `)" = ` + ref + `
if { env; git config --list --show-origin; } | grep -F '` + forge + `'; then exit 1; fi
if grep -rlsF '` + forge + `' ` + workspace.RepoPath + ` .git | grep .; then exit 1; fi
echo 'written by the scripted agent' > hello.txt
git add hello.txt
git commit --quiet -m 'Add hello.txt'
echo 'left for the runner' > left.txt
echo '{"type":"result","subtype":"success","result":"Added hello.txt."}'`
}

func TestIntegrationOneIssueToForgejo(t *testing.T) {
	loopback.Off(t) // the pinned CLIs' downloads and Forgejo on Docker's bridge
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(t.Output(), nil))
	project := "IT" + strconv.Itoa(os.Getpid()) // runs on one Docker never share names
	ref := project + "-1"

	// Everything this test creates carries the project's label, also
	// Forgejo, so the Environment's own Cleanup removes it.
	env := &scripted{Docker: &environment.Docker{Project: project, Log: log, Exec: workspace.RunCommand, Memory: 4 << 30}}
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
	env.forge = strings.TrimPrefix(forgejoURL, "http://")
	repo := forgejoURL + "/it/repo"
	createRepo(t, ctx, forgejoURL, token)

	// The Forgejo Forge takes https only (its token must not travel in
	// clear); the throwaway one speaks http, so its requests are sent there.
	fg, err := forge.NewForgejo(strings.Replace(repo, "http://", "https://", 1), token)
	if err != nil {
		t.Fatal(err)
	}
	fg.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: toHTTP{}}

	p := &Project{Config: config.Project{URL: "http://cinderline.invalid", Key: project, Repository: repo}, State: t.TempDir(), Env: env, Log: log}
	p.Workspace = &workspace.Docker{Project: project, Repository: repo + ".git", Token: token, Exec: workspace.RunCommand}
	p.Forge = func(string) (forge.Forge, error) { return fg, nil }
	image, err := env.Image(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := (&tools.Tools{State: p.State, Exec: workspace.RunCommand}).Mount(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	p.Clients = map[string]client.Client{"claude": &client.CLI{Name: "claude", Project: project, Env: env, Exec: workspace.RunCommand, Credential: itCredential, MCPURL: "http://host.docker.internal/mcp", Tools: mount}}
	branch := name.Name + "/" + ref

	// The prompt is the app's; the scripted agent only checks its first line.
	app := &fakeApp{onNext: stepsOf(
		`{"kind":"run","issue":{"ref":"`+ref+`","title":"Add hello.txt"},"agent":"Coder","session":"new","token":null,"prompt":"# Your task: `+ref+`\nAdd hello.txt."}`,
		`{"kind":"publish"}`,
	)}
	jb := cinderline.Job{
		Issue: cinderline.JobIssue{Ref: ref, Title: "Add hello.txt", Body: "Add a file."},
		Team:  cinderline.Team{Name: "Dev", MaxMinutes: 10, MaxTurns: 10, Agents: []cinderline.TeamAgent{{Name: "Coder", Client: "claude"}}},
	}
	started, err := p.run(ctx, app.client(t), cinderline.Project{Key: project, RepositoryURL: repo, Forge: "forgejo", IdleMinutes: 20}, jb, 0)
	if !started || err != nil {
		t.Fatalf("run = %v, %v", started, err)
	}
	lasts := app.lasts()
	if len(lasts) != 2 || lasts[0].Outcome != "finished" || deref(lasts[0].Report) != "Added hello.txt." {
		t.Fatalf("lasts = %+v; want the scripted agent's finished turn and the publish", lasts)
	}
	if got := env.specs[0].Cmd[0]; got != tools.Path+"/bin/claude" {
		t.Errorf("the Claude Client ran %q, want the tools volume's claude", got)
	}
	// The agent's real command and environment, its MCP config among them.
	if s := fmt.Sprint(env.specs[0]); strings.Contains(s, env.forge) {
		t.Errorf("the agent's spec names the forge %s: %s", env.forge, s)
	}

	pr, found, err := fg.FindPullRequest(ctx, branch)
	if err != nil || !found {
		t.Fatalf("FindPullRequest = %+v, %v, %v; want the run's pull request", pr, found, err)
	}
	if pub := lasts[1]; pub.Outcome != "published" || deref(pub.PullRequest) != pr.URL {
		t.Errorf("publish facts = %+v, want %s", pub, pr.URL)
	}
	if got := get(t, ctx, forgejoURL+"/api/v1/repos/it/repo/raw/hello.txt?ref="+branch, token); got != "written by the scripted agent\n" {
		t.Errorf("hello.txt on %s = %q", branch, got)
	}
	// The agent's commit and the runner's, and nothing else, carry the
	// run's one identity as author and committer.
	var commits []struct {
		Commit struct{ Author, Committer struct{ Name, Email string } }
	}
	if err := json.Unmarshal([]byte(get(t, ctx, forgejoURL+"/api/v1/repos/it/repo/commits?sha="+branch+"&limit=2&stat=false&files=false", token)), &commits); err != nil || len(commits) != 2 {
		t.Fatalf("commits on %s = %+v, %v; want the runner's and the agent's", branch, commits, err)
	}
	who := struct{ Name, Email string }{"Dev / Coder", workspace.Email}
	for i, c := range commits {
		if c.Commit.Author != who || c.Commit.Committer != who {
			t.Errorf("commit %d on %s by %+v, committed by %+v; want %+v", i, branch, c.Commit.Author, c.Commit.Committer, who)
		}
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
