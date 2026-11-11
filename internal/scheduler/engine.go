package scheduler

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/logs"
	"github.com/crossoverJie/botler-agent/internal/monitor"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// FireResult is what the dispatch pipeline reports back to the engine.
//
// The engine deliberately does NOT import internal/dispatch: dispatch reaches the schedule tool
// through runner → tools, and the tool imports this package, so a direct import would form a
// cycle. The dispatch package registers itself through SetDispatch instead (the usual Go
// "driver registers itself" pattern), which keeps the dependency one-way.
type FireResult struct {
	Text   string
	Images []string
	// Status is one of the task-log statuses ("success" / "auto-fixed" / "validation-failed" /
	// "error" / "duplicate" / "unknown-project").
	Status string
}

// FireFunc runs a message-type entry through the LLM pipeline.
type FireFunc func(ctx context.Context, id, message, projectHint string) FireResult

// ScriptFireFunc runs a script-type entry directly (bypassing the LLM).
type ScriptFireFunc func(ctx context.Context, id, script string, args []string, commit *bool) FireResult

var (
	dispatchMu    sync.RWMutex
	fireMessageFn FireFunc
	fireScriptFn  ScriptFireFunc
)

// SetDispatch installs the dispatch pipeline used to fire entries. The dispatch package calls it
// from its own init, so any binary that links both packages is wired automatically.
func SetDispatch(message FireFunc, script ScriptFireFunc) {
	dispatchMu.Lock()
	fireMessageFn = message
	fireScriptFn = script
	dispatchMu.Unlock()
}

func fireMessage(ctx context.Context, id, message, projectHint string) (FireResult, bool) {
	dispatchMu.RLock()
	fn := fireMessageFn
	dispatchMu.RUnlock()
	if fn == nil {
		return FireResult{}, false
	}
	return fn(ctx, id, message, projectHint), true
}

func fireScript(ctx context.Context, id, script string, args []string, commit *bool) (FireResult, bool) {
	dispatchMu.RLock()
	fn := fireScriptFn
	dispatchMu.RUnlock()
	if fn == nil {
		return FireResult{}, false
	}
	return fn(ctx, id, script, args, commit), true
}

// Poll-delay constants (v1: src/scheduler/poll.ts).
const (
	// IdlePollMs is the wait when nothing is scheduled.
	IdlePollMs int64 = 60_000
	// MinSleepMs is the floor of a poll wait.
	MinSleepMs int64 = 1_000
	// MaxSleepMs caps a single wait (~23.1 days) so a far-future `once` entry cannot spin the loop
	// and a timer overflow cannot collapse the wait to ~0.
	MaxSleepMs int64 = 2_000_000_000
)

// PollDelay returns the delay before the next scheduler round.
func PollDelay(soonest, now int64) time.Duration {
	raw := IdlePollMs
	if soonest != NoFire {
		raw = soonest - now
	}
	if raw > MaxSleepMs {
		raw = MaxSleepMs
	}
	if raw < MinSleepMs {
		raw = MinSleepMs
	}
	return time.Duration(raw) * time.Millisecond
}

// nowMs is the wall clock in epoch milliseconds (kept as a function so tests can reason about it).
func nowMs() int64 { return time.Now().UnixMilli() }

// Engine is the in-process scheduler. It only triggers work: all execution, dedup, queueing,
// validation, commit and logging is the dispatch pipeline, reused verbatim.
type Engine struct {
	mu         sync.Mutex
	lastMtime  int64
	wake       chan struct{}
	stop       chan struct{}
	stopOnce   sync.Once
	lastFire   map[string]int64
	lastFireAt int64
	lastFireID string
	// nextFireAt mirrors the loop's current "soonest" so a fire from a retry goroutine does not
	// blank the gauge while the loop is asleep until tomorrow.
	nextFireAt       int64
	schedulesTotal   int
	schedulesEnabled int
}

var (
	currentMu sync.RWMutex
	current   *Engine
)

// Current returns the running engine, if any.
func Current() *Engine {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return current
}

