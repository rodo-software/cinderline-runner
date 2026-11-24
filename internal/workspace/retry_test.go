package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// gitFailure is a failed git container as RunCommand reports it: the argv
// on the first line, git's own output after it.
func gitFailure(output string) error {
	return &CommandError{Argv: []string{"docker", "run", "git"}, Err: exitErr(128), Output: output}
}

const (
	resolveFailed = "fatal: unable to access 'https://git.rodo.nl/willem/cinderline/': Could not resolve host: git.rodo.nl (Domain name not found)\n"
	authFailed    = "remote: Invalid username or password.\nfatal: Authentication failed for 'https://git.rodo.nl/willem/cinderline/'\n"
)

// noWaits makes the retries wait nothing.
func noWaits(t *testing.T) {
	t.Helper()
	saved := gitWaits
	gitWaits = []time.Duration{0, 0}
	t.Cleanup(func() { gitWaits = saved })
}

// isFetch: the call is the helper's fetch from the remote, with the token.
func isFetch(argv []string) bool {
	return slices.Contains(argv, "fetch") && slices.Contains(argv, "GIT_TOKEN")
}

func TestFetchRetriesTransientError(t *testing.T) {
	var logs bytes.Buffer
	noWaits(t)
	fetches := 0
	d, calls := scripted(nil, nil)
	d.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	x := d.Exec
	d.Exec = func(ctx context.Context, env []string, argv ...string) (string, error) {
		x(ctx, env, argv...)
		if isFetch(argv) {
			if fetches++; fetches == 1 {
				return "", gitFailure(resolveFailed)
			}
		}
		return "", nil
	}
	if err := d.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch = %v, want success after one retry", err)
	}
	if fetches != 2 || len(*calls) != 4 {
		t.Errorf("%d fetches in %d calls, want 2 in 4 (volume, init, fetch twice)", fetches, len(*calls))
	}
	for _, want := range []string{"project=CSR", "attempt=1", "Could not resolve host: git.rodo.nl"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q lacks %q", logs.String(), want)
		}
	}
	if n := strings.Count(logs.String(), "\n"); n != 1 {
		t.Errorf("%d log lines, want 1: %s", n, logs.String())
	}
}

func TestFetchGivesUpAfterThreeTransientErrors(t *testing.T) {
	noWaits(t)
	fetches := 0
	d, _ := scripted(nil, nil)
	d.Logger = slog.New(slog.DiscardHandler)
	d.Exec = func(_ context.Context, _ []string, argv ...string) (string, error) {
		if isFetch(argv) {
			fetches++
			return "", gitFailure("error: RPC failed; curl 56 Recv failure: Connection reset by peer\nfatal: early EOF " + string(rune('0'+fetches)) + "\n")
		}
		return "", nil
	}
	err := d.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "early EOF 3") {
		t.Errorf("Fetch = %v, want the third failure", err)
	}
	if fetches != 3 {
		t.Errorf("%d fetches, want 3", fetches)
	}
}

func TestPushFailsAtOnceOnAuthentication(t *testing.T) {
	noWaits(t)
	pushes := 0
	d, _ := scripted(nil, nil)
	d.Logger = slog.New(slog.DiscardHandler)
	d.Exec = func(_ context.Context, _ []string, argv ...string) (string, error) {
		if slices.Contains(argv, "push") {
			pushes++
			return "", gitFailure(authFailed)
		}
		return "", nil
	}
	_, err := d.Push(context.Background(), dir, "cinderline/CSR-30")
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("Push = %v, want the authentication failure", err)
	}
	if pushes != 1 {
		t.Errorf("%d pushes, want 1", pushes)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	saved := gitWaits
	gitWaits = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { gitWaits = saved })
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	x := func(context.Context, []string, ...string) (string, error) {
		calls++
		time.AfterFunc(10*time.Millisecond, cancel) // a stop, during the wait
		return "", gitFailure(resolveFailed)
	}
	done := make(chan error)
	go func() { done <- LsRemote(ctx, x, slog.New(slog.DiscardHandler), "CSR", "https://h/o/r", "", "tok") }()
	select {
	case err := <-done:
		if err == nil || calls != 1 {
			t.Errorf("LsRemote = %v after %d calls, want the error after 1", err, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LsRemote still waiting after its context ended")
	}
}

func TestFetchExtrasRetriesTransientError(t *testing.T) {
	noWaits(t)
	fetches := 0
	x := func(_ context.Context, env []string, _ ...string) (string, error) {
		if len(env) > 0 { // only the fetch gets the token
			if fetches++; fetches == 1 {
				return "", gitFailure("fatal: unable to access 'https://h/o/r/': The requested URL returned error: 502\n")
			}
		}
		return "", nil
	}
	failed := FetchExtras(context.Background(), x, slog.New(slog.DiscardHandler), "CSR", ExtraPath, []Extra{{Name: "lib", URL: "https://h/o/r", Token: "t"}})
	if len(failed) != 0 || fetches != 2 {
		t.Errorf("failed = %v after %d fetches, want none after 2", failed, fetches)
	}
}

func TestTransient(t *testing.T) {
	for _, tt := range []struct {
		output string
		want   bool
	}{
		{resolveFailed, true},
		{"fatal: unable to access 'https://h/o/r/': Failed to connect to h port 443 after 5 ms: Couldn't connect to server\n", true},
		{"fatal: unable to access 'https://h/o/r/': Connection timed out after 300000 milliseconds\n", true},
		{"fatal: unable to access 'https://h/o/r/': Failed to connect to h port 443: Connection refused\n", true},
		{"error: RPC failed; curl 56 Recv failure: Connection reset by peer\n", true},
		{"fatal: unable to access 'https://h/o/r/': Operation timed out after 30001 milliseconds\n", true},
		{"fatal: unable to access 'https://h/o/r/': The requested URL returned error: 503\n", true},
		{"error: RPC failed; HTTP 500 curl 22 The requested URL returned error: 500\n", true},
		{"fatal: early EOF\n", true},
		{"fatal: unexpected disconnect while reading sideband packet\n", true},
		{authFailed, false},
		{"error: RPC failed; HTTP 403 curl 22 The requested URL returned error: 403\n", false},
		{"fatal: unable to access 'https://h/o/r/': The requested URL returned error: 401\n", false},
		{"remote: Repository not found.\nfatal: repository 'https://h/o/r/' not found\n", false},
		{"fatal: unable to access 'https://h/o/r/': The requested URL returned error: 404\n", false},
		{" ! [rejected]        x -> x (non-fast-forward)\n", false},
		{"", false},
	} {
		if got := transient(gitFailure(tt.output)) != ""; got != tt.want {
			t.Errorf("transient(%q) = %v, want %v", tt.output, got, tt.want)
		}
	}
	if transient(nil) != "" {
		t.Error("transient(nil) is not empty")
	}
	// Only git's output counts, not the command line, which holds URLs.
	err := &CommandError{Argv: []string{"git", "ls-remote", "https://h/connection-refused"}, Err: exitErr(2)}
	if transient(err) != "" {
		t.Error("the argv made an error transient")
	}
	if transient(errors.New("fatal: early EOF")) == "" {
		t.Error("an error that is not a CommandError is not read")
	}
}
