package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/web"
)

const webUsage = "[--no-open | --new-token | on | off | port N]"

// web opens the daemon's local web page in the browser, signed in with its
// token, or replaces the token, or turns the page on or off or sets its port,
// for the whole runner (decision 2026-11-26-local-web-page).
func (c *CLI) web(args []string) int {
	f := c.flags("web", webUsage)
	noOpen := f.Bool("no-open", false, "only print the page's URL")
	newToken := f.Bool("new-token", false, "replace the page's token")
	if err := f.Parse(args); err != nil {
		return 2
	}
	verb := f.Arg(0)
	if (*noOpen || *newToken) && f.NArg() > 0 || *noOpen && *newToken ||
		f.NArg() > 1 && verb != "port" || verb == "port" && f.NArg() != 2 || f.NArg() == 1 && verb != "on" && verb != "off" {
		f.Usage()
		return 2
	}
	if *newToken {
		if c.pathsErr != nil {
			return c.fail(c.pathsErr)
		}
		if err := web.NewToken(c.Paths.State); err != nil {
			return c.fail(fmt.Errorf("replace the web token: %w", err))
		}
		fmt.Fprintf(c.Stdout, "The web page has a new token: open pages must be opened again, with `%s web`\n", name.Name)
		return 0
	}
	cfg, err := c.loadConfig()
	if err != nil {
		return c.fail(err)
	}
	if f.NArg() == 0 {
		return c.openWeb(cfg.WebOn(), cfg.WebAddr(), !*noOpen)
	}
	var on *bool
	var port *int
	if verb == "port" {
		n, err := strconv.Atoi(f.Arg(1))
		if err != nil {
			return c.fail(fmt.Errorf("web_port %q must be 1024 to 65535", f.Arg(1)))
		}
		port, cfg.WebPort = &n, n
	} else {
		on = new(verb == "on")
	}
	if err := manage.SetWeb(c.Paths, on, port); err != nil {
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

// openWeb prints the page's URL with its token, and opens it in the browser
// when open, if the page is on and the daemon that serves it runs.
func (c *CLI) openWeb(on bool, addr string, open bool) int {
	if !on {
		return c.fail(fmt.Errorf("the web page is off; turn it on with `%s web on`", name.Name))
	}
	if _, err := (localapi.Client{Socket: c.Paths.Socket}).Status(context.Background()); errors.Is(err, localapi.ErrNotRunning) {
		return c.fail(fmt.Errorf("%s is not running, so there is no web page; start it with `%s run` or `%s service install`", name.Name, name.Name, name.Name))
	} else if err != nil {
		return c.fail(err)
	}
	tok, err := web.Token(c.Paths.State)
	if err != nil {
		return c.fail(err)
	}
	url := "http://" + addr + "/?token=" + tok
	fmt.Fprintln(c.Stdout, url)
	if !open {
		return 0
	}
	opener := c.open
	if opener == nil {
		opener = openBrowser
	}
	if err := opener(url); err != nil {
		fmt.Fprintf(c.Stderr, "Could not open the browser (%v); open the URL above yourself\n", err)
	}
	return 0 // the printed URL is enough
}

// openBrowser starts the default browser on url and does not wait for it.
func openBrowser(url string) error {
	return browser(url).Start()
}
