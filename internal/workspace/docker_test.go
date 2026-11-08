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
	projectLabel = []string{"--label", "cinderline.project=CSR"}
	issueLabels  = []string{"--label", "cinderline.project=CSR", "--label", "cinderline.issue=CSR-29"}
	tokenEnv     = []string{"-e", "GIT_TOKEN", "-e", "GIT_ASKPASS=/tmp/askpass", "-e", "GIT_TERMINAL_PROMPT=0"}
	noTokenEnv   = []string{"-e", "GIT_TERMINAL_PROMPT=0"}
	repoMount    = []string{"-v", "cinderline-CSR-repo:/repo"}
	runsMount    = []string{"-v", "cinderline-CSR-repo:/repo", "-v", "cinderline-CSR-runs:/runs:ro"}
	tokenVar     = []string{"GIT_TOKEN=" + token}
)

func TestDockerFetch(t *testing.T) {
	d, calls := recorder(nil)
	if err := d.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		{nil, []string{"docker", "volume", "create", "--label", "cinderline.project=CSR", "cinderline-CSR-repo"}},
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
	if err := d.Push(context.Background(), "/runs/CSR-29", "cinderline/CSR-29"); err != nil {
		t.Fatal(err)
	}
	want := []execCall{
		// The agent's clone is read only and the fetch from it has no token.
		{nil, helperArgv(issueLabels, noTokenEnv, runsMount,
			"-c", "safe.directory=/runs/CSR-29", "-C", "/repo", "fetch", "--quiet", "--no-tags",
			"/runs/CSR-29", "+refs/heads/cinderline/CSR-29:refs/push/cinderline/CSR-29")},
		{tokenVar, helperArgv(issueLabels, tokenEnv, repoMount,
			"-C", "/repo", "push", "--quiet", "https://git.example.com/o/r.git", "refs/push/cinderline/CSR-29:refs/heads/cinderline/CSR-29")},
		// After the push, the clone's remote-tracking ref is updated so "ahead of
		// the remote" is exact without a fetch (decision 2026-11-07-publish-resilience).
		{nil, agentArgvGit("/runs/CSR-29", "CSR-29",
			"update-ref", "refs/remotes/origin/cinderline/CSR-29", "refs/heads/cinderline/CSR-29")},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", *calls, want)
	}
	tokenOnlyInEnv(t, *calls)
}

