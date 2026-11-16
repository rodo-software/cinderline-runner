package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The cases of legacy runner.test.mjs, lib.test.mjs and lib.prompt.test.mjs
// on questions, pauses and comments during a run. The legacy runner tests
// drive a whole process with fake CLIs; these drive the same branches of
// attempts() and takePaused() against the fakes.

// pausedNote writes a note for CSR-7 whose clone exists, as pausedRun does.
func pausedNote(t *testing.T, p Project, n Paused) Paused {
	t.Helper()
	if n.Dir == "" {
		n.Dir = t.TempDir()
	}
	if err := p.SavePaused("CSR-7", n); err != nil {
		t.Fatal(err)
	}
	return n
}

// take is TakePaused for a paused (not kept) note, which needs no API.
func take(p Project, ref, client string) (Paused, bool) {
	return p.TakePaused(context.Background(), nil, &workspace.Fake{}, ref, "cinderline/"+ref, client, false)
}

func noteFile(p Project) string {
	return filepath.Join(PausedDir(p.State, p.Config), "CSR-7.json")
}

// Ported from runner.test.mjs "an answered issue of an OpenCode orchestrator
// resumes in OpenCode", with legacy readPaused's other refusals: a note
// resumes only in the client that made it, with a session and its clone.
// Taking a note removes it, so a crash during the resumed run starts fresh.
func TestTakePaused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		note   Paused
		client string
		ok     bool
	}{
		{"an OpenCode note in OpenCode", Paused{Session: "ses_paused", Client: "opencode"}, "opencode", true},
		{"an OpenCode note in Claude", Paused{Session: "ses_paused", Client: "opencode"}, "claude", false},
		{"no session", Paused{Client: "claude"}, "claude", false},
		{"a clone that is gone", Paused{Session: "s", Client: "claude", Dir: "/gone/CSR-7"}, "claude", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := project(t)
			note := pausedNote(t, p, tt.note)
			got, ok := take(p, "CSR-7", tt.client)
			if ok != tt.ok || ok && got != note {
				t.Fatalf("TakePaused = %+v, %v; want %+v, %v", got, ok, note, tt.ok)
			}
			if _, err := os.Stat(noteFile(p)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("note after TakePaused: %v, want removed", err)
			}
		})
	}
	if _, ok := take(project(t), "CSR-7", "claude"); ok {
		t.Fatal("TakePaused found a note that was never written")
	}
}
