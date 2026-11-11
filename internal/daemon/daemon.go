// Package daemon is the service process: it holds the local API socket, writes
// the rotating log, and runs the scheduler over the configured projects until
// it is stopped.
package daemon

import (
	"context"
	"errors"
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
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/scheduler"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
)

// Options configure Run.
type Options struct {
	Paths   config.Paths
	Version string // the build version, sent in the client's version header
}

// errStopping answers a reload that arrives while the daemon stops.
var errStopping = errors.New("stopping")

// detectInterval is how often client detection reruns (CSR-59).
const detectInterval = time.Hour

// daemon is the state Run shares with the local API handlers.
type daemon struct {
	sched *scheduler.Scheduler

	mu       sync.Mutex
	projects []scheduler.ProjectStatus // the config's projects; one without a slot carries its error
	slotted  map[string]bool
}

// Run serves until ctx is done, a signal arrives or the local API asks it to
// stop, and returns nil after a clean stop. A held socket gives localapi.ErrHeld.
func Run(ctx context.Context, o Options) error {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	// Decision 2026-11-05-runner-rewrite-details point 14: stop at once; recovery at the next start returns a run in flight to To do.
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
	log := slog.New(slog.NewJSONHandler(lf, nil))
	d := &daemon{sched: &scheduler.Scheduler{Log: log}}

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
		// Decision 2026-11-05-reload-config: a reload restarts the whole scheduler, as stop then start.
		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		slots := d.slots(runCtx, &wg, log, o, cfg)
		d.sched.MergeInterval = cfg.MergePoll() // no Run is going: the last one returned
		d.sched.Drain = drain
		schedDone := make(chan struct{})
		wg.Go(func() { d.sched.Run(runCtx, slots); close(schedDone) })
		log.Info("scheduler started", "slots", len(slots))
		if reply != nil {
			reply <- reloadErr
		}
		select {
		case <-drain:
			log.Info("draining")
			select {
			case <-schedDone:
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
			log.Info("scheduler stopped")
		}
	}
}

// slots builds a client and a slot for every project with a runner token, and
// runs each client's heartbeats in wg until ctx is done. A project without a
// token gets no slot; its error shows in the status.
func (d *daemon) slots(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger, o Options, cfg *config.Config) []scheduler.Slot {
	sec := secrets.New(o.Paths.State)
	var slots []scheduler.Slot
	projects := make([]scheduler.ProjectStatus, 0, len(cfg.Projects))
	slotted := map[string]bool{}
	for _, p := range cfg.Projects {
		st := scheduler.ProjectStatus{Project: p.Key}
		// Volumes, containers and networks are named by the key alone, so
		// two projects with one key would share them (decision 2026-11-11-audit-fixes).
		if slotted[p.Key] {
			log.Error("another project has the same key, project not started", "project", p.Key, "url", p.URL)
			st.LastError, st.LastErrorAt = "another project has key "+p.Key+": only the first one runs; remove one of them", time.Now()
			projects = append(projects, st)
			continue
		}
		token, _, err := sec.Get(p, secrets.RunnerToken)
		if err != nil {
			log.Error("no runner token, project not started", "project", p.Key, "err", err)
			st.LastError, st.LastErrorAt = err.Error(), time.Now()
			projects = append(projects, st)
			continue
		}
		c := cinderline.New(p.URL, token, o.Version)
		c.Log = log.With("project", p.Key)
		env := &environment.Docker{
			Project: p.Key, Log: log, BuildImage: cfg.BuildImage,
			Allow:     environment.ProjectHosts(p.URL, p.AllowHosts),
			OnRefused: func(string) { c.AddRefused() },
		}
		proj, err := run.NewProject(p, sec, o.Paths.State, env, log)
		if err != nil {
			log.Error("project not started", "project", p.Key, "err", err)
			st.LastError, st.LastErrorAt = err.Error(), time.Now()
			projects = append(projects, st)
			continue
		}
		wg.Go(func() {
			// Decision on CSR-74 (Willem, option A): report and heartbeat at start so the app sees the runner at once.
			r := cinderline.Report{RunnerVersion: o.Version, Clients: []cinderline.ReportClient{}, Environments: []string{"default"}}
			if err := c.SyncReport(ctx, r); err != nil && ctx.Err() == nil {
				c.Log.Warn("report failed", "err", err)
			}
			if err := c.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				c.Log.Warn("heartbeat failed", "err", err)
			}
			t := time.NewTicker(cinderline.HeartbeatInterval)
			defer t.Stop()
			c.RunHeartbeats(ctx, t.C, r)
		})
		wg.Go(func() {
			// CSR-137: detection runs at slot start, then every hour.
			t := time.NewTicker(detectInterval)
			defer t.Stop()
			c.SetClients(proj.Detect(ctx))
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					c.SetClients(proj.Detect(ctx))
				}
			}
		})
		slots = append(slots, scheduler.Slot{Project: p.Key, Client: c, Hooks: proj.Hooks()})
		projects = append(projects, st)
		slotted[p.Key] = true
	}
	d.mu.Lock()
	d.projects, d.slotted = projects, slotted
	d.mu.Unlock()
	return slots
}

// status lists the config's projects in order, with the scheduler's facts for those with a slot.
func (d *daemon) status() []scheduler.ProjectStatus {
	live := d.sched.Status()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]scheduler.ProjectStatus, len(d.projects))
	for i, p := range d.projects {
		// One with its own error has no slot, also when a namesake has.
		if s, ok := live[p.Project]; ok && d.slotted[p.Project] && p.LastError == "" {
			p = s
		}
		out[i] = p
	}
	return out
}
