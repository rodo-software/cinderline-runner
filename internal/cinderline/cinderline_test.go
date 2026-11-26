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
	"reflect"
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

// CLR-194: every poll says whether the Project Manager slot is free, false
// too, as sending the field claims the slot; an answer without manager_job,
// as older apps send, offers none, and one with it reads like job.
func TestPollManagerSlot(t *testing.T) {
	s := &server{status: []int{200}, body: `{"poll_after":30,"project":{"key":"CSR"},"job":null}`}
	c := s.client(t)
	ans, err := c.Poll(context.Background(), PollRequest{Running: []string{}})
	if err != nil || ans.ManagerJob != nil || !strings.Contains(s.bodies[0], `"manager_idle":false`) {
		t.Errorf("answer %+v, %v; body %s", ans, err, s.bodies[0])
	}
	s = &server{status: []int{200}, body: `{"poll_after":30,"project":{"key":"CSR"},"job":null,"manager_job":{"issue":{"ref":"CSR-2","title":"Plan","body":"b"},"team":{"name":"Project Manager","agents":[{"name":"Project Manager","client":"claude"}]}}}`}
	c = s.client(t)
	ans, err = c.Poll(context.Background(), PollRequest{Running: []string{}, ManagerIdle: true})
	if err != nil || ans.ManagerJob == nil || ans.ManagerJob.Issue.Ref != "CSR-2" || ans.ManagerJob.Team.Agents[0].Name != "Project Manager" || !strings.Contains(s.bodies[0], `"manager_idle":true`) {
		t.Errorf("answer %+v, %v; body %s", ans, err, s.bodies[0])
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

// A step's error reaches agents through the app's notes, so it names no
// URL, such as the forge's in a failed push (CL decision
// 2026-11-25-forge-off-limits-for-agents).
func TestNextErrorHasNoURLs(t *testing.T) {
	s := &server{status: []int{200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	msg := "docker run --rm alpine/git -C /repo push --quiet https://git.example.com/o/r.git refs/push/b:refs/heads/b: exit status 128\n" +
		"fatal: unable to access 'https://git.example.com/o/r.git/': Could not resolve host"
	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "publish", Outcome: "failed", Error: &msg}}); err != nil {
		t.Fatal(err)
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	want := "docker run --rm alpine/git -C /repo push --quiet <URL> refs/push/b:refs/heads/b: exit status 128\n" +
		"fatal: unable to access '<URL>': Could not resolve host"
	if next.Last.Error == nil || *next.Last.Error != want {
		t.Errorf("sent %s", s.bodies[0])
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

// CLR-193, CLR-248: the refused hosts are cut to the app's limits, the
// first 20 that fit 260 characters with a known reason; a host that holds a
// secret is left out, not redacted, so every entry stays a host:port. A
// method or URL that does not fit, holds a secret or still has a query,
// fragment or user info is left out with the other, the host kept. None
// leaves the key out.
func TestNextRefusedFit(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200, 200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	get := "GET"
	req := func(host, reason, method, u string) RefusedHost {
		return RefusedHost{Host: host, Reason: reason, Method: &method, URL: &u}
	}
	hosts := []RefusedHost{
		{Host: strings.Repeat("a", 259) + ":1", Reason: RefusedNotAllowed},
		{Host: "secret-value-1234567890.evil.example:443", Reason: RefusedNotAllowed},
		{Host: "odd.example:443", Reason: "made_up"},
		req("a.example:443", RefusedNotAllowed, "GET", "https://a.example/p"),
		req("b.example:443", RefusedPrivateAddress, "GET", "https://b.example/"+strings.Repeat("x", 2000)),
		req("c.example:443", RefusedStrict, "GET", "https://c.example/secret-value-1234567890"),
		req("d.example:443", RefusedRunnerMachine, strings.Repeat("M", 21), "https://d.example/"),
		req("e.example:443", RefusedNotAllowed, "GET", "https://e.example/p?q=1"),
		req("f.example:443", RefusedNotAllowed, "GET", "https://u:p@f.example/p"),
		req("g.example:443", RefusedNotAllowed, "GET", "https://g.example/p#f"),
		req("h.example:443", RefusedNotAllowed, "GET", "file:///etc/passwd"),
		{Host: "i.example:443", Reason: RefusedNotAllowed, Method: &get},
	}
	for i := range 25 {
		hosts = append(hosts, RefusedHost{Host: fmt.Sprintf("h%d.example:443", i), Reason: RefusedNotAllowed})
	}
	for _, last := range []*LastStep{{Step: "run", Refused: hosts}, {Step: "run"}} {
		if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: last}); err != nil {
			t.Fatal(err)
		}
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	got := next.Last.Refused
	if len(got) != MaxRefused || got[0].Host != "a.example:443" || got[8].Host != "i.example:443" || got[9].Host != "h0.example:443" || got[19].Host != "h10.example:443" {
		t.Fatalf("sent %+v", got)
	}
	if got[0].Method == nil || *got[0].Method != "GET" || got[0].URL == nil || *got[0].URL != "https://a.example/p" {
		t.Errorf("a.example lost its request: %+v", got[0])
	}
	for i, r := range got[1:9] {
		if r.Method != nil || r.URL != nil {
			t.Errorf("entry %d %s kept %v %v, want neither", i+1, r.Host, r.Method, r.URL)
		}
	}
	if got[1].Reason != RefusedPrivateAddress || got[2].Reason != RefusedStrict || got[3].Reason != RefusedRunnerMachine {
		t.Errorf("reasons = %+v", got[1:4])
	}
	if strings.Contains(s.bodies[0], `"method":null`) || strings.Contains(s.bodies[1], "refused") {
		t.Errorf("sent %s and %s, want no empty fields and no refused", s.bodies[0], s.bodies[1])
	}
}

// CLR-257: the skill packages' states are cut to the app's limits, the
// first 10 with names of at most 64 characters; none leaves the key out.
func TestNextSkillsFit(t *testing.T) {
	s := &server{status: []int{200, 200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	skills := []SkillState{{Name: strings.Repeat("a", 70), State: SkillFailed}}
	for i := range 12 {
		skills = append(skills, SkillState{Name: fmt.Sprintf("p%d", i), State: SkillLoaded})
	}
	for _, ws := range []RunWorkspace{{Skills: skills}, {}} {
		if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Workspace: ws}); err != nil {
			t.Fatal(err)
		}
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	if got := next.Workspace.Skills; len(got) != maxSkills || got[0].Name != strings.Repeat("a", 64) || got[9] != (SkillState{Name: "p8", State: SkillLoaded}) {
		t.Errorf("sent %v", got)
	}
	if strings.Contains(s.bodies[1], `"skills"`) {
		t.Errorf("sent %s, want no skills", s.bodies[1])
	}
}

// A job's skill packages decode, a null path as the repository's root.
func TestJobSkillsDecode(t *testing.T) {
	var j Job
	body := `{"issue":{"ref":"CSR-1"},"skills":[{"name":"impeccable","url":"https://github.com/o/impeccable","commit":"` + strings.Repeat("a", 40) + `","path":"plugin"},{"name":"p","url":"https://git.example/p","commit":"` + strings.Repeat("b", 40) + `","path":null}]}`
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatal(err)
	}
	if len(j.Skills) != 2 || j.Skills[0].Path != "plugin" || j.Skills[1].Path != "" || j.Skills[1].URL != "https://git.example/p" {
		t.Errorf("skills %+v", j.Skills)
	}
}

// CLR-221: the tool calls are cut to the app's limits: the first 200 whose
// name has 1 to 100 characters, each at most 1,000,000 calls. A name that
// holds a secret is left out, not redacted, as is one too long: either would
// count another tool. None leaves the key out.
func TestNextToolCallsFit(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200, 200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	tools := []ToolCalls{{Name: strings.Repeat("a", 101), Calls: 1}, {Name: "mcp__secret-value-1234567890__x", Calls: 1}, {Name: "", Calls: 1}, {Name: "Bash", Calls: 2_000_000}}
	for i := range 205 {
		tools = append(tools, ToolCalls{Name: fmt.Sprintf("t%d", i), Calls: 1})
	}
	for _, u := range []*Usage{{Issue: "CSR-1", Tools: tools}, {Issue: "CSR-1"}} {
		if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "run", Usage: u}}); err != nil {
			t.Fatal(err)
		}
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	if got := next.Last.Usage.Tools; len(got) != maxTools || got[0] != (ToolCalls{Name: "Bash", Calls: maxToolCalls}) || got[1].Name != "t0" || got[199].Name != "t198" {
		t.Errorf("sent %v", got)
	}
	if strings.Contains(s.bodies[0], "secret-value") || strings.Contains(s.bodies[1], `"tools"`) {
		t.Errorf("sent %s and %s", s.bodies[0], s.bodies[1])
	}
	if tools[3].Calls != 2_000_000 {
		t.Error("the caller's list was changed")
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
	// Written atomically: no temporary file is left, and only this user reads it.
	if entries, _ := os.ReadDir(filepath.Dir(c.Outbox)); len(entries) != 1 {
		t.Errorf("outbox folder = %v; want the outbox only", entries)
	}
	if fi, err := os.Stat(c.Outbox); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("outbox mode = %v, %v; want 0600", fi, err)
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

// Decisions 2026-11-17-provider-models and 2026-11-24-model-lists: at most
// 50 providers and 500 model ids each; an id the app would refuse is left
// out, and the age goes along.
func TestProviderModelsFitTheAppsLimits(t *testing.T) {
	s := &server{status: []int{200}, body: `{"poll_after":30}`}
	c := s.client(t)
	ids := []string{"bad id", "badé", strings.Repeat("m", 151), "", strings.Repeat("m", 150), "llama3.1:8b", "org/model_v-2"}
	for i := range 550 {
		ids = append(ids, "m"+strconv.Itoa(i))
	}
	age := 42
	providers := []ProviderModels{{Provider: strings.Repeat("p", 51), Models: []string{"x"}}, {Provider: "studio", Models: ids, ModelsAge: &age}, {Provider: "down"}}
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
	if len(got) != 500 || got[0] != strings.Repeat("m", 150) || got[1] != "llama3.1:8b" || got[2] != "org/model_v-2" || got[3] != "m0" {
		t.Errorf("studio's models: %d, first %q", len(got), got[:min(4, len(got))])
	}
	if pm[0].ModelsAge == nil || *pm[0].ModelsAge != 42 || pm[1].ModelsAge != nil {
		t.Errorf("ages %v and %v, want 42 and none", pm[0].ModelsAge, pm[1].ModelsAge)
	}
	if !strings.Contains(s.bodies[0], `"models":[]`) || strings.Contains(s.bodies[0], `"models_age":null`) || len(providers[1].Models) != 557 {
		t.Errorf("an empty list must be sent as [] and the caller's list stay: %s", s.bodies[0][:200])
	}
}

// Decision 2026-11-24-model-lists: a client's list holds at most 500 ids of
// at most 100 characters the app accepts, its order kept; a client without
// a list sends neither key.
func TestClientModelsFitTheAppsLimits(t *testing.T) {
	s := &server{status: []int{200}, body: `{"poll_after":30}`}
	c := s.client(t)
	ids := []string{"claude-opus-5-5", strings.Repeat("m", 101), strings.Repeat("m", 100), "bad id", "claude-opus-5-5[1m]"}
	for i := range 550 {
		ids = append(ids, "m"+strconv.Itoa(i))
	}
	age := 7
	clients := []ReportClient{{Name: "claude", State: "ready", Models: ids, ModelsAge: &age}, {Name: "opencode", State: "ready"}, {Name: "codex", State: "ready", Models: []string{}, ModelsAge: &age}}
	if _, err := c.Poll(context.Background(), PollRequest{Report: Report{Clients: clients}, Running: []string{}}); err != nil {
		t.Fatal(err)
	}
	var poll PollRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &poll)
	got := poll.Report.Clients
	if m := got[0].Models; len(m) != 500 || m[0] != "claude-opus-5-5" || m[1] != strings.Repeat("m", 100) || m[2] != "m0" || *got[0].ModelsAge != 7 {
		t.Errorf("claude's models: %d, first %q", len(m), m[:min(3, len(m))])
	}
	if strings.Contains(s.bodies[0], `"name":"opencode","version":null,"state":"ready","models`) {
		t.Errorf("opencode sent a list: %s", s.bodies[0])
	}
	if !strings.Contains(s.bodies[0], `"name":"codex","version":null,"state":"ready","models":[],"models_age":7`) {
		t.Errorf("an empty list must be sent as []: %s", s.bodies[0])
	}
	if len(clients[0].Models) != 555 {
		t.Error("the caller's list changed")
	}
}

func TestPollLimitsFreeProvidersToReportedProviders(t *testing.T) {
	s := &server{status: []int{200}, body: `{"poll_after":30}`}
	c := s.client(t)
	ctx := context.Background()

	poll := func(models, free []string) {
		t.Helper()
		if _, err := c.Poll(ctx, PollRequest{Report: Report{ModelProviders: models, FreeProviders: free}}); err != nil {
			t.Fatal(err)
		}
	}
	providers := make([]string, 51)
	for i := range providers {
		providers[i] = "provider-" + strconv.Itoa(i)
	}
	tooLong := strings.Repeat("x", 51)
	models := append([]string{tooLong, "openrouter"}, providers...)
	free := append([]string{"openrouter", tooLong, "unknown", providers[50]}, providers[:50]...)
	poll(models, free)
	var req PollRequest
	if err := json.Unmarshal([]byte(s.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.Report.ModelProviders, models[1:51]) || !slices.Equal(req.Report.FreeProviders, providers[:49]) {
		t.Errorf("model_providers = %q, free_providers = %q", req.Report.ModelProviders, req.Report.FreeProviders)
	}
	poll(providers, providers)
	if err := json.Unmarshal([]byte(s.bodies[1]), &req); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.Report.FreeProviders, providers[:50]) {
		t.Errorf("free_providers = %q, want the first 50", req.Report.FreeProviders)
	}
	poll([]string{"openrouter", "paid"}, []string{"openrouter", "unknown"})
	poll([]string{"paid"}, nil)
	for _, body := range s.bodies[2:] {
		if strings.Contains(body, `"free_providers"`) {
			t.Errorf("empty free_providers should be omitted: %s", body)
		}
	}
}

// CLR-253, decision 2026-11-25-provider-places in the app repository: the
// poll carries the free places of the providers with a limit, the first 50,
// and leaves the field out without one; a job names the provider it takes
// a place on, "" for null or an older app.
func TestPollProviderPlaces(t *testing.T) {
	s := &server{status: []int{200}, body: `{"poll_after":30,"jobs":[{"issue":{"ref":"CSR-1","title":"T","body":""},"team":{"name":"Dev","max_minutes":1,"max_turns":1,"agents":[{"name":"X","client":"opencode"}]},"provider":"lan"},{"issue":{"ref":"CSR-2","title":"T","body":""},"team":{"name":"Dev","max_minutes":1,"max_turns":1,"agents":[{"name":"X","client":"claude"}]},"provider":null}],"manager_job":{"issue":{"ref":"CSR-3","title":"T","body":""},"team":{"name":"PM","max_minutes":1,"max_turns":1,"agents":[{"name":"X","client":"claude"}]}}}`}
	c := s.client(t)
	places := make([]ProviderPlace, 51)
	for i := range places {
		places[i] = ProviderPlace{Provider: "p" + strconv.Itoa(i), Free: i}
	}
	ans, err := c.Poll(context.Background(), PollRequest{ProviderPlaces: places})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Jobs[0].Provider != "lan" || ans.Jobs[1].Provider != "" || ans.ManagerJob.Provider != "" {
		t.Errorf("providers %q, %q, %q; want lan and none", ans.Jobs[0].Provider, ans.Jobs[1].Provider, ans.ManagerJob.Provider)
	}
	var req PollRequest
	if err := json.Unmarshal([]byte(s.bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.ProviderPlaces, places[:50]) || !strings.Contains(s.bodies[0], `"provider_places":[{"provider":"p0","free":0},`) {
		t.Errorf("provider_places = %+v, want the first 50, a 0 sent too", req.ProviderPlaces)
	}
	if _, err := c.Poll(context.Background(), PollRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.bodies[1], "provider_places") {
		t.Errorf("no provider with a limit, yet the field is sent: %s", s.bodies[1])
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
		Waiting: []WaitingRun{{Ref: strings.Repeat("R", 60), Provider: "lan"}, {Ref: "CSR-1", Provider: "lan"}, {Ref: "CSR-30", Provider: "lan"}},
		Review:  []ReviewFact{{Ref: "CSR-1", PullRequest: "open", Conflicts: conflicts}},
	}); err != nil {
		t.Fatal(err)
	}
	var poll PollRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &poll)
	if len(poll.Running) != maxRunning || poll.Running[0] != "CSR-0" || len(poll.Held) != maxListed || poll.Held[0] != "CSR-0" ||
		utf8.RuneCountInString(poll.Report.Errors[0].Message) != MaxErrorMessage ||
		len(poll.Review[0].Conflicts) != maxConflicts || utf8.RuneCountInString(poll.Review[0].Conflicts[0]) != maxText ||
		len(poll.Report.ModelProviders) != maxProviders || poll.Report.ModelProviders[0] != "CSR-0" {
		t.Errorf("poll sent running %d, held %d, message %d, conflicts %d", len(poll.Running), len(poll.Held), utf8.RuneCountInString(poll.Report.Errors[0].Message), len(poll.Review[0].Conflicts))
	}
	if want := []WaitingRun{{Ref: "CSR-1", Provider: "lan"}}; !slices.Equal(poll.Waiting, want) {
		t.Errorf("poll sent waiting %+v, want only the run of running %+v", poll.Waiting, want)
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

// CLR-201: the failed checks of a publish report and of a Review fact are
// cut to the app's limits, the first 20 with a name, each text redacted, a
// log keeping its end; none leaves the key out.
func TestFailedChecksFitAndAreRedacted(t *testing.T) {
	redact.Add("test secret", "secret-value-1234567890")
	s := &server{status: []int{200, 200, 200}, body: `{"state":"x","step":{"kind":"done"}}`}
	c := s.client(t)
	cs := []FailedCheck{{Name: " "}, {
		Name:        strings.Repeat("n", 300),
		Description: "token secret-value-1234567890 " + strings.Repeat("d", 1100),
		URL:         "https://ci.example/" + strings.Repeat("u", 2100),
		Log:         "secret-value-1234567890 " + strings.Repeat("é", 5000) + " secret-value-1234567890 END",
	}}
	for i := range 25 {
		cs = append(cs, FailedCheck{Name: fmt.Sprintf("check %d", i)})
	}
	want := func(got []FailedCheck) {
		t.Helper()
		if len(got) != MaxChecks || got[1].Name != "check 0" || got[19].Name != "check 18" {
			t.Fatalf("sent %d checks, first %+v", len(got), got[:min(2, len(got))])
		}
		g := got[0]
		if utf8.RuneCountInString(g.Name) != maxCheckName || utf8.RuneCountInString(g.Description) != maxText || !strings.HasPrefix(g.Description, "token [redacted test secret] ") ||
			utf8.RuneCountInString(g.URL) != maxURL || utf8.RuneCountInString(g.Log) != maxTail || !strings.HasSuffix(g.Log, " [redacted test secret] END") {
			t.Errorf("sent name %d, description %d %.40q, url %d, log %d ending %q", utf8.RuneCountInString(g.Name), utf8.RuneCountInString(g.Description), g.Description,
				utf8.RuneCountInString(g.URL), utf8.RuneCountInString(g.Log), g.Log[len(g.Log)-40:])
		}
	}

	failed := "failed"
	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "publish", Outcome: "published", Merge: &failed, Checks: cs}}); err != nil {
		t.Fatal(err)
	}
	var next NextRequest
	_ = json.Unmarshal([]byte(s.bodies[0]), &next)
	want(next.Last.Checks)

	if _, err := c.Poll(context.Background(), PollRequest{Review: []ReviewFact{{Ref: "CSR-1", PullRequest: "failed", Conflicts: []string{}, Checks: cs}}}); err != nil {
		t.Fatal(err)
	}
	var poll PollRequest
	_ = json.Unmarshal([]byte(s.bodies[1]), &poll)
	want(poll.Review[0].Checks)
	if strings.Contains(s.bodies[0]+s.bodies[1], "secret-value") || cs[1].Log[:6] != "secret" {
		t.Error("a secret was sent, or the caller's text was changed")
	}

	if _, err := c.Next(context.Background(), "CSR-1", NextRequest{ID: "id", Last: &LastStep{Step: "publish", Outcome: "published"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.bodies[2], "checks") {
		t.Errorf("sent %s, want no checks", s.bodies[2])
	}
}

// CLR-244: no text of a check carries a URL (CL-375); its URL field keeps its own.
func TestFailedChecksLoseTheirURLs(t *testing.T) {
	got := checks([]FailedCheck{{
		Name:        "ci https://ci.example/x",
		Description: "see https://ci.example.com/build/7?x=1 (details)",
		URL:         "https://ci.example.com/build/7",
		Log:         "fetch \"http://127.0.0.1:8080/a\" failed\nssh://git@host/r.git",
	}})
	want := []FailedCheck{{Name: "ci <URL>", Description: "see <URL> (details)", URL: "https://ci.example.com/build/7", Log: "fetch \"<URL>\" failed\n<URL>"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("checks = %+v\nwant %+v", got, want)
	}
}
