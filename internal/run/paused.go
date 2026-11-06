package run

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rodo-software/cinderstack-runner/internal/cinderstack"
	"github.com/rodo-software/cinderstack-runner/internal/workspace"
)

// The kinds of run a paused note can be: a coding run publishes, a writer
// team's run rewrites the issue (decision 2026-11-03-issue-writer-teams).
const (
	KindCode  = "code"
	KindWrite = "write"
)

// Paused is a paused run's note: only a cache of where to resume (decision
// 0091). Without it a fresh run reads the question and answer from the
// comments.
type Paused struct {
	Dir     string `json:"dir"`     // the issue's clone
	Session string `json:"session"` // the client session to resume
	Client  string `json:"client"`  // an OpenCode session resumes only in OpenCode
	Kind    string `json:"kind"`
}

// ResumePrompt is what a paused session gets when it continues (decisions
// 0116, 2026-11-03-reply-person), the text of legacy lib.mjs resumePrompt.
func ResumePrompt(ref string) string {
	return "A person has responded to your question on " + ref + ". Read the latest comments with list_comments on " + ref + ". If the person's answer also asks you something, reply with reply_person on " + ref + " first, then continue the task under the same rules."
}

func (p Project) noteFile(ref string) string {
	return filepath.Join(PausedDir(p.State, p.Config), ref+".json")
}

// SavePaused writes the note of issue ref's paused run.
func (p Project) SavePaused(ref string, n Paused) error {
	if err := os.MkdirAll(PausedDir(p.State, p.Config), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return os.WriteFile(p.noteFile(ref), b, 0o600)
}

// readPaused returns issue ref's note, if there is one it can read.
func (p Project) readPaused(ref string) (Paused, bool) {
	b, err := os.ReadFile(p.noteFile(ref))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			p.Log.Warn("paused note not read", "project", p.Config.Key, "issue", ref, "err", err)
		}
		return Paused{}, false
	}
	var n Paused
	if err := json.Unmarshal(b, &n); err != nil {
		p.Log.Warn("paused note not read", "project", p.Config.Key, "issue", ref, "err", err)
		return Paused{}, false
	}
	return n, true
}

func (p Project) dropPaused(ref string) {
	if err := os.Remove(p.noteFile(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.Log.Warn("paused note not dropped", "project", p.Config.Key, "issue", ref, "err", err)
	}
}

// TakePaused reads and removes issue ref's note, so a crash during the
// resumed run starts fresh. It returns the note only when it names a session
// of this client and its clone is still there.
func (p Project) TakePaused(ref, client string) (Paused, bool) {
	n, ok := p.readPaused(ref)
	p.dropPaused(ref)
	if !ok || n.Session == "" || n.Client != client {
		return Paused{}, false
	}
	if _, err := os.Stat(n.Dir); err != nil {
		return Paused{}, false
	}
	return n, true
}

// DropChangedKind drops issue ref's note when the team changed between
// writer and coding while it waited, so the run starts over with the new
// team, and says so (decision 2026-11-03-issue-writer-teams). It names a
// dropped coding run's last commit, so its work can be found again.
func (p Project) DropChangedKind(ctx context.Context, c *cinderstack.Client, ws workspace.Workspace, ref, kind string) error {
	n, ok := p.readPaused(ref)
	if !ok || n.Session == "" || n.Kind == kind {
		return nil
	}
	p.dropPaused(ref)
	msg := "The team changed while paused, so this run starts over."
	if n.Kind == KindCode {
		head, err := ws.Head(ctx, n.Dir)
		if err != nil {
			p.Log.Warn("last commit not read", "project", p.Config.Key, "issue", ref, "err", err)
		} else {
			// Not legacy's "stays in the runner's clone": where it stays is the Workspace's (M4).
			msg += " The paused coding run's last commit was " + head + "."
		}
	}
	p.Log.Info("team changed while paused, starting over", "project", p.Config.Key, "issue", ref, "was", n.Kind, "now", kind)
	_, err := c.AddComment(ctx, ref, msg)
	return err
}
