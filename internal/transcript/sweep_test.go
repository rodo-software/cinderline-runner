package transcript

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// put writes a run of one step whose file has about size bytes, ends with
// an end line if ended, and was last changed at changed. It returns the
// run's folder.
func put(t *testing.T, s *Store, project, issue string, start time.Time, size int, ended bool, changed time.Time) string {
	t.Helper()
	dir := filepath.Join(s.Dir, project, issue, start.UTC().Format(RunID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"type":"start","kind":"run","pad":"` + strings.Repeat("x", size) + `"}` + "\n")
	if ended {
		b = append(b, `{"type":"end","outcome":"ok"}`+"\n"...)
	}
	name := filepath.Join(dir, "01-run.ndjson")
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{name, dir} {
		if err := os.Chtimes(p, changed, changed); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func days(n int, own map[string]int) func(string) int {
	return func(project string) int {
		if d, ok := own[project]; ok {
			return d
		}
		return n
	}
}

const noLimit = 1 << 40

func TestSweepByAgePerProject(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Log: discard()}
	old := now.Add(-15 * 24 * time.Hour)
	young := now.Add(-13 * 24 * time.Hour)
	aOld := put(t, s, "A", "A-1", old, 10, true, old)
	aYoung := put(t, s, "A", "A-2", young, 10, true, young)
	bOld := put(t, s, "B", "B-1", old, 10, true, old)   // B keeps 30 days
	cOld := put(t, s, "C", "C-1", young, 10, true, old) // C keeps 7 days

	n, err := s.Sweep(now, days(14, map[string]int{"B": 30, "C": 7}), noLimit)
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want 2, nil", n, err)
	}
	if exists(aOld) || !exists(aYoung) || !exists(bOld) || exists(cOld) {
		t.Errorf("left A old %v, A young %v, B old %v, C %v; want only A young and B old",
			exists(aOld), exists(aYoung), exists(bOld), exists(cOld))
	}
}

func TestSweepDaysZeroRemovesEveryEndedRun(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Log: discard()}
	ended := put(t, s, "A", "A-1", now.Add(-2*time.Hour), 10, true, now.Add(-2*time.Hour))
	if n, err := s.Sweep(now, days(0, nil), noLimit); err != nil || n != 1 || exists(ended) {
		t.Fatalf("Sweep = %d, %v; run left: %v", n, err, exists(ended))
	}
}

func TestSweepBySizeRemovesOldestFirst(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Log: discard()}
	day := 24 * time.Hour
	first := put(t, s, "B", "B-1", now.Add(-3*day), 1000, true, now.Add(-3*day))
	second := put(t, s, "A", "A-1", now.Add(-2*day), 1000, true, now.Add(-2*day))
	third := put(t, s, "B", "B-2", now.Add(-1*day), 1000, true, now.Add(-1*day))

	n, err := s.Sweep(now, days(14, nil), 1500) // three runs of about 1050 bytes
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want 2, nil", n, err)
	}
	if exists(first) || exists(second) || !exists(third) {
		t.Errorf("left %v %v %v; want only the newest", exists(first), exists(second), exists(third))
	}
}

func TestSweepKeepsARunInFlight(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Log: discard()}
	old := now.Add(-20 * 24 * time.Hour)
	flight := put(t, s, "A", "A-1", old, 1000, false, now.Add(-10*time.Minute))
	between := put(t, s, "A", "A-2", old, 1000, true, now.Add(-10*time.Minute)) // between two steps
	crashed := put(t, s, "A", "A-3", old, 1000, false, now.Add(-2*time.Hour))

	n, err := s.Sweep(now, days(0, nil), 100<<20)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1, nil", n, err)
	}
	if !exists(flight) || !exists(between) || exists(crashed) {
		t.Errorf("left in flight %v, between steps %v, crashed %v; want the first two", exists(flight), exists(between), exists(crashed))
	}
	// Over the size limit, a run in flight is kept too.
	if n, err := s.Sweep(now, days(14, nil), 1); err != nil || n != 0 || !exists(flight) || !exists(between) {
		t.Fatalf("Sweep over the size = %d, %v", n, err)
	}
}

func TestSweepRemovesEmptyFoldersOnly(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Log: discard()}
	old := now.Add(-20 * 24 * time.Hour)
	put(t, s, "A", "A-1", old, 10, true, old)
	put(t, s, "B", "B-1", old, 10, true, old)
	kept := put(t, s, "B", "B-2", now.Add(-time.Hour), 10, true, now.Add(-time.Hour))
	if err := os.MkdirAll(filepath.Join(s.Dir, "C", "C-1"), 0o700); err != nil { // left empty by an earlier run
		t.Fatal(err)
	}

	if _, err := s.Sweep(now, days(14, nil), noLimit); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(s.Dir, "A")) || exists(filepath.Join(s.Dir, "B", "B-1")) || exists(filepath.Join(s.Dir, "C")) {
		t.Error("an empty project or issue folder is left")
	}
	if !exists(kept) || !exists(s.Dir) {
		t.Error("a folder with a run in it is gone, or the store itself")
	}
}

func TestSweepOfNoFolder(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "transcripts")}
	if n, err := s.Sweep(now, days(14, nil), noLimit); n != 0 || err != nil {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
}

func TestSweepLogsOneLineWhenItRemoved(t *testing.T) {
	var buf bytes.Buffer
	s := &Store{Dir: t.TempDir(), Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if _, err := s.Sweep(now, days(14, nil), noLimit); err != nil || buf.Len() != 0 {
		t.Fatalf("nothing removed, yet logged %q (%v)", buf.String(), err)
	}
	old := now.Add(-20 * 24 * time.Hour)
	put(t, s, "A", "A-1", old, 10, true, old)
	put(t, s, "A", "A-2", now.Add(-2*time.Hour), 1000, true, now.Add(-2*time.Hour))
	put(t, s, "A", "A-3", now.Add(-time.Hour), 1000, true, now.Add(-time.Hour))
	if _, err := s.Sweep(now, days(14, nil), 1500); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "by_age=1") || !strings.Contains(got, "by_size=1") {
		t.Fatalf("log = %q, want one line with 1 by age and 1 by size", got)
	}
}

func TestDiscard(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	keep := s.Begin("CLR", "CLR-1", start.Add(-time.Hour))
	keep.Step("run", Start{}).End("ok", "")
	r := s.Begin("CLR", "CLR-1", start)
	r.Step("run", Start{}).End("ok", "")
	r.Discard()
	if runs := s.List(); len(runs) != 1 || runs[0].Run != start.Add(-time.Hour).UTC().Format(RunID) {
		t.Fatalf("List after Discard = %+v, want the other run only", runs)
	}
	(*Run)(nil).Discard()                    // a refused run: nothing to do
	s.Begin("CLR", "CLR-2", start).Discard() // no step yet: no folder
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
