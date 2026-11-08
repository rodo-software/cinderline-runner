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
	"sync"
	"time"

	"github.com/rodo-software/cinderline-runner/internal/cinderline"
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
	ReviewInterval time.Duration // default 5 minutes; a placeholder M3 settles
	MergeInterval  time.Duration // default 60 seconds; the daemon sets config.json's merge_poll_seconds (legacy MERGE_POLL_SECONDS)
	MaxBackoff     time.Duration // default 60 seconds; the first wait after a failure is 1s or this, if less
	StartBackoff   time.Duration // default 60 seconds: the wait before the next claim after a run that did not start (ErrNotStarted); doubles per failure in a row, up to 10 minutes

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
func (s *Scheduler) slot(ctx context.Context, work context.Context, slot Slot) {
	backoff := min(time.Second, s.MaxBackoff)
	for {
		polled, err := s.cycle(ctx, work, slot)
		if ctx.Err() != nil || work.Err() != nil {
			s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = "" })
			return
		}
		s.setStatus(slot.Project, func(st *ProjectStatus) {
			st.Connected, st.Issue, st.LastError, st.LastErrorAt = false, "", err.Error(), time.Now()
		})
		if polled {
			backoff = min(time.Second, s.MaxBackoff)
		}
		s.Log.Error("slot failed", "project", slot.Project, "err", err, "retry_in", backoff)
		select {
		case <-work.Done():
			return
		case <-time.After(backoff):
		}
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
	if h.Recover != nil {
		if err := h.Recover(work, c); err != nil {
			return false, fmt.Errorf("recover: %w", err)
		}
	}
	// Work names only the first To do issue, so while that one is skipped
	// every poll, changed or not, walks the board again (legacy parity).
	stuck := false
	var strikes struct {
		issue string
		n     int
		wait  time.Duration
	}
	strikes.wait = s.StartBackoff
	for work.Err() == nil {
		s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = "" })
		issue, err := c.Work(work)
		noChange := errors.Is(err, cinderline.ErrNoChange)
		if err != nil && !noChange {
			return polled, err
		}
		polled = true
		s.setStatus(slot.Project, func(st *ProjectStatus) {
			st.Connected, st.LastPoll, st.LastError, st.LastErrorAt = true, time.Now(), "", time.Time{}
		})
		if !noChange && issue == nil {
			stuck = false
			continue // no work
		}
		if noChange && !stuck {
			continue // the long-poll already waited
		}
		if h.Run == nil {
			s.Log.Info("no run step yet, skipping", "project", slot.Project, "issue", issue.Ref)
			continue // not claimed, so it stays To do; the ETag keeps this from spinning
		}
		claimed, err := s.claimNext(work, slot, issue)
		if err != nil {
			return polled, err
		}
		if stuck = claimed == nil; stuck {
			continue
		}
		s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = claimed.Ref })
		if strikes.issue != claimed.Ref {
			strikes.issue, strikes.n = claimed.Ref, 0
		}
		if s.finish(ctx, slot, *claimed, strikes.n+1, strikes.wait) {
			strikes.n++
			s.setStatus(slot.Project, func(st *ProjectStatus) { st.Issue = "" })
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
	}
	var queue []cinderline.Issue
	if first != nil {
		queue = append(queue, *first)
	}
	tried, listed := map[string]bool{}, false
	for {
		if len(queue) == 0 {
			if listed {
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
				s.Log.Warn("skipped before the claim", "project", slot.Project, "issue", issue.Ref, "err", err)
				continue
			}
		}
		var conflict *cinderline.ConflictError
		if _, err := c.ClaimIssue(work, issue.Ref, "todo"); errors.As(err, &conflict) {
			s.Log.Info("claim conflict, moving on", "project", slot.Project, "issue", issue.Ref, "status", conflict.Status)
			continue
		} else if err != nil {
			if work.Err() != nil {
				return nil, work.Err()
			}
			s.Log.Warn("not claimed", "project", slot.Project, "issue", issue.Ref, "err", err)
			continue
		}
		return &issue, nil
	}
}

// finish runs and publishes a claimed issue. From the claim on, every failure
// ends in a comment and Review (plan M3), so an issue never stays In progress
// with nobody working on it. A stop leaves it In progress: the next start's
// recovery puts it back in To do.
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
		if strike == 3 {
			if _, cerr := c.AddComment(ctx, ref, "The run could not start three times in a row; the last error: "+err.Error()+". The issue stays in To do and the runner tries again."); cerr != nil {
				s.Log.Error("failure comment not added", "project", slot.Project, "issue", ref, "err", cerr)
			}
		}
		todo := cinderline.UpdateIssueRequest{Status: "todo", ExpectedStatus: "in_progress"}
		if _, cerr := c.UpdateIssue(ctx, ref, todo); cerr != nil {
			s.Log.Error("not moved to To do", "project", slot.Project, "issue", ref, "err", cerr)
		}
		return true
	}
	s.Log.Error("run failed", "project", slot.Project, "issue", issue.Ref, "err", err)
	if _, cerr := c.AddComment(ctx, issue.Ref, "The run stopped with an error: "+err.Error()); cerr != nil {
		s.Log.Error("failure comment not added", "project", slot.Project, "issue", issue.Ref, "err", cerr)
	}
	// Refused when the issue is no longer In progress, for example after a question (decision 0116).
	review := cinderline.UpdateIssueRequest{Status: "review", ExpectedStatus: "in_progress"}
	if _, cerr := c.UpdateIssue(ctx, issue.Ref, review); cerr != nil {
		s.Log.Error("not moved to Review", "project", slot.Project, "issue", issue.Ref, "err", cerr)
	}
	return false
}

// step calls f and turns a panic into an error, logging its stack, so a
// panicking hook still ends its issue in Review.
func (s *Scheduler) step(slot Slot, issue cinderline.Issue, f func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.Log.Error("panic", "project", slot.Project, "issue", issue.Ref, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return f()
}

// timers calls ReviewBranches and MergeCheck on their tickers until work is
// done, beside the work loop, so a long run does not hold them up.
func (s *Scheduler) timers(work context.Context, slot Slot) {
	review, merge := time.NewTicker(s.ReviewInterval), time.NewTicker(s.MergeInterval)
	defer review.Stop()
	defer merge.Stop()
	for {
		select {
		case <-work.Done():
			return
		case <-review.C:
			s.tick(work, slot, "review branches failed", slot.Hooks.ReviewBranches)
		case <-merge.C:
			s.tick(work, slot, "merge check failed", slot.Hooks.MergeCheck)
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
