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

// CLR-201: a check's log is read from its end with a Range request; a
// server that ignores the range sends it whole and only its end is kept,
// without a character the cut split. An error answer is an error.
func TestLogTail(t *testing.T) {
	g, reqs := recorded(t, "https://github.com/willem/cinderline.git", reply{206, "the end"})
	if s, err := logTail(context.Background(), g.HTTP, "GitHub", "https://api.github.com/x", map[string]string{"Authorization": "Bearer t"}); s != "the end" || err != nil {
		t.Errorf("logTail = %q, %v", s, err)
	}
	if h := (*reqs)[0].header; h.Get("Range") != "bytes=-16384" || h.Get("Authorization") != "Bearer t" || h.Get("Accept") != "text/plain, */*" {
		t.Errorf("header = %v", h)
	}

	whole := "é" + strings.Repeat("x", 3*logBytes-2) + "€" + strings.Repeat("y", logBytes-4) + "END" // € cut in two
	g, _ = recorded(t, "https://github.com/willem/cinderline.git", reply{200, whole})
	s, err := logTail(context.Background(), g.HTTP, "GitHub", "https://api.github.com/x", nil)
	if err != nil || !strings.HasSuffix(s, "END") || len(s) > logBytes || strings.ContainsAny(s, "�x") {
		t.Errorf("logTail of a whole log = %d bytes ending %q, %v", len(s), s[max(0, len(s)-5):], err)
	}

	g, _ = recorded(t, "https://github.com/willem/cinderline.git", reply{404, "Not Found"})
	if _, err := logTail(context.Background(), g.HTTP, "GitHub", "https://api.github.com/x", nil); status(err) != 404 || !strings.Contains(err.Error(), "404: Not Found") {
		t.Errorf("logTail on a 404 = %v", err)
	}
}