func TestDockerPushRefusesDirOutsideRuns(t *testing.T) {
	for _, dir := range []string{"/repo", "/runs", "/runs/", "/runs/../repo", "--upload-pack=x", "runs/CSR-29"} {
		d, calls := recorder(nil)
		if err := d.Push(context.Background(), dir, "cinderline/CSR-29"); err == nil {
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
	if err := d.Push(context.Background(), "/runs/CSR-29", "cinderline/CSR-29"); !errors.Is(err, boom) {
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

// exitErr is a command that ran and exited with that code, as *exec.ExitError.
type exitErr int

func (e exitErr) Error() string { return "exit status" }
func (e exitErr) ExitCode() int { return int(e) }

// scripted returns a Docker whose Exec records every call and answers the
// i-th with outs[i] and errs[i].
func scripted(outs []string, errs map[int]error) (*Docker, *[]execCall) {
	var calls []execCall
	d := &Docker{
		Project:    "CSR",
		Repository: "https://git.example.com/o/r.git",
		Token:      token,
		Exec: func(_ context.Context, env []string, argv ...string) (string, error) {
			calls = append(calls, execCall{env, argv})
			i := len(calls) - 1
			out := ""
			if i < len(outs) {
				out = outs[i]
			}
			return out, errs[i]
		},
	}
	return d, &calls
}

const dir = "/runs/CSR-30"

// agentArgv is the argv of a token-free, networkless git container running
// git in the clone at dir as the agent's user.
func agentArgv(git ...string) []string {
	argv := []string{"docker", "run", "--rm", "--label", "cinderline.project=CSR", "--label", "cinderline.issue=CSR-30",
		"--network", "none", "--user", "1000:1000",
		"-e", "GIT_CONFIG_NOSYSTEM=1", "-e", "GIT_CONFIG_GLOBAL=/dev/null",
		"-v", "cinderline-CSR-repo:/repo:ro", "-v", "cinderline-CSR-runs:/runs",
		"-w", dir, "--entrypoint", "git", GitImage,
		"-c", "safe.directory=/repo", "-c", "core.autocrlf=false", "-c", "core.fileMode=true",
		"-c", "core.hooksPath=/dev/null", "-c", "user.name=cinderline runner", "-c", "user.email=runner@cinderline.invalid"}
	return append(argv, git...)
}

// agentArgvGit is like agentArgv but parameterised by dir and the issue label.
func agentArgvGit(dir, issue string, git ...string) []string {
	argv := []string{"docker", "run", "--rm", "--label", "cinderline.project=CSR", "--label", "cinderline.issue=" + issue,
		"--network", "none", "--user", "1000:1000",
		"-e", "GIT_CONFIG_NOSYSTEM=1", "-e", "GIT_CONFIG_GLOBAL=/dev/null",
		"-v", "cinderline-CSR-repo:/repo:ro", "-v", "cinderline-CSR-runs:/runs",
		"-w", dir, "--entrypoint", "git", GitImage,
		"-c", "safe.directory=/repo", "-c", "core.autocrlf=false", "-c", "core.fileMode=true",
		"-c", "core.hooksPath=/dev/null", "-c", "user.name=cinderline runner", "-c", "user.email=runner@cinderline.invalid"}
	return append(argv, git...)
}

// fetchCalls are the helper's Fetch, as Prepare and MergeBase start with it.
func fetchCalls() []execCall {
	return []execCall{
		{nil, []string{"docker", "volume", "create", "--label", "cinderline.project=CSR", "cinderline-CSR-repo"}},
		{nil, helperArgv(projectLabel, noTokenEnv, repoMount, "init", "--quiet", "--bare", "/repo")},
		{tokenVar, helperArgv(projectLabel, tokenEnv, repoMount,
			"-C", "/repo", "fetch", "--prune", "--quiet", "--no-tags", "https://git.example.com/o/r.git", "+refs/heads/*:refs/heads/*")},
	}
}

func checkCalls(t *testing.T, got []execCall, want []execCall) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls =\n%q\nwant\n%q", got, want)
	}
	tokenOnlyInEnv(t, got)
	// Only the helper's fetch and push carry the token.
	for _, c := range got {
		if c.Env != nil && !reflect.DeepEqual(c.Env, tokenVar) {
			t.Errorf("unexpected env %q for %q", c.Env, c.Argv)
		}
	}
}

func TestDockerPrepare(t *testing.T) {
	for _, tt := range []struct {
		name   string
		exists error // the reply to the check for the remote branch
		start  string
	}{
		{"continues the remote branch", nil, "origin/cinderline/CSR-30"},
		{"starts a new branch from the base", exitErr(1), "origin/main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, calls := scripted(nil, map[int]error{6: tt.exists})
			got, err := d.Prepare(context.Background(), "cinderline/CSR-30", "main")
			if err != nil || got != dir {
				t.Fatalf("Prepare = %q, %v; want %q", got, err, dir)
			}
			want := append(fetchCalls(),
				execCall{nil, []string{"docker", "volume", "create", "--label", "cinderline.project=CSR", "cinderline-CSR-runs"}},
				// As root, a fresh dir for the clone, owned by the agent's user.
				execCall{nil, []string{"docker", "run", "--rm", "--label", "cinderline.project=CSR", "--label", "cinderline.issue=CSR-30",
					"--network", "none", "-v", "cinderline-CSR-runs:/runs", "--entrypoint", "sh", GitImage,
					"-c", `rm -rf -- "$1" && mkdir -- "$1" && chown 1000:1000 -- "$1"`, "sh", dir}},
				execCall{nil, agentArgv("clone", "--quiet", "--shared", "--no-checkout", "/repo", ".")},
				execCall{nil, agentArgv("rev-parse", "--verify", "--quiet", "refs/remotes/origin/cinderline/CSR-30")},
				execCall{nil, agentArgv("checkout", "--quiet", "--no-track", "-B", "cinderline/CSR-30", tt.start)},
			)
			checkCalls(t, *calls, want)
		})
	}
}

func TestDockerPrepareRefusesBadBranch(t *testing.T) {
	for _, branch := range []string{"CSR-30", "cinderline/", "cinderline/../repo", "cinderline/-x", "cinderline/a/b", "cinderline/a b"} {
		d, calls := scripted(nil, nil)
		if _, err := d.Prepare(context.Background(), branch, "main"); err == nil {
			t.Errorf("Prepare(%q) = nil error", branch)
		}
		if len(*calls) != 0 {
			t.Errorf("Prepare(%q) ran %q", branch, *calls)
		}
	}
}

func TestDockerCommit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		staged  error // the reply to diff --cached --quiet
		want    bool
		commits bool
	}{
		{"something to commit", exitErr(1), true, true},
		{"nothing to commit", nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, calls := scripted(nil, map[int]error{1: tt.staged})
			got, err := d.Commit(context.Background(), dir, "CSR-30: -m a title")
			if err != nil || got != tt.want {
				t.Fatalf("Commit = %v, %v; want %v", got, err, tt.want)
			}
			want := []execCall{
				{nil, agentArgv("add", "--all")},
				{nil, agentArgv("diff", "--cached", "--quiet")},
			}
			if tt.commits {
				want = append(want, execCall{nil, agentArgv("commit", "--quiet", "--no-verify", "-m", "CSR-30: -m a title")})
			}
			checkCalls(t, *calls, want)
		})
	}
}

