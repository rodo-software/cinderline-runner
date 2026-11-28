package daemon

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/web"
)

// CLR-303: an update from the page drains the daemon, and Run then returns
// ErrRestart, so `run` runs the new binary in its place (decision
// 2026-11-28-update-restart).
func TestUpdateFromThePageRestarts(t *testing.T) {
	keyring.MockInit()
	defer func(x func(context.Context, []string, ...string) (string, error)) { webExec = x }(webExec)
	webExec = func(_ context.Context, _ []string, argv ...string) (string, error) {
		if argv[len(argv)-1] == "version" {
			return name.Name + " 9.9.9\n", nil
		}
		return "", nil // the install script and update --refresh
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	p := paths(t)
	if err := (&config.Config{Projects: []config.Project{}, Web: "on", WebPort: port}).Save(p.File); err != nil {
		t.Fatal(err)
	}
	_, done := start(t, p)
	tok, err := web.Token(p.State)
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://127.0.0.1:" + strconv.Itoa(port)
	var resp *http.Response
	waitFor(t, "the page", func() bool {
		req, _ := http.NewRequest("POST", origin+"/api/daemon/update", strings.NewReader(""))
		req.Header.Set("Origin", origin)
		req.AddCookie(&http.Cookie{Name: name.Name, Value: tok})
		resp, err = http.DefaultClient.Do(req)
		return err == nil
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/daemon/update = %d, want 202", resp.StatusCode)
	}
	select {
	case err := <-done:
		if err != ErrRestart {
			t.Fatalf("Run = %v, want ErrRestart", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the update")
	}
}
