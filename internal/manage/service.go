package manage

import "github.com/rodo-software/cinderline-runner/internal/name"

// ServiceStart is the command, to type in a terminal, that starts the
// installed user service on goos (decision 2026-11-28-stopped-page-service):
// the runner has none of its own. On macOS the LaunchAgent stays loaded
// after the runner exits, so start, not load.
func ServiceStart(goos string) string {
	switch goos {
	case "windows":
		return "schtasks /run /tn " + name.Name
	case "darwin":
		return "launchctl start " + name.Name
	default:
		return "systemctl --user start " + name.Name
	}
}
