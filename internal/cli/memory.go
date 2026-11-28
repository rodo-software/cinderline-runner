package cli

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/manage"
)

const memoryUsage = "show|set SIZE|clear " + projectUsage

// memory shows, sets or clears the memory limit of one project's agent
// containers, its memory in config.json, and reloads a running runner after
// a change (decision 2026-11-24-agent-memory-limit).
func (c *CLI) memory(args []string) int {
	// The size comes before the flags, as allow-host's host does: flag parsing stops at it.
	nargs := 0
	if len(args) > 0 && args[0] == "set" {
		nargs = 1
	}
	if len(args) < 1+nargs || nargs == 0 && args[0] != "show" && args[0] != "clear" || nargs == 1 && strings.HasPrefix(args[1], "-") {
		c.flags("memory", memoryUsage).Usage()
		return 2
	}
	verb := args[0]
	cfg, i, _, code := c.repoFlags("memory", memoryUsage, args[1+nargs:], 0)
	if code != 0 {
		return code
	}
	p := &cfg.Projects[i]
	const wins = "a mem_limit on the agent service of its compose file wins"
	switch verb {
	case "show":
		if p.Memory == "" {
			fmt.Fprintf(c.Stdout, "Agent containers of %s get %s of memory, the default; %s\n", p.Key, config.DefaultMemory, wins)
		} else {
			fmt.Fprintf(c.Stdout, "Agent containers of %s get %s of memory; %s\n", p.Key, p.Memory, wins)
		}
		return 0
	case "set":
		p.Memory = strings.ToLower(args[1])
	default:
		if p.Memory == "" {
			fmt.Fprintf(c.Stdout, "Project %s has no memory setting; nothing changed\n", p.Key)
			return 0
		}
		p.Memory = ""
	}
	if err := manage.SetMemory(c.Paths, manage.Project{Key: p.Key, URL: p.URL, Workspace: p.Workspace}, p.Memory); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Agent containers of %s get %s of memory from their next run; %s\n", p.Key, cmp.Or(p.Memory, config.DefaultMemory), wins)
	c.reload()
	return 0
}
