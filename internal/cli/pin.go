package cli

import (
	"fmt"
	"slices"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/tools"
)

const pinUsage = "[CLIENT VERSION|CLIENT latest]"

// pin holds an agent CLI at a version for every project (pinned in
// config.json), for when its vendor ships a broken release, or lets it
// follow the vendor's latest again, and reloads a running runner; without
// arguments it shows each client's version and pin (CLR-317).
func (c *CLI) pin(args []string) int {
	f := c.flags("pin", pinUsage)
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() != 0 && f.NArg() != 2 || f.NArg() == 2 && !slices.Contains(tools.Clients, f.Arg(0)) {
		f.Usage()
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	if f.NArg() == 0 {
		live := tools.Current(c.Paths.State)
		for _, cl := range tools.Clients {
			how := "follows its vendor's latest release"
			if v, ok := cfg.Pinned[cl]; ok {
				how = "pinned at " + v
			}
			fmt.Fprintf(c.Stdout, "%s %s, %s\n", cl, live.Version(cl), how)
		}
		return 0
	}
	client, version := f.Arg(0), f.Arg(1)
	if version == "latest" {
		delete(cfg.Pinned, client)
		if len(cfg.Pinned) == 0 {
			cfg.Pinned = nil
		}
	} else {
		if !tools.VersionPat.MatchString(version) {
			return c.fail(fmt.Errorf("version %q is not like 2.1.316", version))
		}
		if cfg.Pinned == nil {
			cfg.Pinned = map[string]string{}
		}
		cfg.Pinned[client] = version
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	if version == "latest" {
		fmt.Fprintf(c.Stdout, "%s follows its vendor's latest release again\n", client)
	} else {
		fmt.Fprintf(c.Stdout, "%s is pinned at %s for every project of this %s\n", client, version, name.Name)
	}
	fmt.Fprintf(c.Stdout, "Runs that start once the version passed its check use it; `%s logs` shows the check\n", name.Name)
	c.reload()
	return 0
}
