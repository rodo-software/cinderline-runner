package daemon

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/rodo-software/cinderline-runner/internal/config"
)

// freePort is a port on 127.0.0.1 nothing listens on just now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeWeb(t *testing.T, p config.Paths, web string, port int) {
	t.Helper()
	if err := (&config.Config{Web: web, WebPort: port}).Save(p.File); err != nil {
		t.Fatal(err)
	}
}

// serves says whether the page answers / on port: 401, as the request has no cookie.
func serves(port int) bool {
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusUnauthorized
}

func TestWebOnServesThePage(t *testing.T) {
	keyring.MockInit()
	p, port := paths(t), freePort(t)
	writeWeb(t, p, "on", port)
	start(t, p)
	waitFor(t, "the page", func() bool { return serves(port) })
}

func TestWebOffDoesNotListen(t *testing.T) {
	keyring.MockInit()
	p, port := paths(t), freePort(t)
	writeWeb(t, p, "off", port)
	c, _ := start(t, p)
	if err := c.Reload(ctxT(t)); err != nil { // returns once the config was applied twice
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
		conn.Close()
		t.Fatal("web off, but the port listens")
	}
}

func TestWebPortTakenDoesNotStopTheDaemon(t *testing.T) {
	keyring.MockInit()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	p, port := paths(t), taken.Addr().(*net.TCPAddr).Port
	writeWeb(t, p, "on", port)
	c, _ := start(t, p)
	// An unchanged config does not try the port again, so it is logged once.
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	status(t, c)
	raw, _ := os.ReadFile(p.Log)
	addr := `"addr":"127.0.0.1:` + strconv.Itoa(port) + `"`
	if n := strings.Count(string(raw), `"level":"ERROR","msg":"web page not served",`+addr); n != 1 {
		t.Errorf("got %d errors about the taken port, want 1:\n%s", n, raw)
	}
}

func TestWebReloadMovesThePort(t *testing.T) {
	keyring.MockInit()
	p, first, second := paths(t), freePort(t), freePort(t)
	writeWeb(t, p, "on", first)
	c, _ := start(t, p)
	waitFor(t, "the page on the first port", func() bool { return serves(first) })

	writeWeb(t, p, "on", second)
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if !serves(second) {
		t.Error("the page does not answer on the new port")
	}
	if serves(first) {
		t.Error("the page still answers on the old port")
	}

	writeWeb(t, p, "off", second)
	if err := c.Reload(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if serves(second) {
		t.Error("web off, but the page still answers")
	}
}
