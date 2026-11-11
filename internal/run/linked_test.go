package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// ember is project B, linked from CSR (A), added on this runner under the
// same Cinderline URL.
var ember = config.Project{URL: cfg.URL + "/", Key: "EMBER", Repository: "https://git.example/o/app",
	ExtraRepositories: []config.ExtraRepository{{URL: "https://git.example/o/docs", Ref: "v2"}, {URL: "https://git.example/o/bad"}}}

// emberLink is the project response of CSR with its link to EMBER.
const emberLink = `{"key":"CSR","name":"CSR","repository_url":"https://git.example/o/r","forge":"forgejo","base_branch":"main","environment":"default",
	"linked_projects":[{"key":"EMBER","name":"Ember\napp","repository_url":"https://git.example/o/app","base_branch":"develop"}]}`

// fetched is one fetchExtras call: its path, its repositories, and how many
// agent turns had started before it.
type fetched struct {
	path  string
	repos []workspace.Extra
	turns int
}

// linkedRun is a solo run of CSR-7 whose project response is projectJSON,
// with every fetch recorded; a repository named in fail fails to fetch.
func linkedRun(t *testing.T, projectJSON string, runs int, fail ...string) (*Project, *client.Fake, *fakeAPI, *[]fetched) {
	t.Helper()
	p := project(t)
	for k, v := range map[secrets.Kind]string{secrets.AnthropicCredential: "sk-ant", secrets.GitToken: "tok-csr"} {
		if _, err := p.Secrets.Set(cfg, k, v); err != nil {
			t.Fatal(err)
		}
	}
	cl := &client.Fake{}
	ws := &workspace.Fake{}
	for range runs {
		cl.Runs = append(cl.Runs, client.RunReply{Result: client.Result{SessionID: "s1", Report: "Done."}})
		ws.Prepares = append(ws.Prepares, workspace.PrepareReply{Dir: t.TempDir()})
		ws.Commits = append(ws.Commits, workspace.CommitReply{})
		ws.MergeBases = append(ws.MergeBases, workspace.MergeBaseReply{})
		ws.FastForwards = append(ws.FastForwards, nil)
	}
	p.Clients = map[string]client.Client{"claude": cl}
	p.Workspace = ws
	p.Forge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	var calls []fetched
	p.fetchExtras = func(_ context.Context, path string, repos []workspace.Extra) map[string]error {
		calls = append(calls, fetched{path, repos, len(cl.Calls())})
		failed := map[string]error{}
		for _, r := range repos {
			if slices.Contains(fail, r.Name) {
				failed[r.Name] = errors.New("fetch failed")
			}
		}
		return failed
	}
	api := soloRunAPI()
	api.routes["GET /project"] = reply{200, projectJSON}
	return &p, cl, api, &calls
}

func runCSR7(t *testing.T, p *Project, api *fakeAPI) {
	t.Helper()
	h := p.Hooks()
	if err := h.Run(context.Background(), api.client(t), cinderline.Issue{Ref: "CSR-7", Team: &cinderline.IssueTeam{ID: 1}}); err != nil {
		t.Fatal(err)
	}
	p.takeHandoff()
}

func progressLines(api *fakeAPI) string {
	var out []string
	for _, w := range api.writes() {
		if strings.HasPrefix(w, "POST /issues/CSR-7/progress") {
			out = append(out, w)
		}
	}
	return strings.Join(out, "\n")
}

// A run of CSR with a link to EMBER, added here, fetches EMBER's repository
// at its base branch with EMBER's git token, and EMBER's extra repositories
// with their own tokens; never a token of another URL or project. One that
// fails is left out and reported. The prompt names what it can read (CSR-161).
func TestALinkedProjectsRepositoriesAreFetchedAndNamed(t *testing.T) {
	p, cl, api, calls := linkedRun(t, emberLink, 1, "EMBER/bad")
	// Same key on another Cinderline: not the linked project.
	other := config.Project{URL: "https://other.example", Key: "EMBER", Repository: "https://git.example/o/wrong"}
	p.Others = []config.Project{cfg, other, ember}
	for _, s := range []struct {
		p config.Project
		k secrets.Kind
		v string
	}{
		{ember, secrets.GitToken, "tok-app"},
		{ember, secrets.ExtraGitToken("https://git.example/o/docs"), "tok-docs"},
		{ember, secrets.ExtraGitToken("https://git.example/o/bad"), "tok-bad"},
		{other, secrets.GitToken, "tok-wrong"},
	} {
		if _, err := p.Secrets.Set(s.p, s.k, s.v); err != nil {
			t.Fatal(err)
		}
	}

	runCSR7(t, p, api)

	if len(*calls) != 1 || (*calls)[0].path != workspace.LinkedPath {
		t.Fatalf("fetches = %+v, want one of the linked volume", *calls)
	}
	want := []workspace.Extra{
		{Name: "EMBER/app", URL: "https://git.example/o/app", Ref: "develop", Token: "tok-app"},
		{Name: "EMBER/docs", URL: "https://git.example/o/docs", Ref: "v2", Token: "tok-docs"},
		{Name: "EMBER/bad", URL: "https://git.example/o/bad", Token: "tok-bad"},
	}
	if got := (*calls)[0].repos; !slices.Equal(got, want) {
		t.Errorf("fetched %+v\nwant %+v", got, want)
	}
	prompt := turns(cl)[0].Prompt
	line := "- EMBER (Ember app): its repositories, read-only: `/linked/EMBER/app`, `/linked/EMBER/docs`\n"
	if !strings.Contains(prompt, line) || !strings.Contains(prompt, "every change to code goes into this run's own repository") ||
		strings.Index(prompt, line) > strings.Index(prompt, "## Issue") {
		t.Errorf("prompt:\n%s", prompt)
	}
	if pr := progressLines(api); !strings.Contains(pr, "https://git.example/o/bad of linked project EMBER not fetched") {
		t.Errorf("progress:\n%s", pr)
	}
}

