// Package scheduler runs one slot per project: recovery at start, then a loop
// that long-polls for work, claims, runs and publishes, plus timers for review
// branches and merge checks. The steps themselves are hooks that M3 fills.
//
// It keeps no run or step table (decision 0091): between loops a slot holds
// nothing but its backoff.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
	"github.com/rodo-software/cinderline-runner/internal/name"
)

// ErrNotStarted marks a Run error from before the first agent turn (tools,
// team, issue, project, base branch, worktree, token): the issue goes back
// to To do without a comment and the slot waits StartBackoff before its
// next claim (decision 2026-11-07-publish-resilience). Run hooks wrap such
// errors with it.
var ErrNotStarted = errors.New("the run did not start")

// maxStartBackoff is the ceiling for the exponential backoff between
// re-claims of an issue whose run did not start.
const maxStartBackoff = 10 * time.Minute

// Hooks are the steps M3 fills. A nil hook is skipped.
type Hooks struct {
	Recover        func(ctx context.Context, c *cinderline.Client) error                         // once at slot start, before the first Work
	Unheld         func(ctx context.Context, c *cinderline.Client) error                         // after each run, while the slot holds no issue; an error only logs
	Pin            func(ctx context.Context, c *cinderline.Client) error                         // before the claims of each poll; an error is a slot failure, so nothing is claimed
	Check          func(ctx context.Context, c *cinderline.Client, issue cinderline.Issue) error // before each claim; an error skips the issue, unclaimed
	Run            func(ctx context.Context, c *cinderline.Client, issue cinderline.Issue) error
	Publish        func(ctx context.Context, c *cinderline.Client, issue cinderline.Issue) error
	ReviewBranches func(ctx context.Context, c *cinderline.Client) error // keep Review branches current, on a timer
	MergeCheck     func(ctx context.Context, c *cinderline.Client) error // merge checks, on a timer
}

// Slot is one project's slot. One per project (plan section 2); a per-project
// slot count would start several of these for one project.
type Slot struct {
	Project string // the project key, for log lines
	Client  *cinderline.Client
	Hooks   Hooks
}

// Scheduler runs slots. Zero fields take their defaults.
type Scheduler struct {
	Log            *slog.Logger  // default slog.Default()
	ReviewInterval time.Duration // default 5 minutes; the app's interval wins when it sends one (CSR-174)
	MergeInterval  time.Duration // default 60 seconds; the daemon sets config.json's merge_poll_seconds (legacy MERGE_POLL_SECONDS); the app's interval wins when it sends one
	MaxBackoff     time.Duration // default 60 seconds; the first wait after a failure is 1s or this, if less
	StartBackoff   time.Duration // default 60 seconds: the wait before the next claim after a run that did not start (ErrNotStarted); doubles per failure in a row, up to 10 minutes
	IdleWait       time.Duration // default 5 seconds: the wait before the next GET /work after an answer that started no run; doubles per such answer in a row, up to MaxIdleWait (CSR-113); the app's interval replaces both when it sends one (CSR-174)
	MaxIdleWait    time.Duration // default 60 seconds
	ReportAfter    time.Duration // default 2 minutes: how long Cinderline may stay unavailable (cinderline.Unavailable) before poll_failed is reported; a deploy takes about a minute (CSR-93)

	// Drain, when non-nil, is closed to begin draining: every slot takes no new
	// work, lets the run in flight finish and publish, and then Run returns.
	Drain <-chan struct{}

	status map[string]*ProjectStatus // by project key; shared with Run's copy, guarded by statusMu
}

// statusMu guards every Scheduler's status. One lock for the package because a
// mutex field would be copied by Run, and a scheduler is one per process.
var statusMu sync.Mutex

