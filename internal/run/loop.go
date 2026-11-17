package run

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/config"
	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/name"
	"github.com/rodo-software/cinderline-runner/internal/workspace"
)

// The loop's waits.
const (
	defaultPollAfter   = 30 * time.Second // while the app sends no poll_after
	defaultReviewAfter = time.Minute      // while the app sends no review_after
	maxPollAfter       = time.Hour        // a bad value must not stall the runner
	maxBackoff         = time.Minute      // between failed polls, from 1s doubling
	upgradeWait        = time.Hour        // after a 426: a new minimum stays until the person updates
	minStartWait       = time.Minute      // after a run that did not start, doubling
	maxStartWait       = 10 * time.Minute
	tidyEvery          = 10 * time.Minute // how often an idle runner sends held, besides after each run
)

// Environments are the environments this runner can run, for the report.
var Environments = []string{"default", "docker"}

// waitFor is the wait before the next poll for an interval d: d plus a
// random quarter of it, at most three seconds, so many runners polling one
// app do not line up. A variable so tests need not wait.
var waitFor = func(d time.Duration) time.Duration {
	if j := min(d/4, 3*time.Second); j > 0 {
		return d + rand.N(j)
	}
	return d
}

// ended is how a run ended, for the loop.
type ended struct {
	ref     string
	started bool
	err     error
}

// loop is the state of one project's loop, owned by its goroutine.
type loop struct {
	p        *Project
	c        *cinderline.Client
	ctx      context.Context
	draining bool

	running string     // the run in flight; "": none
	runDone chan ended // room for one: one run at a time

	upkeep     bool                         // a pass is in flight
	upkeepDone chan []cinderline.ReviewFact // room for one: one pass at a time
	facts      []cinderline.ReviewFact      // the last pass's facts; nil until one ended
	lastUpkeep time.Time
	review     time.Duration // the app's review_after

	lastTidy time.Time // zero: due, as at start and after a run

	backoff  time.Duration // after a failed poll
	upgrade  string        // the 426 logged last
	mismatch string        // why the repository pin refuses; "": it holds

	startWait  time.Duration // after a run that did not start
	startAt    time.Time     // no job before this
	notStarted *cinderline.ReportError
}

