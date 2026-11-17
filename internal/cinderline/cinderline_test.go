package cinderline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// server answers every request with status and body, and records them.
type server struct {
	mu     sync.Mutex
	status []int // in order, the last repeating
	body   string
	header http.Header
	paths  []string
	bodies []string
}

func (s *server) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.bodies = append(s.bodies, string(b))
		s.header = r.Header.Clone()
		status := s.status[0]
		if len(s.status) > 1 {
			s.status = s.status[1:]
		}
		s.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, s.body)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "runner-token", "1.2.3")
	c.HTTP, c.Log = srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

func TestPollSendsTheReportAndReadsTheAnswer(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200}, body: `{"poll_after":30,"project":{"key":"CSR","workspace":null,"base_branch":null,"idle_minutes":20},"job":null,"held":[],"stop":["CSR-1"]}`}
	c := s.client(t)
	ans, err := c.Poll(context.Background(), PollRequest{
		Report:  Report{RunnerVersion: "1.2.3", Errors: []ReportError{{Kind: KindNotStarted, Issue: "CSR-2", Message: "token secret-value-1234567890"}}},
		Running: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ans.PollAfter != 30 || ans.Project.Key != "CSR" || ans.Project.WorkspaceSlug() != "default" || ans.Job != nil || ans.Held == nil || ans.Review != nil || ans.Stop[0] != "CSR-1" {
		t.Errorf("answer = %+v", ans)
	}
	if s.paths[0] != "POST /api/runner/v2/poll" || s.header.Get("Authorization") != "Bearer runner-token" || s.header.Get("Cinderline-Runner-Version") != "1.2.3" {
		t.Errorf("request %s, headers %v", s.paths[0], s.header)
	}
	body := s.bodies[0]
	// model_providers and provider_models are never null (decisions
	// 2026-11-17-local-model-providers and 2026-11-17-provider-models).
	if strings.Contains(body, "secret-value") || !strings.Contains(body, `"running":[]`) || !strings.Contains(body, `"model_providers":[]`) || !strings.Contains(body, `"provider_models":[]`) || strings.Contains(body, `"held"`) || strings.Contains(body, `"review"`) {
		t.Errorf("body = %s", body)
	}
	// An empty review asks for the list.
	if _, err := c.Poll(context.Background(), PollRequest{Running: []string{}, Review: []ReviewFact{}}); err != nil || !strings.Contains(s.bodies[1], `"review":[]`) {
		t.Errorf("body = %s, %v", s.bodies[1], err)
	}
}

func TestNextRedactsWhatItSends(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200}, body: `{"state":"x","step":{"kind":"run","token":null}}`}
	c := s.client(t)
	tail := "printed secret-value-1234567890"
	r := NextRequest{ID: "id", Last: &LastStep{Step: "run", Tail: &tail}, Workspace: RunWorkspace{Notes: []string{"secret-value-1234567890"}}}
	ans, err := c.Next(context.Background(), "CSR-1", r)
	if err != nil || ans.Step.Kind != "run" || ans.Step.Token != nil {
		t.Fatalf("Next = %+v, %v", ans, err)
	}
	if strings.Contains(s.bodies[0], "secret-value") || s.paths[0] != "POST /api/runner/v2/runs/CSR-1/next" {
		t.Errorf("sent %s %s", s.paths[0], s.bodies[0])
	}
	if tail != "printed secret-value-1234567890" {
		t.Error("the caller's text was changed")
	}
}

// The push scan's list is cut to the app's limit, its known secrets first
// as the scan orders them, and a file name that holds a value is redacted.
func TestNextSecretsFitAndAreRedacted(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	path := "keys/secret-value-1234567890.txt"
	found := slices.Repeat([]StepSecret{{Kind: "private key block", Commit: "c1", Path: &path}}, 60)
	found[0].Known = true
	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "stop", Outcome: "secret", Secrets: found}}); err != nil {
		t.Fatal(err)
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	if got := next.Last.Secrets; len(got) != maxSecrets || !got[0].Known || got[1].Path == nil || *got[1].Path != "keys/[redacted test secret].txt" {
		t.Errorf("sent %s", s.bodies[0])
	}
	if path != "keys/secret-value-1234567890.txt" {
		t.Error("the caller's text was changed")
	}
}

