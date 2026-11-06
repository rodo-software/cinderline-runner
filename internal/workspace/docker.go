package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
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
// combined output. It is the one seam for every docker and git command line.
type Exec func(ctx context.Context, env []string, argv ...string) (string, error)

// RunCommand is the real Exec.
func RunCommand(ctx context.Context, env []string, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, tail(string(out), 2000))
	}
	return string(out), nil
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
	if path.Clean(dir) != dir || !strings.HasPrefix(dir, RunsPath+"/") {
		return fmt.Errorf("push %s: %q is not a clone under %s", branch, dir, RunsPath)
	}
	labels := d.labels(strings.TrimPrefix(branch, name.Name+"/"))
	scratch := "refs/push/" + branch
	// The clone belongs to the agent's UID, so git needs it named safe; only
	// that exact path.
	if err := d.helper(ctx, labels, false, true, "-c", "safe.directory="+dir, "-C", RepoPath, "fetch", "--quiet", "--no-tags", dir, "+refs/heads/"+branch+":"+scratch); err != nil {
		return err
	}
	return d.helper(ctx, labels, true, false, "-C", RepoPath, "push", "--quiet", d.Repository, scratch+":refs/heads/"+branch)
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
	x := d.Exec
	if x == nil {
		x = RunCommand
	}
	_, err := x(ctx, env, argv...)
	return err
}

// volume is the name of the project's volume of the given kind.
func (d *Docker) volume(kind string) string {
	return name.Name + "-" + d.Project + "-" + kind
}

// labels are the --label flags for what the runner creates (review item 9):
// the project, and the issue when there is one.
func (d *Docker) labels(issue string) []string {
	l := []string{"--label", name.Name + ".project=" + d.Project}
	if issue != "" {
		l = append(l, "--label", name.Name+".issue="+issue)
	}
	return l
}
