package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

var start = time.Date(2026, 11, 28, 9, 5, 7, 0, time.FixedZone("CET", 3600))

// read returns step n's lines, decoded.
func read(t *testing.T, s *Store, project, issue, run string, n int) []map[string]any {
	t.Helper()
	rc, err := s.Open(project, issue, run, n)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var out []map[string]any
	sc := bufio.NewScanner(rc) // its default limit is maxLine: every line must fit
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if _, err := time.Parse(time.RFC3339, m["t"].(string)); err != nil {
			t.Fatalf("t: %v", err)
		}
		delete(m, "t")
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunOfTwoStepsReadBack(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	r := s.Begin("CLR", "CLR-277", start)
	a := r.Step("run", Start{Agent: "coder", Client: "claude", Model: "opus", Manager: true})
	a.Prompt("do it")
	a.Out("hello")
	a.Refused("example.com:443", "not allowed", "CONNECT", "https://example.com/")
	a.End("ok", "")
	a.Out("after the end") // dropped: end is the last line
	b := r.Step("check", Start{})
	b.Out("FAIL x")

	if _, err := os.Stat(filepath.Join(s.Dir, "CLR", "CLR-277", "20261128T080507Z", "01-run.ndjson")); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	want := []RunInfo{{Project: "CLR", Issue: "CLR-277", Run: "20261128T080507Z", Start: start.UTC(), Ended: false, Manager: true, Steps: 2}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
	steps := s.Steps("CLR", "CLR-277", "20261128T080507Z")
	if len(steps) != 2 || steps[0].Start.IsZero() || steps[0].End.Before(steps[0].Start) || steps[1].Start.IsZero() || !steps[1].End.IsZero() {
		t.Fatalf("Steps = %+v, want both started and the first ended", steps)
	}
	for i := range steps {
		steps[i].Start, steps[i].End = time.Time{}, time.Time{}
	}
	if want := []StepInfo{{N: 1, Kind: "run", Agent: "coder", Client: "claude", Model: "opus", Manager: true, Ended: true, Outcome: "ok"}, {N: 2, Kind: "check"}}; steps[0] != want[0] || steps[1] != want[1] {
		t.Fatalf("Steps = %+v, want %+v", steps, want)
	}

	lines := read(t, s, "CLR", "CLR-277", "20261128T080507Z", 1)
	wantLines := []string{
		`{"agent":"coder","client":"claude","kind":"run","manager":true,"model":"opus","type":"start"}`,
		`{"text":"do it","type":"prompt"}`,
		`{"line":"hello","type":"out"}`,
		`{"host":"example.com:443","method":"CONNECT","reason":"not allowed","type":"refused","url":"https://example.com/"}`,
		`{"error":"","outcome":"ok","type":"end"}`,
	}
	if len(lines) != len(wantLines) {
		t.Fatalf("got %d lines, want %d: %v", len(lines), len(wantLines), lines)
	}
	for i, m := range lines {
		if j, _ := json.Marshal(m); string(j) != wantLines[i] {
			t.Errorf("line %d = %s, want %s", i+1, j, wantLines[i])
		}
	}
	if lines := read(t, s, "CLR", "CLR-277", "20261128T080507Z", 2); len(lines) != 2 || lines[0]["kind"] != "check" || lines[0]["agent"] != nil {
		t.Fatalf("step 2 = %v", lines)
	}

	b.End("failed", "check failed")
	if got := s.List(); !got[0].Ended || got[0].Outcome != "failed" {
		t.Fatalf("List = %+v, want ended with its last step's outcome", got)
	}
	if st := s.Steps("CLR", "CLR-277", "20261128T080507Z")[1]; st.Outcome != "failed" || st.Error != "check failed" {
		t.Fatalf("step 2 = %+v, want its end line's outcome and error", st)
	}
	if _, err := s.Open("CLR", "CLR-277", "20261128T080507Z", 3); err == nil {
		t.Fatal("Open of a missing step: no error")
	}
}

func TestRedaction(t *testing.T) {
	const secret = "sk-transcript-test-0123456789abcdef"
	redact.Add("test key", secret)
	s := &Store{Dir: t.TempDir()}
	st := s.Begin("CLR", "CLR-1", start).Step("run", Start{Model: secret})
	st.Prompt("use " + secret)
	st.Out("token=" + secret)
	st.Refused(secret+".example", secret, "GET", "https://x/?k="+secret)
	st.End("failed", "bad "+secret)

	b, err := os.ReadFile(filepath.Join(s.Dir, "CLR", "CLR-1", "20261128T080507Z", "01-run.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(secret)) || bytes.Count(b, []byte("[redacted test key]")) != 7 {
		t.Fatalf("not redacted:\n%s", b)
	}
}

func TestLongLine(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	st := s.Begin("CLR", "CLR-1", start).Step("run", Start{})
	st.Out(strings.Repeat("x", maxLine+1))
	st.Out(strings.Repeat("<", maxLine/4)) // short, but over maxLine once JSON escapes it
	st.End("failed", strings.Repeat("e", maxLine))
	st.Out("ok") // after the end: dropped

	lines := read(t, s, "CLR", "CLR-1", "20261128T080507Z", 1)
	if len(lines) != 4 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	if got, want := lines[1]["line"], "[65537 bytes, too long to keep]"; got != want {
		t.Errorf("long line = %v, want %q", got, want)
	}
	if got, want := lines[2]["line"], "[16384 bytes, too long to keep]"; got != want {
		t.Errorf("escaped line = %v, want %q", got, want)
	}
	if lines[3]["outcome"] != "failed" || lines[3]["error"] != "[65536 bytes, too long to keep]" {
		t.Errorf("end = %v, want its outcome kept and its error cut", lines[3])
	}
}

func TestConcurrentOut(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	st := s.Begin("CLR", "CLR-1", start).Step("run", Start{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 200 {
				st.Out(strings.Repeat("y", 1000))
			}
		})
	}
	wg.Wait()
	st.End("ok", "")
	if lines := read(t, s, "CLR", "CLR-1", "20261128T080507Z", 1); len(lines) != 1+8*200+1 {
		t.Fatalf("got %d lines", len(lines))
	}
}

func TestPathEscapingNameRefused(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: filepath.Join(dir, "transcripts")}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a..b"} {
		if r := s.Begin(bad, "CLR-1", start); r != nil {
			t.Errorf("Begin(project %q) = %v, want nil", bad, r)
		}
		if r := s.Begin("CLR", bad, start); r != nil {
			t.Errorf("Begin(issue %q) = %v, want nil", bad, r)
		}
		if steps := s.Steps("CLR", "CLR-1", bad); steps != nil {
			t.Errorf("Steps(run %q) = %v", bad, steps)
		}
		if _, err := s.Open("CLR", bad, "20261128T080507Z", 1); err == nil {
			t.Errorf("Open(issue %q): no error", bad)
		}
	}
	// A refused run and its steps write nothing and do not panic.
	r := s.Begin("..", "CLR-1", start)
	st := r.Step("run", Start{})
	st.Out("x")
	st.End("ok", "")
	if r := s.Begin("CLR", "CLR-1", start); r.Step("../run", Start{}) != nil {
		t.Error("Step with a path in its kind: not refused")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("written: %v", entries)
	}
}

func TestWriteErrorLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLR"), nil, 0o600); err != nil { // a file where the project folder goes
		t.Fatal(err)
	}
	s := &Store{Dir: dir, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	st := s.Begin("CLR", "CLR-1", start).Step("run", Start{})
	st.Out("a")
	st.Out("b")
	st.End("ok", "")
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "issue=CLR-1") {
		t.Fatalf("log = %q, want one line tagged with the issue", buf.String())
	}
}

// TestStepEndOfALongStep: the end line is found behind a step's output of
// more than one tail read, and after an end line too long for the first one.
func TestStepEndOfALongStep(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	st := s.Begin("CLR", "CLR-1", start).Step("run", Start{Model: "opus"})
	for range 100 {
		st.Out(strings.Repeat("x", 1000))
	}
	st.End("failed", strings.Repeat("e", 10000))
	got := s.Steps("CLR", "CLR-1", "20261128T080507Z")
	if len(got) != 1 || got[0].Model != "opus" || !got[0].Ended || got[0].Outcome != "failed" || len(got[0].Error) != 10000 {
		t.Fatalf("Steps = %+v", got)
	}
}

func TestTail(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	r := s.Begin("CLR", "CLR-291", start)
	st := r.Step("run", Start{Client: "claude"})
	for i := range 100 {
		st.Out(fmt.Sprintf("line %03d %s", i, strings.Repeat("x", 100)))
	}
	run := start.UTC().Format(RunID)
	all := s.Tail("CLR", "CLR-291", run, 1, 1<<20)
	if n := bytes.Count(all, []byte("\n")); n != 101 || !bytes.HasPrefix(all, []byte(`{"t":`)) {
		t.Fatalf("whole file: %d lines, starts %.20q", n, all)
	}
	part := s.Tail("CLR", "CLR-291", run, 1, 1000)
	if len(part) > 1000 || !bytes.HasPrefix(part, []byte(`{"t":`)) || !bytes.HasSuffix(part, []byte("line 099 "+strings.Repeat("x", 100)+"\"}\n")) {
		t.Errorf("tail of 1000 bytes is not whole lines up to the last: %q", part)
	}
	if got := s.Tail("CLR", "CLR-291", run, 2, 1000); got != nil {
		t.Errorf("a step that is not there: %q", got)
	}
}