// CLR-193: the refused hosts are cut to the app's limits, the first 20 that
// fit 260 characters; a host that holds a secret is left out, not redacted,
// so every entry stays a host:port. None leaves the key out.
func TestNextRefusedHostsFit(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200, 200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	hosts := []string{strings.Repeat("a", 259) + ":1", "secret-value-1234567890.evil.example:443"}
	for i := range 25 {
		hosts = append(hosts, fmt.Sprintf("h%d.example:443", i))
	}
	for _, last := range []*LastStep{{Step: "run", RefusedHosts: hosts}, {Step: "run"}} {
		if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: last}); err != nil {
			t.Fatal(err)
		}
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	if got := next.Last.RefusedHosts; len(got) != maxRefusedHosts || got[0] != "h0.example:443" || got[19] != "h19.example:443" {
		t.Errorf("sent %v", got)
	}
	if strings.Contains(s.bodies[1], "refused_hosts") {
		t.Errorf("sent %s, want no refused_hosts", s.bodies[1])
	}
}

func TestErrorsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		status      int
		body        string
		unavailable bool
		check       func(error) bool
	}{
		{426, `{"minimum_runner_version":"2.0.0"}`, false, func(err error) bool {
			var e *UpgradeRequiredError
			return errors.As(err, &e) && e.MinimumRunnerVersion == "2.0.0"
		}},
		{409, `{"message":"taken","status":"in_progress"}`, false, func(err error) bool {
			var e *ConflictError
			return errors.As(err, &e) && e.Status == "in_progress" && e.Message == "taken"
		}},
		{422, `{"message":"bad"}`, false, func(err error) bool {
			var e *APIError
			return errors.As(err, &e) && e.StatusCode == 422 && e.Message == "bad"
		}},
		{503, `not json`, true, func(err error) bool { return err != nil }},
		{429, ``, true, func(err error) bool { return err != nil }},
		{408, ``, true, func(err error) bool { return err != nil }},
	} {
		c := (&server{status: []int{tc.status}, body: tc.body}).client(t)
		_, err := c.Next(context.Background(), "CSR-1", NextRequest{})
		if !tc.check(err) || Unavailable(err) != tc.unavailable {
			t.Errorf("%d: err %v, unavailable %v", tc.status, err, Unavailable(err))
		}
	}
	if !Unavailable(&url.Error{Op: "Post", URL: "u", Err: errors.New("refused")}) {
		t.Error("no answer is not unavailable")
	}
}

// A Retry-After holds the next request.
func TestRetryAfterHoldsTheNextRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := New(srv.URL, "t", "1")
	c.HTTP, c.Log = srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := c.Poll(context.Background(), PollRequest{}); !Unavailable(err) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.pause(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("pause = %v; want it to wait out the Retry-After", err)
	}
	if d, ok := retryAfter("99999999999"); !ok || d != maxRetryAfter {
		t.Errorf("retryAfter(huge) = %v, %v", d, ok)
	}
}

