package run

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
)

// Note is a kept or paused session's note: only a cache of where to resume
// (decision 0091). Without it a run starts a fresh session.
type Note struct {
	Dir     string    `json:"dir"`     // the issue's clone
	Session string    `json:"session"` // the lead's client session
	Client  string    `json:"client"`  // a session resumes only in its own client
	Origin  string    `json:"origin"`  // the word of the done step: paused or kept
	Time    time.Time `json:"time"`    // when it was written
	// Manager is a run in the Project Manager slot: its clone is read-only,
	// so it is replaced fresh on every run and never pushed (CLR-194).
	Manager bool `json:"manager,omitempty"`
	// Pruned says why tidying deleted the session, for the next run's
	// workspace notes; such a note names no session.
	Pruned string `json:"pruned,omitempty"`
	// Total is the session's usage total so far, for a client that reports
	// one (Codex): a resumed turn's usage is its difference (CLR-232).
	Total *client.Usage `json:"total,omitempty"`
}

// Why tidying deleted a session, as Note.Pruned.
const (
	prunedAge  = "it was not used for longer than keep_sessions_for"
	prunedDisk = "free disk space was below min_free_disk"
)

func (p *Project) noteFile(ref string) string {
	return filepath.Join(PausedDir(p.State, p.Config), ref+".json")
}

// saveNote writes issue ref's note atomically, stamped with the time now:
// a crash must not leave a torn note, which would lose the session.
func (p *Project) saveNote(ref string, n Note) {
	n.Time = time.Now()
	b, _ := json.Marshal(n)
	if err := config.WriteFile(p.noteFile(ref), b); err != nil {
		p.Log.Warn("session note not written", "project", p.Config.Key, "issue", ref, "err", err)
	}
}

// readNote returns issue ref's note, if there is one it can read.
func (p *Project) readNote(ref string) (Note, bool) {
	b, err := os.ReadFile(p.noteFile(ref))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			p.Log.Warn("session note not read", "project", p.Config.Key, "issue", ref, "err", err)
		}
		return Note{}, false
	}
	var n Note
	if err := json.Unmarshal(b, &n); err != nil {
		p.Log.Warn("session note not read", "project", p.Config.Key, "issue", ref, "err", err)
		return Note{}, false
	}
	return n, true
}

func (p *Project) dropNote(ref string) {
	if err := os.Remove(p.noteFile(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.Log.Warn("session note not dropped", "project", p.Config.Key, "issue", ref, "err", err)
	}
}

// forget deletes ref's client session files; a failure only logs.
func (p *Project) forget(ctx context.Context, ref, session string) bool {
	if p.deleteSession == nil {
		return true
	}
	if err := p.deleteSession(ctx, ref, session); err != nil {
		p.Log.Warn("session files not deleted", "project", p.Config.Key, "issue", ref, "err", err)
		return false
	}
	return true
}
