package scheduler

import (
	"sort"
	"strings"

	"github.com/crossoverJie/botler-agent/internal/logs"
)

// Schedule states as the UI presents them.
const (
	StateActive = "active"
	StatePaused = "paused"
	StatePast   = "past"
)

// maxHistoryIDs bounds the history index payload; newest ids win.
const maxHistoryIDs = 200

// historyMessageChars caps the retained copy of a run's user message.
const historyMessageChars = 200

// maxScanLimit is used when an aggregation needs every matching log (QueryLogs pages by limit).
const maxScanLimit = 1 << 30

// ScheduleStateOf derives the UI state from `enabled` + `nextFireAt` only, so it generalizes to
// any future trigger that can become inert.
func ScheduleStateOf(enabled bool, nextFireAt *int64) string {
	if !enabled {
		return StatePaused
	}
	if nextFireAt == nil {
		return StatePast
	}
	return StateActive
}

// scanSchedulerLogs visits every scheduler log (optionally of one phase) in the task-log store.
// The store streams day files line by line, so memory stays bounded per day.
func scanSchedulerLogs(phase string, visit func(logs.TaskLog)) {
	page := logs.QueryLogs(logs.LogQuery{
		Source: "scheduler",
		Phase:  phase,
		Limit:  maxScanLimit,
	})
	for _, l := range page.Logs {
		visit(l)
	}
}

// RunQuery filters one schedule's runs.
type RunQuery struct {
	From   *int64
	To     *int64
	Q      string
	Limit  int
	Offset int
}

// RunsPage is one page of a schedule's run history (same shape as the log page).
type RunsPage struct {
	Logs  []logs.TaskLog `json:"logs"`
	Total int            `json:"total"`
}

// ScheduleRunsQuery returns one page of an entry's runs (self-heal retries included, like v1).
func ScheduleRunsQuery(id string, q RunQuery) (RunsPage, error) {
	page := logs.QueryLogs(logs.LogQuery{
		From:       q.From,
		To:         q.To,
		Source:     "scheduler",
		ScheduleID: id,
		Q:          q.Q,
		Limit:      q.Limit,
		Offset:     q.Offset,
	})
	logsOut := page.Logs
	if logsOut == nil {
		logsOut = []logs.TaskLog{}
	}
	return RunsPage{Logs: logsOut, Total: page.Total}, nil
}

// ScheduleRuns returns one page of an entry's runs.
func ScheduleRuns(id string, limit, offset int) (RunsPage, error) {
	return ScheduleRunsQuery(id, RunQuery{Limit: limit, Offset: offset})
}

// RunToken is the token aggregate of a schedule's runs.
type RunToken struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Total  int `json:"total"`
}

// RunStats aggregates one schedule's runs.
type RunStats struct {
	ScheduleID    string         `json:"scheduleId"`
	TotalRuns     int            `json:"totalRuns"`
	ByStatus      map[string]int `json:"byStatus"`
	FirstRunAt    *int64         `json:"firstRunAt"`
	LastRunAt     *int64         `json:"lastRunAt"`
	AvgDurationMs int            `json:"avgDurationMs"`
	Token         RunToken       `json:"token"`
}

// AggregateRunStats aggregates a pre-collected set of run logs (pure, used by tests).
func AggregateRunStats(scheduleID string, runs []logs.TaskLog) RunStats {
	byStatus := map[string]int{}
	token := RunToken{}
	var durationSum int64
	var first, last *int64
	for i := range runs {
		l := runs[i]
		byStatus[string(l.Status)]++
		token.Input += l.Usage.Input
		token.Output += l.Usage.Output
		token.Total += l.Usage.Total
		durationSum += l.DurationMs
		started := l.StartedAt
		if first == nil || started < *first {
			v := started
			first = &v
		}
		if last == nil || started > *last {
			v := started
			last = &v
		}
	}
	stats := RunStats{
		ScheduleID: scheduleID,
		TotalRuns:  len(runs),
		ByStatus:   byStatus,
		FirstRunAt: first,
		LastRunAt:  last,
		Token:      token,
	}
	if len(runs) > 0 {
		stats.AvgDurationMs = int(durationSum / int64(len(runs)))
	}
	return stats
}

// ScheduleStats aggregates one schedule's execute-phase runs.
func ScheduleStats(id string) (RunStats, error) {
	var runs []logs.TaskLog
	scanSchedulerLogs(logs.PhaseExecute, func(l logs.TaskLog) {
		if sid, ok := logs.ScheduleIDFromTaskID(l.TaskID); ok && sid == id {
			runs = append(runs, l)
		}
	})
	return AggregateRunStats(id, runs), nil
}

// LastRun is the summary of an entry's most recent run.
type LastRun struct {
	StartedAt  int64           `json:"startedAt"`
	Status     logs.TaskStatus `json:"status"`
	DurationMs int64           `json:"durationMs"`
	Project    *string         `json:"project"`
	TokenTotal int             `json:"tokenTotal"`
}

