// Package transcript keeps each run on disk, one folder per run and one
// file of JSON lines per step, so it can be opened after it ended or after a
// restart (decision 2026-11-26-local-web-page, section Transcripts):
//
//	<Dir>/<project key>/<issue ref>/<run id>/<NN>-<kind>.ndjson
//
// It is a cache the runner may lose: a write error is logged and never stops
// the run. Every string passes internal/redact before it is written.
package transcript

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/redact"
)

// RunID is the layout of a run's folder name: its start in UTC.
const RunID = "20060102T150405Z"

// maxLine is the longest line written, bufio.Scanner's default limit, so a
// reader can scan the files without setting one. A longer line has its
// values over cutValue replaced by a marker that names their length; then
// even five values the JSON escaping made six times longer stay under it.
const (
	maxLine  = bufio.MaxScanTokenSize
	cutValue = 1 << 10
)

// Store is the transcripts folder, <state>/transcripts.
type Store struct {
	Dir string
	Log *slog.Logger // write errors; nil means slog.Default()
}

// Start is what a step's first line holds besides its kind; an empty field
// is left out.
type Start struct {
	Agent, Client, Model string
	Manager              bool
}

// Run is one run being written. Its methods are safe from several goroutines
// and a nil *Run, from a refused name, writes nothing.
type Run struct {
	store          *Store
	project, issue string
	dir            string
	mu             sync.Mutex
	steps          int
}

// Step is one step's file. Each method appends one line; nothing is written
// after End. A nil *Step writes nothing.
// shortcut: a step that never gets End keeps its file open until the daemon
// exits; fine while every run ends its steps, add a Close if one cannot.
type Step struct {
	log    *slog.Logger
	mu     sync.Mutex
	f      *os.File // nil once ended or when it could not be created
	failed bool     // a write error was logged
}

// RunInfo is a run on disk. Ended: its last step has an end line, whose
// outcome is Outcome. Manager: its first step is a Project Manager's.
type RunInfo struct {
	Project, Issue, Run string
	Start, End          time.Time // End: of the last step, once Ended
	Ended               bool
	Manager             bool
	Steps               int
	Outcome             string
}

// StepInfo is a step on disk: from its start line its agent, client, model
// and start, and, once Ended, from its end line its end, outcome and error.
type StepInfo struct {
	N                    int
	Kind                 string
	Agent, Client, Model string
	Manager              bool
	Start, End           time.Time
	Ended                bool
	Outcome, Error       string
}

func (s *Store) log() *slog.Logger { return cmp.Or(s.Log, slog.Default()) }

// Plain reports whether name is safe as one path element.
func Plain(name string) bool {
	return name != "" && name != "." && !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..") && filepath.IsLocal(name)
}

// Begin starts the transcript of a run of issue that started at start. It
// returns nil, after logging it, when project or issue is not a plain name.
func (s *Store) Begin(project, issue string, start time.Time) *Run {
	if !Plain(project) || !Plain(issue) {
		s.log().Warn("transcript refused: not a plain name", "project", project, "issue", issue)
		return nil
	}
	return &Run{store: s, project: project, issue: issue,
		dir: filepath.Join(s.Dir, project, issue, start.UTC().Format(RunID))}
}

// Step starts the run's next step, numbered from 1, and writes its start
// line. A file that cannot be created is logged; the step then writes nothing.
func (r *Run) Step(kind string, meta Start) *Step {
	if r == nil {
		return nil
	}
	log := r.store.log().With("project", r.project, "issue", r.issue)
	if !Plain(kind) {
		log.Warn("transcript refused: not a plain name", "kind", kind)
		return nil
	}
	r.mu.Lock()
	r.steps++
	name := filepath.Join(r.dir, fmt.Sprintf("%02d-%s.ndjson", r.steps, kind))
	r.mu.Unlock()
	st := &Step{log: log}
	err := os.MkdirAll(r.dir, 0o700)
	if err == nil {
		st.f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	}
	if err != nil {
		st.fail(err)
		return st
	}
	kv := []any{"kind", kind}
	for _, f := range []struct{ k, v string }{{"agent", meta.Agent}, {"client", meta.Client}, {"model", meta.Model}} {
		if f.v != "" {
			kv = append(kv, f.k, f.v)
		}
	}
	if meta.Manager {
		kv = append(kv, "manager", true)
	}
	st.write(false, "start", kv...)
	return st
}

// Prompt writes the prompt the agent got.
func (s *Step) Prompt(text string) { s.write(false, "prompt", "text", text) }

// Out writes one line of the container's output.
func (s *Step) Out(line string) { s.write(false, "out", "line", line) }

// Refused writes a connection the run's proxy refused.
func (s *Step) Refused(host, reason, method, url string) {
	s.write(false, "refused", "host", host, "reason", reason, "method", method, "url", url)
}

// End writes the step's last line, outcome a short word such as ok, failed
// or stopped, and closes its file.
func (s *Step) End(outcome, errText string) {
	s.write(true, "end", "outcome", outcome, "error", errText)
}

