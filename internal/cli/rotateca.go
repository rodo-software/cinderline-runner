package cli

import (
	"fmt"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/manage"
)

// rotateCA replaces the runner's CA (manage.RotateCA).
func (c *CLI) rotateCA(args []string) int {
	if !c.parsed(c.flags("rotate-ca", ""), args, 0) {
		return 2
	}
	if c.pathsErr != nil {
		return c.fail(c.pathsErr)
	}
	authority, err := manage.RotateCA(c.Paths)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.Stdout, "New CA %s, valid until %s; agent containers started from now on trust it, those running keep the old one\n",
		authority.CertFile, authority.Cert.NotAfter.Format(time.DateOnly))
	return 0
}
