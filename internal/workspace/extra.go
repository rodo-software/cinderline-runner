package workspace

import (
	"cmp"
	"context"
)

// ExtraPath is where agent containers see a project's extra repositories,
// read-only, one folder per repository (CSR-160).
const ExtraPath = "/extra"

// Extra is one extra repository to fetch: its folder name, URL, ref (empty
// for the default branch) and the git token stored for that URL.
type Extra struct {
	Name, URL, Ref, Token string
}

// ExtraMount is the docker run flags that mount the project's extra
// repositories read-only.
func ExtraMount(project string) []string {
	return []string{"-v", Volume(project, "extra") + ":" + ExtraPath + ":ro"}
}

// pruneExtraScript removes every folder under ExtraPath not named in its
// arguments: repositories removed from the config.
const pruneExtraScript = `cd ` + ExtraPath + ` || exit 1
for d in * .[!.]*; do
  [ -e "$d" ] || continue
  keep=
  for n in "$@"; do [ "$d" = "$n" ] && keep=1; done
  [ -n "$keep" ] || rm -rf -- "$d"
done`

// fetchExtraScript fetches $3 (a branch or tag, or HEAD for the default
// branch) of URL $2 into folder $1 and checks it out. A failure removes the
// folder, so a run never reads a stale copy. The URL is given on every fetch,
// never stored as a remote.
const fetchExtraScript = `d=` + ExtraPath + `/$1
{ [ -d "$d/.git" ] || git init --quiet "$d"; } &&
  git -C "$d" fetch --quiet --depth 1 --no-tags "$2" "$3" &&
  git -C "$d" checkout --quiet --force --detach FETCH_HEAD &&
  git -C "$d" clean --quiet -ffdx ||
  { rm -rf -- "$d"; exit 1; }`

// FetchExtras brings the project's extra-repository volume up to repos, each
// in its own throwaway helper container that gets only that repository's
// token. It returns the error of each repository that failed, by name; a
// failure to prepare the volume fails them all. A nil x means RunCommand.
func FetchExtras(ctx context.Context, x Exec, project string, repos []Extra) map[string]error {
	if x == nil {
		x = RunCommand
	}
	fail := func(err error) map[string]error {
		m := map[string]error{}
		for _, r := range repos {
			m[r.Name] = err
		}
		return m
	}
	labels := Labels(project, "")
	vol := Volume(project, "extra")
	if _, err := x(ctx, nil, append(append([]string{"docker", "volume", "create"}, labels...), vol)...); err != nil {
		return fail(err)
	}
	argv := append([]string{"docker", "run", "--rm"}, labels...)
	argv = append(argv, "--network", "none", "-v", vol+":"+ExtraPath, "--entrypoint", "sh", GitImage, "-c", pruneExtraScript, "sh")
	for _, r := range repos {
		argv = append(argv, r.Name)
	}
	if _, err := x(ctx, nil, argv...); err != nil {
		return fail(err)
	}
	failed := map[string]error{}
	for _, r := range repos {
		if err := withToken(ctx, x, labels, []string{"-v", vol + ":" + ExtraPath}, r.Token, fetchExtraScript, r.Name, r.URL, cmp.Or(r.Ref, "HEAD")); err != nil {
			failed[r.Name] = err
		}
	}
	return failed
}

// LsRemote checks that token can read ref (empty: the default branch) of the
// repository at url, as a branch or a tag. A nil x means RunCommand.
func LsRemote(ctx context.Context, x Exec, project, url, ref, token string) error {
	refs := []string{"HEAD"}
	if ref != "" {
		refs = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	}
	if x == nil {
		x = RunCommand
	}
	return withToken(ctx, x, Labels(project, ""), nil, token,
		`exec git ls-remote --quiet --exit-code "$@"`, append([]string{url}, refs...)...)
}

// withToken runs script with args as $1... in a throwaway git container
// that gets token by name, so its value travels in the docker command's
// environment, never in argv, and askpass answers git with it.
func withToken(ctx context.Context, x Exec, labels, mounts []string, token, script string, args ...string) error {
	argv := append([]string{"docker", "run", "--rm"}, labels...)
	argv = append(argv, "-e", "GIT_TOKEN", "-e", "GIT_ASKPASS=/tmp/askpass", "-e", "GIT_TERMINAL_PROMPT=0")
	argv = append(argv, mounts...)
	argv = append(argv, "--entrypoint", "sh", GitImage, "-c", askpassScript, "sh", "sh", "-c", script, "sh")
	_, err := x(ctx, []string{"GIT_TOKEN=" + token}, append(argv, args...)...)
	return err
}
