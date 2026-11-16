package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

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
