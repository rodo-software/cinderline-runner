package cinderline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// CLR-245: the reason of a publish report and of a Review fact is sent
// redacted, without URLs (CL-375) and clipped to its first 2,000
// characters; nil, or nothing but spaces, leaves the key out.
func TestReasonFitsAndLosesURLs(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200, 200, 200, 200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	reason := "GitLab answered 405: see https://gitlab.com/g/r/-/merge_requests/5 token secret-value-1234567890 " + strings.Repeat("é", 3000)
	want := func(got *string) {
		t.Helper()
		if got == nil {
			t.Fatal("no reason sent")
		}
		if !strings.HasPrefix(*got, "GitLab answered 405: see <URL> token [redacted test secret] ") || utf8.RuneCountInString(*got) != maxReason || strings.Contains(*got, "http") {
			t.Errorf("reason = %.120q (%d characters)", *got, utf8.RuneCountInString(*got))
		}
	}
	if maxReason != 2000 {
		t.Errorf("maxReason = %d, want the app's 2000", maxReason)
	}

	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "publish", Outcome: "published", Reason: &reason}}); err != nil {
		t.Fatal(err)
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	want(next.Last.Reason)

	if _, err := c.Poll(context.Background(), PollRequest{Review: []ReviewFact{{Ref: "CSR-1", PullRequest: "open", Conflicts: []string{}, Reason: &reason}}}); err != nil {
		t.Fatal(err)
	}
	var poll PollRequest
	_ = json.Unmarshal([]byte(s.bodies[1]), &poll)
	want(poll.Review[0].Reason)
	if !strings.Contains(reason, "https://") {
		t.Error("the caller's reason was changed")
	}

	blank := "  \n "
	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "publish", Outcome: "published", Reason: &blank}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Poll(context.Background(), PollRequest{Review: []ReviewFact{{Ref: "CSR-1", PullRequest: "open", Conflicts: []string{}}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.bodies[2], "reason") || strings.Contains(s.bodies[3], "reason") {
		t.Errorf("sent %s and %s, want no reason", s.bodies[2], s.bodies[3])
	}
}
