package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

// GitImage is the helper's git image, pinned by digest (alpine/git 2.54.0,
// multi-arch index).
const GitImage = "alpine/git:2.54.0@sha256:ac6c828f15d82775d204d3d75112e1d34af8945da80ccf922713c9891ba68e7a"

// Where the volumes are mounted. The repo volume sits at the same path in
// every container, so the alternates of the agents' clones resolve in the
// helper too.
const (
	RepoPath = "/repo"
	RunsPath = "/runs"
)

// askpassScript writes git's askpass into the throwaway container and then
// runs its arguments. The askpass answers with the token from the container's
// environment; Bitbucket needs the username x-token-auth, the other forges
// take any (as today's askpass.sh).
const askpassScript = `printf '%s\n' '#!/bin/sh' 'case "$1" in Username*) echo x-token-auth ;; *) echo "$GIT_TOKEN" ;; esac' > /tmp/askpass && chmod 700 /tmp/askpass && exec "$@"`

// Exec runs argv with env added to the runner's environment and returns its
// standard output. A command that ran and failed returns an error with an
// ExitCode method, as *exec.ExitError. It is the one seam for every docker
// and git command line.
type Exec func(ctx context.Context, env []string, argv ...string) (string, error)

// RunCommand is the real Exec. Standard error goes only into the error, so
// docker's pull progress never mixes into git's output.
func RunCommand(ctx context.Context, env []string, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, tail(stdout.String()+stderr.String(), 2000))
	}
	return stdout.String(), nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Docker is the Workspace of the Docker environments, with git in containers.
// This file holds the helper side (decision 2026-11-05-runner-rewrite-details
// point 12): the only container that gets the git token, which owns the
// project's clone in the repo volume, fetches into it and pushes from it.
type Docker struct {
	Project    string // project key, in volume names and labels
	Repository string // the configured repository URL, the only remote
	Token      string // the git token; reaches only the helper's environment
	Exec       Exec   // nil means RunCommand
}

// Fetch makes the project's repo volume and bare clone if missing and
// mirrors the remote's branches into it.
func (d *Docker) Fetch(ctx context.Context) error {
	labels := d.labels("")
	if err := d.run(ctx, nil, append(append([]string{"docker", "volume", "create"}, labels...), d.volume("repo"))...); err != nil {
		return err
	}
	if err := d.helper(ctx, labels, false, false, "init", "--quiet", "--bare", RepoPath); err != nil {
		return err
	}
	// The URL is given on every fetch and push, never stored as a remote, so
	// a changed repository can't leave the token going to the old one.
	return d.helper(ctx, labels, true, false, "-C", RepoPath, "fetch", "--prune", "--quiet", "--no-tags", d.Repository, "+refs/heads/*:refs/heads/*")
}

// Push fetches branch from the agent's clone at dir, mounted read-only and
// without the token, into the helper's clone, then pushes it from there. No
// git runs inside the agent's clone with the token, so its hooks and config
// can't run with it or redirect it.
func (d *Docker) Push(ctx context.Context, dir, branch string) error {
	if err := checkDir(dir); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	labels := d.labels(strings.TrimPrefix(branch, name.Name+"/"))
	scratch := "refs/push/" + branch
	// The clone belongs to the agent's UID, so git needs it named safe; only
	// that exact path.
	if err := d.helper(ctx, labels, false, true, "-c", "safe.directory="+dir, "-C", RepoPath, "fetch", "--quiet", "--no-tags", dir, "+refs/heads/"+branch+":"+scratch); err != nil {
		return err
	}
	if err := d.helper(ctx, labels, true, false, "-C", RepoPath, "push", "--quiet", d.Repository, scratch+":refs/heads/"+branch); err != nil {
		return err
	}
	// Record what was pushed in the clone so "ahead of the remote" is exact
	// without a fetch (decision 2026-11-07-publish-resilience).
	_, err := d.git(ctx, dir, "update-ref", "refs/remotes/origin/"+branch, "refs/heads/"+branch)
	return err
}

