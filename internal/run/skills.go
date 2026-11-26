package run

import (
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/client"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// maxSkills is the most skill packages a job names (the app's contract).
const maxSkills = 10

var (
	commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// skillPathChars keeps a package's path free of what a docker --mount
	// value or a shell would read: commas, quotes, colons, whitespace.
	skillPathChars = regexp.MustCompile(`^[A-Za-z0-9._@+/-]+$`)
)

// checkSkill says why the runner cannot use a skill package as the app
// named it, before any of its fields becomes a path or an argument; nil:
// it can. The errors leave the URL out, which may hold user info.
func checkSkill(s cinderline.Skill) error {
	switch {
	case !config.ValidSkillName(s.Name):
		return errors.New("its name is not lower-case letters and digits with single hyphens, at most 64 characters")
	case config.CheckSkillURL(s.URL) != nil:
		return errors.New("its URL is not an https URL without user info, query or fragment, at most 500 characters")
	case !commitID.MatchString(s.Commit):
		return errors.New("its commit is not 40 lower-case hex digits")
	case s.Path != "" && !validSkillPath(s.Path):
		return errors.New("its path is not a clean relative path without .., at most 200 characters")
	}
	return nil
}

// validSkillPath says p is a clean relative path in the repository.
func validSkillPath(p string) bool {
	return len(p) <= 200 && skillPathChars.MatchString(p) && !path.IsAbs(p) && path.Clean(p) == p && p != "." &&
		!slices.Contains(strings.Split(p, "/"), "..")
}

// skillAllowed says the runner's local config allows s on a strict runner:
// an allow_skills entry with its name and its URL.
func (p *Project) skillAllowed(s cinderline.Skill) bool {
	return slices.Contains(p.AllowSkills, config.AllowedSkill{Name: s.Name, URL: s.URL})
}

// skillPackages checks the skill packages the job names, refuses them on a
// strict runner unless config allows them, and fetches the others at their
// commit. It returns those loaded, and the state of each one for next. One
// that is not loaded is logged and the run goes on without it.
func (p *Project) skillPackages(ctx context.Context, ref string, skills []cinderline.Skill) (loaded []client.SkillMount, states []cinderline.SkillState) {
	log := p.Log.With("project", p.Config.Key, "issue", ref)
	names := map[string]bool{}
	geminiSkills := map[string]string{} // skill folder: the package that has it as a plain skill
	for i, s := range skills {
		state := cinderline.SkillFailed
		err := checkSkill(s)
		switch {
		case i >= maxSkills:
			log.Warn("skill package not loaded: the job names more than 10", "skill", s.Name)
		case err != nil:
			log.Warn("skill package not loaded: it failed the runner's check", "skill", s.Name, "err", err)
		case names[s.Name]:
			log.Warn("skill package not loaded: an earlier package of the job has its name", "skill", s.Name)
		case p.StrictHosts && !p.skillAllowed(s):
			state = cinderline.SkillRefusedStrict
			log.Info("skill package refused: strict host blocking is on and `"+name.Name+" allow-skill` does not allow it with this URL", "skill", s.Name, "url", s.URL)
		case p.fillSkill == nil:
			log.Warn("skill package not loaded: this runner cannot fetch it", "skill", s.Name)
		default:
			vol, contents, err := p.fillSkill(ctx, s.URL, s.Commit, s.Path)
			if err != nil {
				log.Warn("skill package not loaded: it could not be fetched at its commit", "skill", s.Name, "url", s.URL, "commit", s.Commit, "err", err)
				break
			}
			state = cinderline.SkillLoaded
			loaded = append(loaded, client.SkillMount{Name: s.Name, Volume: vol, Path: s.Path, SkillContents: contents})
			log.Info("skill package loaded", "skill", s.Name, "commit", s.Commit, "volume", vol)
			if contents.Extension {
				break // an extension's skills are Gemini CLI's own to name
			}
			for _, k := range contents.Skills {
				if first, ok := geminiSkills[k]; ok {
					log.Warn("skill not loaded in Gemini CLI turns: an earlier package has a skill folder of its name", "skill", s.Name, "skill_folder", k, "earlier", first)
				} else {
					geminiSkills[k] = s.Name
				}
			}
		}
		if i < maxSkills && err == nil {
			names[s.Name] = true
		}
		states = append(states, cinderline.SkillState{Name: s.Name, State: state})
	}
	return loaded, states
}