func TestDockerMergeBase(t *testing.T) {
	merge := execCall{nil, agentArgv("merge", "--quiet", "--no-edit", "origin/main")}
	fetch := execCall{nil, agentArgv("fetch", "--quiet", "--prune", "--no-tags", "/repo", "+refs/heads/*:refs/remotes/origin/*")}

	d, calls := scripted(nil, nil)
	conflicts, err := d.MergeBase(context.Background(), dir, "main")
	if err != nil || conflicts != nil {
		t.Fatalf("MergeBase = %q, %v; want merged", conflicts, err)
	}
	checkCalls(t, *calls, append(fetchCalls(), fetch, merge))

	d, calls = scripted([]string{5: "a.go\x00dir/b c.go\x00"}, map[int]error{4: exitErr(1)})
	conflicts, err = d.MergeBase(context.Background(), dir, "main")
	if err != nil || !reflect.DeepEqual(conflicts, []string{"a.go", "dir/b c.go"}) {
		t.Fatalf("MergeBase = %q, %v; want the two conflicts", conflicts, err)
	}
	checkCalls(t, *calls, append(fetchCalls(), fetch, merge,
		execCall{nil, agentArgv("diff", "-z", "--name-only", "--diff-filter=U")},
		execCall{nil, agentArgv("merge", "--abort")},
	))

	// A failed merge without conflicts is an error, not "merged".
	d, _ = scripted(nil, map[int]error{4: exitErr(1)})
	if _, err := d.MergeBase(context.Background(), dir, "main"); err == nil {
		t.Error("MergeBase = nil error for a failed merge without conflicts")
	}
}

func TestDockerIsAncestor(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	for _, tt := range []struct {
		a, b     string
		reply    error
		wantArgs []string
		want     bool
	}{
		{"main", "HEAD", nil, []string{"refs/remotes/origin/main", "HEAD"}, true},
		{"HEAD", "main", exitErr(1), []string{"HEAD", "refs/remotes/origin/main"}, false},
		{sha, "HEAD", nil, []string{sha, "HEAD"}, true},
	} {
		d, calls := scripted(nil, map[int]error{0: tt.reply})
		got, err := d.IsAncestor(context.Background(), dir, tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Errorf("IsAncestor(%s, %s) = %v, %v; want %v", tt.a, tt.b, got, err, tt.want)
		}
		checkCalls(t, *calls, []execCall{{nil, agentArgv(append([]string{"merge-base", "--is-ancestor"}, tt.wantArgs...)...)}})
	}

	// Any other failure is an error, not "no".
	d, _ := scripted(nil, map[int]error{0: exitErr(128)})
	if _, err := d.IsAncestor(context.Background(), dir, "main", "HEAD"); err == nil {
		t.Error("IsAncestor = nil error on exit 128")
	}
}