// helper runs git with args in a throwaway helper container with the repo
// volume, and the runs volume read-only when runs is set. Only with token
// does the container get the token, by name: its value travels in the docker
// command's environment, never in argv.
func (d *Docker) helper(ctx context.Context, labels []string, token, runs bool, args ...string) error {
	argv := append([]string{"docker", "run", "--rm"}, labels...)
	var env []string
	if token {
		argv = append(argv, "-e", "GIT_TOKEN", "-e", "GIT_ASKPASS=/tmp/askpass")
		env = []string{"GIT_TOKEN=" + d.Token}
	}
	argv = append(argv, "-e", "GIT_TERMINAL_PROMPT=0", "-v", d.volume("repo")+":"+RepoPath)
	if runs {
		argv = append(argv, "-v", d.volume("runs")+":"+RunsPath+":ro")
	}
	argv = append(argv, "--entrypoint", "sh", GitImage, "-c", askpassScript, "sh", "git")
	return d.run(ctx, env, append(argv, args...)...)
}

func (d *Docker) run(ctx context.Context, env []string, argv ...string) error {
	_, err := d.output(ctx, env, argv...)
	return err
}

func (d *Docker) output(ctx context.Context, env []string, argv ...string) (string, error) {
	x := d.Exec
	if x == nil {
		x = RunCommand
	}
	return x(ctx, env, argv...)
}

// volume is the name of the project's volume of the given kind.
func (d *Docker) volume(kind string) string { return Volume(d.Project, kind) }

// Volume is the name of a project's volume of the given kind: repo, runs,
// or state for the clients' sessions and config dirs.
func Volume(project, kind string) string {
	return name.Name + "-" + project + "-" + kind
}

func (d *Docker) labels(issue string) []string { return Labels(d.Project, issue) }

// ProjectLabel is the label every container, network and volume the runner
// creates carries; recovery finds stale ones by it (review item 9).
func ProjectLabel(project string) string { return name.Name + ".project=" + project }

// Labels are the --label flags for what the runner creates: the project, and
// the issue when there is one.
func Labels(project, issue string) []string {
	l := []string{"--label", ProjectLabel(project)}
	if issue != "" {
		l = append(l, "--label", name.Name+".issue="+issue)
	}
	return l
}

// The agent side (decision 2026-11-05-runner-rewrite-details point 12): one
// clone per issue at /runs/<REF> in the runs volume, sharing the helper's
// objects through alternates. Its git runs in a container without the token
// and without a network, as the agent's user.

var _ Workspace = (*Docker)(nil)

// AgentUser is the user agent containers and the agent side's git run as,
// so the agent can write everything git writes in its clone (as uid 1000
// in today's runner image).
const AgentUser = "1000:1000"

// gitConfig is the runner's own git configuration passed to every agent-side
// git invocation, on the command line or through GIT_CONFIG_KEY_* env vars.
// The list stays here, so both forms derive from the same source.
var gitConfig = [][2]string{
	{"safe.directory", RepoPath},
	{"core.autocrlf", "false"},
	{"core.fileMode", "true"},
	{"core.hooksPath", "/dev/null"},
	{"user.name", name.Name + " runner"},
	{"user.email", "runner@" + name.Name + ".invalid"},
}

