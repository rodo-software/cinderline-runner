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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/environment"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/run"
	"github.com/rodo-software/cinderline-runner/internal/secrets"
	"github.com/rodo-software/cinderline-runner/internal/workspace"

	"golang.org/x/term"
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

// runLoop runs a project's loop; a variable so tests can hold a run in flight.
var runLoop = (*run.Project).Loop

// dockerMemory reads Docker's total memory; a variable so tests need no Docker.
var dockerMemory = func(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return environment.DockerMemory(ctx, workspace.RunCommand)
}

// slot is one project's loop. It holds the project ID's Docker names until it ends.
type slot struct {
	id    string
	proj  *run.Project
	drain chan struct{} // closed: take no new work, end once nothing is in flight
	stale bool          // drain is closed
}

// daemon is the state Run shares with the local API handlers. Only Run's
// goroutine writes it, under mu.
type daemon struct {
	mu       sync.Mutex
	projects []localapi.ProjectStatus // the config's projects; one that does not run carries its error
	ids      []string                 // per entry of projects, its ID
	live     []*run.Project           // per entry of projects, its loop under this config; nil while it does not run
	slots    map[string]*slot         // by ID: the loop that runs now, under this config or an older one

	// Only Run's goroutine uses these.
	ctx        context.Context
	log        *slog.Logger
	o          Options
	cfg        *config.Config
	models     *run.Models        // one fetch of the providers' models for this config's projects
	stopModels context.CancelFunc // ends the fetch for the config before
	ended      chan *slot         // a slot's loop has ended
	budget     *run.Budget        // the memory every project's runs share, one for the process
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
	f, isFile := o.Verbose.(*os.File)
	log := logger(lf, o.Verbose, isFile && term.IsTerminal(int(f.Fd())))
	d := &daemon{ctx: ctx, log: log, o: o, slots: map[string]*slot{}, ended: make(chan *slot), stopModels: func() {}, budget: run.NewBudget(dockerMemory)}
	defer func() { d.stopModels() }()

	reloads := make(chan chan error)
	drain := make(chan struct{})
	var drainOnce sync.Once // a second drain request must not close it again
	srv := &http.Server{Handler: localapi.Handler(localapi.Handlers{
		Version: o.Version,
		Status:  d.status,
		Memory: func() localapi.Memory {
			size, reserved := d.budget.Status()
			return localapi.Memory{Budget: size, Reserved: reserved}
		},
		Stop:   stop,
		Drain:  func() { drainOnce.Do(func() { close(drain) }) },
		Follow: lf.follow,
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
	warnDropped(log, cfg)
	log.Info("started", "version", o.Version, "socket", o.Paths.Socket)
	d.apply(cfg)

	done, drained := ctx.Done(), (<-chan struct{})(drain)
	for {
		if (done == nil || drained == nil) && len(d.slots) == 0 {
			log.Info("stopped")
			return nil
		}
		select {
		case <-drained:
			drained = nil
			log.Info("draining")
			d.drainAll()
		case <-done:
			done = nil // every loop sees ctx and ends
		case s := <-d.ended:
			d.end(s)
			if done != nil && drained != nil {
				d.fill()
			}
		case reply := <-reloads:
			if done == nil || drained == nil {
				reply <- errStopping
				continue
			}
			log.Info("reloading")
			next, err := config.Load(o.Paths.File)
			if err != nil {
				log.Error("config failed to reload, keeping the old one", "err", err)
				reply <- err
				continue
			}
			warnDropped(log, next)
			// Decision 2026-11-24-rolling-reload: each loop drains on its own
			// and starts again with the newest config once nothing is in flight.
			d.drainAll()
			d.apply(next)
			reply <- nil
		}
	}
}

// warnDropped warns of the old fields Load dropped from the config file.
func warnDropped(log *slog.Logger, cfg *config.Config) {
	if len(cfg.Dropped) > 0 {
		log.Warn("config fields of an older runner ignored; the next change to the config leaves them out", "fields", cfg.Dropped)
	}
}

// apply makes cfg the config: it lists its projects and starts those whose
// ID no loop holds now. A project that cannot run gets its error in the status.
func (d *daemon) apply(cfg *config.Config) {
	d.stopModels()
	d.models = &run.Models{}
	if len(cfg.Projects) > 0 && len(cfg.Providers) > 0 {
		var ctx context.Context
		ctx, d.stopModels = context.WithCancel(d.ctx)
		go d.models.Watch(ctx, cfg.Providers, secrets.New(d.o.Paths.State), d.log)
	} else {
		d.stopModels = func() {}
	}
	d.cfg = cfg
	projects := make([]localapi.ProjectStatus, len(cfg.Projects))
	ids := make([]string, len(cfg.Projects))
	seen := map[string]bool{}
	for i, p := range cfg.Projects {
		ids[i] = p.ID()
		projects[i] = localapi.ProjectStatus{Project: p.Label()} // the key: the ID is another name for an entry of an older runner
		// Volumes, containers and networks are named by the ID, so two
		// projects with one ID would share them.
		if seen[ids[i]] {
			err := errors.New("another project has key " + ids[i] + ": only the first one runs; remove one of them")
			d.log.Error("another project has the same key, project not started", "project", p.Key, "err", err)
			projects[i].LastError, projects[i].LastErrorAt = err.Error(), time.Now()
		}
		seen[ids[i]] = true
	}
	d.mu.Lock()
	d.projects, d.ids, d.live = projects, ids, make([]*run.Project, len(projects))
	d.mu.Unlock()
	d.fill()
}

// fill starts every project of the config that does not run yet, has no
// error, and whose ID no loop holds: an older loop must end first, as both
// would use the same Docker names.
func (d *daemon) fill() {
	// Run's goroutine is the only writer, so it reads without mu; building a
	// project reads the keychain, which may ask the person, so mu is not
	// held meanwhile and status still answers.
	for i, p := range d.cfg.Projects {
		if d.live[i] != nil || d.projects[i].LastError != "" || d.slots[d.ids[i]] != nil {
			continue
		}
		proj, err := d.start(p)
		d.mu.Lock()
		if err != nil {
			d.projects[i].LastError, d.projects[i].LastErrorAt = err.Error(), time.Now()
		}
		d.live[i] = proj
		d.mu.Unlock()
	}
}

// start builds a client and a project for p and runs its loop and its
// hourly client detection until ctx is done, or until its drain once the
// loop is done. A project without a runner token does not run.
func (d *daemon) start(p config.Project) (*run.Project, error) {
	log, cfg := d.log, d.cfg
	sec := secrets.New(d.o.Paths.State)
	token, _, err := sec.Get(p, secrets.RunnerToken)
	if err != nil {
		log.Error("no runner token, project not started", "project", p.Key, "err", err)
		return nil, err
	}
	c := cinderline.New(p.URL, token, d.o.Version)
	c.Log = log.With("project", p.Key)
	c.Outbox = run.OutboxFile(d.o.Paths.State, p)
	var proj *run.Project
	env := &environment.Docker{
		Project: p.ID(), Log: log, Exec: workspace.RunCommand, Memory: p.MemoryLimit(),
		Allow:     environment.ProjectHosts(p.URL, slices.Concat(cfg.AllowHosts, p.AllowHosts)),
		OnRefused: func(issue, hp string) { proj.AddRefused(issue, hp) },
	}
	proj, err = run.NewProject(p, cfg.Providers, sec, d.o.Paths.State, env, log)
	if err != nil {
		log.Error("project not started", "project", p.Key, "err", err)
		return nil, err
	}
	proj.KeepFor, proj.MinFree, proj.Others, proj.Models, proj.StrictHosts, proj.Budget = cfg.KeepSessions(), cfg.MinFree(), cfg.Projects, d.models, cfg.StrictHosts, d.budget
	s := &slot{id: p.ID(), proj: proj, drain: make(chan struct{})}
	d.mu.Lock()
	d.slots[s.id] = s
	d.mu.Unlock()
	ctx := d.ctx
	go func() {
		detectCtx, stopDetect := context.WithCancel(ctx)
		var wg sync.WaitGroup
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
		runLoop(proj, ctx, c, s.drain)
		stopDetect() // a drained loop ends its detection too
		wg.Wait()
		d.ended <- s // Run receives until every slot has ended
	}()
	log.Info("project started", "project", p.Key)
	return proj, nil
}

// drainAll makes every loop take no new work and end once nothing is in flight.
func (d *daemon) drainAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.slots {
		if !s.stale {
			s.stale = true
			close(s.drain)
		}
	}
}

// end forgets the slot, whose loop has ended.
func (d *daemon) end(s *slot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.slots[s.id] == s {
		delete(d.slots, s.id)
	}
	d.log.Info("project stopped", "project", s.proj.Config.Key)
}

// status lists the config's projects in order, with their loops' facts for
// those that run, then the loops of projects the config no longer has that
// still finish their runs. A project whose loop runs on an older config
// shows that loop, draining (decision 2026-11-24-rolling-reload).
func (d *daemon) status() []localapi.ProjectStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]localapi.ProjectStatus, len(d.projects))
	for i, p := range d.projects {
		// One with its own error does not run, also when a namesake does.
		switch s := d.slots[d.ids[i]]; {
		case p.LastError != "":
		case d.live[i] != nil:
			p = d.live[i].Status()
		case s != nil:
			p = s.proj.Status()
			p.Draining = true
		}
		out[i] = p
	}
	var gone []localapi.ProjectStatus
	for id, s := range d.slots {
		if !slices.Contains(d.ids, id) {
			p := s.proj.Status()
			p.Draining = true
			gone = append(gone, p)
		}
	}
	slices.SortFunc(gone, func(a, b localapi.ProjectStatus) int { return strings.Compare(a.Project, b.Project) })
	return append(out, gone...)
}
