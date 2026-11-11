package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// sampleInterval is the history sampling period (v1 parity).
const sampleInterval = 5 * time.Second

// Memory mirrors v1's process.memoryUsage() block.
type Memory struct {
	RSS          uint64 `json:"rss"`
	HeapUsed     uint64 `json:"heapUsed"`
	HeapTotal    uint64 `json:"heapTotal"`
	External     uint64 `json:"external"`
	ArrayBuffers uint64 `json:"arrayBuffers"`
}

// Lag mirrors v1's eventLoopLagMs block. Go has no event loop, so phase one reports zeros
// (documented deviation, removed in phase two).
type Lag struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

// ModelCache mirrors the model-resolution cache block.
type ModelCache struct {
	Queries int64   `json:"queries"`
	Hits    int64   `json:"hits"`
	HitRate float64 `json:"hitRate"`
}

// Snapshot is the /healthz payload.
type Snapshot struct {
	OK               bool                    `json:"ok"`
	PID              int                     `json:"pid"`
	UptimeSec        int64                   `json:"uptimeSec"`
	StartedAt        int64                   `json:"startedAt"`
	Memory           Memory                  `json:"memory"`
	EventLoopLagMs   Lag                     `json:"eventLoopLagMs"`
	QueueDepth       int                     `json:"queueDepth"`
	ActiveTasks      int                     `json:"activeTasks"`
	ActiveAgents     int                     `json:"activeAgents"`
	TotalDispatched  int64                   `json:"totalDispatched"`
	DuplicatesTotal  int64                   `json:"duplicatesTotal"`
	TotalFailed      int64                   `json:"totalFailed"`
	StatusCounts     map[string]int64        `json:"statusCounts"`
	ModelCache       ModelCache              `json:"modelCache"`
	SchedulesTotal   int                     `json:"schedulesTotal"`
	SchedulesEnabled int                     `json:"schedulesEnabled"`
	NextFireAtSec    int64                   `json:"nextFireAtSec"`
	LastFireAtSec    int64                   `json:"lastFireAtSec"`
	LastFireID       string                  `json:"lastFireId"`
	Channels         map[string]ChannelState `json:"channels"`
}

// memoryUsage returns a memory snapshot. RSS comes from /proc/self/statm on Linux and falls back
// to MemStats.Sys elsewhere; heap* are always Go runtime numbers.
func memoryUsage() Memory {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss := uint64(0)
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				rss = pages * uint64(os.Getpagesize())
			}
		}
	}
	if rss == 0 {
		rss = ms.Sys
	}
	external := uint64(0)
	if ms.Sys > ms.HeapSys {
		external = ms.Sys - ms.HeapSys
	}
	return Memory{
		RSS:          rss,
		HeapUsed:     ms.HeapAlloc,
		HeapTotal:    ms.HeapSys,
		External:     external,
		ArrayBuffers: 0,
	}
}

// BuildSnapshot assembles the instantaneous process snapshot shared by both HTTP exits.
func BuildSnapshot() Snapshot {
	var s Snapshot
	s.OK = true
	s.PID = os.Getpid()
	s.UptimeSec = int64(time.Since(time.UnixMilli(StartedAtMs())).Seconds())
	s.StartedAt = StartedAtMs()
	s.Memory = memoryUsage()
	s.EventLoopLagMs = Lag{}

	mu.Lock()
	s.QueueDepth = state.QueueDepth
	s.ActiveTasks = state.ActiveTasks
	s.ActiveAgents = state.ActiveAgents
	s.TotalDispatched = state.TotalDispatched
	s.DuplicatesTotal = state.DuplicatesTotal
	s.TotalFailed = state.TotalFailed
	s.StatusCounts = make(map[string]int64, len(state.StatusCounts))
	for k, v := range state.StatusCounts {
		s.StatusCounts[k] = v
	}
	s.ModelCache = ModelCache{Queries: state.ModelCacheQueries, Hits: state.ModelCacheHits}
	if state.ModelCacheQueries > 0 {
		s.ModelCache.HitRate = float64(state.ModelCacheHits) / float64(state.ModelCacheQueries)
	}
	s.SchedulesTotal = state.SchedulesTotal
	s.SchedulesEnabled = state.SchedulesEnabled
	if state.NextFireAt > 0 {
		s.NextFireAtSec = roundSec(state.NextFireAt)
	}
	if state.LastFireAt > 0 {
		s.LastFireAtSec = roundSec(state.LastFireAt)
	}
	s.LastFireID = state.LastFireID
	s.Channels = make(map[string]ChannelState, len(state.Channels))
	for k, v := range state.Channels {
		s.Channels[k] = v
	}
	mu.Unlock()
	return s
}

func roundSec(ms int64) int64 { return (ms + 500) / 1000 }

func emitMetric(b *strings.Builder, name, kind, help, value string, labels string) {
	b.WriteString("# HELP " + name + " " + help + "\n")
	b.WriteString("# TYPE " + name + " " + kind + "\n")
	b.WriteString(name + labels + " " + value + "\n")
}

