package cli

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"

	"github.com/kardianos/service"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// service installs or removes the user-level service that runs the daemon at login.
func (c *CLI) service(args []string) int {
	if len(args) != 1 || (args[0] != "install" && args[0] != "uninstall") {
		c.usage()
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		return c.fail(err)
	}
	switch args[0] {
	case "install":
		if err := c.checkDocker(context.Background(), false); err != nil {
			return c.fail(err)
		}
		if err := installService(exe); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Installed %s as a user service: it runs %q run at login and is running now\n", name.Name, name.Name)
	case "uninstall":
		if err := uninstallService(exe); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.Stdout, "Removed the %s service\n", name.Name)
	}
	return 0
}

// serviceConfig builds the kardianos/service config for the user-level runner.
// It carries the installing shell's PATH because launchd and systemd user units
// inherit a minimal PATH that lacks /usr/local/bin and /opt/homebrew/bin.
func serviceConfig(exe, path string) *service.Config {
	return &service.Config{
		Name:        name.Name,
		DisplayName: "Cinderline runner",
		Description: "The Cinderline runner, a native daemon for issue-driven development.",
		Executable:  exe,
		Arguments:   []string{"run"},
		// Restart on failure only, so "stop" really stops (decision 2026-11-07-service-restart).
		// launchd's KeepAlive is a plain boolean here, so macOS does not restart a crash.
		Option: service.KeyValue{
			"UserService": true,
			"RunAtLoad":   true,
			"KeepAlive":   false,
			"Restart":     "on-failure",
		},
		EnvVars: map[string]string{"PATH": path},
	}
}

// taskXML returns a Windows Task Scheduler XML document that runs exe as user at logon.
func taskXML(exe, user string) []byte {
	type logonTrigger struct {
		XMLName xml.Name `xml:"LogonTrigger"`
		Enabled bool     `xml:"Enabled"`
		UserID  string   `xml:"UserId"`
	}
	type triggers struct {
		Logon logonTrigger `xml:"LogonTrigger"`
	}
	type principal struct {
		ID        string `xml:"id,attr"`
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	}
	type principals struct {
		Principal principal `xml:"Principal"`
	}
	type restartOnFailure struct {
		Interval string `xml:"Interval"`
		Count    int    `xml:"Count"`
	}
	type settings struct {
		MultipleInstancesPolicy    string           `xml:"MultipleInstancesPolicy"`
		DisallowStartIfOnBatteries string           `xml:"DisallowStartIfOnBatteries"`
		StopIfGoingOnBatteries     string           `xml:"StopIfGoingOnBatteries"`
		StartWhenAvailable         bool             `xml:"StartWhenAvailable"`
		ExecutionTimeLimit         string           `xml:"ExecutionTimeLimit"`
		RestartOnFailure           restartOnFailure `xml:"RestartOnFailure"`
	}
	type exec struct {
		Command   string `xml:"Command"`
		Arguments string `xml:"Arguments"`
	}
	type actions struct {
		Context string `xml:"Context,attr"`
		Exec    exec   `xml:"Exec"`
	}
	type task struct {
		XMLName    xml.Name   `xml:"Task"`
		Version    string     `xml:"version,attr"`
		Namespace  string     `xml:"xmlns,attr"`
		Triggers   triggers   `xml:"Triggers"`
		Principals principals `xml:"Principals"`
		Settings   settings   `xml:"Settings"`
		Actions    actions    `xml:"Actions"`
	}

	t := task{
		Version:   "1.2",
		Namespace: "http://schemas.microsoft.com/windows/2004/02/mit/task",
		Triggers: triggers{
			Logon: logonTrigger{
				Enabled: true,
				UserID:  user,
			},
		},
		Principals: principals{
			Principal: principal{
				ID:        "Author",
				UserID:    user,
				LogonType: "InteractiveToken",
				RunLevel:  "LeastPrivilege",
			},
		},
		Settings: settings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: "false",
			StopIfGoingOnBatteries:     "false",
			StartWhenAvailable:         true,
			ExecutionTimeLimit:         "PT0S",
			RestartOnFailure: restartOnFailure{
				Interval: "PT1M",
				Count:    3,
			},
		},
		Actions: actions{
			Context: "Author",
			Exec: exec{
				Command:   exe,
				Arguments: "run",
			},
		},
	}

	b, _ := xml.MarshalIndent(t, "", "  ")
	return append([]byte(`<?xml version="1.0" encoding="UTF-8"?>`+"\n"), b...)
}

// schtasksArgs builds the schtasks arguments for the given action.
func schtasksArgs(action, xmlPath string) []string {
	switch action {
	case "create":
		return []string{"/create", "/tn", name.Name, "/xml", xmlPath, "/f"}
	case "delete":
		return []string{"/delete", "/tn", name.Name, "/f"}
	default:
		return []string{"/" + action, "/tn", name.Name}
	}
}