// ProjectStatus is what Status reports about one slot.
type ProjectStatus struct {
	Project     string    `json:"project"`
	Connected   bool      `json:"connected"`            // the last Work succeeded and no failure since
	LastPoll    time.Time `json:"last_poll,omitzero"`   // the last successful Work
	Issue       string    `json:"issue,omitempty"`      // the issue between claim and the end of Run and Publish
	LastError   string    `json:"last_error,omitempty"` // the last slot failure; cleared by the next successful Work
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// Status returns the status of every slot Run has started, by project key.
// Safe to call while Run runs.
func (s *Scheduler) Status() map[string]ProjectStatus {
	statusMu.Lock()
	defer statusMu.Unlock()
	out := make(map[string]ProjectStatus, len(s.status))
	for k, v := range s.status {
		out[k] = *v
	}
	return out
}

// setStatus changes project's status under the lock.
func (s *Scheduler) setStatus(project string, change func(*ProjectStatus)) {
	statusMu.Lock()
	defer statusMu.Unlock()
	change(s.status[project])
}

// Run starts one goroutine per slot and blocks until ctx is done and every slot has returned.
func (s *Scheduler) Run(ctx context.Context, slots []Slot) {
	cfg := *s
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ReviewInterval <= 0 {
		cfg.ReviewInterval = 5 * time.Minute
	}
	if cfg.MergeInterval <= 0 {
		cfg.MergeInterval = 60 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.StartBackoff <= 0 {
		cfg.StartBackoff = 60 * time.Second
	}
	if cfg.IdleWait <= 0 {
		cfg.IdleWait = 5 * time.Second
	}
	if cfg.MaxIdleWait <= 0 {
		cfg.MaxIdleWait = 60 * time.Second
	}
	if cfg.ReportAfter <= 0 {
		cfg.ReportAfter = 2 * time.Minute
	}
	statusMu.Lock()
	if s.status == nil {
		s.status = map[string]*ProjectStatus{}
	}
	for _, slot := range slots {
		if s.status[slot.Project] == nil {
			s.status[slot.Project] = &ProjectStatus{Project: slot.Project}
		}
	}
	cfg.status = s.status
	statusMu.Unlock()

	work, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	if cfg.Drain != nil {
		go func() {
			select {
			case <-cfg.Drain:
				cancelWork()
			case <-work.Done():
			}
		}()
	}

	var wg sync.WaitGroup
	for _, slot := range slots {
		wg.Go(func() { cfg.timers(work, slot) })
		wg.Go(func() { cfg.slot(ctx, work, slot) })
	}
	wg.Wait()
}

// slot runs cycles until ctx or work is done, backing off after each failure.
// While Cinderline is only unavailable, for less than ReportAfter, a failure
// is a warning and not reported: a deploy is the normal case (CSR-93).
//
// A 426 means this runner is too old for the app: the slot logs it once at
// Error, shows it in the status, and asks again only after
// cinderline.UpgradeWait, until a newer runner runs (CSR-187).
func (s *Scheduler) slot(ctx context.Context, work context.Context, slot Slot) {
	backoff := min(time.Second, s.MaxBackoff)
	var down time.Time // when the current run of failures began
	var upgradeLogged string
	for {
		polled, err := s.cycle(ctx, work, slot)
		if ctx.Err() != nil || work.Err() != nil {
			s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = "" })
			return
		}
		if polled {
			upgradeLogged = ""
		}
		if polled || down.IsZero() {
			down = time.Now()
		}
		c := slot.Client
		if up := (*cinderline.UpgradeRequiredError)(nil); errors.As(err, &up) {
			msg := fmt.Sprintf("%s needs runner ≥ %s; run `%s update` and then `%s run`", name.Name, up.MinimumRunnerVersion, name.Name, name.Name)
			if msg != upgradeLogged {
				s.Log.Error(msg, "project", slot.Project)
				upgradeLogged = msg
			}
			s.setStatus(slot.Project, func(st *ProjectStatus) {
				st.Connected, st.Issue, st.LastError, st.LastErrorAt = false, "", msg, time.Now()
			})
			wait, _ := c.PollAfter(cinderline.PollWork) // the time left of the 426's wait
			select {
			case <-work.Done():
				return
			case <-time.After(max(wait, backoff)): // backoff: a 426 from the last sync can outlive the wait
			}
			c.SyncNow()
			continue
		}
		var re *cinderline.ReportError
		brief := cinderline.Unavailable(err) && time.Since(down) < s.ReportAfter
		switch {
		case brief:
		case errors.As(err, &re) && re.Issue == "":
			c.SetError(*re)
		default:
			msg := err.Error()
			if i := strings.Index(msg, "\n"); i >= 0 {
				msg = msg[:i]
			}
			c.SetError(cinderline.ReportError{Kind: cinderline.KindPollFailed, Message: msg})
		}
		s.setStatus(slot.Project, func(st *ProjectStatus) {
			st.Connected, st.Issue, st.LastError, st.LastErrorAt = false, "", err.Error(), time.Now()
		})
		if polled {
			backoff = min(time.Second, s.MaxBackoff)
		}
		if brief {
			s.Log.Warn("cinderline unavailable", "project", slot.Project, "err", err, "retry_in", backoff)
		} else {
			s.Log.Error("slot failed", "project", slot.Project, "err", err, "retry_in", backoff)
		}
		select {
		case <-work.Done():
			return
		case <-time.After(backoff):
		}
		// In sync mode the retry asks at once, so the backoff, not the sync
		// interval, paces it (CSR-183).
		c.SyncNow()
		backoff = min(backoff*2, s.MaxBackoff)
	}
}

