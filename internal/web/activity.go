package web

import (
	"bytes"
	"cmp"
	"encoding/json"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/transcript"
)

// maxActivity is the most characters of a run's activity, the … of a cut one
// included.
const maxActivity = 160

// activityTail is how much of an agent turn's file activity reads: the end
// of the turn only, so a long turn costs no more than a short one.
const activityTail = 64 << 10

// stepWords says what a step of each kind does, for a step without output.
var stepWords = map[string]string{
	"run":     "agent turn",
	"resolve": "resolving a merge conflict",
	"check":   "running the check command",
	"publish": "pushing and opening the pull request",
	"stop":    "stopping",
}

// activity is one line of what a run does now, from its last step: for an
// agent turn its newest tool call or text, as its client's parser reads the
// out lines at the end of its file, else the step's kind in words. Empty for
// a run without steps.
func activity(s transcript.Store, project, issue, run string) string {
	steps := s.Steps(project, issue, run)
	if len(steps) == 0 {
		return ""
	}
	st := steps[len(steps)-1]
	now := cmp.Or(stepWords[st.Kind], st.Kind)
	if st.Kind == "run" || st.Kind == "resolve" {
		for line := range bytes.Lines(s.Tail(project, issue, run, st.N, activityTail)) {
			var l struct{ Type, Line string }
			if json.Unmarshal(line, &l) != nil || l.Type != "out" {
				continue
			}
			for _, e := range parseOut(st.Client, l.Line) {
				switch {
				case e.Kind == "tool_call":
					now = agentSays(st.Agent, strings.TrimSpace(e.Name+" "+firstLine(e.Input)))
				case e.Kind == "text" && strings.TrimSpace(e.Text) != "":
					now = agentSays(st.Agent, firstLine(e.Text))
				}
			}
		}
	}
	if r := []rune(now); len(r) > maxActivity {
		now = string(r[:maxActivity-1]) + "…"
	}
	return now
}

func agentSays(agent, what string) string {
	if agent == "" {
		return what
	}
	return agent + ": " + what
}

// firstLine is the first line of s that is not blank, trimmed.
func firstLine(s string) string {
	for l := range strings.Lines(s) {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
