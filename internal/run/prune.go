package run

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
)

// Prune deletes what the project's notes keep that nobody will resume
// (CSR-145). An issue that is Done, Cancelled or archived loses its note,
// clone and session files. A session kept after a finished run, on an issue
// that is not To do, In progress or Blocked, is deleted when its last run is
// older than KeepFor, and then oldest first while free space is below
// MinFree; its note then says why, for the next run's first comment. A
// paused run's note (a question, a turn limit, an orchestrator between
// rounds) is never pruned, and a clone with commits no remote branch has is
// never deleted.
//
// It runs at start, after the stranded branches are pushed, and before every
// run; the claimed issue is In progress then, so it is never touched. Only a
// stop is an error: pruning never keeps work from starting.
func (p Project) Prune(ctx context.Context, c *cinderline.Client) error {
	entries, err := os.ReadDir(PausedDir(p.State, p.Config))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.Log.Warn("paused notes not read", "project", p.Config.Key, "err", err)
	}
	type kept struct {
		ref string
		n   Paused
	}
	var idle []kept
	stranded := p.strandedDirs(ctx)
	for _, e := range entries {
		ref, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		issue, err := c.GetIssue(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A note the runner cannot check must not keep it from working.
			p.Log.Warn("paused note not checked", "project", p.Config.Key, "issue", ref, "err", err)
			continue
		}
		n, _ := p.readPaused(ref)
		status := issue.Status.Value
		switch {
		case issue.ArchivedAt != nil || status == "done" || status == "cancelled":
			p.remove(ctx, ref, n, "", stranded)
		case status == "blocked" || status == "todo" || status == "in_progress":
			// Part of a run, or a follow-up about to resume (decision 0122).
		case n.Kept.IsZero():
			// A paused note of an issue a person moved on, as before CSR-145
			// (0114); a pruned session's note stays for its comment.
			if n.Pruned == "" {
				p.dropPaused(ref)
				p.Log.Info("paused note dropped", "project", p.Config.Key, "issue", ref, "status", status)
			}
		case p.KeepFor > 0 && time.Since(n.Kept) > p.KeepFor:
			p.remove(ctx, ref, n, prunedAge, stranded)
		default:
			idle = append(idle, kept{ref, n})
		}
	}
	if p.MinFree <= 0 || p.Workspace == nil {
		return ctx.Err()
	}
	slices.SortFunc(idle, func(a, b kept) int { return a.n.Kept.Compare(b.n.Kept) })
	free, err := p.Workspace.Free(ctx)
	for ; err == nil && free < p.MinFree && len(idle) > 0; idle = idle[1:] {
		p.remove(ctx, idle[0].ref, idle[0].n, prunedDisk, stranded)
		free, err = p.Workspace.Free(ctx)
	}
	switch {
	case err != nil:
		p.Log.Warn("free disk space not checked", "project", p.Config.Key, "err", err)
	case free < p.MinFree:
		p.Log.Warn("free disk space below min_free_disk, with no kept session left to delete", "project", p.Config.Key, "free_bytes", free, "min_free_bytes", p.MinFree)
	}
	return ctx.Err()
}

// strandedDirs returns a func that reports whether a clone has commits no
// remote branch has, listing them once on first use. When the list fails it
// reports every clone stranded, so none is deleted.
func (p Project) strandedDirs(ctx context.Context) func(dir string) bool {
	var dirs map[string]bool
	var failed bool
	return func(dir string) bool {
		if dirs == nil && !failed {
			clones, err := p.Workspace.Stranded(ctx)
			if failed = err != nil; failed {
				p.Log.Warn("stranded clones not listed, none deleted", "project", p.Config.Key, "err", err)
			}
			dirs = map[string]bool{}
			for _, cl := range clones {
				dirs[cl.Dir] = true
			}
		}
		return failed || dirs[dir]
	}
}

// remove deletes ref's clone, its client session files and its note, in that
// order, so a failure leaves the note for the next prune to retry. With a
// reason (pruned), the note is replaced by one that says why, for the
// next run's first comment.
func (p Project) remove(ctx context.Context, ref string, n Paused, pruned string, stranded func(string) bool) {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	var before int64 = -1
	if n.Dir != "" && p.Workspace != nil {
		if stranded(n.Dir) {
			log.Warn("clone has unpushed commits, not deleted", "dir", n.Dir)
			return
		}
		if free, err := p.Workspace.Free(ctx); err == nil {
			before = free
		}
		if err := p.Workspace.Remove(ctx, n.Dir); err != nil {
			log.Warn("clone not deleted", "err", err)
			return
		}
	}
	if (n.Session != "" || n.Dir != "") && !p.forget(ctx, ref, n.Session) {
		return
	}
	if pruned == "" {
		p.dropPaused(ref)
	} else if err := p.SavePaused(ref, Paused{Pruned: pruned}); err != nil {
		log.Warn("pruned note not written", "err", err)
	}
	args := []any{"reason", pruned}
	if pruned == "" {
		args = []any{"reason", "issue closed"}
	}
	if !n.Kept.IsZero() {
		args = append(args, "age", time.Since(n.Kept).Round(time.Minute).String())
	}
	if before >= 0 {
		if after, err := p.Workspace.Free(ctx); err == nil {
			args = append(args, "freed_bytes", max(0, after-before))
		}
	}
	log.Info("session deleted", args...)
}
