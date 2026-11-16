package forge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain sends every read once, as the recorded cases expect;
// TestReadRetries turns the retries on.
func TestMain(m *testing.M) {
	readWaits = nil
	os.Exit(m.Run())
}

func withReadWaits(t *testing.T) {
	readWaits = []time.Duration{0, 0}
	t.Cleanup(func() { readWaits = nil })
}

// CSR-181: a read that fails and then answers succeeds; a write is sent once.
func TestReadRetries(t *testing.T) {
	withReadWaits(t)
	g, reqs := recorded(t, "https://github.com/willem/cinderline.git",
		reply{502, `<html>Bad Gateway</html>`}, reply{429, `{"message":"slow down"}`}, reply{200, `{"default_branch":"main"}`})
	if b, err := g.DefaultBranch(context.Background()); b != "main" || err != nil {
		t.Errorf("DefaultBranch = %q, %v; want main", b, err)
	}
	if len(*reqs) != 3 {
		t.Errorf("%d requests, want 3", len(*reqs))
	}

	g, _ = recorded(t, "https://github.com/willem/cinderline.git", reply{502, ""}, reply{502, ""}, reply{503, ""})
	if _, err := g.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("DefaultBranch = %v, want the third answer's 503", err)
	}

	g, reqs = recorded(t, "https://github.com/willem/cinderline.git", reply{502, ""})
	if _, err := g.OpenPullRequest(context.Background(), PullRequestInput{Branch: "cinderline/EMBER-1", Base: "main"}); err == nil {
		t.Error("OpenPullRequest succeeded, want the 502")
	}
	if len(*reqs) != 1 {
		t.Errorf("%d requests for a write, want 1", len(*reqs))
	}

	g, _ = recorded(t, "https://github.com/willem/cinderline.git", reply{404, `{"message":"Not Found"}`})
	if _, err := g.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("DefaultBranch = %v, want the 404 at once", err)
	}
}
