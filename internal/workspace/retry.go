package workspace

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// gitWaits are the waits between the tries of a git command that needs the
// network: three tries over about half a minute, so a blip of the network or
// of Docker's DNS doesn't fail the run (CLR-220). A variable so tests need
// not wait.
var gitWaits = []time.Duration{5 * time.Second, 25 * time.Second}

// Git's own words for a network failure that may pass, and for a refusal
// that a retry can't fix and that, repeated, can get a token or IP blocked.
// A refusal wins: "RPC failed; HTTP 403" is one.
var (
	transientGit = regexp.MustCompile(`(?i)could not resolve host|failed to connect|connection timed out|connection refused|connection reset|operation timed out|returned error: 5\d\d|rpc failed|early eof|unexpected disconnect`)
	refusedGit   = regexp.MustCompile(`(?i)authentication failed|returned error: 4\d\d|http 4\d\d|repository.*not found`)
)

// retryNetwork runs call, a git command that needs the network, again after
// each of gitWaits while it fails with a transient network error. It
// returns call's last result, or at once when ctx ends.
func retryNetwork(ctx context.Context, log *slog.Logger, project string, call func() (string, error)) (string, error) {
	for attempt := 1; ; attempt++ {
		out, err := call()
		line := transient(err)
		if line == "" || attempt > len(gitWaits) || ctx.Err() != nil {
			return out, err
		}
		log.Warn("git network error, retrying", "project", project, "attempt", attempt, "err", line)
		t := time.NewTimer(gitWaits[attempt-1])
		select {
		case <-ctx.Done():
			t.Stop()
			return out, err
		case <-t.C:
		}
	}
}

// transient is the line of git's output that makes err a transient network
// error, or empty. Only the output counts: a CommandError's command line
// holds URLs and scripts.
func transient(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if ce := (*CommandError)(nil); errors.As(err, &ce) {
		text = ce.Output
	}
	if refusedGit.MatchString(text) {
		return ""
	}
	for line := range strings.Lines(text) {
		if transientGit.MatchString(line) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
