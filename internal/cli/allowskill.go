package cli

import (
	"bufio"
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

const allowSkillUsage = "add NAME URL|list|remove NAME [" + projectUsage + "]"

// allowSkill adds, lists or removes allow_skills entries, either for one
// project (--project) or for all current and future projects of this
// runner, and reloads a running runner. With strict host blocking on, a run
// loads only the skill packages an entry allows, by name and URL, so a
// catalog entry that later points elsewhere is refused again (CLR-257).
func (c *CLI) allowSkill(args []string) int {
	// NAME and URL come before the flags, as allow-host's HOST:PORT does: flag parsing stops at them.
	nargs := map[string]int{"add": 2, "remove": 1, "list": 0}
	n, known := 0, false
	if len(args) > 0 {
		n, known = nargs[args[0]]
	}
	if !known || len(args) < 1+n || slices.ContainsFunc(args[1:1+n], func(a string) bool { return strings.HasPrefix(a, "-") }) {
		c.flags("allow-skill", allowSkillUsage).Usage()
		return 2
	}
	verb := args[0]
	f := c.flags("allow-skill", allowSkillUsage)
	project := f.String("project", "", "project key (omit for all current and future projects)")
	instance, ws := projectFlags(f)
	if !c.parsed(f, args[1+n:], 0) {
		return 2
	}
	if *project == "" && (*instance != "" || *ws != "") {
		f.Usage()
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	skills := &cfg.AllowSkills
	var projKey string
	if *project != "" {
		p, err := findProject(cfg, *project, *instance, *ws)
		if err != nil {
			return c.fail(err)
		}
		i := slices.IndexFunc(cfg.Projects, func(q config.Project) bool { return q.URL == p.URL && q.Workspace == p.Workspace && q.Key == p.Key })
		skills = &cfg.Projects[i].AllowSkills
		projKey = p.Key
	}
	scope := "runner-wide allow_skills"
	if projKey != "" {
		scope = "the allow_skills of " + projKey
	}
	if verb == "list" {
		if len(*skills) == 0 {
			fmt.Fprintf(c.Stdout, "No entries in %s\n", scope)
		}
		for _, s := range *skills {
			fmt.Fprintln(c.Stdout, s.Name+" "+s.URL)
		}
		return 0
	}
	skill, was := config.AllowedSkill{Name: args[1]}, ""
	j := slices.IndexFunc(*skills, func(e config.AllowedSkill) bool { return e.Name == skill.Name })
	if verb == "add" {
		skill.URL = args[2]
		switch {
		case !config.ValidSkillName(skill.Name):
			return c.fail(fmt.Errorf("%q is not a skill package's name: lower-case letters and digits with single hyphens, at most 64 characters, as in Cinderline's catalog", skill.Name))
		case config.CheckSkillURL(skill.URL) != nil:
			return c.fail(fmt.Errorf("skill package %s: %w", skill.Name, config.CheckSkillURL(skill.URL)))
		case j >= 0 && (*skills)[j] == skill:
			fmt.Fprintf(c.Stdout, "%s already allows %s from %s; nothing changed\n", scope, skill.Name, skill.URL)
			return 0
		}
		if projKey == "" {
			fmt.Fprintf(c.Stdout, "This allows skill package %s from %s for all current and future projects of this %s. Type yes to continue: ", skill.Name, skill.URL, name.Name)
			var ans string
			if c.Stdin != nil {
				sc := bufio.NewScanner(c.Stdin)
				sc.Scan()
				ans = strings.ToLower(strings.TrimSpace(sc.Text()))
			}
			if ans != "yes" && ans != "y" {
				fmt.Fprintln(c.Stderr, "Cancelled")
				return 1
			}
		}
		*skills = slices.Clone(*skills)
		if j >= 0 {
			was = (*skills)[j].URL
			(*skills)[j] = skill // one URL per name
		} else {
			*skills = append(*skills, skill)
		}
	} else {
		if j < 0 {
			now := "none"
			if len(*skills) > 0 {
				var names []string
				for _, s := range *skills {
					names = append(names, s.Name)
				}
				now = strings.Join(names, ", ")
			}
			return c.fail(fmt.Errorf("%s has no entry %s; its entries: %s", scope, skill.Name, now))
		}
		skill = (*skills)[j]
		*skills = slices.Delete(slices.Clone(*skills), j, j+1)
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	switch {
	case was != "":
		fmt.Fprintf(c.Stdout, "%s now allows %s from %s only, no longer from %s; runs started from now on load it from there\n", scope, skill.Name, skill.URL, was)
	case verb == "add":
		fmt.Fprintf(c.Stdout, "Added %s from %s to %s; runs started from now on load it, even with strict host blocking on\n", skill.Name, skill.URL, scope)
	default:
		fmt.Fprintf(c.Stdout, "Removed %s from %s\n", skill.Name, scope)
	}
	c.reload()
	return 0
}
