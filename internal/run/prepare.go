package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// baseBranch is the branch runs start from and pull requests target: the
// project's base branch, else the forge's default branch.
func baseBranch(ctx context.Context, proj cinderline.Project, fg forge.Forge) (string, error) {
	if proj.BaseBranch != "" {
		return proj.BaseBranch, nil
	}
	b, err := fg.DefaultBranch(ctx)
	if err == nil && b == "" {
		err = errors.New("the forge names none")
	}
	if err != nil {
		return "", fmt.Errorf("could not find the repository's default branch; set a base branch on the project in Cinderline (%w)", err)
	}
	return b, nil
}

// extraRepositories fetches the project's extra repositories and returns
// those the run can read, and a note for each one that failed: the run
// goes on without it.
func (p *Project) extraRepositories(ctx context.Context, ref string) (ok []config.ExtraRepository, notes []string) {
	if p.fetchExtras == nil || len(p.Config.ExtraRepositories) == 0 {
		return nil, nil
	}
	failed := map[string]error{}
	var repos []workspace.Extra
	for _, r := range p.Config.ExtraRepositories {
		token, _, err := p.Secrets.Get(p.Config, secrets.ExtraGitToken(r.URL))
		if err != nil {
			failed[r.Name()] = err
			continue
		}
		repos = append(repos, workspace.Extra{Name: r.Name(), URL: r.URL, Ref: r.Ref, Token: token})
	}
	maps.Copy(failed, p.fetchExtras(ctx, workspace.ExtraPath, repos))
	for _, r := range p.Config.ExtraRepositories {
		if err := failed[r.Name()]; err != nil {
			p.Log.Warn("extra repository not fetched", "project", p.Config.Key, "issue", ref, "repository", r.URL, "err", err)
			notes = append(notes, "Extra repository "+stripURL(r.URL)+" not fetched; the run goes on without it")
			continue
		}
		ok = append(ok, r)
	}
	return ok, notes
}

// Linked is a project this project's agents may read, with the folders of
// its repositories this run fetched under workspace.LinkedPath, as KEY/name.
type Linked struct {
	Key   string
	Paths []string
}

// linkedProjects fetches the repositories of the linked projects that are
// added on this runner: each one's own repository at its base branch and
// its extra repositories, each with the token stored for its URL. A linked
// project not added here, or a repository that fails, gets a note and the
// run goes on without it. The linked volume is filled even without links,
// so a removed link's code is gone.
func (p *Project) linkedProjects(ctx context.Context, ref string, linked []cinderline.LinkedProject) (links []Linked, notes []string) {
	if p.fetchExtras == nil {
		return nil, nil
	}
	links = make([]Linked, len(linked))
	var repos []workspace.Extra
	failed := map[string]error{}
	urls := map[string]string{} // by folder, for the notes
	for i, l := range linked {
		links[i] = Linked{Key: l.Key}
		j := slices.IndexFunc(p.Others, func(o config.Project) bool {
			// A token is for one workspace, so a link is in this project's.
			return o.Key == l.Key && o.Workspace == p.Config.Workspace && strings.TrimRight(o.URL, "/") == strings.TrimRight(p.Config.URL, "/")
		})
		if j < 0 {
			p.Log.Info("linked project not added on this runner", "project", p.Config.Key, "issue", ref, "linked", l.Key)
			notes = append(notes, "Linked project "+l.Key+" is not added on this runner, so the run goes on without its code; `"+name.Name+" add` it to give runs its code")
			continue
		}
		b := p.Others[j]
		names := map[string]bool{}
		for k, r := range append([]config.ExtraRepository{{URL: b.Repository, Ref: l.BaseBranch}}, b.ExtraRepositories...) {
			folder := l.Key + "/" + r.Name()
			kind := secrets.ExtraGitToken(r.URL)
			if k == 0 {
				kind = secrets.GitToken
			}
			if names[r.Name()] {
				// The one already listed keeps the folder.
				notes = append(notes, "Repository "+stripURL(r.URL)+" of linked project "+l.Key+" not fetched: another of its repositories has the same name")
				continue
			}
			names[r.Name()] = true
			links[i].Paths = append(links[i].Paths, folder)
			urls[folder] = r.URL
			err := r.CheckRefAndName()
			token := ""
			if err == nil {
				token, _, err = p.Secrets.Get(b, kind)
			}
			if err != nil {
				failed[folder] = err
				continue
			}
			repos = append(repos, workspace.Extra{Name: folder, URL: r.URL, Ref: r.Ref, Token: token})
		}
	}
	maps.Copy(failed, p.fetchExtras(ctx, workspace.LinkedPath, repos))
	for i := range links {
		links[i].Paths = slices.DeleteFunc(links[i].Paths, func(folder string) bool {
			err := failed[folder]
			if err != nil {
				p.Log.Warn("linked repository not fetched", "project", p.Config.Key, "issue", ref, "repository", urls[folder], "err", err)
				notes = append(notes, "Repository "+stripURL(urls[folder])+" of linked project "+links[i].Key+" not fetched; the run goes on without it")
			}
			return err != nil
		})
	}
	return links, notes
}