// cycle is recovery and then the work loop, until an error, a panic or work is
// done. It returns whether a Work call succeeded. work is the context for
// taking new work; ctx is the real context for finish (drain must not cancel a
// run in flight).
func (s *Scheduler) cycle(ctx context.Context, work context.Context, slot Slot) (polled bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	c, h := slot.Client, slot.Hooks
	// Before recovery, so a move a run decided wins over the reset (CSR-159).
	c.Replay(work)
	if h.Recover != nil {
		if err := h.Recover(work, c); err != nil {
			return false, fmt.Errorf("recover: %w", err)
		}
	}
	// Work names only the first To do issue, so while that one is skipped
	// every poll, changed or not, walks the board again (legacy parity).
	// A cycle starts stuck: a failure may have cut the last walk short, and
	// the client's ETag would hide the unchanged board (CSR-93).
	stuck := h.Run != nil
	var strikes struct {
		issue string
		n     int
		wait  time.Duration
	}
	strikes.wait = s.StartBackoff
	idle := s.IdleWait
	// waitIdle paces the next Work after an answer that started no run: the
	// app's interval when the answer sent one, else idle, doubling. The loop
	// condition ends the slot if work is done meanwhile.
	waitIdle := func() {
		d, ok := c.PollAfter(cinderline.PollWork)
		if !ok {
			d = idle
		}
		select {
		case <-work.Done():
		case <-time.After(cinderline.Jitter(d)):
			idle = min(idle*2, s.MaxIdleWait)
		}
	}
	for work.Err() == nil {
		s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = "" })
		c.Replay(work)
		issue, err := c.Work(work)
		noChange := errors.Is(err, cinderline.ErrNoChange)
		if err != nil && !noChange {
			return polled, err
		}
		polled = true
		c.ClearError(cinderline.KindPollFailed)
		s.setStatus(slot.Project, func(st *ProjectStatus) {
			st.Connected, st.LastPoll, st.LastError, st.LastErrorAt = true, time.Now(), "", time.Time{}
		})
		if !noChange && issue == nil {
			stuck = false
			waitIdle()
			continue // no work
		}
		if !noChange && issue != nil {
			idle = s.IdleWait
		}
		if noChange && !stuck {
			waitIdle()
			continue // unchanged
		}
		if h.Run == nil {
			s.Log.Info("no run step yet, skipping", "project", slot.Project, "issue", issue.Ref)
			waitIdle()
			continue // not claimed, so it stays To do; the ETag keeps this from spinning
		}
		claimed, err := s.claimNext(work, slot, issue)
		if err != nil {
			return polled, err
		}
		if stuck = claimed == nil; stuck {
			waitIdle()
			continue
		}
		s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = claimed.Ref })
		if strikes.issue != claimed.Ref {
			strikes.issue, strikes.n = claimed.Ref, 0
		}
		notStarted := s.finish(ctx, slot, *claimed, strikes.n+1, strikes.wait)
		c.SyncNow() // the answer from before the run is out of date
		s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = "" })
		// Here, not on a timer: between runs the slot holds nothing, so no claim races it (decision 2026-11-16-unheld-in-progress).
		s.tick(work, slot, "unheld check failed", h.Unheld)
		if notStarted {
			strikes.n++
			select {
			case <-work.Done():
				return polled, work.Err()
			case <-time.After(strikes.wait):
			}
			strikes.wait = min(strikes.wait*2, maxStartBackoff)
		} else {
			strikes.issue, strikes.n, strikes.wait = "", 0, s.StartBackoff
		}
	}
	return polled, work.Err()
}