func TestDownloadAttachmentIsCapped(t *testing.T) {
	s := &server{status: []int{200}, body: strings.Repeat("x", 10)}
	c := s.client(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := c.DownloadAttachment(context.Background(), "CSR-1", 7, path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != s.body || s.paths[0] != "GET /api/runner/v2/issues/CSR-1/attachments/7" {
		t.Errorf("file %q from %s", b, s.paths[0])
	}
	s.body = strings.Repeat("x", MaxAttachmentBytes+1)
	big := filepath.Join(t.TempDir(), "big")
	if err := c.DownloadAttachment(context.Background(), "CSR-1", 8, big); !errors.Is(err, errTooLarge) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(big); !os.IsNotExist(err) {
		t.Error("a refused download left its file")
	}
}

// The outbox keeps requests in order across a restart; a replay stops at
// the first the app does not answer, and drops one it refuses for good.
func TestOutbox(t *testing.T) {
	s := &server{status: []int{200, 422, 503, 200}, body: `{}`}
	c := s.client(t)
	c.Outbox = filepath.Join(t.TempDir(), "outbox.json")
	for _, id := range []string{"a", "b", "c", "d"} {
		c.Queue("CSR-"+id, NextRequest{ID: id})
	}
	// A new client, as after a restart, reads the file.
	again := New("", "", "")
	again.HTTP, again.Log, again.base, again.Outbox = c.HTTP, c.Log, c.base, c.Outbox
	if q := again.Queued(); strings.Join(q, ",") != "CSR-a,CSR-b,CSR-c,CSR-d" {
		t.Fatalf("queued = %v", q)
	}
	again.Replay(context.Background())
	if q := again.Queued(); strings.Join(q, ",") != "CSR-c,CSR-d" {
		t.Errorf("after the replay: %v; want the 503 and the one after it", q)
	}
	again.Replay(context.Background())
	if q := again.Queued(); len(q) != 0 {
		t.Errorf("queued = %v", q)
	}
	if _, err := os.Stat(c.Outbox); !os.IsNotExist(err) {
		t.Error("an empty outbox left its file")
	}
	var ids []string
	for _, b := range s.bodies {
		var r NextRequest
		_ = json.Unmarshal([]byte(b), &r)
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, "") != "abccd" {
		t.Errorf("sent %v", ids)
	}
}

// A request the app answers with a 5xx every time is dropped after
// maxServerErrors replays, its count kept across a restart; the ones after
// it are then sent.
func TestAQueuedRequestTheAppFailsOnIsDroppedInTheEnd(t *testing.T) {
	s := &server{status: append(slices.Repeat([]int{500}, maxServerErrors), 200), body: `{}`}
	c := s.client(t)
	c.Outbox = filepath.Join(t.TempDir(), "outbox.json")
	c.Queue("CSR-a", NextRequest{ID: "a"})
	c.Queue("CSR-b", NextRequest{ID: "b"})
	for range maxServerErrors - 1 {
		c.Replay(context.Background())
	}
	again := New("", "", "")
	again.HTTP, again.Log, again.base, again.Outbox = c.HTTP, c.Log, c.base, c.Outbox
	if again.load(); len(again.queued) != 2 || again.queued[0].ServerErrors != maxServerErrors-1 {
		t.Fatalf("outbox after %d failed replays = %+v", maxServerErrors-1, again.queued)
	}
	again.Replay(context.Background())
	if q := again.Queued(); len(q) != 0 || len(s.bodies) != maxServerErrors+1 {
		t.Errorf("queued %v after %d requests; want a dropped and b delivered", q, len(s.bodies))
	}
}

// No answer at all never drops a request.
func TestAQueuedRequestWaitsOutAnOutage(t *testing.T) {
	c := New("http://127.0.0.1:1", "t", "1")
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	c.Queue("CSR-a", NextRequest{ID: "a"})
	for range maxServerErrors + 2 {
		c.Replay(context.Background())
	}
	if len(c.queued) != 1 || c.queued[0].ServerErrors != 0 {
		t.Errorf("outbox = %+v", c.queued)
	}
}

// Decision 2026-11-17-provider-models: at most 50 providers and 200 model
// ids each; an id the app would refuse in a typed model is left out.
func TestProviderModelsFitTheAppsLimits(t *testing.T) {
	s := &server{status: []int{200}, body: `{"poll_after":30}`}
	c := s.client(t)
	ids := []string{"bad id", "badé", strings.Repeat("m", 151), "", strings.Repeat("m", 150), "llama3.1:8b", "org/model_v-2"}
	for i := range 250 {
		ids = append(ids, "m"+strconv.Itoa(i))
	}
	providers := []ProviderModels{{Provider: strings.Repeat("p", 51), Models: []string{"x"}}, {Provider: "studio", Models: ids}, {Provider: "down"}}
	for i := range 60 {
		providers = append(providers, ProviderModels{Provider: "p" + strconv.Itoa(i), Models: []string{}})
	}
	if _, err := c.Poll(context.Background(), PollRequest{Report: Report{ProviderModels: providers}, Running: []string{}}); err != nil {
		t.Fatal(err)
	}
	var poll PollRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &poll)
	pm := poll.Report.ProviderModels
	if len(pm) != maxProviders || pm[0].Provider != "studio" || pm[1].Provider != "down" || pm[1].Models == nil || len(pm[1].Models) != 0 {
		t.Fatalf("provider_models = %d entries, first %+v", len(pm), pm[:min(2, len(pm))])
	}
	got := pm[0].Models
	if len(got) != maxModels || got[0] != strings.Repeat("m", 150) || got[1] != "llama3.1:8b" || got[2] != "org/model_v-2" || got[3] != "m0" {
		t.Errorf("studio's models: %d, first %q", len(got), got[:min(4, len(got))])
	}
	if !strings.Contains(s.bodies[0], `"models":[]`) || len(providers[1].Models) != 257 {
		t.Errorf("an empty list must be sent as [] and the caller's list stay: %s", s.bodies[0][:200])
	}
}

// Texts and lists are cut to the app's limits, in characters.
func TestRequestsFitTheAppsLimits(t *testing.T) {
	s := &server{status: []int{200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	refs := make([]string, 250)
	for i := range refs {
		refs[i] = "CSR-" + strconv.Itoa(i)
	}
	long := strings.Repeat("é", 3000)
	conflicts := slices.Repeat([]string{long}, 1200)
	if _, err := c.Poll(context.Background(), PollRequest{
		Report:  Report{Errors: []ReportError{{Kind: KindNotStarted, Message: long}}, ModelProviders: append([]string{strings.Repeat("p", 51)}, refs...)},
		Running: append([]string{strings.Repeat("R", 60)}, refs...), Held: refs,
		Review: []ReviewFact{{Ref: "CSR-1", PullRequest: "open", Conflicts: conflicts}},
	}); err != nil {
		t.Fatal(err)
	}
	var poll PollRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &poll)
	if len(poll.Running) != maxRunning || poll.Running[0] != "CSR-0" || len(poll.Held) != maxListed || poll.Held[0] != "CSR-0" ||
		utf8.RuneCountInString(poll.Report.Errors[0].Message) != maxErrorMessage ||
		len(poll.Review[0].Conflicts) != maxConflicts || utf8.RuneCountInString(poll.Review[0].Conflicts[0]) != maxText ||
		len(poll.Report.ModelProviders) != maxProviders || poll.Report.ModelProviders[0] != "CSR-0" {
		t.Errorf("poll sent running %d, held %d, message %d, conflicts %d", len(poll.Running), len(poll.Held), utf8.RuneCountInString(poll.Report.Errors[0].Message), len(poll.Review[0].Conflicts))
	}

	tail, report := strings.Repeat("x", 5000)+"END", strings.Repeat("r", 70000)
	errText := strings.Repeat("e", 20000)
	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{
		ID:        "id",
		Last:      &LastStep{Step: "run", Issue: strings.Repeat("I", 40), Tail: &tail, Report: &report, Error: &errText, Commits: &report, Conflicts: conflicts},
		Workspace: RunWorkspace{Notes: slices.Repeat([]string{long}, 25)},
	}); err != nil {
		t.Fatal(err)
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[1]), &next)
	l := next.Last
	if len(*l.Tail) != maxTail || !strings.HasSuffix(*l.Tail, "END") || len(*l.Report) != maxLongText || len(*l.Commits) != maxLongText ||
		len(*l.Error) != maxStepError || len(l.Conflicts) != maxConflicts || len(next.Workspace.Notes) != maxNotes ||
		utf8.RuneCountInString(next.Workspace.Notes[0]) != maxText || len(l.Issue) != maxIssue {
		t.Errorf("next sent tail %d, report %d, error %d, conflicts %d, notes %d", len(*l.Tail), len(*l.Report), len(*l.Error), len(l.Conflicts), len(next.Workspace.Notes))
	}
	if len(tail) != 5003 || len(conflicts[0]) != 6000 {
		t.Error("the caller's texts were changed")
	}
}