// Loop polls the app for the project every poll_after seconds and does
// what it answers, until ctx is done, or drain is closed and nothing is in
// flight any more. A plain stop (ctx) cancels a run at once; drain takes no
// new job and lets the run in flight finish.
func (p *Project) Loop(ctx context.Context, c *cinderline.Client, drain <-chan struct{}) {
	l := &loop{p: p, c: c, ctx: ctx, runDone: make(chan ended, 1), upkeepDone: make(chan []cinderline.ReviewFact, 1), review: defaultReviewAfter}
	// What an earlier process left: no run is in flight yet. Only hygiene, so a failure only logs.
	if err := p.Env.Cleanup(ctx); err != nil && ctx.Err() == nil {
		p.Log.Warn("stale containers and networks not removed", "project", p.Config.Key, "err", err)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	detected := p.detected
	for {
		if l.draining && l.running == "" && !l.upkeep {
			return
		}
		select {
		case <-ctx.Done():
			if l.running != "" {
				<-l.runDone // the run sees ctx too and ends without a word
			}
			if l.upkeep {
				<-l.upkeepDone
			}
			return
		case <-drain:
			l.draining, drain = true, nil
			p.Log.Info("draining", "project", p.Config.Key)
		case e := <-l.runDone:
			l.ended(e)
			timer.Reset(0) // the app may have the next job
		case facts := <-l.upkeepDone:
			l.upkeep, l.facts, l.lastUpkeep = false, facts, time.Now()
			timer.Reset(0) // the poll that asked for the pass could not take a job
		case <-detected:
			detected = nil
			timer.Reset(0) // the first poll that can take a job

		case <-timer.C:
			timer.Reset(l.poll())
		}
	}
}

// poll sends one poll and acts on the answer. It returns the wait until the next.
func (l *loop) poll() (wait time.Duration) {
	p := l.p
	defer func() {
		if r := recover(); r != nil {
			wait = l.failed(fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
		}
	}()
	l.c.Replay(l.ctx)
	// A run, an upkeep pass and tidying work on the same clones and bare
	// repository, so one at a time: a poll that starts a pass says it is not
	// idle, and tidying is done here, before the pass starts.
	idle := l.idle()
	review := l.running == "" && !l.upkeep && !l.draining && l.mismatch == "" && time.Since(l.lastUpkeep) >= l.review
	req := cinderline.PollRequest{Report: l.report(), Running: l.runningRefs(), Idle: idle && !review}
	var asked []string
	var stranded []workspace.Clone
	tidy := idle && (l.lastTidy.IsZero() || time.Since(l.lastTidy) >= tidyEvery)
	if tidy {
		var ok bool
		if asked, stranded, ok = p.held(l.ctx); !ok {
			tidy = false
		}
		req.Held = asked
	}
	if review {
		req.Review = l.facts
		if req.Review == nil {
			req.Review = []cinderline.ReviewFact{}
		}
	}

	ans, err := l.c.Poll(l.ctx, req)
	if err != nil {
		return l.failed(err)
	}
	l.backoff, l.upgrade = 0, ""
	p.setStatus(func(s *localapi.ProjectStatus) {
		s.Connected, s.LastPoll, s.LastError, s.LastErrorAt = true, time.Now(), "", time.Time{}
	})
	if ans.ReviewAfter > 0 {
		l.review = time.Duration(min(ans.ReviewAfter, int(maxPollAfter/time.Second))) * time.Second
	}
	proj := ans.Project
	if m := p.pin(proj); m != l.mismatch {
		if l.mismatch = m; m != "" {
			p.Log.Error("repository mismatch, no work until it is fixed", "project", p.Config.Key, "err", m)
		}
	}

	for _, ref := range ans.Stop {
		p.stopTurn(ref)
	}
	if tidy {
		if len(asked) > 0 {
			p.tidy(l.ctx, asked, ans.Held, stranded)
		}
		l.lastTidy = time.Now()
	}
	if review {
		l.facts = nil // delivered
		if ans.Review != nil {
			l.startUpkeep(proj, ans.Review)
		} else {
			l.lastUpkeep = time.Now()
		}
	}
	if ans.Job != nil && req.Idle && l.mismatch == "" {
		l.start(proj, *ans.Job)
	}

	after := defaultPollAfter
	if ans.PollAfter > 0 {
		after = time.Duration(min(ans.PollAfter, int(maxPollAfter/time.Second))) * time.Second
	}
	return waitFor(after)
}

// failed shows a failed poll in the status and returns the wait before the
// next: an hour after a 426, else 1 second doubling to a minute.
func (l *loop) failed(err error) time.Duration {
	p := l.p
	if l.ctx.Err() != nil {
		return 0 // stopping
	}
	p.setStatus(func(s *localapi.ProjectStatus) {
		s.Connected, s.LastError, s.LastErrorAt = false, firstLine(err.Error()), time.Now()
	})
	var up *cinderline.UpgradeRequiredError
	if errors.As(err, &up) {
		msg := fmt.Sprintf("%s needs runner ≥ %s; run `%s update` and then `%s run`", name.Name, up.MinimumRunnerVersion, name.Name, name.Name)
		if msg != l.upgrade {
			p.Log.Error(msg, "project", p.Config.Key)
			l.upgrade = msg
		}
		p.setStatus(func(s *localapi.ProjectStatus) { s.LastError = msg })
		return waitFor(upgradeWait)
	}
	l.backoff = min(max(2*l.backoff, time.Second), maxBackoff)
	p.Log.Warn("poll failed", "project", p.Config.Key, "err", err, "retry_in", l.backoff)
	return waitFor(l.backoff)
}

// report is the runner's report, whole.
func (l *loop) report() cinderline.Report {
	p := l.p
	p.mu.Lock()
	clients := append([]cinderline.ReportClient{}, p.clients...)
	refused := p.refused
	providers := []string{}
	if p.openRouter {
		providers = append(providers, "openrouter")
	}
	p.mu.Unlock()
	for _, pr := range p.Providers {
		providers = append(providers, pr.Name)
	}
	errs := []cinderline.ReportError{}
	if l.mismatch != "" {
		errs = append(errs, cinderline.ReportError{Kind: cinderline.KindRepositoryMismatch, Message: l.mismatch})
	}
	if l.notStarted != nil {
		errs = append(errs, *l.notStarted)
	}
	return cinderline.Report{RunnerVersion: l.c.Version(), Clients: clients, Environments: Environments, Errors: errs, RefusedConnections: refused, ModelProviders: providers, ProviderModels: p.Models.Get(), StrictHosts: p.StrictHosts}
}

// runningRefs are the run in flight and the runs whose report waits in the outbox.
func (l *loop) runningRefs() []string {
	refs := []string{}
	if l.running != "" {
		refs = append(refs, l.running)
	}
	for _, ref := range l.c.Queued() {
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// idle says the runner can start a run now: none and no upkeep pass is in
// flight, and client detection has a result, so the report names the clients.
func (l *loop) idle() bool {
	return l.running == "" && !l.upkeep && !l.draining && l.mismatch == "" && l.p.hasClients() && !time.Now().Before(l.startAt)
}

// start runs the job in its own goroutine. The ref is running at once, so
// no poll can miss it.
func (l *loop) start(proj cinderline.Project, jb cinderline.Job) {
	ref := jb.Issue.Ref
	l.running = ref
	l.p.setStatus(func(s *localapi.ProjectStatus) { s.Issue = ref })
	l.p.Log.Info("run started", "project", l.p.Config.Key, "issue", ref)
	go func() {
		started, err := l.p.run(l.ctx, l.c, proj, jb)
		l.runDone <- ended{ref, started, err}
	}()
}

// ended records how a run ended. One that did not start is reported and
// holds off the next job, longer each time; one that starts ends that.
func (l *loop) ended(e ended) {
	p := l.p
	l.running, l.lastTidy = "", time.Time{}
	p.setStatus(func(s *localapi.ProjectStatus) { s.Issue = "" })
	switch {
	case e.err != nil:
		l.startWait = min(max(2*l.startWait, minStartWait), maxStartWait)
		l.startAt = time.Now().Add(l.startWait)
		l.notStarted = &cinderline.ReportError{Kind: cinderline.KindNotStarted, Issue: e.ref, Message: firstLine(e.err.Error())}
		p.Log.Warn("run did not start", "project", p.Config.Key, "issue", e.ref, "err", e.err, "retry_in", l.startWait)
	case e.started:
		l.startWait, l.startAt, l.notStarted = 0, time.Time{}, nil
		p.Log.Info("run ended", "project", p.Config.Key, "issue", e.ref)
	}
}

// startUpkeep runs one upkeep pass in its own goroutine.
func (l *loop) startUpkeep(proj cinderline.Project, items []cinderline.ReviewItem) {
	l.upkeep = true
	go func() { l.upkeepDone <- l.p.upkeep(l.ctx, proj, items) }()
}

// pin refuses a repository other than the one configured on this machine,
// so the git token never follows a changed URL. It returns why; "": it holds.
func (p *Project) pin(proj cinderline.Project) string {
	switch {
	case proj.RepositoryURL == "":
		return "the project in Cinderline names no repository for " + p.Config.Key + "; this runner is configured for " + stripURL(p.Config.Repository)
	case config.CheckRepositoryURL(proj.RepositoryURL) != nil:
		return "the project in Cinderline names the repository " + stripURL(proj.RepositoryURL) + ", which is not an https URL; the runner signs in to git with its token over https only and is configured for " + stripURL(p.Config.Repository)
	case !config.SameRepository(proj.RepositoryURL, p.Config.Repository):
		return "the project in Cinderline names the repository " + stripURL(proj.RepositoryURL) + ", but this runner is configured for " + stripURL(p.Config.Repository)
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
