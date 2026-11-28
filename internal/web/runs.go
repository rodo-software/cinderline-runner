package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/transcript"
)

// maxRuns is the most runs GET /api/runs answers.
const maxRuns = 500

// followEvery is how often the events of a step in flight read its file
// again for what was appended.
const followEvery = 300 * time.Millisecond

// runJSON is what the list and the detail of a run share.
type runJSON struct {
	Project  string    `json:"project"`
	Issue    string    `json:"issue"`
	Run      string    `json:"run"`
	Start    time.Time `json:"start"`
	Live     bool      `json:"live"`
	Manager  bool      `json:"manager"`
	Outcome  string    `json:"outcome"`
	IssueURL string    `json:"issue_url"`
}

type stepJSON struct {
	N       int       `json:"n"`
	Kind    string    `json:"kind"`
	Agent   string    `json:"agent"`
	Client  string    `json:"client"`
	Model   string    `json:"model"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end,omitzero"` // left out while the step runs
	Outcome string    `json:"outcome"`
	Error   string    `json:"error"`
}

func toRun(ri transcript.RunInfo, cfg *config.Config) runJSON {
	return runJSON{Project: ri.Project, Issue: ri.Issue, Run: ri.Run, Start: ri.Start, Live: !ri.Ended,
		Manager: ri.Manager, Outcome: ri.Outcome, IssueURL: issueURL(cfg, ri.Project, ri.Issue)}
}

// issueURL is the issue's page in Cinderline: the number after the key's
// dash (CLR-12.1 gives 12.1) under the project's url; empty for a project
// the config no longer has.
func issueURL(cfg *config.Config, project, issue string) string {
	i := slices.IndexFunc(cfg.Projects, func(p config.Project) bool { return p.Key == project })
	_, num, ok := strings.Cut(issue, "-")
	if i < 0 || !ok {
		return ""
	}
	u, err := url.JoinPath(cfg.Projects[i].URL, "projects", project, "issues", num)
	if err != nil {
		return ""
	}
	return u
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// runRoutes adds the runs of the transcript store to mux.
func runRoutes(mux *http.ServeMux, h *Handlers) {
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		n := 100
		if v := q.Get("limit"); v != "" {
			var err error
			if n, err = strconv.Atoi(v); err != nil || n < 1 || n > maxRuns {
				http.Error(w, "limit must be a number of runs from 1 to "+strconv.Itoa(maxRuns), http.StatusBadRequest)
				return
			}
		}
		project := q.Get("project")
		if project != "" && !transcript.Plain(project) {
			http.Error(w, "project is not a plain name", http.StatusBadRequest)
			return
		}
		runs := h.Transcripts.List()
		slices.SortStableFunc(runs, func(a, b transcript.RunInfo) int { return b.Start.Compare(a.Start) })
		cfg := h.Config()
		out := []any{} // [] rather than null when there are none
		for _, ri := range runs {
			if len(out) == n {
				break
			}
			if project == "" || ri.Project == project {
				out = append(out, struct {
					runJSON
					Steps int `json:"steps"`
				}{toRun(ri, cfg), ri.Steps})
			}
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/runs/{project}/{issue}/{run}", func(w http.ResponseWriter, r *http.Request) {
		project, issue, run := r.PathValue("project"), r.PathValue("issue"), r.PathValue("run")
		if !transcript.Plain(project) || !transcript.Plain(issue) || !transcript.Plain(run) {
			http.Error(w, "not a plain name", http.StatusBadRequest)
			return
		}
		ri, steps, ok := h.Transcripts.Info(project, issue, run)
		if !ok || len(steps) == 0 {
			http.NotFound(w, r)
			return
		}
		out := make([]stepJSON, len(steps))
		for i, st := range steps {
			out[i] = stepJSON{N: st.N, Kind: st.Kind, Agent: st.Agent, Client: st.Client, Model: st.Model,
				Start: st.Start, End: st.End, Outcome: st.Outcome, Error: st.Error}
		}
		writeJSON(w, struct {
			runJSON
			Steps []stepJSON `json:"steps"`
		}{toRun(ri, h.Config()), out})
	})
	mux.HandleFunc("GET /api/runs/{project}/{issue}/{run}/steps/{n}/events", func(w http.ResponseWriter, r *http.Request) {
		project, issue, run := r.PathValue("project"), r.PathValue("issue"), r.PathValue("run")
		if !transcript.Plain(project) || !transcript.Plain(issue) || !transcript.Plain(run) {
			http.Error(w, "not a plain name", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil || n < 1 {
			http.Error(w, "step must be a number from 1", http.StatusBadRequest)
			return
		}
		from := 0
		if v := r.URL.Query().Get("from"); v != "" {
			if from, err = strconv.Atoi(v); err != nil || from < 0 {
				http.Error(w, "from must be a number of lines from 0", http.StatusBadRequest)
				return
			}
		}
		f, err := h.Transcripts.Open(project, issue, run, n)
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		streamStep(r, newSSE(w), f, from, h.Tick*pingTicks)
	})
}

// streamStep sends each line of a step's file from line from on, then
// follows the file until its end line or the request's end. A bufio.Reader
// tries its reader again after io.EOF, so reading on picks up what was
// appended.
func streamStep(r *http.Request, s sse, f io.Reader, from int, pingEvery time.Duration) {
	if s.ping() != nil { // sends the headers when there is nothing to send yet
		return
	}
	rd := bufio.NewReader(f)
	var part []byte // a line read up to the end of the file, without its newline yet
	poll := time.NewTicker(followEvery)
	defer poll.Stop()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for i := 0; ; {
		b, err := rd.ReadBytes('\n')
		part = append(part, b...)
		if err == nil {
			line := part[:len(part)-1]
			part = nil
			var typ struct{ Type string }
			// Every stored line is an object that starts with {"t":, so i goes
			// in front of it without encoding it again. Any other line, only
			// in a damaged file, is skipped but counted.
			ok := json.Unmarshal(line, &typ) == nil && bytes.HasPrefix(line, []byte(`{"`))
			if ok && i >= from {
				if s.send(append(fmt.Appendf(nil, `{"i":%d,`, i), line[1:]...)) != nil {
					return
				}
			}
			i++
			if typ.Type == "end" { // a step writes nothing after it
				s.done()
				return
			}
			continue
		}
		if err != io.EOF {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
		case <-ping.C:
			if s.ping() != nil {
				return
			}
		}
	}
}
