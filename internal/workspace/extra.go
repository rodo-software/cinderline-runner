package workspace

import (
	"cmp"
	"context"
	"log/slog"
	"strings"
)

// ExtraPath is where agent containers see a project's extra repositories,
// read-only, one folder per repository (CSR-160). LinkedPath is where they
// see the repositories of linked projects, one folder per project key with
// a folder per repository in it (CSR-161). AttachmentsPath is where they see
// the files attached to the run's issue (CSR-173).
const (
	ExtraPath       = "/extra"
	LinkedPath      = "/linked"
	AttachmentsPath = "/attachments"
)

// Extra is one repository to fetch: its folder under the mount (KEY/name
// under LinkedPath), URL, ref (empty for the default branch) and the git
// token stored for that URL.
type Extra struct {
	Name, URL, Ref, Token string
}

// ExtraMount is the docker run flags that mount the volume of path
// (ExtraPath or LinkedPath) read-only.
func ExtraMount(project, path string) []string {
	return []string{"-v", volumeOf(project, path) + ":" + path + ":ro"}
}

// volumeOf is the volume mounted at path: cinderline-<KEY>-extra or -linked.
func volumeOf(project, path string) string { return Volume(project, strings.TrimPrefix(path, "/")) }

// pruneExtraScript, run in the mount, removes every entry in the top two
// levels that is not a folder named in its arguments, inside one, or above
// one: repositories and links that are gone.
const pruneExtraScript = `for d in * .[!.]* */* */.[!.]*; do
  [ -e "$d" ] || continue
  keep=
  for n in "$@"; do
    case "$d" in "$n"|"$n"/*) keep=1 ;; esac
    case "$n" in "$d"/*) keep=1 ;; esac
  done
  [ -n "$keep" ] || rm -rf -- "$d"
done`

// fetchExtraScript fetches $3 (a branch or tag, or HEAD for the default
// branch) of URL $2 into folder $1 of the mount and checks it out. A failure
// removes the folder, so a run never reads a stale copy. The URL is given on
// every fetch, never stored as a remote, and the FETCH_HEAD that names it is
// removed: an agent never gets a forge's URL (CL decision
// 2026-11-25-forge-off-limits-for-agents).
const fetchExtraScript = `d=$PWD/$1
{ [ -d "$d/.git" ] || { mkdir -p "$d" && git init --quiet "$d"; }; } &&
  git -C "$d" fetch --quiet --depth 1 --no-tags "$2" "$3" &&
  git -C "$d" checkout --quiet --force --detach FETCH_HEAD &&
  rm -f -- "$d/.git/FETCH_HEAD" &&
  git -C "$d" clean --quiet -ffdx ||
  { rm -rf -- "$d"; exit 1; }`

// FetchExtras brings the project's volume of path (ExtraPath or LinkedPath)
// up to repos, each in its own throwaway helper container that gets only
// that repository's token. It returns the error of each repository that
// failed, by name; a failure to prepare the volume fails them all. Retries
// are logged to log.
func FetchExtras(ctx context.Context, x Exec, log *slog.Logger, project, path string, repos []Extra) map[string]error {
	fail := func(err error) map[string]error {
		m := map[string]error{}
		for _, r := range repos {
			m[r.Name] = err
		}
		return m
	}
	labels := Labels(project, "")
	mount := []string{"-v", volumeOf(project, path) + ":" + path, "-w", path}
	if _, err := x(ctx, nil, append(append([]string{"docker", "volume", "create"}, labels...), volumeOf(project, path))...); err != nil {
		return fail(err)
	}
	argv := append([]string{"docker", "run", "--rm"}, labels...)
	argv = append(append(argv, "--network", "none"), mount...)
	argv = append(argv, "--entrypoint", "sh", GitImage, "-c", pruneExtraScript, "sh")
	for _, r := range repos {
		argv = append(argv, r.Name)
	}
	if _, err := x(ctx, nil, argv...); err != nil {
		return fail(err)
	}
	failed := map[string]error{}
	for _, r := range repos {
		if err := withToken(ctx, x, log, project, labels, mount, r.Token, fetchExtraScript, r.Name, r.URL, cmp.Or(r.Ref, "HEAD")); err != nil {
			failed[r.Name] = err
		}
	}
	return failed
}

// LsRemote checks that token can read ref (empty: the default branch) of the
// repository at url, as a branch or a tag. Retries are logged to log.
func LsRemote(ctx context.Context, x Exec, log *slog.Logger, project, url, ref, token string) error {
	refs := []string{"HEAD"}
	if ref != "" {
		refs = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	}
	return withToken(ctx, x, log, project, Labels(project, ""), nil, token,
		`exec git ls-remote --quiet --exit-code "$@"`, append([]string{url}, refs...)...)
}

// withToken runs script with args as $1... in a throwaway git container
// that gets token by name, so its value travels in the docker command's
// environment, never in argv, and askpass answers git with it. It needs the
// network, so a transient network error is retried.
func withToken(ctx context.Context, x Exec, log *slog.Logger, project string, labels, mounts []string, token, script string, args ...string) error {
	argv := append([]string{"docker", "run", "--rm"}, labels...)
	argv = append(argv, "-e", "GIT_TOKEN", "-e", "GIT_ASKPASS=/tmp/askpass", "-e", "GIT_TERMINAL_PROMPT=0")
	argv = append(argv, mounts...)
	argv = append(argv, "--entrypoint", "sh", GitImage, "-c", askpassScript, "sh", "sh", "-c", script, "sh")
	argv = append(argv, args...)
	_, err := retryNetwork(ctx, log, project, func() (string, error) { return x(ctx, []string{"GIT_TOKEN=" + token}, argv...) })
	return err
}