func TestDockerClean(t *testing.T) {
	status := execCall{nil, agentArgv("status", "--porcelain")}
	merging := execCall{nil, agentArgv("rev-parse", "--verify", "--quiet", "MERGE_HEAD")}
	for _, tt := range []struct {
		name  string
		outs  []string
		errs  map[int]error
		calls []execCall
		want  bool
	}{
		{"clean", nil, map[int]error{1: exitErr(1)}, []execCall{status, merging}, true},
		{"changes", []string{"?? x\n"}, nil, []execCall{status}, false},
		{"merge in progress", []string{"", "abc\n"}, nil, []execCall{status, merging}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, calls := scripted(tt.outs, tt.errs)
			got, err := d.Clean(context.Background(), dir)
			if err != nil || got != tt.want {
				t.Fatalf("Clean = %v, %v; want %v", got, err, tt.want)
			}
			checkCalls(t, *calls, tt.calls)
		})
	}
}

func TestDockerConflictMarkers(t *testing.T) {
	grep := execCall{nil, agentArgv("grep", "-q", "-e", "^<<<<<<< ", "-e", "^>>>>>>> ", "--", "a.go", "-b.go")}
	for _, tt := range []struct {
		reply error
		want  bool
	}{{nil, true}, {exitErr(1), false}} {
		d, calls := scripted(nil, map[int]error{0: tt.reply})
		got, err := d.ConflictMarkers(context.Background(), dir, []string{"a.go", "-b.go"})
		if err != nil || got != tt.want {
			t.Errorf("ConflictMarkers = %v, %v; want %v", got, err, tt.want)
		}
		checkCalls(t, *calls, []execCall{grep})
	}

	// No files: git grep would search the whole tree, so nothing runs.
	d, calls := scripted(nil, nil)
	if got, err := d.ConflictMarkers(context.Background(), dir, nil); got || err != nil || len(*calls) != 0 {
		t.Errorf("ConflictMarkers(nil) = %v, %v, ran %q", got, err, *calls)
	}
}

func TestDockerResetHeadLog(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	d, calls := scripted(nil, nil)
	if err := d.Reset(context.Background(), dir, sha); err != nil {
		t.Fatal(err)
	}
	// reset --hard also ends a merge in progress.
	checkCalls(t, *calls, []execCall{
		{nil, agentArgv("reset", "--quiet", "--hard", sha)},
		{nil, agentArgv("clean", "-fdq")},
	})
	if err := d.Reset(context.Background(), dir, "--hard"); err == nil {
		t.Error("Reset accepted a commit that is not a hash")
	}

	d, calls = scripted([]string{sha + "\n"}, nil)
	if got, err := d.Head(context.Background(), dir); got != sha || err != nil {
		t.Errorf("Head = %q, %v", got, err)
	}
	checkCalls(t, *calls, []execCall{{nil, agentArgv("rev-parse", "HEAD")}})

	d, calls = scripted([]string{"abc1234 CSR-30: x\n"}, nil)
	if got, err := d.Log(context.Background(), dir, "main"); got != "abc1234 CSR-30: x" || err != nil {
		t.Errorf("Log = %q, %v", got, err)
	}
	checkCalls(t, *calls, []execCall{{nil, agentArgv("log", "--oneline", "refs/remotes/origin/main..HEAD")}})
}

