package cli

import (
	"bufio"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

const allowSkillUsage = "add NAME URL|list|remove NAME [" + projectUsage + "]"

// allowSkill adds, lists or removes allow_skills entries, either for one
// project (--project) or for all current and future projects of this
// runner, through manage (CLR-296), and reloads a running runner. With
// strict host blocking on, a run loads only the skill packages an entry
// allows, by name and URL, so a catalog entry that later points elsewhere
// is refused again (CLR-257).
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
	var projKey, id string
	if *project != "" {
		p, err := manage.FindByKey(cfg, *project, *instance, *ws)
		if err != nil {
			return c.fail(err)
		}
		i := slices.IndexFunc(cfg.Projects, func(q config.Project) bool { return q.URL == p.URL && q.Workspace == p.Workspace && q.Key == p.Key })
		skills = &cfg.Projects[i].AllowSkills
		projKey, id = p.Key, p.ID()
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
	if j := slices.IndexFunc(*skills, func(e config.AllowedSkill) bool { return e.Name == skill.Name }); j >= 0 {
		was = (*skills)[j].URL
	}
	if verb == "add" {
		skill.URL = args[2]
	}
	switch {
	case verb == "remove":
		err = manage.RemoveSkill(c.Paths, id, skill.Name)
	case projKey == "":
		// Asks only for an add that would go through.
		if err = manage.CheckAddSkill(c.Paths, id, skill.Name, skill.URL); err != nil {
			break
		}
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
		err = manage.AddSkill(c.Paths, id, skill.Name, skill.URL)
	default:
		err = manage.AddSkill(c.Paths, id, skill.Name, skill.URL)
	}
	if a := (manage.SkillAllowed{}); errors.As(err, &a) {
		fmt.Fprintf(c.Stdout, "%s already allows %s from %s; nothing changed\n", scope, a.Skill.Name, a.Skill.URL)
		return 0
	}
	if err != nil {
		return c.fail(err)
	}
	switch {
	case verb == "remove":
		fmt.Fprintf(c.Stdout, "Removed %s from %s\n", skill.Name, scope)
	case was != "":
		fmt.Fprintf(c.Stdout, "%s now allows %s from %s only, no longer from %s; runs started from now on load it from there\n", scope, skill.Name, skill.URL, was)
	default:
		fmt.Fprintf(c.Stdout, "Added %s from %s to %s; runs started from now on load it, even with strict host blocking on\n", skill.Name, skill.URL, scope)
	}
	c.reload()
	return 0
}