// claimNext pins the repository, then claims the first To do issue that
// passes Check: first (Work's answer, nil when re-walking), then the board in
// order. An issue that is skipped or not claimed does not block the ones
// behind it. It returns nil when nothing was claimed.
func (s *Scheduler) claimNext(work context.Context, slot Slot, first *cinderline.Issue) (*cinderline.Issue, error) {
	c, h := slot.Client, slot.Hooks
	if h.Pin != nil {
		if err := h.Pin(work, c); err != nil {
			return nil, fmt.Errorf("repository: %w", err)
		}
		c.ClearError(cinderline.KindRepositoryMismatch)
	}
	var queue []cinderline.Issue
	if first != nil {
		queue = append(queue, *first)
	}
	tried, listed := map[string]bool{}, false
	for {
		if len(queue) == 0 {
			if listed {
				c.KeepIssueErrors(tried)
				return nil, nil
			}
			// shortcut: one page of To do issues, as the API returns them; page if a board outgrows it.
			all, err := c.ListIssues(work, "todo")
			if err != nil {
				return nil, err
			}
			queue, listed = all, true
			continue
		}
		issue := queue[0]
		queue = queue[1:]
		if tried[issue.Ref] {
			continue
		}
		tried[issue.Ref] = true
		if h.Check != nil {
			if err := h.Check(work, c, issue); err != nil {
				if work.Err() != nil {
					return nil, work.Err()
				}
				if cinderline.Unavailable(err) {
					return nil, err // no reason to skip it: the slot backs off and walks again (CSR-93)
				}
				s.Log.Warn("skipped before the claim", "project", slot.Project, "issue", issue.Ref, "err", err)
				var re *cinderline.ReportError
				if errors.As(err, &re) && re.Issue != "" {
					if c.SetError(*re) {
						if _, cerr := c.AddComment(work, issue.Ref, "The runner skipped this issue before claiming it: "+re.Message); cerr != nil {
							s.Log.Warn("failed to add comment for skipped issue", "project", slot.Project, "issue", issue.Ref, "err", cerr)
						}
					}
				}
				continue
			}
			c.ClearError(issue.Ref)
		}
		var conflict *cinderline.ConflictError
		if _, err := c.ClaimIssue(work, issue.Ref, "todo"); errors.As(err, &conflict) {
			s.Log.Info("claim conflict, moving on", "project", slot.Project, "issue", issue.Ref, "status", conflict.Status)
			continue
		} else if err != nil {
			if work.Err() != nil {
				return nil, work.Err()
			}
			if cinderline.Unavailable(err) {
				return nil, err
			}
			s.Log.Warn("not claimed", "project", slot.Project, "issue", issue.Ref, "err", err)
			continue
		}
		return &issue, nil
	}
}

