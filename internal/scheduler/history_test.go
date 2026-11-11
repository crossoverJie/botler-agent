package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/logs"
)

func scheduleLog(id, taskID, phase, source string, startedAt int64, status logs.TaskStatus, project *string, userMessage string, total int) logs.TaskLog {
	l := logs.TaskLog{
		ID:          "log-" + id + "-" + taskID,
		TaskID:      taskID,
		Phase:       phase,
		Source:      source,
		Provider:    "custom",
		Model:       "test-model",
		Project:     project,
		Projects:    []string{},
		Status:      status,
		StartedAt:   startedAt,
		EndedAt:     startedAt + 100,
		DurationMs:  100,
		UserMessage: userMessage,
		ReplyText:   "ok",
		Usage:       logs.TokenUsageLog{Input: total, Total: total * 2},
	}
	if project != nil {
		l.Projects = []string{*project}
	}
	return l
}

func int64Ptr(v int64) *int64 { return &v }

func TestScheduleStateOf(t *testing.T) {
	if got := ScheduleStateOf(false, int64Ptr(1000)); got != StatePaused {
		t.Fatalf("disabled = %q", got)
	}
	if got := ScheduleStateOf(true, nil); got != StatePast {
		t.Fatalf("enabled without a next fire = %q", got)
	}
	if got := ScheduleStateOf(true, int64Ptr(1000)); got != StateActive {
		t.Fatalf("enabled with a next fire = %q", got)
	}
}

