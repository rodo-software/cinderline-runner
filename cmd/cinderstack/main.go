package main

import (
	"fmt"
	"io"
	"os"

	"github.com/rodo-software/cinderstack-runner/internal/name"
)

// Release builds set version with -ldflags "-X main.version=<v>".
var version = "dev"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, name.Name+" "+version)
		return 0
	}
	fmt.Fprintln(stderr, "usage: "+name.Name+" version")
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
