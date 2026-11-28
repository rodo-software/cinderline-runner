package manage

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// The functions here manage allow_skills, of the project with ID project
// or, for an empty one, of all current and future projects of this runner.
// With strict host blocking on, a run loads only the skill packages an
// entry allows, by name and URL (CLR-257). A name has one URL: adding a
// new one replaces the old.

// SkillAllowed is AddSkill's refusal of an entry that is there already, in
// the project with key Project, or runner-wide for "".
type SkillAllowed struct {
	Project string
	Skill   config.AllowedSkill
}

func (a SkillAllowed) Error() string {
	return skillScope(a.Project) + " already allows " + a.Skill.Name + " from " + a.Skill.URL
}

// skillScope names the allow_skills of the project with key, or the runner's for "".
func skillScope(key string) string {
	if key == "" {
		return "runner-wide allow_skills"
	}
	return "the allow_skills of " + key
}

// CheckAddSkill refuses what AddSkill would refuse, and writes nothing:
// the CLI asks its confirmation only for an add that would go through.
func CheckAddSkill(paths config.Paths, project, name, url string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	_, _, err = checkAddSkill(cfg, project, name, url)
	return err
}

// AddSkill allows skill package name from url in the project, or runner-wide.
func AddSkill(paths config.Paths, project, name, url string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	skills, j, err := checkAddSkill(cfg, project, name, url)
	if err != nil {
		return err
	}
	*skills = slices.Clone(*skills)
	if j >= 0 {
		(*skills)[j].URL = url
	} else {
		*skills = append(*skills, config.AllowedSkill{Name: name, URL: url})
	}
	return save(paths, cfg)
}

// RemoveSkill removes the entry of skill package name from the project, or
// runner-wide.
func RemoveSkill(paths config.Paths, project, name string) error {
	cfg, err := Load(paths)
	if err != nil {
		return err
	}
	skills, key, err := skillList(cfg, project)
	if err != nil {
		return err
	}
	j := slices.IndexFunc(*skills, func(e config.AllowedSkill) bool { return e.Name == name })
	if j < 0 {
		now := "none"
		if len(*skills) > 0 {
			var names []string
			for _, s := range *skills {
				names = append(names, s.Name)
			}
			now = strings.Join(names, ", ")
		}
		return Refused{fmt.Errorf("%s has no entry %s; its entries: %s", skillScope(key), name, now)}
	}
	*skills = slices.Delete(slices.Clone(*skills), j, j+1)
	return save(paths, cfg)
}

// checkAddSkill returns the allow_skills the entry would join and the index
// of its name there (-1: none), or why it may not.
func checkAddSkill(cfg *config.Config, project, name, url string) (*[]config.AllowedSkill, int, error) {
	skills, key, err := skillList(cfg, project)
	if err != nil {
		return nil, 0, err
	}
	if !config.ValidSkillName(name) {
		return nil, 0, Refused{fmt.Errorf("%q is not a skill package's name: lower-case letters and digits with single hyphens, at most 64 characters, as in Cinderline's catalog", name)}
	}
	if err := config.CheckSkillURL(url); err != nil {
		return nil, 0, Refused{fmt.Errorf("skill package %s: %w", name, err)}
	}
	j := slices.IndexFunc(*skills, func(e config.AllowedSkill) bool { return e.Name == name })
	if s := (config.AllowedSkill{Name: name, URL: url}); j >= 0 && (*skills)[j] == s {
		return nil, 0, Refused{SkillAllowed{key, s}}
	}
	return skills, j, nil
}

// skillList returns the allow_skills of the project in cfg and its key, or
// the runner-wide ones and "".
func skillList(cfg *config.Config, project string) (skills *[]config.AllowedSkill, key string, err error) {
	if project == "" {
		return &cfg.AllowSkills, "", nil
	}
	p, i, err := findProject(cfg, project)
	if err != nil {
		return nil, "", err
	}
	return &cfg.Projects[i].AllowSkills, p.Key, nil
}
