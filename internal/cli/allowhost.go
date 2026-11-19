package cli

import (
	"bufio"
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

const allowHostUsage = "add HOST:PORT|list|remove HOST:PORT [" + projectUsage + "]"

// allowHost adds, lists or removes allow_hosts entries, either for one
// project (--project) or for all current and future projects of this runner,
// and reloads a running runner. The egress proxy matches hosts in lower case,
// so entries are stored and compared in lower case.
func (c *CLI) allowHost(args []string) int {
	// The host comes before the flags, as key's NAME does: flag parsing stops at it.
	nargs := 0
	if len(args) > 0 && (args[0] == "add" || args[0] == "remove") {
		nargs = 1
	}
	if len(args) < 1+nargs || nargs == 0 && args[0] != "list" || nargs == 1 && strings.HasPrefix(args[1], "-") {
		c.flags("allow-host", allowHostUsage).Usage()
		return 2
	}
	verb := args[0]
	f := c.flags("allow-host", allowHostUsage)
	project := f.String("project", "", "project key (omit for all current and future projects)")
	instance, ws := projectFlags(f)
	if !c.parsed(f, args[1+nargs:], 0) {
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
	hosts := &cfg.AllowHosts
	var projKey string
	if *project != "" {
		p, err := findProject(cfg, *project, *instance, *ws)
		if err != nil {
			return c.fail(err)
		}
		i := slices.IndexFunc(cfg.Projects, func(q config.Project) bool { return q.URL == p.URL && q.Workspace == p.Workspace && q.Key == p.Key })
		hosts = &cfg.Projects[i].AllowHosts
		projKey = p.Key
	}
	if verb == "list" {
		if len(*hosts) == 0 {
			if projKey == "" {
				fmt.Fprintln(c.Stdout, "No runner-wide allow_hosts entries")
			} else {
				fmt.Fprintf(c.Stdout, "Project %s has no allow_hosts entries\n", projKey)
			}
		}
		for _, hp := range *hosts {
			fmt.Fprintln(c.Stdout, hp)
		}
		return 0
	}
	hp := strings.ToLower(args[1])
	j := slices.IndexFunc(*hosts, func(e string) bool { return strings.EqualFold(e, hp) })
	if verb == "add" {
		switch {
		case !config.ValidHostPort(hp):
			return c.fail(fmt.Errorf("%q must be host:port, without wildcards", args[1]))
		case !config.AllowedHost(hp):
			return c.fail(fmt.Errorf("%q is the runner's own machine, which allow_hosts never opens; add a model provider instead with `%s add-provider NAME URL`", args[1], name.Name))
		case j >= 0:
			if projKey == "" {
				fmt.Fprintf(c.Stdout, "All projects already allow %s; nothing changed\n", (*hosts)[j])
			} else {
				fmt.Fprintf(c.Stdout, "Project %s already allows %s; nothing changed\n", projKey, (*hosts)[j])
			}
			return 0
		}
		if projKey == "" {
			fmt.Fprintf(c.Stdout, "This allows %s for all current and future projects of this %s. Type yes to continue: ", hp, name.Name)
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
		*hosts = append(slices.Clone(*hosts), hp)
	} else {
		if j < 0 {
			now := "none"
			if len(*hosts) > 0 {
				now = strings.Join(*hosts, ", ")
			}
			if projKey == "" {
				return c.fail(fmt.Errorf("runner-wide allow_hosts has no entry %s; its entries: %s", args[1], now))
			}
			return c.fail(fmt.Errorf("project %s has no allow_hosts entry %s; its entries: %s", projKey, args[1], now))
		}
		hp = (*hosts)[j]
		*hosts = slices.Delete(slices.Clone(*hosts), j, j+1)
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	switch {
	case verb == "add" && projKey == "":
		fmt.Fprintf(c.Stdout, "Added %s to runner-wide allow_hosts; runs of all current and future projects started from now on may reach it\n", hp)
	case verb == "add":
		fmt.Fprintf(c.Stdout, "Added %s to the allow_hosts of %s; runs started from now on may reach it\n", hp, projKey)
	case projKey == "":
		fmt.Fprintf(c.Stdout, "Removed %s from runner-wide allow_hosts\n", hp)
	default:
		fmt.Fprintf(c.Stdout, "Removed %s from the allow_hosts of %s\n", hp, projKey)
	}
	c.reload()
	return 0
}
