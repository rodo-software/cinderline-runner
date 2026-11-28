package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/ca"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// CLR-303: stop and drain answer 204, flushed, before they call their func,
// which ends the server.
func TestAPIStopAndDrainAnswerFirst(t *testing.T) {
	for _, path := range []string{"/api/daemon/stop", "/api/daemon/drain"} {
		dir := t.TempDir()
		var w *httptest.ResponseRecorder
		called := false
		after := func() {
			called = true
			if w.Code != http.StatusNoContent || !w.Flushed {
				t.Errorf("%s: called at %d, flushed %v; want after a flushed 204", path, w.Code, w.Flushed)
			}
		}
		h := Handler(dir, port, Handlers{Stop: after, Drain: after})
		r := httptest.NewRequest("POST", path, nil)
		r.Host = "127.0.0.1:7420"
		r.Header.Set("Origin", "http://127.0.0.1:7420")
		r.AddCookie(&http.Cookie{Name: name.Name, Value: token(t, dir)})
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !called || w.Code != http.StatusNoContent {
			t.Errorf("%s = %d, called %v; want 204 and the func", path, w.Code, called)
		}
	}
}

// CLR-303: rotate-ca replaces the CA in the state folder.
func TestAPIRotateCA(t *testing.T) {
	dir := t.TempDir()
	old, err := ca.Ensure(ca.Dir(dir), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(dir, port, Handlers{Paths: config.Paths{State: dir}, Log: slog.New(slog.DiscardHandler)})
	if w := send(t, h, "POST", token(t, dir), "/api/daemon/rotate-ca", ""); w.Code != http.StatusNoContent {
		t.Fatalf("POST = %d %s, want 204", w.Code, w.Body)
	}
	now, err := ca.Ensure(ca.Dir(dir), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if now.Cert.Equal(old.Cert) {
		t.Error("the CA was kept")
	}
}

// updateHandler serves the update route on a runner of version 1.2.3 whose
// install step is faked: its new binary answers version with newVersion and
// waits for refresh's error before update --refresh ends. restarts counts
// the calls of Restart.
func updateHandler(t *testing.T, newVersion string) (h http.Handler, tok string, refresh chan error, restarts *atomic.Int32, log *syncBuffer) {
	t.Helper()
	t.Cleanup(func() { updating.Lock(); updating.state = ""; updating.Unlock() }) // package-wide, as a moved page keeps it
	dir := t.TempDir()
	refresh, restarts, log = make(chan error), new(atomic.Int32), new(syncBuffer)
	x := func(_ context.Context, _ []string, argv ...string) (string, error) {
		switch strings.Join(argv[1:], " ") {
		case "version":
			return name.Name + " " + newVersion + "\n", nil
		case "update --refresh":
			return "", <-refresh
		}
		return "", nil // the install script
	}
	h = Handler(dir, port, Handlers{Status: func() localapi.Status { return localapi.Status{Version: "1.2.3"} }, Version: "1.2.3",
		Exec: x, Log: slog.New(slog.NewTextHandler(log, nil)), Restart: func() { restarts.Add(1) }})
	return h, token(t, dir), refresh, restarts, log
}

// updateState is the update field of GET /api/status.
func updateState(t *testing.T, h http.Handler, tok string) string {
	t.Helper()
	var st struct{ Update string }
	getJSON(t, h, tok, "/api/status", &st)
	return st.Update
}

// CLR-303: the update runs in the background: 202, a second one 409 while
// it runs, then the restart once it has refreshed the tools.
func TestAPIUpdate(t *testing.T) {
	h, tok, refresh, restarts, log := updateHandler(t, "9.9.9")
	if w := send(t, h, "POST", tok, "/api/daemon/update", ""); w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s, want 202", w.Code, w.Body)
	}
	if got := updateState(t, h, tok); got != "running" {
		t.Errorf("update %q while it runs, want running", got)
	}
	if w := send(t, h, "POST", tok, "/api/daemon/update", ""); w.Code != http.StatusConflict {
		t.Errorf("second POST = %d %s, want 409", w.Code, w.Body)
	}
	refresh <- nil
	waitUntil(t, "the restart", func() bool { return restarts.Load() == 1 })
	if got := updateState(t, h, tok); got != "running" {
		t.Errorf("update %q while the daemon drains to restart, want running", got)
	}
	if !strings.Contains(log.String(), "9.9.9") {
		t.Errorf("log %q, want the new version", log)
	}
}

// A failed update shows in the status and the log, restarts nothing, and
// may be tried again.
func TestAPIUpdateFails(t *testing.T) {
	h, tok, refresh, restarts, log := updateHandler(t, "9.9.9")
	send(t, h, "POST", tok, "/api/daemon/update", "")
	refresh <- errors.New("docker build failed")
	waitUntil(t, "the failure", func() bool { return strings.HasPrefix(updateState(t, h, tok), "failed: ") })
	if got := updateState(t, h, tok); !strings.Contains(got, "docker build failed") {
		t.Errorf("update %q, want the error", got)
	}
	if !strings.Contains(log.String(), "docker build failed") || restarts.Load() != 0 {
		t.Errorf("log %q, %d restarts; want the error and none", log, restarts.Load())
	}
	if w := send(t, h, "POST", tok, "/api/daemon/update", ""); w.Code != http.StatusAccepted {
		t.Errorf("POST after a failure = %d, want 202", w.Code)
	}
	refresh <- nil
	waitUntil(t, "the restart", func() bool { return restarts.Load() == 1 })
}

// An update that finds the running version current clears the field and
// restarts nothing.
func TestAPIUpdateAlreadyCurrent(t *testing.T) {
	h, tok, _, restarts, _ := updateHandler(t, "1.2.3")
	send(t, h, "POST", tok, "/api/daemon/update", "")
	waitUntil(t, "the end of the update", func() bool { return updateState(t, h, tok) == "" })
	if restarts.Load() != 0 {
		t.Error("restarted although already current")
	}
}

// syncBuffer is a bytes.Buffer the update's goroutine can log to while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %s", what)
		}
	}
}
