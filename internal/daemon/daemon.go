// Package daemon is the service process: it holds the local API socket, writes
// the rotating log, and runs one loop per configured project until it is
// stopped.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/redact"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// Options configure Run.
type Options struct {
	Paths   config.Paths
	Version string    // the build version, sent in the client's version header
	Verbose io.Writer // non-nil (run -v): the log also goes here as text, and both outputs log at Debug
}

// errStopping answers a reload that arrives while the daemon stops.
var errStopping = errors.New("stopping")

// detectInterval is how often client detection reruns (CSR-59).
const detectInterval = time.Hour

// daemon is the state Run shares with the local API handlers.
type daemon struct {
	mu       sync.Mutex
	projects []localapi.ProjectStatus // the config's projects; one that does not run carries its error
	live     map[string]*run.Project  // the projects that run, by ID
}

// Run serves until ctx is done, a signal arrives or the local API asks it to
// stop, and returns nil after a clean stop. A held socket gives localapi.ErrHeld.
func Run(ctx context.Context, o Options) error {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	// A plain stop cancels a run in flight at once; the app blocks its issue at the next poll after the restart.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(o.Paths.Socket), 0o700); err != nil {
		return err
	}
	ln, err := localapi.Listen(o.Paths.Socket)
	if err != nil {
		return err
	}
	lf := &logFile{path: o.Paths.Log, max: maxLogSize}
	defer lf.Close()
	var log *slog.Logger
	if o.Verbose != nil {
		debug := &slog.HandlerOptions{Level: slog.LevelDebug}
		log = slog.New(redact.Handler(slog.NewMultiHandler(slog.NewJSONHandler(lf, debug), slog.NewTextHandler(o.Verbose, debug))))
	} else {
		log = slog.New(redact.Handler(slog.NewJSONHandler(lf, nil)))
	}
	d := &daemon{}

	reloads := make(chan chan error)
	drain := make(chan struct{})
	var drainOnce sync.Once // a second drain request must not close it again
	srv := &http.Server{Handler: localapi.Handler(localapi.Handlers{
		Version: o.Version,
		Status:  d.status,
		Stop:    stop,
		Drain:   func() { drainOnce.Do(func() { close(drain) }) },
		Follow:  lf.follow,
		Reload: func() error {
			reply := make(chan error, 1)
			select {
			case reloads <- reply:
			case <-ctx.Done():
				return errStopping
			case <-drain:
				return errStopping
			}
			select {
			case err := <-reply:
				return err
			case <-ctx.Done():
				return errStopping
			case <-drain:
				return errStopping
			}
		},
	})}
	go srv.Serve(ln)
	defer srv.Close() // closes the listener, which removes the socket file

	cfg, err := config.Load(o.Paths.File)
	if err != nil {
		log.Error("config failed to load", "err", err)
		return err
	}
	log.Info("started", "version", o.Version, "socket", o.Paths.Socket)
	var reply chan error
	var reloadErr error
	for {
		// Decision 2026-11-05-reload-config: a reload restarts every loop, as stop then start.
		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		n := d.start(runCtx, &wg, log, o, cfg, drain)
		loopsDone := make(chan struct{})
		go func() { wg.Wait(); close(loopsDone) }()
		log.Info("projects started", "projects", n)
		if reply != nil {
			reply <- reloadErr
		}
		select {
		case <-drain:
			log.Info("draining")
			select {
			case <-loopsDone:
			case <-ctx.Done():
			}
			cancel()
			wg.Wait()
			log.Info("stopped")
			return nil
		case <-ctx.Done():
			cancel()
			wg.Wait()
			log.Info("stopped")
			return nil
		case reply = <-reloads:
			log.Info("reloading")
			next, err := config.Load(o.Paths.File)
			if err != nil {
				log.Error("config failed to reload, keeping the old one", "err", err)
			} else {
				cfg = next
			}
			reloadErr = err
			cancel()
			wg.Wait()
			log.Info("projects stopped")
		}
	}
}

// start builds a client and a project for every configured project with a
// runner token and runs its loop and its hourly client detection in wg
// until ctx is done, or until drain once its loop is done. A project
// without a token does not run; its error shows in the status. It returns
// how many run.
func (d *daemon) start(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger, o Options, cfg *config.Config, drain <-chan struct{}) int {
	sec := secrets.New(o.Paths.State)
	projects := make([]localapi.ProjectStatus, 0, len(cfg.Projects))
	live := map[string]*run.Project{}
	for _, p := range cfg.Projects {
		id := p.ID()
		st := localapi.ProjectStatus{Project: id}
		fail := func(msg string, err error) {
			log.Error(msg, "project", id, "err", err)
			st.LastError, st.LastErrorAt = err.Error(), time.Now()
			projects = append(projects, st)
		}
		// Volumes, containers and networks are named by the ID, so two
		// projects with one ID would share them.
		if live[id] != nil {
			fail("another project has the same key, project not started", errors.New("another project has key "+id+": only the first one runs; remove one of them"))
			continue
		}
		token, _, err := sec.Get(p, secrets.RunnerToken)
		if err != nil {
			fail("no runner token, project not started", err)
			continue
		}
		c := cinderline.New(p.URL, token, o.Version)
		c.Log = log.With("project", id)
		c.Outbox = run.OutboxFile(o.Paths.State, p)
		var proj *run.Project
		env := &environment.Docker{
			Project: id, Log: log, Exec: workspace.RunCommand,
			Allow:     environment.ProjectHosts(p.URL, p.AllowHosts),
			OnRefused: func(string) { proj.AddRefused() },
		}
		proj, err = run.NewProject(p, cfg.Providers, sec, o.Paths.State, env, log)
		if err != nil {
			fail("project not started", err)
			continue
		}
		proj.KeepFor, proj.MinFree, proj.Others = cfg.KeepSessions(), cfg.MinFree(), cfg.Projects
		detectCtx, stopDetect := context.WithCancel(ctx)
		wg.Go(func() {
			defer stopDetect() // a drained loop ends its detection too
			proj.Loop(ctx, c, drain)
		})
		wg.Go(func() {
			t := time.NewTicker(detectInterval)
			defer t.Stop()
			for proj.Detect(detectCtx); ; proj.Detect(detectCtx) {
				select {
				case <-detectCtx.Done():
					return
				case <-t.C:
				}
			}
		})
		projects = append(projects, st)
		live[id] = proj
	}
	d.mu.Lock()
	d.projects, d.live = projects, live
	d.mu.Unlock()
	return len(live)
}

// status lists the config's projects in order, with their loops' facts for those that run.
func (d *daemon) status() []localapi.ProjectStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]localapi.ProjectStatus, len(d.projects))
	for i, p := range d.projects {
		// One with its own error does not run, also when a namesake does.
		if live := d.live[p.Project]; live != nil && p.LastError == "" {
			p = live.Status()
		}
		out[i] = p
	}
	return out
}