// Start starts the scheduler. It is a no-op unless SCHEDULER_ENABLED=1. The holiday calendar is
// warmed at startup (blocking only when the cache lacks the current/next year), refreshed every
// 24h, and a save of either schedules.json or the calendar wakes the loop immediately.
func Start(ctx context.Context) error {
	if !config.C().SchedulerEnabled {
		infof("scheduler disabled (SCHEDULER_ENABLED != 1)")
		return nil
	}
	e := &Engine{
		lastMtime: -1,
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		lastFire:  map[string]int64{},
	}
	currentMu.Lock()
	current = e
	currentMu.Unlock()
	// A schedule or calendar write wakes the loop immediately (one-way: store never imports engine).
	SetSavedListener(e.Reload)
	SetHolidaysSavedListener(e.Reload)
	infof("started (config: %s)", config.C().SchedulesFile)

	year := time.Now().Year()
	cached := LoadHolidays()
	_, hasThis := cached[strconv.Itoa(year)]
	_, hasNext := cached[strconv.Itoa(year+1)]
	if !hasThis || !hasNext {
		// Blocking warm-up is bounded (~2 × 10s) and only happens with an incomplete cache.
		EnsureHolidays(ctx)
	} else {
		go EnsureHolidays(ctx)
	}
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.stop:
				return
			case <-ticker.C:
				EnsureHolidays(ctx)
			}
		}
	}()
	go e.loop(ctx)
	return nil
}

// Stop stops the firing loop (idempotent).
func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.stop) })
}

// Reload wakes the loop so it re-reads schedules.json / the holiday calendar immediately.
func (e *Engine) Reload() {
	e.mu.Lock()
	e.lastMtime = -1
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Reload is the package-level convenience used by the WebUI after a config restore.
func Reload() {
	if e := Current(); e != nil {
		e.Reload()
	}
}

func (e *Engine) fileMtime() int64 {
	st, err := os.Stat(config.C().SchedulesFile)
	if err != nil {
		return 0 // missing file → treat as "no file", never fail
	}
	return st.ModTime().UnixMilli()
}

func (e *Engine) configChanged() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fileMtime() != e.lastMtime
}

func (e *Engine) markMtime() {
	e.mu.Lock()
	e.lastMtime = e.fileMtime()
	e.mu.Unlock()
}

// watermarkOf returns the instant the entry's next fire is measured from (its last fire, or when
// it was first seen). A first sighting starts at `now` = no catch-up.
func (e *Engine) watermarkOf(id string, now int64) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if prev, ok := e.lastFire[id]; ok {
		return prev
	}
	e.lastFire[id] = now
	return now
}

func (e *Engine) setWatermark(id string, ts int64) {
	e.mu.Lock()
	e.lastFire[id] = ts
	e.mu.Unlock()
}

// pruneWatermarks forgets entries that are gone or disabled, so re-enabling never back-fires.
func (e *Engine) pruneWatermarks(entries []Schedule) {
	live := make(map[string]bool, len(entries))
	for _, s := range entries {
		live[s.ID] = true
	}
	e.mu.Lock()
	for id := range e.lastFire {
		if !live[id] {
			delete(e.lastFire, id)
		}
	}
	e.mu.Unlock()
}

func (e *Engine) markFired(id string, ts int64) {
	e.mu.Lock()
	e.lastFireAt = ts
	e.lastFireID = id
	total, enabled, next := e.schedulesTotal, e.schedulesEnabled, e.nextFireAt
	e.mu.Unlock()
	monitor.SetSchedulerSnapshot(total, enabled, next, ts, id)
}

// sleep waits for ms, returning early when the loop is woken (config write) or stopped.
func (e *Engine) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-e.stop:
	case <-e.wake:
	case <-timer.C:
	}
}

// safeNextFire wraps NextFireEpoch so a single bad entry can never take down the loop.
func safeNextFire(s Schedule, from int64) (next int64) {
	defer func() {
		if r := recover(); r != nil {
			warnf("entry %q failed: %v", s.ID, r)
			next = NoFire
		}
	}()
	next, err := NextFireEpoch(s, from)
	if err != nil {
		warnf("entry %q failed: %v", s.ID, err)
		return NoFire
	}
	return next
}

