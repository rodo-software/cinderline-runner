package cli

import (
	"fmt"
	"strconv"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// web turns the daemon's local web page on or off or sets its port, for the
// whole runner, or says whether it is on and where (decision
// 2026-11-26-local-web-page).
func (c *CLI) web(args []string) int {
	f := c.flags("web", "[on|off|port N]")
	if err := f.Parse(args); err != nil {
		return 2
	}
	verb := f.Arg(0)
	if f.NArg() > 1 && verb != "port" || verb == "port" && f.NArg() != 2 || f.NArg() == 1 && verb != "on" && verb != "off" {
		f.Usage()
		return 2
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	if f.NArg() == 0 {
		state := "off"
		if cfg.WebOn() {
			state = "on"
		}
		fmt.Fprintf(c.Stdout, "The web page is %s, at http://%s\n", state, cfg.WebAddr())
		return 0
	}
	if verb == "port" {
		n, err := strconv.Atoi(f.Arg(1))
		if err != nil {
			return c.fail(fmt.Errorf("web_port %q must be 1024 to 65535", f.Arg(1)))
		}
		cfg.WebPort = n // Save refuses a port out of range
	} else {
		cfg.Web = verb
	}
	if err := cfg.Save(c.Paths.File); err != nil {
		return c.fail(err)
	}
	if verb == "port" {
		fmt.Fprintf(c.Stdout, "The web page of this %s is at http://%s\n", name.Name, cfg.WebAddr())
	} else {
		fmt.Fprintf(c.Stdout, "The web page of this %s is %s\n", name.Name, verb)
	}
	c.reload()
	return 0
}
