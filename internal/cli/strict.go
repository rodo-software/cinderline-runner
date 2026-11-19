package cli

import (
	"fmt"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// strictHosts turns strict host blocking on or off for the whole runner,
// or says which it is: when on, Cinderline cannot open a host for an
// issue's runs (decision 2026-11-17-hosts-from-cinderline).
func (c *CLI) strictHosts(args []string) int {
	f := c.flags("strict-hosts", "[on|off]")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() > 1 || f.NArg() == 1 && f.Arg(0) != "on" && f.Arg(0) != "off" {
		f.Usage()
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	if f.NArg() == 0 {
		state := "off: a person can open a host for one issue's runs in Cinderline"
		if cfg.StrictHosts {
			state = "on: only allow_hosts in config.json opens a host, which `" + name.Name + " allow-host` sets"
		}
		fmt.Fprintln(c.Stdout, "Strict host blocking is "+state)
		return 0
	}
	cfg.StrictHosts = f.Arg(0) == "on"
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "Strict host blocking is %s for every project of this %s\n", f.Arg(0), name.Name)
	c.reload()
	return 0
}
