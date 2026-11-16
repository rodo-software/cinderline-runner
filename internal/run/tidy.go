package run

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// held lists the issues the project keeps a session note or a clone of, for
// the poll's held, oldest note first, a clone without a note before them,
// so the ones the app's limit leaves out wait for a later tidy. It returns
// the clones with unpushed commits too. ok is false when the clones could
// not be listed: then nothing may be deleted.
func (p *Project) held(ctx context.Context) (refs []string, stranded []workspace.Clone, ok bool) {
	entries, err := os.ReadDir(PausedDir(p.State, p.Config))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.Log.Warn("session notes not read", "project", p.Config.Key, "err", err)
	}
	for _, e := range entries {
		if ref, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			refs = append(refs, ref)
		}
	}
	if p.Workspace != nil {
		dirs, err := p.Workspace.List(ctx)
		if err == nil {
			stranded, err = p.Workspace.Stranded(ctx)
		}
		if err != nil {
			p.Log.Warn("clones not listed, nothing tidied", "project", p.Config.Key, "err", err)
			return nil, nil, false
		}
		for _, dir := range dirs {
			if ref := path.Base(dir); !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	age := map[string]time.Time{} // zero: no note
	for _, ref := range refs {
		n, _ := p.readNote(ref)
		age[ref] = n.Time
	}
	slices.SortStableFunc(refs, func(a, b string) int { return age[a].Compare(age[b]) })
	return refs, stranded, true
}

// tidy pushes the stranded clones and then does what the app answered for
// each held issue: drop deletes its clone, session files and note, expire
// deletes a clone without a note at once and applies keep_sessions_for and
// then min_free_disk to a session, oldest first, and keep leaves it. Work
// in a clone is committed and pushed before it is deleted, and a clone
// whose work cannot be pushed stays. Only refs in asked are touched: the
// app's answer never names a path. Every failure only logs.
func (p *Project) tidy(ctx context.Context, asked []string, items []cinderline.HeldItem, stranded []workspace.Clone) {
	for _, c := range stranded {
		ref, _ := strings.CutPrefix(c.Branch, name.Name+"/")
		if err := p.Workspace.Push(ctx, c.Dir, c.Branch); err != nil {
			p.Log.Warn("stranded branch not pushed", "project", p.Config.Key, "issue", ref, "err", err)
			continue
		}
		p.Log.Info("stranded branch pushed", "project", p.Config.Key, "issue", ref, "branch", c.Branch)
	}

	type kept struct {
		ref string
		n   Note
	}
	var idle []kept
	for _, it := range items {
		if !slices.Contains(asked, it.Ref) {
			continue
		}
		n, noted := p.readNote(it.Ref)
		n.Dir = cmp.Or(n.Dir, workspace.RunsPath+"/"+it.Ref)
		switch it.Session {
		case "drop":
			p.remove(ctx, it.Ref, n, "issue closed")
		case "expire":
			switch {
			case !noted:
				p.remove(ctx, it.Ref, n, "no session resumes the clone")
			case n.Pruned != "" || n.Session == "":
				// Nothing to expire: a pruned note waits for the next run.
			case p.KeepFor > 0 && time.Since(n.Time) > p.KeepFor:
				p.remove(ctx, it.Ref, n, prunedAge)
			default:
				idle = append(idle, kept{it.Ref, n})
			}
		case "keep":
		default:
			p.Log.Warn("held answer not understood", "project", p.Config.Key, "issue", it.Ref, "session", it.Session)
		}
	}
	if p.MinFree <= 0 || p.Workspace == nil || len(idle) == 0 {
		return
	}
	slices.SortFunc(idle, func(a, b kept) int { return a.n.Time.Compare(b.n.Time) })
	free, err := p.Workspace.Free(ctx)
	for ; err == nil && free < p.MinFree && len(idle) > 0; idle = idle[1:] {
		p.remove(ctx, idle[0].ref, idle[0].n, prunedDisk)
		free, err = p.Workspace.Free(ctx)
	}
	switch {
	case err != nil:
		p.Log.Warn("free disk space not checked", "project", p.Config.Key, "err", err)
	case free < p.MinFree:
		p.Log.Warn("free disk space below min_free_disk, with no kept session left to delete", "project", p.Config.Key, "free_bytes", free, "min_free_bytes", p.MinFree)
	}
}

// remove deletes ref's clone, once its work is kept, its client session
// files and its note, in that order, so a failure leaves the note for the
// next tidy. why is logged; for a session pruned by the limits it replaces
// the note, so the next run says why it starts fresh.
func (p *Project) remove(ctx context.Context, ref string, n Note, why string) {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	if n.Dir != "" && p.Workspace != nil {
		if err := p.keepWork(ctx, ref, "", ref+": work left in the clone"); err != nil {
			log.Warn("clone not deleted", "err", err)
			return
		}
		if err := p.Workspace.Remove(ctx, n.Dir); err != nil {
			log.Warn("clone not deleted", "err", err)
			return
		}
	}
	if n.Session != "" && !p.forget(ctx, ref, n.Session) {
		return
	}
	if why == prunedAge || why == prunedDisk {
		p.saveNote(ref, Note{Pruned: why})
	} else {
		p.dropNote(ref)
	}
	log.Info("session deleted", "reason", why)
}