func TestDockerStranded(t *testing.T) {
	// Clones with unpushed commits are listed.
	d, calls := scripted([]string{"", "/runs/CSR-5\tcinderline/CSR-5\n/runs/CSR-8\tcinderline/CSR-8\n"}, nil)
	clones, err := d.Stranded(context.Background())
	if err != nil || !reflect.DeepEqual(clones, []Clone{
		{Dir: "/runs/CSR-5", Branch: "cinderline/CSR-5"},
		{Dir: "/runs/CSR-8", Branch: "cinderline/CSR-8"},
	}) {
		t.Fatalf("Stranded = %v, %v", clones, err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(*calls))
	}
	// First call: create the runs volume.
	wantVolume := execCall{nil, []string{"docker", "volume", "create", "--label", "cinderline.project=CSR", "cinderline-CSR-runs"}}
	if !reflect.DeepEqual((*calls)[0], wantVolume) {
		t.Errorf("volume create =\n%q\nwant\n%q", (*calls)[0], wantVolume)
	}
	// Second call: the container with the shell script.
	run := (*calls)[1]
	if run.Env != nil {
		t.Errorf("env = %q, want nil", run.Env)
	}
	if len(run.Argv) < 3 {
		t.Fatalf("argv too short")
	}
	if run.Argv[0] != "docker" || run.Argv[1] != "run" || run.Argv[2] != "--rm" {
		t.Errorf("argv starts with %q, want docker run --rm", run.Argv[0:3])
	}
	// Check key docker flags.
	joined := strings.Join(run.Argv, " ")
	if !strings.Contains(joined, "--network none") {
		t.Error("missing --network none")
	}
	if !strings.Contains(joined, "--user 1000:1000") {
		t.Error("missing --user")
	}
	if !strings.Contains(joined, "GIT_CONFIG_NOSYSTEM=1") {
		t.Error("missing GIT_CONFIG_NOSYSTEM")
	}
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=6") {
		t.Error("missing GIT_CONFIG_COUNT")
	}
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=safe.directory") {
		t.Error("missing safe.directory config key")
	}
	if !strings.Contains(joined, "GIT_CONFIG_VALUE_0=/repo") {
		t.Error("missing safe.directory=/repo")
	}
	if !strings.Contains(joined, "GIT_CONFIG_KEY_3=core.hooksPath") {
		t.Error("missing core.hooksPath config key")
	}
	if !strings.Contains(joined, "GIT_CONFIG_VALUE_3=/dev/null") {
		t.Error("missing core.hooksPath=/dev/null")
	}
	if !strings.Contains(joined, "cinderline-CSR-repo:/repo:ro") {
		t.Error("missing repo volume")
	}
	if !strings.Contains(joined, "cinderline-CSR-runs:/runs") {
		t.Error("missing runs volume")
	}
	if !strings.Contains(joined, "--entrypoint sh") {
		t.Error("missing --entrypoint sh")
	}
	// Check the script contains expected patterns.
	if !strings.Contains(joined, "/runs/*/") {
		t.Error("script missing /runs/*/ loop")
	}
	if !strings.Contains(joined, "symbolic-ref") {
		t.Error("script missing symbolic-ref")
	}
	if !strings.Contains(joined, "branch -r --contains") {
		t.Error("script missing branch -r --contains")
	}
	if !strings.Contains(joined, "cinderline/*)") {
		t.Error("script missing cinderline/* pattern")
	}
	if !strings.Contains(joined, "printf") {
		t.Error("script missing printf")
	}
	if !strings.Contains(joined, "${d%/}") {
		t.Error("script missing ${d%/}")
	}

	// Empty output returns nil, nil.
	d, _ = scripted([]string{"", ""}, nil)
	clones, err = d.Stranded(context.Background())
	if err != nil || clones != nil {
		t.Errorf("Stranded on empty output = %v, %v; want nil, nil", clones, err)
	}

	// Volume create failure is an error.
	boom := errors.New("volume boom")
	d, _ = scripted(nil, map[int]error{0: boom})
	if _, err := d.Stranded(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Stranded on volume create failure = %v, want boom", err)
	}

	// Docker run failure is an error.
	d, _ = scripted(nil, map[int]error{1: boom})
	if _, err := d.Stranded(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Stranded on docker run failure = %v, want boom", err)
	}
}

func TestDockerAgentRefusesDirOutsideRuns(t *testing.T) {
	for _, bad := range []string{"/repo", "/runs", "/runs/../repo", "/runs/a/b", "--x"} {
		d, calls := scripted(nil, nil)
		if _, err := d.Head(context.Background(), bad); err == nil {
			t.Errorf("Head(%q) = nil error", bad)
		}
		if len(*calls) != 0 {
			t.Errorf("Head(%q) ran %q", bad, *calls)
		}
	}
}
