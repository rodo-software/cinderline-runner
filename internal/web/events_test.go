package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/transcript"
)

const eventsPath = "/api/runs/CLR/CLR-7/20261128T090000Z/steps/1/events"

// eventsStore is a store with run CLR-7 of t1, its first step begun.
func eventsStore(t *testing.T) (Handlers, *transcript.Step) {
	t.Helper()
	s := transcript.Store{Dir: t.TempDir()}
	st := s.Begin("CLR", "CLR-7", t1).Step("run", transcript.Start{Agent: "coder"})
	cfg := &config.Config{}
	return Handlers{Transcripts: s, Config: func() *config.Config { return cfg }, Tick: time.Hour}, st
}

type eventOut struct {
	I      *int
	Type   string
	Line   string
	Events []Event
}

// nextEvent is the stream's next data, as an event of a line.
func nextEvent(t *testing.T, data <-chan string) eventOut {
	t.Helper()
	got := next(t, data, "event")
	var e eventOut
	if err := json.Unmarshal([]byte(got), &e); err != nil || e.I == nil {
		t.Fatalf("event %s: not a line with i (%v)", got, err)
	}
	return e
}

func wantEvent(t *testing.T, e eventOut, i int, typ, line string) {
	t.Helper()
	if *e.I != i || e.Type != typ || e.Line != line {
		t.Errorf("event %d %s %q, want %d %s %q", *e.I, e.Type, e.Line, i, typ, line)
	}
}

func wantDone(t *testing.T, data <-chan string) {
	t.Helper()
	if got := next(t, data, "done"); got != "event: done" {
		t.Errorf("got %s, want event: done", got)
	}
}

// nothing fails when data has an event within d.
func nothing(t *testing.T, data <-chan string, d time.Duration) {
	t.Helper()
	select {
	case got := <-data:
		t.Fatalf("unexpected event %s", got)
	case <-time.After(d):
	}
}

func TestEventsOfAFinishedStep(t *testing.T) {
	h, st := eventsStore(t)
	st.Out("one")
	st.Out("two")
	st.End("ok", "")
	data, _ := streamPath(t, h, eventsPath)
	wantEvent(t, nextEvent(t, data), 0, "start", "")
	wantEvent(t, nextEvent(t, data), 1, "out", "one")
	wantEvent(t, nextEvent(t, data), 2, "out", "two")
	end := next(t, data, "end")
	if !strings.HasPrefix(end, `{"i":3,"t":"`) || !strings.Contains(end, `"type":"end","outcome":"ok"`) {
		t.Errorf("end event %s, want the stored line with i 3 first", end)
	}
	wantDone(t, data)
}

func TestEventsFromSkips(t *testing.T) {
	h, st := eventsStore(t)
	st.Out("one")
	st.Out("two")
	st.End("ok", "")
	data, _ := streamPath(t, h, eventsPath+"?from=2")
	wantEvent(t, nextEvent(t, data), 2, "out", "two")
	wantEvent(t, nextEvent(t, data), 3, "end", "")
	wantDone(t, data)

	data, _ = streamPath(t, h, eventsPath+"?from=4") // a page that has them all
	wantDone(t, data)
}

func TestEventsFollowAGrowingStep(t *testing.T) {
	h, st := eventsStore(t)
	data, _ := streamPath(t, h, eventsPath)
	wantEvent(t, nextEvent(t, data), 0, "start", "")
	st.Out("one")
	wantEvent(t, nextEvent(t, data), 1, "out", "one")
	st.Out("two")
	st.End("failed", "boom")
	wantEvent(t, nextEvent(t, data), 2, "out", "two")
	wantEvent(t, nextEvent(t, data), 3, "end", "")
	wantDone(t, data)
}