var (
	issueRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	hash     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// checkDir refuses anything but a clone directly under the runs volume.
func checkDir(dir string) error {
	if path.Clean(dir) != dir || path.Dir(dir) != RunsPath || !issueRef.MatchString(path.Base(dir)) {
		return fmt.Errorf("%q is not a clone under %s", dir, RunsPath)
	}
	return nil
}

// Prepare fetches, then makes a fresh clone for branch (always
// cinderstack/<REF>), continuing the remote branch if a run pushed one, else
// starting from base. A clone left from before is removed, as today's runner
// does with old worktrees; a paused run keeps its clone by not preparing.
func (d *Docker) Prepare(ctx context.Context, branch, base string) (string, error) {
	ref, ok := strings.CutPrefix(branch, name.Name+"/")
	if !ok || !issueRef.MatchString(ref) {
		return "", fmt.Errorf("prepare: %q is not a branch %s/<REF>", branch, name.Name)
	}
	dir := RunsPath + "/" + ref
	if err := d.Fetch(ctx); err != nil {
		return "", err
	}
	if err := d.run(ctx, nil, append(append([]string{"docker", "volume", "create"}, d.labels("")...), d.volume("runs"))...); err != nil {
		return "", err
	}
	// The volume's root belongs to root, so root makes the clone's dir.
	argv := append(append([]string{"docker", "run", "--rm"}, d.labels(ref)...),
		"--network", "none", "-v", d.volume("runs")+":"+RunsPath, "--entrypoint", "sh", GitImage,
		"-c", `rm -rf -- "$1" && mkdir -- "$1" && chown `+AgentUser+` -- "$1"`, "sh", dir)
	if err := d.run(ctx, nil, argv...); err != nil {
		return "", err
	}
	if _, err := d.git(ctx, dir, "clone", "--quiet", "--shared", "--no-checkout", RepoPath, "."); err != nil {
		return "", err
	}
	start := "origin/" + branch
	if ok, err := d.succeeds(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/"+start); err != nil {
		return "", err
	} else if !ok {
		start = "origin/" + base
	}
	if _, err := d.git(ctx, dir, "checkout", "--quiet", "--no-track", "-B", branch, start); err != nil {
		return "", fmt.Errorf("prepare %s from %s: %w", branch, start, err)
	}
	return dir, nil
}

// Commit commits everything in the clone, without hooks.
func (d *Docker) Commit(ctx context.Context, dir, message string) (bool, error) {
	if _, err := d.git(ctx, dir, "add", "--all"); err != nil {
		return false, err
	}
	if same, err := d.succeeds(ctx, dir, "diff", "--cached", "--quiet"); same || err != nil {
		return false, err
	}
	_, err := d.git(ctx, dir, "commit", "--quiet", "--no-verify", "-m", message)
	return err == nil, err
}

// MergeBase fetches into the helper's clone and from there into the agent's,
// then merges the base. A conflicted merge is aborted.
func (d *Docker) MergeBase(ctx context.Context, dir, base string) ([]string, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	if err := d.Fetch(ctx); err != nil {
		return nil, err
	}
	// From the path, never the clone's remote, which the agent can change.
	if _, err := d.git(ctx, dir, "fetch", "--quiet", "--prune", "--no-tags", RepoPath, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return nil, err
	}
	merged, mergeErr := d.succeeds(ctx, dir, "merge", "--quiet", "--no-edit", "origin/"+base)
	if merged || mergeErr != nil {
		return nil, mergeErr
	}
	out, err := d.git(ctx, dir, "diff", "-z", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, fmt.Errorf("merge origin/%s failed without conflicts", base)
	}
	if _, err := d.git(ctx, dir, "merge", "--abort"); err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// IsAncestor reports whether ancestor is an ancestor of descendant.
func (d *Docker) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	return d.succeeds(ctx, dir, "merge-base", "--is-ancestor", rev(ancestor), rev(descendant))
}

// rev is HEAD or a commit as given, and any other name the remote's branch.
func rev(s string) string {
	if s == "HEAD" || hash.MatchString(s) {
		return s
	}
	return "refs/remotes/origin/" + s
}

// Clean reports whether git status shows nothing and no merge is in progress.
func (d *Docker) Clean(ctx context.Context, dir string) (bool, error) {
	if out, err := d.git(ctx, dir, "status", "--porcelain"); out != "" || err != nil {
		return false, err
	}
	merging, err := d.succeeds(ctx, dir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	return !merging && err == nil, err
}

// ConflictMarkers greps files for a line starting with a conflict marker.
func (d *Docker) ConflictMarkers(ctx context.Context, dir string, files []string) (bool, error) {
	if len(files) == 0 {
		return false, nil
	}
	return d.succeeds(ctx, dir, append([]string{"grep", "-q", "-e", "^<<<<<<< ", "-e", "^>>>>>>> ", "--"}, files...)...)
}

// Reset moves the branch back to commit, which also ends a merge in
// progress, and removes untracked files: everything was committed before the
// merge, so they are the resolver's.
func (d *Docker) Reset(ctx context.Context, dir, commit string) error {
	if !hash.MatchString(commit) {
		return fmt.Errorf("reset: %q is not a commit", commit)
	}
	if _, err := d.git(ctx, dir, "reset", "--quiet", "--hard", commit); err != nil {
		return err
	}
	_, err := d.git(ctx, dir, "clean", "-fdq")
	return err
}

// Head returns the commit HEAD is on.
func (d *Docker) Head(ctx context.Context, dir string) (string, error) {
	out, err := d.git(ctx, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// Log returns git log --oneline of the commits not on the remote's base.
func (d *Docker) Log(ctx context.Context, dir, base string) (string, error) {
	out, err := d.git(ctx, dir, "log", "--oneline", rev(base)+"..HEAD")
	return strings.TrimSpace(out), err
}

// succeeds runs git and reports whether it exited 0; exit 1 is "no", any
// other failure an error.
func (d *Docker) succeeds(ctx context.Context, dir string, args ...string) (bool, error) {
	_, err := d.git(ctx, dir, args...)
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// git runs git with args in the clone at dir, in a container with no token,
// no network and the repo volume read-only. The runner's own config is given
// on the command line, which wins over the clone's, and the image's system
// and global config are ignored, so the host's never applies. Hooks are off:
// the git image lacks the project's toolchain, and the agent can write them.
func (d *Docker) git(ctx context.Context, dir string, args ...string) (string, error) {
	if err := checkDir(dir); err != nil {
		return "", err
	}
	argv := append([]string{"docker", "run", "--rm"}, d.labels(path.Base(dir))...)
	argv = append(argv, "--network", "none", "--user", AgentUser,
		"-e", "GIT_CONFIG_NOSYSTEM=1", "-e", "GIT_CONFIG_GLOBAL=/dev/null",
		"-v", d.volume("repo")+":"+RepoPath+":ro", "-v", d.volume("runs")+":"+RunsPath,
		"-w", dir, "--entrypoint", "git", GitImage)
	for _, kv := range gitConfig {
		argv = append(argv, "-c", kv[0]+"="+kv[1])
	}
	return d.output(ctx, nil, append(argv, args...)...)
}

// Stranded lists clones whose branch has commits that no remote branch has
// (decision 2026-11-07-publish-resilience). One docker run loops over every
// directory under /runs, checking with git symbolic-ref and branch -r.
func (d *Docker) Stranded(ctx context.Context) ([]Clone, error) {
	labels := d.labels("")
	if err := d.run(ctx, nil, append(append([]string{"docker", "volume", "create"}, labels...), d.volume("runs"))...); err != nil {
		return nil, err
	}
	script := `for d in /runs/*/; do
  [ -d "$d" ] || continue
  branch=$(git -C "$d" symbolic-ref --quiet --short HEAD 2>/dev/null) || continue
  case $branch in
  ` + name.Name + `/*) ;;
  *) continue ;;
  esac
  if git -C "$d" branch -r --contains HEAD 2>/dev/null | grep -q .; then
    continue
  fi
  printf '%s\t%s\n' "${d%/}" "$branch"
done`
	argv := append([]string{"docker", "run", "--rm"}, labels...)
	argv = append(argv, "--network", "none", "--user", AgentUser,
		"-e", "GIT_CONFIG_NOSYSTEM=1", "-e", "GIT_CONFIG_GLOBAL=/dev/null",
		"-e", "GIT_CONFIG_COUNT="+strconv.Itoa(len(gitConfig)))
	for i, kv := range gitConfig {
		argv = append(argv, "-e", "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0], "-e", "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	argv = append(argv,
		"-v", d.volume("repo")+":"+RepoPath+":ro", "-v", d.volume("runs")+":"+RunsPath,
		"--entrypoint", "sh", GitImage, "-c", script)
	out, err := d.output(ctx, nil, argv...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var clones []Clone
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		dir, branch, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("stranded: cannot parse %q", line)
		}
		clones = append(clones, Clone{Dir: dir, Branch: branch})
	}
	return clones, nil
}
