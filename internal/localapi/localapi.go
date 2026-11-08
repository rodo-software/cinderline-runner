// Package localapi is the daemon's HTTP API on a local Unix socket, and its
// client for the CLI. The socket sits in a folder only the user can open; there
// is no further auth (plan M2).
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/scheduler"
)

var (
	// ErrHeld means a running daemon holds the socket.
	ErrHeld = errors.New("another " + name.Name + " is running and holds the socket")
	// ErrNotRunning means no daemon answers on the socket.
	ErrNotRunning = errors.New(name.Name + " is not running")
)

// Listen takes the socket at path. A socket file that answers a dial is held
// and gives ErrHeld; any other file there is stale and is removed.
func Listen(path string) (net.Listener, error) {
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: %w", path, ErrHeld)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return net.Listen("unix", path)
}

// Status is the answer of GET /status.
type Status struct {
	Version  string                    `json:"version"`
	Projects []scheduler.ProjectStatus `json:"projects"`
}

// Handlers are what the daemon does for each endpoint.
type Handlers struct {
	Version string
	Status  func() []scheduler.ProjectStatus
	Reload  func() error
	Stop    func()
	// Drain makes the daemon finish its runs in flight, take no new work, and exit.
	Drain func()
	// Follow returns the last n lines of the log file and a channel of every
	// line logged after them, closed when the subscriber is dropped; cancel ends it.
	Follow func(n int) (tail []byte, lines <-chan []byte, cancel func(), err error)
}

// Handler serves the endpoints from h.
func Handler(h Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Status{Version: h.Version, Projects: h.Status()})
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		n := 100
		if s := r.URL.Query().Get("tail"); s != "" {
			var err error
			if n, err = strconv.Atoi(s); err != nil || n < 0 {
				http.Error(w, "tail must be a number of lines", http.StatusBadRequest)
				return
			}
		}
		tail, lines, cancel, err := h.Follow(n)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer cancel()
		w.Header().Set("Content-Type", "application/x-ndjson")
		rc := http.NewResponseController(w)
		w.Write(tail)
		rc.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				if _, err := w.Write(line); err != nil {
					return
				}
				rc.Flush()
			}
		}
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		http.NewResponseController(w).Flush() // answer before the server closes
		h.Stop()
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		http.NewResponseController(w).Flush()
		h.Drain()
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Client talks to the daemon on Socket.
type Client struct {
	Socket string
}

// Status returns the daemon's status.
func (c Client) Status(ctx context.Context) (Status, error) {
	var s Status
	resp, err := c.do(ctx, "GET", "/status")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// Logs copies the last tail lines of the log and then every new line to w,
// until ctx is done or the daemon closes the stream.
func (c Client) Logs(ctx context.Context, tail int, w io.Writer) error {
	resp, err := c.do(ctx, "GET", "/logs?tail="+strconv.Itoa(tail))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// Stop stops the daemon at once.
func (c Client) Stop(ctx context.Context) error { return c.post(ctx, "/stop") }

// Drain makes the daemon finish its runs in flight, take no new work, and exit.
func (c Client) Drain(ctx context.Context) error { return c.post(ctx, "/drain") }

// Reload makes the daemon re-read its config and restart its slots.
func (c Client) Reload(ctx context.Context) error { return c.post(ctx, "/reload") }

func (c Client) post(ctx context.Context, path string) error {
	resp, err := c.do(ctx, "POST", path)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// do sends a request and turns a failed dial into ErrNotRunning and a non-2xx
// answer into an error with its body.
func (c Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
			if err != nil && ctx.Err() == nil {
				return nil, fmt.Errorf("%w (%w)", ErrNotRunning, err)
			}
			return conn, err
		},
	}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+name.Name+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}
