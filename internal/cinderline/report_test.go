package cinderline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// reportServer answers PUT /report with status (204 unless set) and records
// the errors list of each report it receives.
type reportServer struct {
	mu     sync.Mutex
	status int
	got    []string
}

func (s *reportServer) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/runner/v1/report" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var rep struct {
			RunnerVersion string          `json:"runner_version"`
			Errors        json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(b, &rep); err != nil || rep.RunnerVersion != version {
			t.Errorf("report body %s", b)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.got = append(s.got, string(rep.Errors))
		if s.status == 0 {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, `{"message":"down"}`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, runnerToken, version)
	c.HTTP = srv.Client()
	return c
}

func (s *reportServer) reports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func (s *reportServer) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// CSR-60, decision 2026-11-08-errors-reach-cinderline: the report carries the
// errors held in memory, is sent only when it differs from the last report
// the server accepted, and SetError says whether an error is new or changed.
func TestSyncReportOncePerChange(t *testing.T) {
	srv := &reportServer{}
	c := srv.start(t)
	ctx := context.Background()
	base := Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}}
	sync := func(want ...string) {
		t.Helper()
		before := len(srv.reports())
		if err := c.SyncReport(ctx, base); err != nil {
			t.Fatalf("SyncReport: %v", err)
		}
		if got := srv.reports()[before:]; len(got) != len(want) || len(want) == 1 && got[0] != want[0] {
			t.Fatalf("reports sent = %q, want %q", got, want)
		}
	}

	sync(`[]`) // the first report always goes, with an empty list
	sync()     // unchanged: nothing sent

	if !c.SetError(ReportError{Kind: KindPollFailed, Message: "work: 500"}) {
		t.Error("a new error is not a change")
	}
	if c.SetError(ReportError{Kind: KindPollFailed, Message: "work: 500"}) {
		t.Error("the same error again is a change")
	}
	if !c.SetError(ReportError{Kind: KindMissingKey, Issue: "CSR-9", Message: "no openai key"}) {
		t.Error("a new issue error is not a change")
	}
	if !c.SetError(ReportError{Kind: KindMissingKey, Issue: "CSR-9", Message: "no gemini key"}) {
		t.Error("a new message for the same issue is not a change")
	}
	if !c.SetError(ReportError{Kind: KindUnsupportedClient, Issue: "CSR-3", Message: "aider"}) {
		t.Error("an error on another issue is not a change")
	}
	// Sorted by kind, then issue; one error per issue, the latest.
	sync(`[{"kind":"missing_key","issue":"CSR-9","message":"no gemini key"},{"kind":"poll_failed","message":"work: 500"},{"kind":"unsupported_client","issue":"CSR-3","message":"aider"}]`)
	sync()

	c.ClearError(KindPollFailed)
	c.KeepIssueErrors(map[string]bool{"CSR-9": true, "CSR-4": true})
	sync(`[{"kind":"missing_key","issue":"CSR-9","message":"no gemini key"}]`)

	// A report the server refused is sent again on the next sync.
	c.ClearError("CSR-9")
	srv.fail(500)
	if err := c.SyncReport(ctx, base); err == nil {
		t.Fatal("SyncReport hid the server's refusal")
	}
	srv.fail(0)
	sync(`[]`)
	sync()

	if !c.SetError(ReportError{Kind: KindMissingKey, Issue: "CSR-9", Message: "no gemini key"}) {
		t.Error("an error that was cleared and came back is not a change")
	}
}

// CSR-59: detection hands its result to SetClients; the next SyncReport sends
// it, and an unchanged result sends no report.
func TestSyncReportSendsClientsOnlyWhenChanged(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var rep struct {
			Clients json.RawMessage `json:"clients"`
		}
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Errorf("report body %s", b)
		}
		mu.Lock()
		got = append(got, string(rep.Clients))
		mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, runnerToken, version)
	c.HTTP = srv.Client()
	ctx := context.Background()
	base := Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}}
	v := "2.1.316"
	sync := func(want ...string) {
		t.Helper()
		mu.Lock()
		before := len(got)
		mu.Unlock()
		if err := c.SyncReport(ctx, base); err != nil {
			t.Fatalf("SyncReport: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if sent := got[before:]; len(sent) != len(want) || len(want) == 1 && sent[0] != want[0] {
			t.Fatalf("clients sent = %q, want %q", sent, want)
		}
	}

	sync(`[]`) // before any detection: the base report's empty list
	c.SetClients([]ReportClient{{Name: "claude", Version: &v, State: "ready"}, {Name: "codex", State: "missing_credential"}})
	sync(`[{"name":"claude","version":"2.1.316","state":"ready"},{"name":"codex","version":null,"state":"missing_credential"}]`)
	c.SetClients([]ReportClient{{Name: "claude", Version: &v, State: "ready"}, {Name: "codex", State: "missing_credential"}})
	sync() // unchanged result sends no report
	c.SetClients([]ReportClient{{Name: "claude", Version: &v, State: "invalid_credential"}, {Name: "codex", State: "missing_credential"}})
	sync(`[{"name":"claude","version":"2.1.316","state":"invalid_credential"},{"name":"codex","version":null,"state":"missing_credential"}]`)
	sync()
}

// CSR-63: the runner counts the connections the egress proxy refused, per
// project since it started, and sends the count in the report, so a model
// that probes once in 100,000 turns still shows up. Zero is left out.
func TestSyncReportCountsRefusedConnections(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var rep map[string]json.RawMessage
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Errorf("report body %s", b)
		}
		mu.Lock()
		got = append(got, string(rep["refused_connections"]))
		mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, runnerToken, version)
	c.HTTP = srv.Client()
	base := Report{RunnerVersion: version, Clients: []ReportClient{}, Environments: []string{"default"}}
	ctx := context.Background()
	sync := func() {
		t.Helper()
		if err := c.SyncReport(ctx, base); err != nil {
			t.Fatal(err)
		}
	}

	sync()
	c.AddRefused()
	c.AddRefused()
	sync()
	sync() // unchanged: nothing sent
	c.AddRefused()
	sync()
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"", "2", "3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("refused_connections sent = %q, want %q", got, want)
	}
}
