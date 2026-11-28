package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

type statsEvent struct {
	Time       time.Time
	Containers []environment.ContainerStats
	Error      *string
}

func nextStats(t *testing.T, data <-chan string) statsEvent {
	t.Helper()
	line := next(t, data, "stats event")
	var ev statsEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("event %s: %v", line, err)
	}
	return ev
}

// The stream measures now and at every interval, an empty list as [], and
// a docker error as its text without ending.
func TestStatsStreamsAtTheInterval(t *testing.T) {
	var calls atomic.Int32
	agent := environment.ContainerStats{Name: "agent", CPUPercent: 12.5, MemoryBytes: 1 << 30, MemoryLimitBytes: 4 << 30}
	h := Handlers{StatsEvery: 20 * time.Millisecond, Stats: func(_ context.Context, project, issue string) ([]environment.ContainerStats, error) {
		if project != "CLR" || issue != "CLR-7" {
			t.Errorf("Stats(%q, %q)", project, issue)
		}
		switch calls.Add(1) {
		case 1:
			return []environment.ContainerStats{agent}, nil
		case 2:
			return nil, errors.New("docker: boom")
		}
		return nil, nil
	}}
	// Before the request: the handler starts its ticker and sends the first
	// event before streamPath returns.
	start := time.Now()
	data, _ := streamPath(t, h, "/api/runs/CLR/CLR-7/stats")
	if ev := nextStats(t, data); len(ev.Containers) != 1 || ev.Containers[0] != agent || ev.Error != nil || ev.Time.IsZero() {
		t.Errorf("first event %+v, want the agent", ev)
	}
	if ev := nextStats(t, data); ev.Error == nil || *ev.Error != "docker: boom" || ev.Containers == nil {
		t.Errorf("second event %+v, want the error and []", ev)
	}
	if ev := nextStats(t, data); ev.Containers == nil || len(ev.Containers) != 0 || ev.Error != nil {
		t.Errorf("third event %+v, want []", ev)
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Errorf("three events in %s, want one every 20ms", d)
	}
}

// Closing the page ends the polling.
func TestStatsStopsOnCancel(t *testing.T) {
	var calls atomic.Int32
	h := Handlers{StatsEvery: 5 * time.Millisecond, Stats: func(context.Context, string, string) ([]environment.ContainerStats, error) {
		calls.Add(1)
		return nil, nil
	}}
	dir := t.TempDir()
	tok := token(t, dir)
	srv := httptest.NewServer(Handler(dir, port, h))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/runs/CLR/CLR-7/stats", nil)
	req.Host = "127.0.0.1:7420"
	req.AddCookie(&http.Cookie{Name: name.Name, Value: tok})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond) // the server sees the closed connection
	n := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != n {
		t.Errorf("Stats called %d more times after the page closed", got-n)
	}
}

func TestStatsRefusesNamesThatAreNotPlain(t *testing.T) {
	dir := t.TempDir()
	h := Handler(dir, port, Handlers{Stats: func(context.Context, string, string) ([]environment.ContainerStats, error) {
		t.Error("Stats called")
		return nil, nil
	}})
	if w := doTo(t, h, "GET", "/api/runs/CLR/..%2f/stats", token(t, dir), nil); w.Code != http.StatusBadRequest {
		t.Errorf("GET = %d, want 400", w.Code)
	}
}