// BuildMetrics renders the Prometheus text exposition. Metric names are exactly v1's
// (src/monitor/health.ts); writing one of them short would break existing alert rules.
func BuildMetrics() string {
	s := BuildSnapshot()
	var b strings.Builder
	emitMetric(&b, "botler_uptime_seconds", "gauge", "Process uptime in seconds", strconv.FormatInt(s.UptimeSec, 10), "")
	emitMetric(&b, "botler_memory_rss_bytes", "gauge", "Resident set size in bytes", u(s.Memory.RSS), "")
	emitMetric(&b, "botler_memory_heap_used_bytes", "gauge", "Go heap used in bytes", u(s.Memory.HeapUsed), "")
	emitMetric(&b, "botler_memory_heap_total_bytes", "gauge", "Go heap total in bytes", u(s.Memory.HeapTotal), "")
	emitMetric(&b, "botler_memory_external_bytes", "gauge", "External memory in bytes", u(s.Memory.External), "")
	emitMetric(&b, "botler_memory_array_buffers_bytes", "gauge", "ArrayBuffer memory in bytes", "0", "")
	emitMetric(&b, "botler_event_loop_lag_p50_ms", "gauge", "Event loop lag p50 in ms", "0", "")
	emitMetric(&b, "botler_event_loop_lag_p90_ms", "gauge", "Event loop lag p90 in ms", "0", "")
	emitMetric(&b, "botler_event_loop_lag_p99_ms", "gauge", "Event loop lag p99 in ms", "0", "")
	emitMetric(&b, "botler_queue_depth", "gauge", "Tasks queued awaiting execution", strconv.Itoa(s.QueueDepth), "")
	emitMetric(&b, "botler_active_tasks", "gauge", "Tasks currently executing", strconv.Itoa(s.ActiveTasks), "")
	emitMetric(&b, "botler_active_agents", "gauge", "Agent prompts in flight", strconv.Itoa(s.ActiveAgents), "")
	emitMetric(&b, "botler_dispatches_total", "counter", "Non-duplicate dispatches", strconv.FormatInt(s.TotalDispatched, 10), "")
	emitMetric(&b, "botler_duplicates_total", "counter", "Messages dropped by dedup", strconv.FormatInt(s.DuplicatesTotal, 10), "")
	emitMetric(&b, "botler_failures_total", "counter", "Failed tasks (error + validation-failed)", strconv.FormatInt(s.TotalFailed, 10), "")
	emitMetric(&b, "botler_model_cache_queries_total", "counter", "Model resolution cache queries", strconv.FormatInt(s.ModelCache.Queries, 10), "")
	emitMetric(&b, "botler_model_cache_hits_total", "counter", "Model resolution cache hits", strconv.FormatInt(s.ModelCache.Hits, 10), "")
	for status, n := range s.StatusCounts {
		emitMetric(&b, "botler_tasks_total", "counter", "Tasks by final status", strconv.FormatInt(n, 10), `{status="`+status+`"}`)
	}
	emitMetric(&b, "botler_schedules_total", "gauge", "Total schedule entries", strconv.Itoa(s.SchedulesTotal), "")
	emitMetric(&b, "botler_schedules_enabled_total", "gauge", "Enabled schedule entries", strconv.Itoa(s.SchedulesEnabled), "")
	emitMetric(&b, "botler_scheduler_next_fire_epoch_seconds", "gauge", "Epoch seconds of next scheduled fire (0 = none)", strconv.FormatInt(s.NextFireAtSec, 10), "")
	emitMetric(&b, "botler_scheduler_last_fire_epoch_seconds", "gauge", "Epoch seconds of last scheduled fire (0 = none)", strconv.FormatInt(s.LastFireAtSec, 10), "")
	for name, c := range s.Channels {
		v := "0"
		if c.Up {
			v = "1"
		}
		emitMetric(&b, "botler_channel_up", "gauge", "Channel online (1) or down (0)", v, `{channel="`+name+`"}`)
	}
	return b.String()
}

func u(v uint64) string { return strconv.FormatUint(v, 10) }

// HistoryPayload is the /healthz/history body.
type HistoryPayload struct {
	IntervalSec int      `json:"intervalSec"`
	Capacity    int      `json:"capacity"`
	Samples     []Sample `json:"samples"`
}

// GetHistory returns the rolling trend payload.
func GetHistory() HistoryPayload {
	return HistoryPayload{IntervalSec: int(sampleInterval / time.Second), Capacity: HistoryCapacity, Samples: History()}
}

var samplingOnce sync.Once

// EnsureSampling starts the 5s history sampler exactly once. It is invoked by both the 8899
// server and the WebUI server, so the WebUI monitor view keeps real data when the port is off.
func EnsureSampling() {
	samplingOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(sampleInterval)
			defer ticker.Stop()
			for range ticker.C {
				s := BuildSnapshot()
				PushSample(Sample{
					TS:          time.Now().UnixMilli(),
					RSS:         s.Memory.RSS,
					HeapUsed:    s.Memory.HeapUsed,
					QueueDepth:  s.QueueDepth,
					ActiveTasks: s.ActiveTasks,
					LagP50:      0,
					LagP99:      0,
				})
			}
		}()
	})
}

// HandleHealth serves /healthz, /healthz/history and /metrics. Both HTTP exits (the standalone
// 8899 port and the WebUI) register exactly these handlers.
func HandleHealth(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, BuildSnapshot())
	})
	mux.HandleFunc("/healthz/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, GetHistory())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(BuildMetrics()))
	})
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"ok":false,"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"encode failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

// StartHealthServer starts the local-only (127.0.0.1) health/metrics server. Listen errors are
// logged only: monitoring must never be able to kill the agent.
func StartHealthServer() {
	EnsureSampling()
	port := config.C().MonitorPort
	mux := http.NewServeMux()
	HandleHealth(mux)
	// v1 serves the snapshot on "/" too and answers anything else with a JSON 404. This catch-all
	// lives on the standalone port only: the WebUI's "/" is the task-log page.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
			return
		}
		writeJSON(w, BuildSnapshot())
	})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() {
		fmt.Printf("[monitor] health server on http://%s\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[monitor] health server error: %v\n", err)
		}
	}()
}
