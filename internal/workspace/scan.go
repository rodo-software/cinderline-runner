package workspace

import (
	"cmp"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// The push scan (decision 2026-11-17-open-questions-answered, point 2): a
// secret the runner knows stops the push; a well-known shape of secret it
// does not know is only a warning.

// Secret is a secret the scan found in a commit about to be pushed: its kind
// and place, never its value.
type Secret struct {
	Kind   string // the redact label, such as "anthropic key", or a shape below
	Commit string
	Path   string // "" when it is in the commit's message or author
	Known  bool   // a value the runner holds, which stops the push
}

// ErrSecret is the error of a push the scan refused.
var ErrSecret = errors.New("the branch holds a secret the runner knows, so it is not pushed")

// shapes are well-known secret formats; the first that matches a line names it.
var shapes = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----`)},
	{"anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"openrouter key", regexp.MustCompile(`sk-or-v1-[0-9a-f]{32,}`)},
	{"openai key", regexp.MustCompile(`sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}`)},
	{"github token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}`)},
	{"gitlab token", regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{"aws access key", regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`)},
	{"google api key", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"slack token", regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`)},
}

// scanArgs is git log of every commit on branch that no other branch of the
// helper's mirror of the remote has, with its patch: all of the branch's own
// work, so the last push of a run reports everything. --text, --no-textconv
// and --no-ext-diff keep attributes in the repository from hiding a file's
// lines; a merge shows only what differs from git's own merge.
func scanArgs(scratch, branch string) []string {
	return []string{"-C", RepoPath, "log", "-p", "--text", "--no-color", "--no-textconv", "--no-ext-diff",
		"--diff-merges=remerge", "--pretty=medium", "--no-decorate", scratch, "--not", "--exclude=refs/heads/" + branch, "--glob=refs/heads/*"}
}

// scan finds secrets in the output of scanArgs: in each commit's header and
// message, and in the lines its patch adds. Lines it removes or leaves are
// not looked at. Known ones come first, so a cap on the list keeps them.
//
// Only git starts a line with "commit ", "diff --git " or "@@": message
// lines are indented and patch lines start with "+", "-", " " or "\".
func scan(log string) []Secret {
	var found []Secret
	seen := map[Secret]bool{}
	var commit, path string
	const message, header, hunk = 0, 1, 2
	state := message
	for line := range strings.Lines(log) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "commit "):
			commit, path, state = strings.TrimPrefix(line, "commit "), "", message
		case strings.HasPrefix(line, "diff --git "):
			path, state = "", header
			continue
		case strings.HasPrefix(line, "@@"):
			state = hunk
			continue
		case state == header && strings.HasPrefix(line, "+++ "):
			path = filePath(line[4:])
		case state != message && !strings.HasPrefix(line, "+"):
			continue
		}
		s := Secret{Commit: commit, Path: path}
		s.Kind, s.Known = redact.Find(line)
		for i := 0; s.Kind == "" && i < len(shapes); i++ {
			if shapes[i].re.MatchString(line) {
				s.Kind = shapes[i].kind
			}
		}
		if s.Kind != "" && !seen[s] {
			seen[s] = true
			found = append(found, s)
		}
	}
	slices.SortStableFunc(found, func(a, b Secret) int {
		if a.Known == b.Known {
			return 0
		}
		if a.Known {
			return -1
		}
		return 1
	})
	return found
}

// filePath is the path of a "+++ " line: git quotes unusual names C-style
// and ends names with a space in a tab.
func filePath(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if u, err := strconv.Unquote(s); err == nil && strings.HasPrefix(s, `"`) {
		s = u
	}
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, "b/")
}

// hasKnown reports whether found holds a secret the runner knows.
func hasKnown(found []Secret) bool {
	return slices.ContainsFunc(found, func(s Secret) bool { return s.Known })
}

// describe names the known secrets of found and where they are, for the error.
func describe(found []Secret) string {
	var parts []string
	for _, s := range found {
		if s.Known {
			parts = append(parts, s.Kind+" in "+s.Commit[:min(len(s.Commit), 12)]+" "+cmp.Or(s.Path, "(commit message)"))
		}
	}
	return strings.Join(parts, ", ")
}
