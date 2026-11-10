// Package cli is the command line: the dispatcher and one function per command.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/forge"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// CLI is what a command runs with. Tests set Paths and Secrets to temp ones.
type CLI struct {
	Version string
	Paths   config.Paths
	Secrets *secrets.Secrets
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer

	exec          workspace.Exec                                            // nil means workspace.RunCommand
	http          *http.Client                                              // the tools downloads; nil means http.DefaultClient
	removeService func(exe string) error                                    // nil means uninstallService
	startService  func(exe string) (bool, error)                            // nil means startService; update restarts the service with it
	processGone   func(pid int) bool                                        // nil means processGone; drainAndWait waits with it until the daemon's process has exited
	Forge         func(kind, repository, token string) (forge.Forge, error) // nil: forge.New
	pathsErr      error                                                     // from config.DefaultPaths; only commands that need the paths fail on it
	WindowsBuild  uint32                                                    // OS build on Windows, 0 elsewhere; Main sets it
	exe           string                                                    // the binary update replaces; "" means os.Executable()
}

// command is one subcommand. A new command is one function and one entry in commands.
type command struct {
	name, args, help string
	run              func(c *CLI, args []string) int
}

// commands is a function, not a var: version prints the usage, which lists commands.
func commands() []command {
	return []command{
		{"add", "", "add a project, asking for its details", (*CLI).add},
		{"remove", "[-url URL] KEY", "remove a project and its secrets", (*CLI).remove},
		{"run", "[-v]", "run in the foreground until Ctrl-C; -v also logs to stderr, with Debug lines", (*CLI).run},
		{"status", "", "show the running " + name.Name + " and its projects", (*CLI).status},
		{"logs", "[-n LINES]", "print the log's last lines, then follow it", (*CLI).logs},
		{"stop", "[--drain]", "stop the running " + name.Name + " at once, or with --drain once its runs have finished", (*CLI).stopDaemon},
		{"update", "[--refresh]", "update to the latest release, then stop the running " + name.Name + " once its runs have finished", (*CLI).update},
		{"service", "install|uninstall", "install or remove the user-level service that runs " + name.Name + " at login", (*CLI).service},
		{"uninstall", "[-yes]", "remove the service, containers, volumes, images, config and secrets of " + name.Name, (*CLI).uninstall},
		{"login", "CLIENT --project KEY", "sign a client CLI in with its own login, in a container; the credentials stay on the project's state volume", (*CLI).login},
		{"version", "", "print the version", (*CLI).version},
	}
}

// Main runs the command in args with this machine's paths and secrets and returns the exit code.
func Main(version string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c := &CLI{Version: version, Stdin: stdin, Stdout: stdout, Stderr: stderr}
	c.WindowsBuild = windowsBuild()
	c.Paths, c.pathsErr = config.DefaultPaths()
	if c.pathsErr == nil {
		c.Secrets = secrets.New(c.Paths.State)
	}
	return c.Run(args)
}

// Run dispatches args to a command: exit 0 ok, 1 failure, 2 usage error.
func (c *CLI) Run(args []string) int {
	if len(args) > 0 {
		for _, cmd := range commands() {
			if cmd.name == args[0] {
				if cmd.name != "version" {
					if err := checkWindowsBuild(c.WindowsBuild); err != nil {
						return c.fail(err)
					}
				}
				return cmd.run(c, args[1:])
			}
		}
	}
	c.usage()
	return 2
}

func (c *CLI) usage() {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: %s <command>\n\ncommands:\n", name.Name)
	for _, cmd := range commands() {
		fmt.Fprintf(&b, "  %-24s %s\n", strings.TrimSpace(cmd.name+" "+cmd.args), cmd.help)
	}
	io.WriteString(c.Stderr, b.String())
}

// flags returns the flag set of a command; its parse errors and usage go to stderr.
func (c *CLI) flags(cmd, args string) *flag.FlagSet {
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	f.SetOutput(c.Stderr)
	f.Usage = func() {
		fmt.Fprintf(c.Stderr, "usage: %s\n", strings.TrimSpace(name.Name+" "+cmd+" "+args))
		f.PrintDefaults()
	}
	return f
}

// fail prints err on stderr and returns exit code 1.
func (c *CLI) fail(err error) int {
	fmt.Fprintln(c.Stderr, "error:", err)
	return 1
}

// loadConfig returns the config, or an empty one when the file does not exist yet.
func (c *CLI) loadConfig() (*config.Config, error) {
	if c.pathsErr != nil {
		return nil, c.pathsErr
	}
	cfg, err := config.Load(c.Paths.File)
	if errors.Is(err, fs.ErrNotExist) {
		return &config.Config{}, nil
	}
	return cfg, err
}

func (c *CLI) version(args []string) int {
	if len(args) > 0 {
		c.usage()
		return 2
	}
	fmt.Fprintln(c.Stdout, name.Name+" "+c.Version)
	return 0
}

// storeName says where a secret went, for the person reading the output.
func (c *CLI) storeName(s secrets.Store) string {
	if s == secrets.Keychain {
		return "the keychain"
	}
	return "the fallback file in " + c.Paths.State
}

// without returns ps without the project with p's URL and key.
func without(ps []config.Project, p config.Project) []config.Project {
	var out []config.Project
	for _, q := range ps {
		if q.URL != p.URL || q.Key != p.Key {
			out = append(out, q)
		}
	}
	return out
}
