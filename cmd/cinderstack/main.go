package main

import (
	"os"

	"github.com/rodo-software/cinderstack-runner/internal/cli"
)

// Release builds set version with -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	os.Exit(cli.Main(version, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