func (s *Step) write(last bool, typ string, kv ...any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	t := time.Now()
	b := line(t, typ, kv, false)
	if len(b) > maxLine {
		b = line(t, typ, kv, true)
	}
	if _, err := s.f.Write(b); err != nil {
		s.fail(err)
	}
	if last {
		if err := s.f.Close(); err != nil {
			s.fail(err)
		}
		s.f = nil
	}
}

// fail logs the step's first write error only: a full disk would otherwise
// log every line of output.
func (s *Step) fail(err error) {
	if !s.failed {
		s.failed = true
		s.log.Warn("transcript not written", "err", err)
	}
}

// line encodes one line from key, value pairs, every string value redacted
// and, if cut, replaced by a marker when it is longer than cutValue.
func line(t time.Time, typ string, kv []any, cut bool) []byte {
	b := fmt.Appendf(nil, `{"t":%q,"type":%q`, t.UTC().Format(time.RFC3339Nano), typ)
	for i := 0; i < len(kv); i += 2 {
		v := kv[i+1]
		if s, ok := v.(string); ok {
			s = redact.String(s)
			if cut && len(s) > cutValue {
				s = fmt.Sprintf("[%d bytes, too long to keep]", len(s))
			}
			v = s
		}
		j, _ := json.Marshal(v) // a string or a bool: cannot fail
		b = fmt.Appendf(b, `,%q:%s`, kv[i], j)
	}
	return append(b, "}\n"...)
}

// List returns the runs on disk, by project, issue and start.
func (s *Store) List() []RunInfo {
	var out []RunInfo
	for _, p := range dirs(s.Dir) {
		for _, i := range dirs(filepath.Join(s.Dir, p)) {
			for _, r := range dirs(filepath.Join(s.Dir, p, i)) {
				if ri, _, ok := s.Info(p, i, r); ok {
					out = append(out, ri)
				}
			}
		}
	}
	return out
}

// Info returns a run and its steps; ok is false when run is not a run id.
func (s *Store) Info(project, issue, run string) (ri RunInfo, steps []StepInfo, ok bool) {
	start, err := time.Parse(RunID, run)
	if err != nil {
		return RunInfo{}, nil, false
	}
	ri = RunInfo{Project: project, Issue: issue, Run: run, Start: start}
	if steps = s.Steps(project, issue, run); len(steps) > 0 {
		last := steps[len(steps)-1]
		ri.Ended, ri.Manager, ri.Steps, ri.Outcome, ri.End = last.Ended, steps[0].Manager, len(steps), last.Outcome, last.End
	}
	return ri, steps, true
}

