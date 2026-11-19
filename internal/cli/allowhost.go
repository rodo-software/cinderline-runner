package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

const allowHostUsage = "add HOST:PORT|list|remove HOST:PORT " + projectUsage

// allowHost adds, lists or removes a project's allow_hosts entries, the
// hosts every run of the project may reach, and reloads a running runner.
// The egress proxy matches hosts in lower case, so entries are stored and
// compared in lower case.
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
	cfg, i, _, code := c.repoFlags("allow-host", allowHostUsage, args[1+nargs:], 0)
	if code != 0 {
		return code
	}
	p := cfg.Projects[i]
	if verb == "list" {
		if len(p.AllowHosts) == 0 {
			fmt.Fprintf(c.Stdout, "Project %s has no allow_hosts entries\n", p.Key)
		}
		for _, hp := range p.AllowHosts {
			fmt.Fprintln(c.Stdout, hp)
		}
		return 0
	}
	hp := strings.ToLower(args[1])
	j := slices.IndexFunc(p.AllowHosts, func(e string) bool { return strings.EqualFold(e, hp) })
	if verb == "add" {
		switch {
		case !config.ValidHostPort(hp):
			return c.fail(fmt.Errorf("%q must be host:port, without wildcards", args[1]))
		case !config.AllowedHost(hp):
			return c.fail(fmt.Errorf("%q is the runner's own machine, which allow_hosts never opens; add a model provider instead with `%s add-provider NAME URL`", args[1], name.Name))
		case j >= 0:
			fmt.Fprintf(c.Stdout, "Project %s already allows %s; nothing changed\n", p.Key, p.AllowHosts[j])
			return 0
		}
		cfg.Projects[i].AllowHosts = append(slices.Clone(p.AllowHosts), hp)
	} else {
		if j < 0 {
			now := "none"
			if len(p.AllowHosts) > 0 {
				now = strings.Join(p.AllowHosts, ", ")
			}
			return c.fail(fmt.Errorf("project %s has no allow_hosts entry %s; its entries: %s", p.Key, args[1], now))
		}
		hp = p.AllowHosts[j]
		cfg.Projects[i].AllowHosts = slices.Delete(slices.Clone(p.AllowHosts), j, j+1)
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	if verb == "add" {
		fmt.Fprintf(c.Stdout, "Added %s to the allow_hosts of %s; runs started from now on may reach it\n", hp, p.Key)
	} else {
		fmt.Fprintf(c.Stdout, "Removed %s from the allow_hosts of %s\n", hp, p.Key)
	}
	c.reload()
	return 0
}
