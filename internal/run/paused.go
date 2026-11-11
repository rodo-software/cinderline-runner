package run

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Paused is a paused or finished run's note: only a cache of where to
// resume (decision 0091). Without it a fresh run reads the question and
// answer, or the follow-up, from the comments.
type Paused struct {
	Dir     string `json:"dir"`     // the issue's clone
	Session string `json:"session"` // the client session to resume
	Client  string `json:"client"`  // an OpenCode session resumes only in OpenCode
	// Kept is when the run that kept its session for a follow-up ended
	// (CSR-145); zero for a run paused for a person or between rounds, which
	// pruning never touches.
	Kept time.Time `json:"kept,omitzero"`
	// Pruned says why pruning deleted the kept session, for the next run's
	// first comment; such a note names no session.
	Pruned string `json:"pruned,omitempty"`
}

// Why Prune deleted a kept session, as Paused.Pruned.
const (
	prunedAge  = "it was not used for longer than keep_sessions_for"
	prunedDisk = "free disk space was below min_free_disk"
)

// ResumePrompt is what a paused session gets when it continues (decisions
// 0116, 2026-11-03-reply-person), the text of legacy lib.mjs resumePrompt.
func ResumePrompt(ref string) string {
	return "A person has responded to your question on " + ref + ". Read the latest comments with list_comments on " + ref + ". If the person's answer also asks you something, reply with reply_person on " + ref + " first, then continue the task under the same rules."
}

// FollowUpPrompt is what a finished run's kept session gets when its issue
// is back in To do (CSR-145).
func FollowUpPrompt(ref string) string {
	return "Your earlier run on " + ref + " finished, and a person has moved " + ref + " back to To do. Read the comments since your last turn with list_comments on " + ref + ": they hold a follow-up question or change request. This worktree is up to date with the remote branch, which a person may have changed. If a person asks you something, reply with reply_person on " + ref + " first, then continue the task under the same rules."
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
// resumed run starts fresh. It returns the note only when resumable does.
func (p Project) TakePaused(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, ref, branch, client string) (Paused, bool) {
	n, ok := p.resumable(ctx, c, ws, ref, branch, client)
	p.dropPaused(ref)
	return n, ok
}

// resumable returns issue ref's note when it names a session of this client
// and its clone is still there, and leaves it in place. A kept session's
// clone is first fast-forwarded to the remote branch; when that fails, or
// when pruning deleted the session, the note is dropped and the issue gets a
// comment that this run starts fresh (CSR-145).
func (p Project) resumable(ctx context.Context, c *cinderline.Client, ws workspace.Workspace, ref, branch, client string) (Paused, bool) {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	n, ok := p.readPaused(ref)
	if ok && n.Pruned != "" {
		p.dropPaused(ref)
		p.comment(ctx, c, ref, "The agent's earlier session on "+ref+" was deleted because "+n.Pruned+", so this run starts a fresh session from the branch and the comments.")
		return Paused{}, false
	}
	if !ok || n.Session == "" || n.Client != client {
		return Paused{}, false
	}
	if ok, err := ws.Exists(ctx, n.Dir); !ok {
		if err != nil {
			log.Warn("paused clone not checked", "err", err)
		}
		return Paused{}, false
	}
	if n.Kept.IsZero() {
		return n, true
	}
	if err := ws.FastForward(ctx, n.Dir, branch); err != nil {
		if ctx.Err() != nil {
			return Paused{}, false
		}
		log.Warn("kept session dropped: its clone is not up to date", "err", err)
		p.dropPaused(ref)
		p.forget(ctx, ref, n.Session)
		p.comment(ctx, c, ref, "The agent's earlier session on "+ref+" could not be continued: its worktree could not be brought up to date with the remote branch "+branch+" (a rewritten branch, or uncommitted changes). This run starts a fresh session from the branch and the comments.")
		return Paused{}, false
	}
	return n, true
}

// keep writes the note of a finished run's session, so a follow-up resumes
// it (CSR-145), or drops the note when there is no session to keep.
func (p Project) keep(ref string, n Paused) {
	if n.Session == "" || n.Dir == "" {
		p.dropPaused(ref)
		return
	}
	n.Kept = time.Now()
	if err := p.SavePaused(ref, n); err != nil {
		p.Log.Warn("kept session note not written", "project", p.Config.Key, "issue", ref, "err", err)
	}
}

// comment adds a runner comment; a failure only logs.
func (p Project) comment(ctx context.Context, c *cinderline.Client, ref, body string) {
	if _, err := c.AddComment(ctx, ref, body); err != nil {
		p.Log.Warn("comment not added", "project", p.Config.Key, "issue", ref, "err", err)
	}
}

// forget deletes ref's client session files; a failure only logs.
func (p Project) forget(ctx context.Context, ref, session string) bool {
	if p.deleteSession == nil {
		return true
	}
	if err := p.deleteSession(ctx, ref, session); err != nil {
		p.Log.Warn("session files not deleted", "project", p.Config.Key, "issue", ref, "err", err)
		return false
	}
	return true
}
