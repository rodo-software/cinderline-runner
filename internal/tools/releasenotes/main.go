// Command releasenotes prints the release notes header that names the
// pinned agent CLIs; the release workflow feeds it to GoReleaser.
package main

import (
	"fmt"

	"github.com/rodo-software/cinderline-runner/internal/tools"
)

func main() {
	fmt.Print(tools.ReleaseNotes())
}
