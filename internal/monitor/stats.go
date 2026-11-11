// Package monitor holds the in-process observability state: process counters written by the
// dispatcher / runner / scheduler / channels and read only by the health endpoints.
//
// It is deliberately tiny and dependency-free: monitoring must never become a failure source for
// the agent. All helpers are O(1) and self-clamping.
package monitor

import (
	"sync"
	"time"
)

// ChannelState is a per-channel liveness snapshot.
type ChannelState struct {
	Up          bool   `json:"up"`
	FirstUpAt   int64  `json:"firstUpAt,omitempty"`
	LastUpAt    int64  `json:"lastUpAt,omitempty"`
	LastErrorAt int64  `json:"lastErrorAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

// Stats is the snapshot exposed by /healthz.
type Stats struct {
	StartedAt int64 `json:"startedAt"`

	QueueDepth             int              `json:"queueDepth"`
	ActiveTasks            int              `json:"activeTasks"`
	ActiveAgents           int              `json:"activeAgents"`
	TotalDispatched        int64            `json:"totalDispatched"`
	DuplicatesTotal        int64            `json:"duplicatesTotal"`
	TotalFailed            int64            `json:"totalFailed"`
	StatusCounts           map[string]int64 `json:"statusCounts"`
	LastDispatchAt         int64            `json:"-"`
	LastDispatchDurationMs int64            `json:"-"`

	ModelCacheQueries int64 `json:"-"`
	ModelCacheHits    int64 `json:"-"`

	SchedulesTotal   int    `json:"schedulesTotal"`
	SchedulesEnabled int    `json:"schedulesEnabled"`
	NextFireAt       int64  `json:"-"`
	LastFireAt       int64  `json:"-"`
	LastFireID       string `json:"-"`

	Channels map[string]ChannelState `json:"channels"`
}

// Sample is one point of the rolling history buffer.
type Sample struct {
	TS          int64  `json:"ts"`
	RSS         uint64 `json:"rss"`
	HeapUsed    uint64 `json:"heapUsed"`
	QueueDepth  int    `json:"queueDepth"`
	ActiveTasks int    `json:"activeTasks"`
	LagP50      int    `json:"lagP50"`
	LagP99      int    `json:"lagP99"`
}

// HistoryCapacity is ~1 hour at 5s sampling.
const HistoryCapacity = 720

var (
	mu        sync.Mutex
	startedAt = time.Now().UnixMilli()
	state     = newState()
	history   []Sample
)

func newState() *Stats {
	return &Stats{StartedAt: startedAt, StatusCounts: map[string]int64{}, Channels: map[string]ChannelState{}}
}

// StartedAtMs is the single process-start timestamp shared by both HTTP exits.
func StartedAtMs() int64 { return startedAt }

// MarkEnqueued records a task joining the serial queue.
func MarkEnqueued() {
	mu.Lock()
	defer mu.Unlock()
	state.QueueDepth++
}

// MarkStarted records a queued task entering the active slot.
func MarkStarted() {
	mu.Lock()
	defer mu.Unlock()
	if state.QueueDepth > 0 {
		state.QueueDepth--
	}
	state.ActiveTasks++
}

// MarkFinished records the active task finishing.
func MarkFinished() {
	mu.Lock()
	defer mu.Unlock()
	if state.ActiveTasks > 0 {
		state.ActiveTasks--
	}
}

// RecordStatus buckets a finished task by its final status.
func RecordStatus(status string) {
	mu.Lock()
	defer mu.Unlock()
	state.StatusCounts[status]++
}

// MarkDuplicate records a message dropped by the dedup window.
func MarkDuplicate() {
	mu.Lock()
	defer mu.Unlock()
	state.DuplicatesTotal++
}

// MarkDispatched records a dispatch that entered the execution chain.
func MarkDispatched() {
	mu.Lock()
	defer mu.Unlock()
	state.TotalDispatched++
	state.LastDispatchAt = time.Now().UnixMilli()
}

// MarkFailed records an error / validation-failed task.
func MarkFailed() {
	mu.Lock()
	defer mu.Unlock()
	state.TotalFailed++
}

// MarkModelCache records a model-resolution query (hit or miss).
func MarkModelCache(hit bool) {
	mu.Lock()
	defer mu.Unlock()
	state.ModelCacheQueries++
	if hit {
		state.ModelCacheHits++
	}
}

// MarkAgentStart records an in-flight agent run.
func MarkAgentStart() {
	mu.Lock()
	defer mu.Unlock()
	state.ActiveAgents++
}

// MarkAgentEnd records an agent run finishing.
func MarkAgentEnd() {
	mu.Lock()
	defer mu.Unlock()
	if state.ActiveAgents > 0 {
		state.ActiveAgents--
	}
}

// SetLastDispatchDuration records how long the last dispatch took.
func SetLastDispatchDuration(ms int64) {
	mu.Lock()
	defer mu.Unlock()
	state.LastDispatchDurationMs = ms
}

// SetChannelUp marks a channel as serving (firstUpAt is never reset).
func SetChannelUp(name string) {
	mu.Lock()
	defer mu.Unlock()
	prev := state.Channels[name]
	now := time.Now().UnixMilli()
	if prev.FirstUpAt == 0 {
		prev.FirstUpAt = now
	}
	prev.Up = true
	prev.LastUpAt = now
	state.Channels[name] = prev
}

// SetChannelDown marks a channel as failed.
func SetChannelDown(name string, err error) {
	mu.Lock()
	defer mu.Unlock()
	prev := state.Channels[name]
	prev.Up = false
	prev.LastErrorAt = time.Now().UnixMilli()
	if err != nil {
		prev.LastError = err.Error()
	}
	state.Channels[name] = prev
}

// SetSchedulerSnapshot records the scheduler gauges.
func SetSchedulerSnapshot(total, enabled int, nextFireAt, lastFireAt int64, lastFireID string) {
	mu.Lock()
	defer mu.Unlock()
	state.SchedulesTotal = total
	state.SchedulesEnabled = enabled
	state.NextFireAt = nextFireAt
	state.LastFireAt = lastFireAt
	state.LastFireID = lastFireID
}

// StatsSnapshot returns a copy of the counters (safe to serialize outside the lock).
func StatsSnapshot() Stats {
	mu.Lock()
	defer mu.Unlock()
	out := *state
	out.StatusCounts = make(map[string]int64, len(state.StatusCounts))
	for k, v := range state.StatusCounts {
		out.StatusCounts[k] = v
	}
	out.Channels = make(map[string]ChannelState, len(state.Channels))
	for k, v := range state.Channels {
		out.Channels[k] = v
	}
	return out
}

// PushSample appends a pre-assembled sample to the ring buffer.
func PushSample(s Sample) {
	mu.Lock()
	defer mu.Unlock()
	history = append(history, s)
	if len(history) > HistoryCapacity {
		history = history[len(history)-HistoryCapacity:]
	}
}

// History returns a copy of the rolling history samples.
func History() []Sample {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Sample, len(history))
	copy(out, history)
	return out
}

// ModelCacheSnapshot returns the model-resolution cache counters.
func ModelCacheSnapshot() (queries, hits int64) {
	mu.Lock()
	defer mu.Unlock()
	return state.ModelCacheQueries, state.ModelCacheHits
}
