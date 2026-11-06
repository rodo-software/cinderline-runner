package workspace

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const token = "s3cret-token"

// execCall is one recorded call through the Exec seam.
type execCall struct {
	Env  []string
	Argv []string
}

// recorder returns a Docker whose Exec records every call and answers with
// the errors in fail, by argv index of the call (nil when absent).
func recorder(fail map[int]error) (*Docker, *[]execCall) {
	var calls []execCall
	d := &Docker{
		Project:    "CSR",
		Repository: "https://git.example.com/o/r.git",
		Token:      token,
		Exec: func(_ context.Context, env []string, argv ...string) (string, error) {
			calls = append(calls, execCall{env, argv})
			return "", fail[len(calls)-1]
		},
	}
	return d, &calls
}

// helperArgv is the argv of a helper container running git args.
func helperArgv(labels, env, mounts []string, git ...string) []string {
	argv := []string{"docker", "run", "--rm"}
	argv = append(argv, labels...)
	argv = append(argv, env...)
	argv = append(argv, mounts...)
	argv = append(argv, "--entrypoint", "sh", GitImage, "-c", askpassScript, "sh", "git")
	return append(argv, git...)
}

var (
	projectLabel = []string{"--label", "cinderstack.project=CSR"}
	issueLabels  = []string{"--label", "cinderstack.project=CSR", "--label", "cinderstack.issue=CSR-29"}
	tokenEnv     = []string{"-e", "GIT_TOKEN", "-e", "GIT_ASKPASS=/tmp/askpass", "-e", "GIT_TERMINAL_PROMPT=0"}
	noTokenEnv   = []string{"-e", "GIT_TERMINAL_PROMPT=0"}
	repoMount    = []string{"-v", "cinderstack-CSR-repo:/repo"}
	runsMount    = []string{"-v", "cinderstack-CSR-repo:/repo", "-v", "cinderstack-CSR-runs:/runs:ro"}
	tokenVar     = []string{"GIT_TOKEN=" + token}
)

func TestDockerFetch(t *testing.T) {
	d, calls := recorder(nil)
	if err := d.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		{nil, []string{"docker", "volume", "create", "--label", "cinderstack.project=CSR", "cinderstack-CSR-repo"}},
		{nil, helperArgv(projectLabel, noTokenEnv, repoMount, "init", "--quiet", "--bare", "/repo")},
		{tokenVar, helperArgv(projectLabel, tokenEnv, repoMount,
			"-C", "/repo", "fetch", "--prune", "--quiet", "--no-tags", "https://git.example.com/o/r.git", "+refs/heads/*:refs/heads/*")},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
	tokenOnlyInEnv(t, *calls)
}

func TestDockerPush(t *testing.T) {
	d, calls := recorder(nil)
	if err := d.Push(context.Background(), "/runs/CSR-29", "cinderstack/CSR-29"); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		// The agent's clone is read only and the fetch from it has no token.
		{nil, helperArgv(issueLabels, noTokenEnv, runsMount,
			"-c", "safe.directory=/runs/CSR-29", "-C", "/repo", "fetch", "--quiet", "--no-tags",
			"/runs/CSR-29", "+refs/heads/cinderstack/CSR-29:refs/push/cinderstack/CSR-29")},
		{tokenVar, helperArgv(issueLabels, tokenEnv, repoMount,
			"-C", "/repo", "push", "--quiet", "https://git.example.com/o/r.git", "refs/push/cinderstack/CSR-29:refs/heads/cinderstack/CSR-29")},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
	tokenOnlyInEnv(t, *calls)
}

func TestDockerPushRefusesDirOutsideRuns(t *testing.T) {
	for _, dir := range []string{"/repo", "/runs", "/runs/", "/runs/../repo", "--upload-pack=x", "runs/CSR-29"} {
		d, calls := recorder(nil)
		if err := d.Push(context.Background(), dir, "cinderstack/CSR-29"); err == nil {
			t.Errorf("Push(%q) = nil, want an error", dir)
		}
		if len(*calls) != 0 {
			t.Errorf("Push(%q) ran %q", dir, *calls)
		}
	}
}

func TestDockerStopsAtFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	d, calls := recorder(map[int]error{0: boom})
	if err := d.Push(context.Background(), "/runs/CSR-29", "cinderstack/CSR-29"); !errors.Is(err, boom) {
		t.Errorf("Push = %v, want boom", err)
	}
	if len(*calls) != 1 {
		t.Errorf("ran %d commands after a failed fetch, want 1", len(*calls))
	}

	d, calls = recorder(map[int]error{1: boom})
	if err := d.Fetch(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Fetch = %v, want boom", err)
	}
	if len(*calls) != 2 {
		t.Errorf("ran %d commands after a failed init, want 2", len(*calls))
	}
}

func TestAskpassScript(t *testing.T) {
	// The token is read from the environment at run time, never written into the script.
	for _, s := range []string{"x-token-auth", `"$GIT_TOKEN"`, `exec "$@"`} {
		if !strings.Contains(askpassScript, s) {
			t.Errorf("askpassScript lacks %q:\n%s", s, askpassScript)
		}
	}
	if !strings.Contains(GitImage, "@sha256:") {
		t.Errorf("GitImage %q is not pinned by digest", GitImage)
	}
}

func TestRunCommand(t *testing.T) {
	out, err := RunCommand(context.Background(), []string{"GOFLAGS=-count=1"}, "go", "env", "GOFLAGS")
	if err != nil || strings.TrimSpace(out) != "-count=1" {
		t.Fatalf("RunCommand = %q, %v; want the extra environment passed", out, err)
	}
	_, err = RunCommand(context.Background(), nil, "go", "no-such-command")
	if err == nil || !strings.Contains(err.Error(), "go no-such-command") || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("RunCommand error = %v, want the argv and the output's tail", err)
	}
}

// tokenOnlyInEnv fails when the token shows up anywhere in an argv.
func tokenOnlyInEnv(t *testing.T, calls []execCall) {
	t.Helper()
	for _, c := range calls {
		if strings.Contains(strings.Join(c.Argv, " "), token) {
			t.Errorf("token in argv %q", c.Argv)
		}
	}
}