// finish runs and publishes a claimed issue. From the claim on, every failure
// ends in a comment and Blocked (EMBER-177), so an issue never stays In progress
// with nobody working on it. A stop leaves it In progress: the next start's
// recovery moves it to Blocked with a note.
//
// A Run error wrapping ErrNotStarted (a failure before the first agent turn)
// puts the issue back in To do without a comment, logs it, and tells the
// caller to wait before the next claim. It is not a slot failure (decision
// 2026-11-07-publish-resilience).
// strike is how many times in a row this issue has then failed to start;
// wait is the wait the caller will take next, logged as retry_in.
// Returns whether cycle should wait (ErrNotStarted was handled).
func (s *Scheduler) finish(ctx context.Context, slot Slot, issue cinderline.Issue, strike int, wait time.Duration) (notStarted bool) {
	c, h := slot.Client, slot.Hooks
	err := s.step(slot, issue, func() error { return h.Run(ctx, c, issue) })
	// Phase 9's gate goes here, between run and publish (plan section 5); nothing yet.
	if err == nil && h.Publish != nil {
		err = s.step(slot, issue, func() error { return h.Publish(ctx, c, issue) })
	}
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, ErrNotStarted) {
		ref := issue.Ref
		s.Log.Warn("not started, back in To do",
			"project", slot.Project,
			"issue", ref,
			"err", err,
			"retry_in", wait)
		// Through the outbox, so an outage at the end of a run never leaves
		// the issue In progress (CSR-159).
		if strike == 3 {
			c.Send(ctx, cinderline.Write{Ref: ref, Comment: "The run could not start three times in a row; the last error: " + err.Error() + ". The issue stays in To do and the runner tries again."})
		}
		c.Send(ctx, cinderline.Write{Ref: ref, Move: &cinderline.UpdateIssueRequest{Status: "todo", ExpectedStatus: "in_progress"}})
		return true
	}
	s.Log.Error("run failed", "project", slot.Project, "issue", issue.Ref, "err", err)
	c.Send(ctx, cinderline.Write{Ref: issue.Ref, Comment: "The run stopped with an error: " + err.Error()})
	// Refused when the issue is no longer In progress, for example after a question (decision 0116).
	// Blocked, not Review: a failed run has nothing to review and waits for a person (EMBER-177).
	c.Send(ctx, cinderline.Write{Ref: issue.Ref, Move: &cinderline.UpdateIssueRequest{Status: "blocked", ExpectedStatus: "in_progress"}})
	return false
}

// step calls f and turns a panic into an error, logging its stack, so a
// panicking hook still ends its issue in Blocked.
func (s *Scheduler) step(slot Slot, issue cinderline.Issue, f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.Log.Error("panic", "project", slot.Project, "issue", issue.Ref, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return f()
}

// timers calls ReviewBranches and MergeCheck until work is done, beside the
// work loop, so a long run does not hold them up; one at a time, as before.
// Each waits the interval the app sent with its last answer, else its
// default, with jitter, measured from the end of its call before (CSR-174):
// a changed interval applies to the next wait. A failed call keeps the
// interval of the last successful answer.
func (s *Scheduler) timers(work context.Context, slot Slot) {
	wait := func(p cinderline.Poll, fallback time.Duration) time.Duration {
		if d, ok := slot.Client.PollAfter(p); ok {
			return cinderline.Jitter(d)
		}
		return cinderline.Jitter(fallback)
	}
	review := time.NewTimer(wait(cinderline.PollReview, s.ReviewInterval))
	merge := time.NewTimer(wait(cinderline.PollMerge, s.MergeInterval))
	defer review.Stop()
	defer merge.Stop()
	for {
		select {
		case <-work.Done():
			return
		case <-review.C:
			s.tick(work, slot, "review branches failed", slot.Hooks.ReviewBranches)
			review.Reset(wait(cinderline.PollReview, s.ReviewInterval))
		case <-merge.C:
			s.tick(work, slot, "merge check failed", slot.Hooks.MergeCheck)
			merge.Reset(wait(cinderline.PollMerge, s.MergeInterval))
		}
	}
}

// tick calls hook, if any, and logs its error or panic; neither stops the timers.
func (s *Scheduler) tick(work context.Context, slot Slot, msg string, hook func(context.Context, *cinderline.Client) error) {
	if hook == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			s.Log.Warn(msg, "project", slot.Project, "err", fmt.Sprintf("panic: %v\n%s", p, debug.Stack()))
		}
	}()
	if err := hook(work, slot.Client); err != nil && work.Err() == nil {
		s.Log.Warn(msg, "project", slot.Project, "err", err)
	}
}
