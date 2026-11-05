package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUnixSocket proves the platform supports the Unix sockets M2's local API needs.
func TestUnixSocket(t *testing.T) {
	// Not t.TempDir(): macOS limits socket paths to 104 bytes.
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4)
		n, _ := io.ReadFull(c, buf)
		c.Write(buf[:n])
	}()

	c, err := net.Dial("unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q, want ping", buf)
	}
}