// A link goes one way: with only the link CSR to EMBER, a run of EMBER gets
// nothing of CSR. A project response without linked_projects (an app from
// before links) is one with an empty list. The linked volume is still
// pruned, so a removed link's code is gone.
func TestWithoutLinksARunGetsNoLinkedCode(t *testing.T) {
	for name, body := range map[string]string{
		"no field":   strings.Replace(emberLink, `"linked_projects":[{"key":"EMBER","name":"Ember\napp","repository_url":"https://git.example/o/app","base_branch":"develop"}]`, `"x":1`, 1),
		"empty list": strings.Replace(emberLink, `{"key":"EMBER","name":"Ember\napp","repository_url":"https://git.example/o/app","base_branch":"develop"}`, ``, 1),
	} {
		t.Run(name, func(t *testing.T) {
			p, cl, api, calls := linkedRun(t, body, 1)
			p.Others = []config.Project{cfg, ember}
			runCSR7(t, p, api)
			if len(*calls) != 1 || (*calls)[0].path != workspace.LinkedPath || len((*calls)[0].repos) != 0 {
				t.Errorf("fetches = %+v, want the linked volume pruned to nothing", *calls)
			}
			if prompt := turns(cl)[0].Prompt; strings.Contains(prompt, "Linked projects") || strings.Contains(prompt, "/linked") {
				t.Errorf("prompt:\n%s", prompt)
			}
		})
	}
}

// A linked project not added on this runner gives a progress line that names
// it and the add command, no paths in the prompt, and the run goes on.
func TestALinkedProjectNotOnThisRunner(t *testing.T) {
	p, cl, api, calls := linkedRun(t, emberLink, 1)
	p.Others = []config.Project{cfg}
	runCSR7(t, p, api)
	if pr := progressLines(api); !strings.Contains(pr, "Linked project EMBER is not added on this runner") || !strings.Contains(pr, "cinderline add") {
		t.Errorf("progress:\n%s", pr)
	}
	if len(*calls) != 1 || len((*calls)[0].repos) != 0 {
		t.Errorf("fetches = %+v", *calls)
	}
	prompt := turns(cl)[0].Prompt
	if !strings.Contains(prompt, "- EMBER (Ember app): its code is not available in this run\n") || strings.Contains(prompt, "/linked/") {
		t.Errorf("prompt:\n%s", prompt)
	}
}

// A follow-up on a kept session and a resume after an answered question
// fetch the linked repositories again before the agent starts, and their
// prompts name the links (CSR-161).
func TestAResumeAndAFollowUpFetchAgain(t *testing.T) {
	p, cl, api, calls := linkedRun(t, emberLink, 3)
	p.Others = []config.Project{cfg, ember}
	if _, err := p.Secrets.Set(ember, secrets.GitToken, "tok-app"); err != nil {
		t.Fatal(err)
	}
	runCSR7(t, p, api) // fresh
	runCSR7(t, p, api) // follow-up on the kept session
	n, ok := p.readPaused("CSR-7")
	if !ok {
		t.Fatal("no kept session")
	}
	// As a run paused for a person's answer leaves it.
	if err := p.SavePaused("CSR-7", Paused{Dir: n.Dir, Session: n.Session, Client: n.Client}); err != nil {
		t.Fatal(err)
	}
	runCSR7(t, p, api)

	if len(*calls) != 3 {
		t.Fatalf("%d fetches, want 3", len(*calls))
	}
	for i, f := range *calls {
		if f.turns != i || len(f.repos) != 1 {
			t.Errorf("fetch %d after %d turns with %+v; want after %d", i, f.turns, f.repos, i)
		}
	}
	section := LinkedSection([]Linked{{Key: "EMBER", Name: "Ember app", Paths: []string{"EMBER/app"}}})
	if got := turns(cl)[1].Prompt; got != FollowUpPrompt("CSR-7")+section {
		t.Errorf("follow-up prompt:\n%s", got)
	}
	if got := turns(cl)[2].Prompt; got != ResumePrompt("CSR-7")+section {
		t.Errorf("resume prompt:\n%s", got)
	}
}