func TestBuildOverviewMergesLatestRun(t *testing.T) {
	project := "notes"
	entries := []Schedule{
		{ID: "a", Enabled: true, Timezone: "UTC", Message: "m", At: "08:00"},
		{ID: "b", Enabled: false, Timezone: "UTC", Message: "m", At: "09:00"},
	}
	runLogs := []logs.TaskLog{
		scheduleLog("a", "schedule:a:1000", logs.PhaseExecute, "scheduler", 1000, logs.StatusSuccess, &project, "run 1", 10),
		scheduleLog("a", "schedule:a:2000", logs.PhaseExecute, "scheduler", 2000, logs.StatusError, &project, "run 2", 20),
		// A non-scheduler log must be ignored by the schedule-id linkage.
		scheduleLog("a", "chat:1", logs.PhaseExecute, "telegram", 3000, logs.StatusSuccess, nil, "chat", 5),
	}
	items := BuildOverview(entries, runLogs)
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if items[0].ID != "a" || items[0].State != StateActive || items[0].NextFireAt == nil {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[0].LastRun == nil || items[0].LastRun.StartedAt != 2000 || items[0].LastRun.Status != logs.StatusError {
		t.Fatalf("latest run = %+v", items[0].LastRun)
	}
	if items[0].LastRun.TokenTotal != 40 {
		t.Fatalf("latest run token = %d", items[0].LastRun.TokenTotal)
	}
	if items[1].State != StatePaused || items[1].NextFireAt != nil {
		t.Fatalf("disabled item = %+v", items[1])
	}
	if items[1].LastRun != nil {
		t.Fatalf("disabled item should have no run: %+v", items[1].LastRun)
	}
}

func TestOrphanHistoryIndex(t *testing.T) {
	project := "cook"
	long := strings.Repeat("x", 400)
	runLogs := []logs.TaskLog{
		scheduleLog("a", "schedule:gone:1000", logs.PhaseExecute, "scheduler", 1000, logs.StatusSuccess, &project, "first", 1),
		scheduleLog("a", "schedule:gone:3000", logs.PhaseExecute, "scheduler", 3000, logs.StatusError, &project, long, 1),
		scheduleLog("a", "schedule:gone:2000", logs.PhaseExecute, "scheduler", 2000, logs.StatusSuccess, &project, "middle", 1),
		scheduleLog("b", "schedule:kept:1500", logs.PhaseExecute, "scheduler", 1500, logs.StatusSuccess, nil, "kept", 1),
		scheduleLog("c", "not-a-schedule-id", logs.PhaseExecute, "scheduler", 1600, logs.StatusSuccess, nil, "ignored", 1),
	}
	entries := []Schedule{{ID: "kept", Enabled: true, Timezone: DefaultTZ, Message: "m", At: "08:00"}}
	got := OrphanHistoryIndex(entries, runLogs)
	if len(got) != 1 {
		t.Fatalf("orphan entries = %+v", got)
	}
	if got[0].ScheduleID != "gone" || got[0].RunCount != 3 {
		t.Fatalf("orphan = %+v", got[0])
	}
	if got[0].FirstRunAt != 1000 || got[0].LastRunAt != 3000 {
		t.Fatalf("first/last = %d/%d", got[0].FirstRunAt, got[0].LastRunAt)
	}
	if got[0].LastStatus != logs.StatusError {
		t.Fatalf("last status = %q", got[0].LastStatus)
	}
	if len(got[0].LastMessage) != historyMessageChars {
		t.Fatalf("message should be capped at %d chars, got %d", historyMessageChars, len(got[0].LastMessage))
	}
	if got[0].LastProject == nil || *got[0].LastProject != project {
		t.Fatalf("last project = %v", got[0].LastProject)
	}

	// Ordering: newest last run first.
	more := []logs.TaskLog{
		scheduleLog("x", "schedule:old:1000", logs.PhaseExecute, "scheduler", 1000, logs.StatusSuccess, nil, "m", 1),
		scheduleLog("y", "schedule:new:5000", logs.PhaseExecute, "scheduler", 5000, logs.StatusSuccess, nil, "m", 1),
	}
	ordered := OrphanHistoryIndex(nil, more)
	if len(ordered) != 2 || ordered[0].ScheduleID != "new" {
		t.Fatalf("ordering = %+v", ordered)
	}
}

func TestAggregateRunStats(t *testing.T) {
	project := "notes"
	runs := []logs.TaskLog{
		scheduleLog("a", "schedule:a:1000", logs.PhaseExecute, "scheduler", 1000, logs.StatusSuccess, &project, "m", 10),
		scheduleLog("a", "schedule:a:2000", logs.PhaseExecute, "scheduler", 2000, logs.StatusError, &project, "m", 20),
	}
	stats := AggregateRunStats("a", runs)
	if stats.ScheduleID != "a" || stats.TotalRuns != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.ByStatus[string(logs.StatusSuccess)] != 1 || stats.ByStatus[string(logs.StatusError)] != 1 {
		t.Fatalf("byStatus = %+v", stats.ByStatus)
	}
	if stats.FirstRunAt == nil || *stats.FirstRunAt != 1000 || stats.LastRunAt == nil || *stats.LastRunAt != 2000 {
		t.Fatalf("first/last = %v/%v", stats.FirstRunAt, stats.LastRunAt)
	}
	if stats.AvgDurationMs != 100 {
		t.Fatalf("avgDurationMs = %d", stats.AvgDurationMs)
	}
	if stats.Token.Input != 30 || stats.Token.Total != 60 {
		t.Fatalf("token = %+v", stats.Token)
	}
	// An empty run set yields zeroed stats, not a nil-pointer panic.
	empty := AggregateRunStats("none", nil)
	if empty.TotalRuns != 0 || empty.FirstRunAt != nil || empty.LastRunAt != nil {
		t.Fatalf("empty stats = %+v", empty)
	}
}

func TestHistoryQueriesReadTheTaskLogStore(t *testing.T) {
	c := newTestConfig(t)
	now := time.Now().UnixMilli()
	project := "notes"
	entries := []Schedule{
		{ID: "live", Enabled: true, Timezone: "UTC", Message: "m", At: "08:00"},
	}
	if _, err := SaveSchedules(entries); err != nil {
		t.Fatalf("SaveSchedules: %v", err)
	}
	logs.AppendTaskLog(scheduleLog("live", "schedule:live:1000", logs.PhaseExecute, "scheduler", now-2000, logs.StatusSuccess, &project, "live run", 10))
	logs.AppendTaskLog(scheduleLog("live", "schedule:live:1000", logs.PhaseSelfHeal, "scheduler", now-1900, logs.StatusSuccess, &project, "heal", 1))
	logs.AppendTaskLog(scheduleLog("gone", "schedule:gone:1000", logs.PhaseExecute, "scheduler", now-1000, logs.StatusError, &project, "gone run", 5))
	logs.AppendTaskLog(scheduleLog("other", "chat:1", logs.PhaseExecute, "telegram", now, logs.StatusSuccess, nil, "chat", 1))

	overview, err := Overview()
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(overview) != 1 || overview[0].ID != "live" {
		t.Fatalf("overview = %+v", overview)
	}
	if overview[0].LastRun == nil || overview[0].LastRun.StartedAt != now-2000 {
		t.Fatalf("overview last run = %+v", overview[0].LastRun)
	}

	index, err := HistoryIndex()
	if err != nil {
		t.Fatalf("HistoryIndex: %v", err)
	}
	if len(index) != 1 || index[0].ScheduleID != "gone" || index[0].RunCount != 1 {
		t.Fatalf("history index = %+v", index)
	}

	page, err := ScheduleRuns("live", 10, 0)
	if err != nil {
		t.Fatalf("ScheduleRuns: %v", err)
	}
	if page.Total != 2 || len(page.Logs) != 2 {
		t.Fatalf("runs page = %+v", page)
	}

	stats, err := ScheduleStats("live")
	if err != nil {
		t.Fatalf("ScheduleStats: %v", err)
	}
	// Stats count execute-phase runs only (self-heal retries excluded).
	if stats.TotalRuns != 1 || stats.Token.Input != 10 {
		t.Fatalf("stats = %+v", stats)
	}
	_ = c
}

func TestNextFireAtOfInertOnce(t *testing.T) {
	past := time.Now().Add(-time.Hour).Format("2006-01-02T15:04:05Z07:00")
	e := Schedule{ID: "once", Enabled: true, Timezone: "UTC", Message: "m", Once: past}
	if got := nextFireAtOf(e); got != nil {
		t.Fatalf("a spent once entry must have no next fire, got %v", *got)
	}
	future := time.Now().Add(time.Hour).Format("2006-01-02T15:04:05Z07:00")
	e.Once = future
	if got := nextFireAtOf(e); got == nil {
		t.Fatal("a future once entry must have a next fire")
	}
}