func toLastRun(l *logs.TaskLog) *LastRun {
	if l == nil {
		return nil
	}
	project := l.Project
	if project == nil {
		if joined := strings.Join(l.Projects, ", "); joined != "" {
			project = &joined
		}
	}
	return &LastRun{
		StartedAt:  l.StartedAt,
		Status:     l.Status,
		DurationMs: l.DurationMs,
		Project:    project,
		TokenTotal: l.Usage.Total,
	}
}

// OverviewItem is one schedule with its computed next fire, UI state and latest run.
type OverviewItem struct {
	Schedule
	NextFireAt *int64   `json:"nextFireAt"`
	State      string   `json:"state"`
	LastRun    *LastRun `json:"lastRun"`
}

// OverviewList renders as a JSON array (the WebUI expects an array).
type OverviewList []OverviewItem

// nextFireAtOf computes the next fire of an enabled entry, or nil when there is none.
func nextFireAtOf(s Schedule) *int64 {
	if !s.Enabled {
		return nil
	}
	next := safeNextFire(s, nowMs())
	if next == NoFire {
		return nil
	}
	return &next
}

// BuildOverview merges entries with their latest run logs (pure, used by tests).
func BuildOverview(entries []Schedule, runLogs []logs.TaskLog) OverviewList {
	latest := map[string]*logs.TaskLog{}
	for i := range runLogs {
		l := &runLogs[i]
		id, ok := logs.ScheduleIDFromTaskID(l.TaskID)
		if !ok {
			continue
		}
		prev := latest[id]
		if prev == nil || l.StartedAt > prev.StartedAt {
			latest[id] = l
		}
	}
	out := make(OverviewList, 0, len(entries))
	for _, e := range entries {
		next := nextFireAtOf(e)
		item := e
		out = append(out, OverviewItem{
			Schedule:   item,
			NextFireAt: next,
			State:      ScheduleStateOf(e.Enabled, next),
			LastRun:    toLastRun(latest[e.ID]),
		})
	}
	return out
}

// Overview returns every configured schedule with its computed next fire and latest run.
// Only execute-phase runs are considered (self-heal retries excluded), matching v1.
func Overview() (OverviewList, error) {
	entries, _ := LoadSchedules()
	var runLogs []logs.TaskLog
	scanSchedulerLogs(logs.PhaseExecute, func(l logs.TaskLog) { runLogs = append(runLogs, l) })
	return BuildOverview(entries, runLogs), nil
}

// HistoryEntry summarizes a schedule id that only exists in the task logs.
type HistoryEntry struct {
	ScheduleID  string          `json:"scheduleId"`
	RunCount    int             `json:"runCount"`
	FirstRunAt  int64           `json:"firstRunAt"`
	LastRunAt   int64           `json:"lastRunAt"`
	LastStatus  logs.TaskStatus `json:"lastStatus"`
	LastProject *string         `json:"lastProject"`
	// LastMessage is the latest run's user message — the only context left once the config is gone.
	LastMessage string `json:"lastMessage"`
}

type historyAcc struct {
	count int
	first int64
	last  int64
	log   logs.TaskLog
}

// OrphanHistoryIndex returns every schedule id found in runLogs that is no longer configured,
// newest last-run first (pure, used by tests). An empty entries list indexes every id.
func OrphanHistoryIndex(entries []Schedule, runLogs []logs.TaskLog) []HistoryEntry {
	acc := map[string]*historyAcc{}
	for _, l := range runLogs {
		id, ok := logs.ScheduleIDFromTaskID(l.TaskID)
		if !ok {
			continue
		}
		prev := acc[id]
		if prev == nil {
			logCopy := l
			acc[id] = &historyAcc{count: 1, first: l.StartedAt, last: l.StartedAt, log: logCopy}
			continue
		}
		prev.count++
		if l.StartedAt < prev.first {
			prev.first = l.StartedAt
		}
		if l.StartedAt > prev.last {
			prev.last = l.StartedAt
			prev.log = l
		}
	}
	exclude := make(map[string]bool, len(entries))
	for _, e := range entries {
		exclude[e.ID] = true
	}
	out := make([]HistoryEntry, 0, len(acc))
	for id, a := range acc {
		if exclude[id] {
			continue
		}
		project := a.log.Project
		if project == nil {
			if joined := strings.Join(a.log.Projects, ", "); joined != "" {
				project = &joined
			}
		}
		msg := a.log.UserMessage
		if len(msg) > historyMessageChars {
			msg = msg[:historyMessageChars]
		}
		out = append(out, HistoryEntry{
			ScheduleID:  id,
			RunCount:    a.count,
			FirstRunAt:  a.first,
			LastRunAt:   a.last,
			LastStatus:  a.log.Status,
			LastProject: project,
			LastMessage: msg,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastRunAt > out[j].LastRunAt })
	if len(out) > maxHistoryIDs {
		out = out[:maxHistoryIDs]
	}
	return out
}

// HistoryIndex returns schedules that were removed from schedules.json but still have runs.
func HistoryIndex() ([]HistoryEntry, error) {
	entries, _ := LoadSchedules()
	var runLogs []logs.TaskLog
	scanSchedulerLogs(logs.PhaseExecute, func(l logs.TaskLog) { runLogs = append(runLogs, l) })
	return OrphanHistoryIndex(entries, runLogs), nil
}