// dirs returns the plain names of the folders in dir.
func dirs(dir string) []string {
	entries, _ := os.ReadDir(dir) // a missing folder holds no runs
	var out []string
	for _, e := range entries {
		if e.IsDir() && Plain(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// Steps returns a run's steps by number; nil for a name that is not plain.
func (s *Store) Steps(project, issue, run string) []StepInfo {
	if !Plain(project) || !Plain(issue) || !Plain(run) {
		return nil
	}
	dir := filepath.Join(s.Dir, project, issue, run)
	entries, _ := os.ReadDir(dir)
	var out []StepInfo
	for _, e := range entries {
		num, rest, _ := strings.Cut(e.Name(), "-")
		kind, ok := strings.CutSuffix(rest, ".ndjson")
		n, err := strconv.Atoi(num)
		if !ok || err != nil || n < 1 || !e.Type().IsRegular() {
			continue
		}
		st := StepInfo{N: n, Kind: kind}
		readEnds(filepath.Join(dir, e.Name()), &st)
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b StepInfo) int { return a.N - b.N }) // 100 sorts after 99
	return out
}

// readEnds fills st from the file's start line and its end line, if its
// first line is one and its last line is one. It reads the first line and
// the file's tail only, so a long step costs no more than a short one.
func readEnds(name string, st *StepInfo) {
	f, err := os.Open(name)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	first, _ := bufio.NewReader(io.NewSectionReader(f, 0, maxLine)).ReadBytes('\n')
	var s struct {
		T                          time.Time
		Type, Agent, Client, Model string
		Manager                    bool
	}
	if json.Unmarshal(first, &s) == nil && s.Type == "start" {
		st.Start, st.Agent, st.Client, st.Model, st.Manager = s.T, s.Agent, s.Client, s.Model, s.Manager
	}
	var e struct {
		T                    time.Time
		Type, Outcome, Error string
	}
	if json.Unmarshal(lastLine(f, fi.Size()), &e) == nil && e.Type == "end" {
		st.Ended, st.End, st.Outcome, st.Error = true, e.T, e.Outcome, e.Error
	}
}

// lastLine returns the last line of f, of size bytes, without its newline.
// It reads a small tail first and, when the line is longer, the most a line
// may take: every line fits in maxLine.
func lastLine(f *os.File, size int64) []byte {
	for _, n := range []int64{4 << 10, maxLine + 1} {
		buf := make([]byte, min(size, n))
		if _, err := f.ReadAt(buf, size-int64(len(buf))); err != nil && err != io.EOF {
			return nil
		}
		whole := int64(len(buf)) == size
		buf = bytes.TrimSuffix(buf, []byte("\n"))
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 || whole {
			return buf[i+1:]
		}
	}
	return nil
}

// Open opens step n of a run.
func (s *Store) Open(project, issue, run string, n int) (io.ReadCloser, error) {
	return s.open(project, issue, run, n)
}

func (s *Store) open(project, issue, run string, n int) (*os.File, error) {
	for _, st := range s.Steps(project, issue, run) {
		if st.N == n {
			return os.Open(filepath.Join(s.Dir, project, issue, run, fmt.Sprintf("%02d-%s.ndjson", n, st.Kind)))
		}
	}
	return nil, fmt.Errorf("transcript %s/%s/%s step %d: %w", project, issue, run, n, fs.ErrNotExist)
}

// Tail returns the whole lines in the last max bytes of step n of a run, each
// with its newline; nil when the step cannot be read. Like lastLine, it reads
// only the tail, so a long step costs no more than a short one.
func (s *Store) Tail(project, issue, run string, n int, max int64) []byte {
	f, err := s.open(project, issue, run, n)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	size := fi.Size()
	buf := make([]byte, min(size, max))
	if _, err := f.ReadAt(buf, size-int64(len(buf))); err != nil && err != io.EOF {
		return nil
	}
	if int64(len(buf)) < size { // it starts inside a line
		_, buf, _ = bytes.Cut(buf, []byte("\n"))
	}
	return buf[:bytes.LastIndexByte(buf, '\n')+1] // a line being written has no newline yet
}

// Discard removes the run's folder, for a project that keeps no transcript
// once the run ended (transcript_days 0). Call it after the run's last step.
func (r *Run) Discard() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.RemoveAll(r.dir); err != nil {
		r.store.log().Warn("transcript not removed", "project", r.project, "issue", r.issue, "err", err)
	}
}

// inFlight is how recently a run may have changed and still be in flight, so
// Sweep keeps it, also between two steps, when its last step has ended.
const inFlight = time.Hour

// Sweep removes every run older, by its start, than daysFor(its project)
// days, then the oldest runs while all of them take more than maxBytes; a
// run changed within inFlight is kept. Then it removes the issue and project
// folders left empty and logs one line if it removed a run. err joins the
// removals that failed.
func (s *Store) Sweep(now time.Time, daysFor func(project string) int, maxBytes int64) (removed int, err error) {
	type run struct {
		dir   string
		start time.Time
		size  int64
		busy  bool
	}
	var (
		runs          []run
		total         int64
		byAge, bySize int
		errs          []error
	)
	remove := func(dir string) bool {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			return false
		}
		return true
	}
	for _, p := range dirs(s.Dir) {
		keep := time.Duration(daysFor(p)) * 24 * time.Hour
		for _, i := range dirs(filepath.Join(s.Dir, p)) {
			for _, name := range dirs(filepath.Join(s.Dir, p, i)) {
				start, err := time.Parse(RunID, name)
				if err != nil {
					continue
				}
				r := run{dir: filepath.Join(s.Dir, p, i, name), start: start}
				var changed time.Time
				r.size, changed = usage(r.dir)
				r.busy = now.Sub(changed) < inFlight
				if !r.busy && now.Sub(start) > keep {
					if remove(r.dir) {
						byAge++
						continue
					}
				}
				runs = append(runs, r)
				total += r.size
			}
		}
	}
	slices.SortFunc(runs, func(a, b run) int { return cmp.Or(a.start.Compare(b.start), cmp.Compare(a.dir, b.dir)) })
	for _, r := range runs {
		if total <= maxBytes {
			break
		}
		if !r.busy && remove(r.dir) {
			total -= r.size
			bySize++
		}
	}
	// shortcut: a run of the project that starts its first step in the
	// instant between its MkdirAll and its file writes nothing (logged);
	// fine hourly, add a retry in Step if it shows up.
	for _, p := range dirs(s.Dir) {
		for _, i := range dirs(filepath.Join(s.Dir, p)) {
			os.Remove(filepath.Join(s.Dir, p, i)) // fails, as meant, while it holds anything
		}
		os.Remove(filepath.Join(s.Dir, p))
	}
	if removed = byAge + bySize; removed > 0 {
		s.log().Info("transcripts removed", "runs", removed, "by_age", byAge, "by_size", bySize)
	}
	return removed, errors.Join(errs...)
}

// usage returns the bytes of the files in a run's folder and when it or one
// of them last changed.
func usage(dir string) (size int64, changed time.Time) {
	if fi, err := os.Stat(dir); err == nil {
		changed = fi.ModTime()
	}
	entries, _ := os.ReadDir(dir) // unreadable: as empty, and its removal reports the error
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		if fi.ModTime().After(changed) {
			changed = fi.ModTime()
		}
	}
	return size, changed
}