// runWorkspace is the facts of the run's clone and mounts for next.
func runWorkspace(branch, base string, extras []config.ExtraRepository, links []Linked, notes []string) cinderline.RunWorkspace {
	w := cinderline.RunWorkspace{Branch: branch, Base: base, ExtraRepositories: []cinderline.WorkspaceRepository{}, LinkedProjects: []cinderline.WorkspaceLink{}, Notes: append([]string{}, notes...)}
	for _, r := range extras {
		w.ExtraRepositories = append(w.ExtraRepositories, cinderline.WorkspaceRepository{Path: workspace.ExtraPath + "/" + r.Name(), URL: stripURL(r.URL)})
	}
	for _, l := range links {
		w.LinkedProjects = append(w.LinkedProjects, cinderline.WorkspaceLink{Key: l.Key, Paths: append([]string{}, l.Paths...)})
	}
	return w
}

// prepareRun prepares the run's environment and clone. What it could not
// prepare but the run can do without goes into the workspace's notes; an
// error means the run did not start.
func (p *Project) prepareRun(ctx context.Context, proj cinderline.Project, jb cinderline.Job) (*job, *conflict, *held, error) {
	ref, team := jb.Issue.Ref, jb.Team
	// The ref becomes a branch, a folder and a file name.
	if !workspace.IssueRef.MatchString(ref) {
		return nil, nil, nil, fmt.Errorf("%q is not an issue reference", ref)
	}
	if len(team.Agents) == 0 {
		return nil, nil, nil, errors.New("the team has no agents")
	}
	lead := team.Agents[0]
	if p.Clients[lead.Client] == nil {
		return nil, nil, nil, fmt.Errorf("agent %s runs on %s, which this runner does not run", lead.Name, lead.Client)
	}
	fg, err := p.Forge(proj.Forge)
	if err != nil {
		return nil, nil, nil, err
	}
	base, err := baseBranch(ctx, proj, fg)
	if err != nil {
		return nil, nil, nil, err
	}
	if p.prepare != nil {
		if err := p.prepare(ctx, proj, base); err != nil {
			return nil, nil, nil, fmt.Errorf("the run's environment could not be prepared: %w", err)
		}
	}
	j := &job{
		ref: ref, title: jb.Issue.Title, body: jb.Issue.Body, branch: name.Name + "/" + ref, base: base,
		team: team, check: proj.CheckCommand, idle: time.Duration(max(proj.IdleMinutes, 0)) * time.Minute, fg: fg,
	}
	extras, notes := p.extraRepositories(ctx, ref)
	// Fetched on every run, so a resume and a follow-up read fresh code too.
	links, more := p.linkedProjects(ctx, ref, proj.LinkedProjects)
	notes = append(notes, more...)
	// A run's attachments come from its steps; another run's files must not linger.
	if err := p.resetAttachments(); err != nil {
		return nil, nil, nil, err
	}
	h := &held{reports: map[string]string{}, agents: map[string]string{}}
	h.lead = session{issue: ref, agent: lead.Name}
	n, resumed, more := p.resume(ctx, j, lead.Client)
	notes = append(notes, more...)
	if resumed {
		j.dir, h.lead.id, h.lead.origin = n.Dir, n.Session, n.Origin
	} else if err := p.keepWork(ctx, ref, leadAuthor(team), ref+": "+j.title); err != nil {
		return nil, nil, nil, err
	} else if j.dir, err = p.Workspace.Prepare(ctx, j.branch, base); err != nil {
		return nil, nil, nil, err
	}
	j.ws = runWorkspace(j.branch, base, extras, links, notes)
	// Also on a fresh clone, where the merge changes nothing, as Prepare
	// does not say whether it continued the remote branch.
	return j, p.catchUp(ctx, j), h, nil
}