func (e *Engine) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stop:
			return
		default:
		}

		if e.configChanged() {
			e.markMtime()
		}
		all, _ := LoadSchedules()
		entries := make([]Schedule, 0, len(all))
		for _, s := range all {
			if s.Enabled {
				entries = append(entries, s)
			}
		}
		e.pruneWatermarks(entries)
		e.mu.Lock()
		e.schedulesTotal = len(all)
		e.schedulesEnabled = len(entries)
		e.mu.Unlock()

		soonest := NoFire
		for _, s := range entries {
			now := time.Now().UnixMilli()
			next := safeNextFire(s, e.watermarkOf(s.ID, now))
			if next <= now {
				// Advance the watermark before firing: dispatch is awaited and can take a while,
				// and the next fire must be measured from this point.
				e.setWatermark(s.ID, now)
				e.fire(ctx, s, 0)
				next = safeNextFire(s, time.Now().UnixMilli())
			}
			if next < soonest {
				soonest = next
			}
		}

		e.mu.Lock()
		total, enabled, lastAt, lastID := e.schedulesTotal, e.schedulesEnabled, e.lastFireAt, e.lastFireID
		e.mu.Unlock()
		nextFireAt := int64(0)
		if soonest != NoFire {
			nextFireAt = soonest
		}
		e.mu.Lock()
		e.nextFireAt = nextFireAt
		e.mu.Unlock()
		monitor.SetSchedulerSnapshot(total, enabled, nextFireAt, lastAt, lastID)

		e.sleep(ctx, PollDelay(soonest, time.Now().UnixMilli()))
	}
}

// retrySignature captures the execution-relevant config of an entry: a pending retry is cancelled
// if the action / trigger / project changed during the backoff window.
func retrySignature(s Schedule) string {
	action := "message:" + s.Message
	if s.Script != "" {
		action = "script:" + s.Script + "|args:" + strings.Join(s.ScriptArgs, "\x00")
	}
	return action + "|trigger:" + s.Trigger() + "|project:" + s.Project
}

// fire runs one entry. Retries are handed to a timer so they never block the single-threaded round.
func (e *Engine) fire(ctx context.Context, s Schedule, attempt int) {
	// v1 wraps every entry in try/catch ("A single bad entry must never take down the whole
	// loop"); the retry goroutine needs the same guard.
	defer func() {
		if r := recover(); r != nil {
			warnf("[scheduler] entry %q panicked: %v", s.ID, r)
		}
	}()
	id := fmt.Sprintf("schedule:%s:%d", s.ID, time.Now().UnixMilli())
	var res FireResult
	var ok bool
	if s.Script != "" {
		res, ok = fireScript(ctx, id, s.Script, s.ScriptArgs, s.Commit)
	} else {
		res, ok = fireMessage(ctx, id, s.Message, s.Project)
	}
	if !ok {
		warnf("%s not fired: dispatch pipeline is not wired (import internal/dispatch)", s.ID)
		return
	}

	// A script task must retry only on `error`: re-running on `validation-failed` would repeat its
	// side effects. Message tasks retry on both (the LLM self-heals).
	failed := logs.TaskStatus(res.Status) == logs.StatusError ||
		(logs.TaskStatus(res.Status) == logs.StatusValidationFailed && s.Script == "")
	retryMax := 0
	var backoffMs int64
	if s.Retry != nil {
		retryMax = s.Retry.Max
		backoffMs = s.Retry.BackoffMs
	}
	if failed && attempt < retryMax {
		sig := retrySignature(s)
		entryID := s.ID
		go func() {
			timer := time.NewTimer(time.Duration(backoffMs) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-e.stop:
				return
			case <-timer.C:
			}
			cur, ok := FindSchedule(entryID)
			if !ok || !cur.Enabled {
				return
			}
			if retrySignature(cur) != sig {
				infof("%s retry cancelled: entry config changed during backoff", entryID)
				return
			}
			e.fire(ctx, cur, attempt+1)
		}()
		return
	}

	// A no-project reminder has no data subproject to route to, so routing returns the generic
	// "couldn't decide a project" text; for such entries the schedule's own message IS the reminder.
	text := res.Text
	if logs.TaskStatus(res.Status) == logs.StatusUnknownProject && s.Message != "" {
		text = s.Message
	}
	if s.Recipient != nil {
		r := push.Deliver(push.Payload{Text: text, Images: res.Images}, *s.Recipient)
		if !r.OK {
			warnf("%s push failed: %s", s.ID, r.Error)
		}
	}
	e.markFired(s.ID, time.Now().UnixMilli())
	infof("%s -> %s", s.ID, res.Status)
}