func TestEventsHoldBackAHalfLine(t *testing.T) {
	h, _ := eventsStore(t)
	f, err := os.OpenFile(filepath.Join(h.Transcripts.Dir, "CLR", "CLR-7", "20261128T090000Z", "01-run.ndjson"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := streamPath(t, h, eventsPath)
	wantEvent(t, nextEvent(t, data), 0, "start", "")
	f.WriteString(`{"t":"2026-11-28T09:00:01Z","type":"out","li`)
	nothing(t, data, 700*time.Millisecond) // two polls
	f.WriteString(`ne":"half"}` + "\n")
	wantEvent(t, nextEvent(t, data), 1, "out", "half")
	f.WriteString("\n{}\nnot json\n" + `{"t":"2026-11-28T09:00:02Z","type":"out","line":"after"}` + "\n")
	wantEvent(t, nextEvent(t, data), 5, "out", "after") // the damaged lines are skipped, but counted
}

func TestEventsEndWithTheRequest(t *testing.T) {
	h, _ := eventsStore(t)
	dir := t.TempDir()
	tok := token(t, dir)
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequestWithContext(ctx, "GET", eventsPath, nil)
	r.Host = "127.0.0.1:7420"
	r.AddCookie(&http.Cookie{Name: name.Name, Value: tok})
	done := make(chan struct{})
	go func() {
		Handler(dir, port, h).ServeHTTP(httptest.NewRecorder(), r)
		close(done)
	}()
	time.Sleep(400 * time.Millisecond) // past the first read, into following
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler still runs after the request was cancelled")
	}
}

func TestEventsNotFoundOrBad(t *testing.T) {
	h, _ := eventsStore(t)
	dir := t.TempDir()
	tok := token(t, dir)
	hh := Handler(dir, port, h)
	for path, code := range map[string]int{
		"/api/runs/CLR/CLR-7/20261128T090000Z/steps/2/events":  http.StatusNotFound,
		"/api/runs/CLR/CLR-8/20261128T090000Z/steps/1/events":  http.StatusNotFound,
		"/api/runs/CLR/CLR-7/latest/steps/1/events":            http.StatusNotFound,
		"/api/runs/CLR/CLR-7/20261128T090000Z/steps/x/events":  http.StatusBadRequest,
		"/api/runs/CLR/CLR-7/20261128T090000Z/steps/0/events":  http.StatusBadRequest,
		"/api/runs/CLR/%2E%2E/20261128T090000Z/steps/1/events": http.StatusBadRequest,
		eventsPath + "?from=-1":                                http.StatusBadRequest,
		eventsPath + "?from=x":                                 http.StatusBadRequest,
	} {
		if w := doTo(t, hh, "GET", path, tok, nil); w.Code != code {
			t.Errorf("GET %s = %d, want %d", path, w.Code, code)
		}
	}
}

func TestEventsParseOutLinesByClient(t *testing.T) {
	text := `{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"thinking","thinking":"Hm."}]}}`
	s := transcript.Store{Dir: t.TempDir()}
	run := s.Begin("CLR", "CLR-7", t1)
	claude := run.Step("run", transcript.Start{Agent: "coder", Client: "claude"})
	claude.Out(text)
	claude.End("ok", "")
	other := run.Step("run", transcript.Start{Agent: "coder", Client: "gemini"})
	other.Out(text)
	other.End("ok", "")
	cfg := &config.Config{}
	h := Handlers{Transcripts: s, Config: func() *config.Config { return cfg }, Tick: time.Hour}

	data, _ := streamPath(t, h, eventsPath+"?from=1") // the start line is read for its client, not sent
	e := nextEvent(t, data)
	wantEvent(t, e, 1, "out", text)
	if want := []Event{{Kind: "text", Text: "Looking."}, {Kind: "thinking", Text: "Hm."}}; !reflect.DeepEqual(e.Events, want) {
		t.Errorf("claude events %+v, want %+v", e.Events, want)
	}
	if e := nextEvent(t, data); e.Events != nil {
		t.Errorf("end line has events %+v", e.Events)
	}

	data, _ = streamPath(t, h, strings.Replace(eventsPath, "steps/1", "steps/2", 1))
	nextEvent(t, data)
	e = nextEvent(t, data)
	wantEvent(t, e, 1, "out", text)
	if want := []Event{{Kind: "raw"}}; !reflect.DeepEqual(e.Events, want) {
		t.Errorf("gemini events %+v, want %+v", e.Events, want)
	}
}
