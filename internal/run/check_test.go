package run

import (
	"fmt"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/environment"
)

// CSR-80: the runner runs the project's check command when a check step
// asks for it (next_test.go); the fix loop, its count and the verdict are
// the app's (decision 2026-11-16-check-step). These are the helpers of
// those tests.

// exitError is a check that ran and failed, as the Docker environment returns it.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

func pass() environment.RunReply { return environment.RunReply{Output: "ok"} }
func fail(out string) environment.RunReply {
	return environment.RunReply{Output: out, Err: exitError(1)}
}

func envRuns(e *environment.Fake) []environment.Spec {
	var out []environment.Spec
	for _, c := range e.Calls() {
		if c.Method == "Run" {
			out = append(out, c.Args[0].(environment.Spec))
		}
	}
	return out
}

func TestAPublishCommentStartsWithTheVerdict(t *testing.T) {
	if got := withVerdict("Checks passed on the first try.", "Opened x."); got != "Checks passed on the first try.\n\nOpened x." {
		t.Errorf("withVerdict = %q", got)
	}
	if got := withVerdict("", "Opened x."); got != "Opened x." {
		t.Errorf("withVerdict without a verdict = %q", got)
	}
}
