package web

import (
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/transcript"
)

func TestActivity(t *testing.T) {
	const (
		call = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`
		text = `{"type":"assistant","message":{"content":[{"type":"text","text":"Tests pass.\nNow the docs."}]}}`
		res  = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	)
	for _, tc := range []struct {
		name, kind, client string
		out                []string
		want               string
	}{
		{"run without output", "run", "claude", nil, "agent turn"},
		{"resolve without output", "resolve", "claude", nil, "resolving a merge conflict"},
		{"check", "check", "", []string{"ok  ./..."}, "running the check command"},
		{"publish", "publish", "", nil, "pushing and opening the pull request"},
		{"stop", "stop", "", nil, "stopping"},
		{"a kind it does not know", "wait", "", nil, "wait"},
		{"claude, last a tool call", "run", "claude", []string{text, call, res}, `coder: Bash {"command":"go test ./..."}`},
		{"claude, last text", "run", "claude", []string{call, res, text}, "coder: Tests pass."},
		{"resolve, last a tool call", "resolve", "claude", []string{call}, `coder: Bash {"command":"go test ./..."}`},
		{"an unknown client", "run", "aider", []string{"editing a.go"}, "agent turn"},
		{"a long line", "run", "claude", []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("é", 300) + `"}]}}`},
			"coder: " + strings.Repeat("é", 152) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := transcript.Store{Dir: t.TempDir()}
			r := s.Begin("CLR", "CLR-291", t1)
			r.Step("run", transcript.Start{Agent: "coder", Client: "claude"}).End("ok", "") // an earlier step does not count
			st := r.Step(tc.kind, transcript.Start{Agent: "coder", Client: tc.client})
			st.Prompt("do it")
			for _, l := range tc.out {
				st.Out(l)
			}
			got := activity(s, "CLR", "CLR-291", t1.Format(transcript.RunID))
			if got != tc.want {
				t.Errorf("activity = %q, want %q", got, tc.want)
			}
			if n := len([]rune(got)); n > maxActivity {
				t.Errorf("%d characters, more than %d", n, maxActivity)
			}
		})
	}
	if got := activity(transcript.Store{Dir: t.TempDir()}, "CLR", "CLR-1", t1.Format(transcript.RunID)); got != "" {
		t.Errorf("a run without steps: %q", got)
	}
}

func TestRunsActivityOfLiveRunsOnly(t *testing.T) {
	_, tok, h := runsHandler(t)
	var got []struct {
		Issue    string
		Live     bool
		Activity string
	}
	getJSON(t, h, tok, "/api/runs", &got)
	for _, r := range got {
		if r.Live != (r.Activity != "") {
			t.Errorf("%s: live %v, activity %v", r.Issue, r.Live, r.Activity)
		} else if r.Live && r.Activity != "agent turn" { // CLR-12.1: a run step without output
			t.Errorf("%s: activity %q", r.Issue, r.Activity)
		}
	}
}