// resume returns the note of the session the run continues, if it can: a
// session of this client, in a clone that is still there, of the run kind
// the team has now. A kept session's clone is first fast-forwarded to the
// remote branch. What it had to drop gets a note for the workspace. A solo
// run's note is removed, so a crash during it starts fresh; a team's stays.
func (p *Project) resume(ctx context.Context, j *job, cl string) (Note, bool, []string) {
	ref := j.ref
	if !j.isTeam() {
		defer p.dropNote(ref)
	}
	n, ok := p.readNote(ref)
	switch {
	case !ok:
		return Note{}, false, nil
	case n.Pruned != "":
		p.dropNote(ref)
		return Note{}, false, []string{"The agent's earlier session on " + ref + " was deleted because " + n.Pruned + ", so this run starts a fresh session"}
	case n.Session == "":
		return Note{}, false, nil
	case n.Client != cl:
		return Note{}, false, nil
	}
	if ok, err := p.Workspace.Exists(ctx, n.Dir); !ok {
		if err != nil {
			p.Log.Warn("paused clone not checked", "project", p.Config.Key, "issue", ref, "err", err)
		}
		return Note{}, false, nil
	}
	if n.Origin != "kept" {
		return n, true, nil
	}
	if err := p.Workspace.FastForward(ctx, n.Dir, j.branch); err != nil {
		p.Log.Warn("kept session dropped: its clone is not up to date", "project", p.Config.Key, "issue", ref, "err", err)
		p.dropNote(ref)
		p.forget(ctx, ref, n.Session)
		return Note{}, false, []string{"The agent's earlier session on " + ref + " could not be continued: its clone could not be brought up to date with the remote branch " + j.branch + "; this run starts a fresh session"}
	}
	return n, true, nil
}

// keepWork commits what the issue's clone at RunsPath/<ref> holds and
// pushes its branch when it has commits no remote branch has, before the
// clone is replaced or deleted: uncommitted or unpushed work is never lost.
// A clone that is not there is no error; an error keeps the clone.
func (p *Project) keepWork(ctx context.Context, ref, author, message string) error {
	dir, branch := workspace.RunsPath+"/"+ref, name.Name+"/"+ref
	if ok, err := p.Workspace.Exists(ctx, dir); !ok || err != nil {
		return err
	}
	if _, err := p.Workspace.Commit(ctx, dir, author, message); err != nil {
		return fmt.Errorf("the issue's clone could not be committed, so it is kept: %w", err)
	}
	clones, err := p.Workspace.Stranded(ctx)
	if err != nil {
		return err // unknown, so the clone may not be replaced
	}
	for _, c := range clones {
		switch {
		case c.Dir != dir:
		case c.Branch != branch:
			return fmt.Errorf("the issue's clone has unpushed commits on %s, not %s, so it is kept", c.Branch, branch)
		default:
			if _, err := p.Workspace.Push(ctx, dir, branch); err != nil {
				return fmt.Errorf("the issue's clone has unpushed commits, kept: %w", err)
			}
			p.Log.Info("unpushed work pushed", "project", p.Config.Key, "issue", ref, "branch", branch)
		}
	}
	return nil
}

// catchUp merges the base into continued work before the first step. What
// a paused turn left is committed first. It returns a conflict for the
// run's first request; an error only logs: publish tries again.
func (p *Project) catchUp(ctx context.Context, j *job) *conflict {
	log := p.Log.With("project", p.Config.Key, "issue", j.ref)
	author := leadAuthor(j.team)
	if _, err := p.Workspace.Commit(ctx, j.dir, author, j.ref+": "+j.title); err != nil {
		log.Warn("base not merged in", "err", err)
		return nil
	}
	cf, err := updateBranch(ctx, p.Workspace, j.ref, j.dir, j.base, author)
	switch {
	case err != nil:
		log.Warn("base not merged in", "err", err)
	case cf != nil:
		log.Info("base not merged in", "base", j.base, "conflicts", cf.files)
	}
	return cf
}
