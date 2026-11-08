package cli

import (
	"encoding/xml"
	"reflect"
	"strings"
	"testing"

	"github.com/rodo-software/cinderline-runner/internal/name"
)

// TestServiceUsage: service without install or uninstall, or with extra arguments, is a usage error.
func TestServiceUsage(t *testing.T) {
	for _, args := range [][]string{{"service"}, {"service", "start"}, {"service", "install", "extra"}} {
		c, _, errb := newCLI(t, "")
		if code := c.Run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "usage") {
			t.Errorf("%v: stderr %q, want usage", args, errb.String())
		}
	}
	c, _, errb := newCLI(t, "")
	c.Run(nil)
	if !strings.Contains(errb.String(), "service install|uninstall") {
		t.Errorf("usage %q does not list the service command", errb.String())
	}
}

// TestServiceConfig: the kardianos/service config is a user-level unit that runs
// "<binary> run" at login, restarts it only on failure and carries the installer's PATH.
func TestServiceConfig(t *testing.T) {
	cfg := serviceConfig("/home/ann/.local/bin/cinderline", "/opt/homebrew/bin:/usr/bin")
	if cfg.Name != name.Name {
		t.Errorf("Name %q, want %q", cfg.Name, name.Name)
	}
	if cfg.Executable != "/home/ann/.local/bin/cinderline" {
		t.Errorf("Executable %q", cfg.Executable)
	}
	if !reflect.DeepEqual(cfg.Arguments, []string{"run"}) {
		t.Errorf("Arguments %v, want [run]", cfg.Arguments)
	}
	for _, opt := range []string{"UserService", "RunAtLoad"} {
		if v, ok := cfg.Option[opt].(bool); !ok || !v {
			t.Errorf("Option %s = %v, want true", opt, cfg.Option[opt])
		}
	}
	// Restart on failure only, so "cinderline stop" really stops (decision 2026-11-07-service-restart).
	if v, ok := cfg.Option["KeepAlive"].(bool); !ok || v {
		t.Errorf("Option KeepAlive = %v, want false", cfg.Option["KeepAlive"])
	}
	if cfg.Option["Restart"] != "on-failure" {
		t.Errorf("Option Restart = %v, want on-failure", cfg.Option["Restart"])
	}
	if cfg.EnvVars["PATH"] != "/opt/homebrew/bin:/usr/bin" {
		t.Errorf("EnvVars PATH %q", cfg.EnvVars["PATH"])
	}
}

// task is the part of the Task Scheduler XML the runner depends on.
type task struct {
	Triggers struct {
		Logon struct {
			UserID string `xml:"UserId"`
		} `xml:"LogonTrigger"`
	}
	Principals struct {
		Principal struct {
			UserID    string `xml:"UserId"`
			LogonType string
			RunLevel  string
		}
	}
	Settings struct {
		MultipleInstancesPolicy    string
		DisallowStartIfOnBatteries string
		StopIfGoingOnBatteries     string
		ExecutionTimeLimit         string
		RestartOnFailure           struct {
			Interval string
			Count    int
		}
	}
	Actions struct {
		Exec struct {
			Command   string
			Arguments string
		}
	}
}

// TestTaskXML: the Windows task runs "<binary> run" as the current user at logon,
// restarts on failure, has no time limit and ignores a second start.
func TestTaskXML(t *testing.T) {
	exe := `C:\Users\Ann Lee\AppData\Local\Programs\cinderline\cinderline.exe`
	b := taskXML(exe, `PC\Ann Lee`)
	if !strings.HasPrefix(string(b), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Errorf("XML declaration must say UTF-8, the encoding it is written in: %.60q", b)
	}
	var tk task
	if err := xml.Unmarshal(b, &tk); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	want := map[string][2]string{
		"Exec.Command":               {tk.Actions.Exec.Command, exe},
		"Exec.Arguments":             {tk.Actions.Exec.Arguments, "run"},
		"LogonTrigger.UserId":        {tk.Triggers.Logon.UserID, `PC\Ann Lee`},
		"Principal.UserId":           {tk.Principals.Principal.UserID, `PC\Ann Lee`},
		"Principal.LogonType":        {tk.Principals.Principal.LogonType, "InteractiveToken"},
		"Principal.RunLevel":         {tk.Principals.Principal.RunLevel, "LeastPrivilege"},
		"MultipleInstancesPolicy":    {tk.Settings.MultipleInstancesPolicy, "IgnoreNew"},
		"DisallowStartIfOnBatteries": {tk.Settings.DisallowStartIfOnBatteries, "false"},
		"StopIfGoingOnBatteries":     {tk.Settings.StopIfGoingOnBatteries, "false"},
		"ExecutionTimeLimit":         {tk.Settings.ExecutionTimeLimit, "PT0S"},
		"RestartOnFailure.Interval":  {tk.Settings.RestartOnFailure.Interval, "PT1M"},
	}
	for k, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", k, v[0], v[1])
		}
	}
	if tk.Settings.RestartOnFailure.Count != 3 {
		t.Errorf("RestartOnFailure.Count = %d, want 3", tk.Settings.RestartOnFailure.Count)
	}
}

// TestSchtasksArgs: the schtasks argument lists name the task after the product and
// register it from the XML file, overwriting an earlier one.
func TestSchtasksArgs(t *testing.T) {
	cases := map[string][]string{
		"create": {"/create", "/tn", name.Name, "/xml", `C:\tmp\task.xml`, "/f"},
		"run":    {"/run", "/tn", name.Name},
		"end":    {"/end", "/tn", name.Name},
		"delete": {"/delete", "/tn", name.Name, "/f"},
	}
	for action, want := range cases {
		if got := schtasksArgs(action, `C:\tmp\task.xml`); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %q, want %q", action, got, want)
		}
	}
}
