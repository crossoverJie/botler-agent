package logs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

func withTempLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := config.C()
	config.Set(&config.Config{LogDir: dir})
	t.Cleanup(func() { config.Set(prev) })
	return dir
}

func logAt(id string, ts int64, status TaskStatus, projects []string, source string) TaskLog {
	return TaskLog{
		ID:           id,
		TaskID:       "task-" + id,
		Phase:        PhaseExecute,
		Source:       source,
		Provider:     "custom",
		Model:        "m",
		Project:      LegacyProject(projects),
		Projects:     projects,
		Status:       status,
		StartedAt:    ts,
		EndedAt:      ts + 5,
		DurationMs:   5,
		UserMessage:  "hello " + id,
		ReplyText:    "reply " + id,
		Images:       []string{},
		Tools:        []ToolCallLog{},
		Conversation: []ConversationEntry{},
		Usage:        TokenUsageLog{Input: 10, Output: 5, Total: 15},
		Calls:        []CallUsage{},
	}
}

func TestScheduleIDFromTaskID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"schedule:abc:1700000000000", "abc", true},
		{"schedule:with:colon:1700000000000", "with:colon", true},
		{"random-uuid", "", false},
		{"schedule:abc", "", false},
		{"schedule:abc:notanumber", "", false},
	}
	for _, c := range cases {
		got, ok := ScheduleIDFromTaskID(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("ScheduleIDFromTaskID(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMatchesLogQuery(t *testing.T) {
	multi := logAt("a", 1000, StatusSuccess, []string{"cook", "notes"}, "wechat")
	legacy := logAt("b", 2000, StatusError, nil, "telegram")
	single := "cook"
	legacy.Project = &single

	if !MatchesLogQuery(multi, LogQuery{Project: "notes"}) {
		t.Fatal("multi-project log should match either selected project")
	}
	if MatchesLogQuery(multi, LogQuery{Project: "vocab"}) {
		t.Fatal("unrelated project must not match")
	}
	if !MatchesLogQuery(legacy, LogQuery{Project: "cook"}) {
		t.Fatal("legacy singular project must match")
	}
	if MatchesLogQuery(multi, LogQuery{Source: "telegram"}) {
		t.Fatal("source filter must not match")
	}
	if !MatchesLogQuery(multi, LogQuery{Source: "wechat"}) {
		t.Fatal("source filter should match")
	}
	if MatchesLogQuery(multi, LogQuery{Phase: PhaseSelfHeal}) {
		t.Fatal("phase filter must not match")
	}
	if !MatchesLogQuery(multi, LogQuery{Q: "REPLY A"}) {
		t.Fatal("keyword search must be case-insensitive over the reply text")
	}
	if MatchesLogQuery(multi, LogQuery{Q: "missing"}) {
		t.Fatal("keyword search must not match unrelated text")
	}
	if !MatchesLogQuery(multi, LogQuery{Q: "notes"}) {
		t.Fatal("keyword search must cover project names")
	}
	multi.TaskID = "schedule:job1:1700000000000"
	if !MatchesLogQuery(multi, LogQuery{ScheduleID: "job1"}) {
		t.Fatal("schedule id filter should match")
	}
	if MatchesLogQuery(multi, LogQuery{ScheduleID: "job2"}) {
		t.Fatal("schedule id filter must not match")
	}
}

func TestFilterAndPageLogs(t *testing.T) {
	all := []TaskLog{
		logAt("old", 1000, StatusSuccess, []string{"cook"}, "cli"),
		logAt("new", 3000, StatusSuccess, []string{"cook"}, "cli"),
		logAt("mid", 2000, StatusError, []string{"cook"}, "cli"),
	}
	page := FilterAndPageLogs(all, LogQuery{Limit: 2, Offset: 0})
	if page.Total != 3 || len(page.Logs) != 2 {
		t.Fatalf("unexpected page: total=%d len=%d", page.Total, len(page.Logs))
	}
	if page.Logs[0].ID != "new" || page.Logs[1].ID != "mid" {
		t.Fatalf("logs must be sorted newest first, got %s,%s", page.Logs[0].ID, page.Logs[1].ID)
	}
	page = FilterAndPageLogs(all, LogQuery{Limit: 2, Offset: 2})
	if len(page.Logs) != 1 || page.Logs[0].ID != "old" {
		t.Fatalf("offset paging broken: %+v", page.Logs)
	}
	page = FilterAndPageLogs(all, LogQuery{Limit: 2, Offset: 99})
	if len(page.Logs) != 0 || page.Total != 3 {
		t.Fatalf("out-of-range offset must return an empty page, got %+v", page)
	}
}

func TestAppendQueryAndGetLog(t *testing.T) {
	dir := withTempLogDir(t)
	ts := time.Date(2026, 3, 4, 9, 30, 0, 0, time.Local).UnixMilli()
	AppendTaskLog(logAt("x1", ts, StatusSuccess, []string{"cook"}, "cli"))
	AppendTaskLog(logAt("x2", ts+1000, StatusDuplicate, []string{"cook"}, "cli"))

	name := fileForDay(ts)
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("expected day file %s: %v", name, err)
	}

	page := QueryLogs(LogQuery{})
	if page.Total != 2 || page.Logs[0].ID != "x2" {
		t.Fatalf("unexpected query result: %+v", page)
	}
	got, ok := GetLog("x1")
	if !ok || got.UserMessage != "hello x1" {
		t.Fatalf("GetLog failed: %+v ok=%v", got, ok)
	}
	if _, ok := GetLog("nope"); ok {
		t.Fatal("GetLog must report a miss")
	}
}

func TestQuerySkipsCorruptLines(t *testing.T) {
	dir := withTempLogDir(t)
	ts := time.Date(2026, 3, 5, 9, 0, 0, 0, time.Local).UnixMilli()
	AppendTaskLog(logAt("ok", ts, StatusSuccess, []string{"cook"}, "cli"))
	f, err := os.OpenFile(filepath.Join(dir, fileForDay(ts)), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if page := QueryLogs(LogQuery{}); page.Total != 1 {
		t.Fatalf("corrupt lines must be skipped, got total=%d", page.Total)
	}
}

func TestSummary(t *testing.T) {
	withTempLogDir(t)
	ts := time.Date(2026, 3, 6, 9, 0, 0, 0, time.Local).UnixMilli()
	AppendTaskLog(logAt("s1", ts, StatusSuccess, []string{"cook", "notes"}, "cli"))
	AppendTaskLog(logAt("s2", ts+10, StatusError, nil, "wechat"))

	s := Summary()
	if s.TotalTasks != 2 {
		t.Fatalf("totalTasks = %d", s.TotalTasks)
	}
	if s.ByStatus[string(StatusSuccess)] != 1 || s.ByStatus[string(StatusError)] != 1 {
		t.Fatalf("byStatus = %+v", s.ByStatus)
	}
	if s.ByProject["cook"] != 1 || s.ByProject["notes"] != 1 || s.ByProject["(none)"] != 1 {
		t.Fatalf("byProject = %+v", s.ByProject)
	}
	if s.BySource["cli"] != 1 || s.BySource["wechat"] != 1 {
		t.Fatalf("bySource = %+v", s.BySource)
	}
	if s.Token.Input != 20 || s.Token.Output != 10 || s.Token.Total != 30 {
		t.Fatalf("token = %+v", s.Token)
	}
	if s.AvgDurationMs != 5 {
		t.Fatalf("avgDurationMs = %d", s.AvgDurationMs)
	}
}

func TestTokenTimeSeriesDayBuckets(t *testing.T) {
	withTempLogDir(t)
	day1 := time.Date(2026, 3, 9, 10, 0, 0, 0, time.Local).UnixMilli()
	day2 := time.Date(2026, 3, 10, 11, 0, 0, 0, time.Local).UnixMilli()
	AppendTaskLog(logAt("t1", day1, StatusSuccess, []string{"cook"}, "cli"))
	AppendTaskLog(logAt("t2", day1+3600_000, StatusSuccess, []string{"cook"}, "cli"))
	AppendTaskLog(logAt("t3", day2, StatusSuccess, []string{"notes"}, "cli"))

	series := TokenTimeSeries(TokenTimeQuery{})
	if series.Granularity != GranularityDay {
		t.Fatalf("granularity = %s", series.Granularity)
	}
	if len(series.Groups) != 1 || series.Groups[0].Key != "(all)" {
		t.Fatalf("expected one (all) group, got %+v", series.Groups)
	}
	buckets := series.Groups[0].Buckets
	if len(buckets) != 2 {
		t.Fatalf("expected 2 continuous day buckets, got %d (%+v)", len(buckets), buckets)
	}
	if buckets[0].Tasks != 2 || buckets[1].Tasks != 1 {
		t.Fatalf("task counts = %d,%d", buckets[0].Tasks, buckets[1].Tasks)
	}
	if buckets[0].Total != 30 || buckets[1].Total != 15 {
		t.Fatalf("token totals = %d,%d", buckets[0].Total, buckets[1].Total)
	}
	if buckets[0].End != buckets[1].Start {
		t.Fatalf("buckets must be contiguous: %+v", buckets)
	}

	byProject := TokenTimeSeries(TokenTimeQuery{GroupBy: "project"})
	if len(byProject.Groups) != 2 {
		t.Fatalf("expected 2 project groups, got %d", len(byProject.Groups))
	}
	keys := map[string]bool{}
	for _, g := range byProject.Groups {
		keys[g.Key] = true
		if len(g.Buckets) != 2 {
			t.Fatalf("every group must get the full bucket sequence, got %d", len(g.Buckets))
		}
	}
	if !keys["cook"] || !keys["notes"] {
		t.Fatalf("unexpected group keys: %+v", keys)
	}
}

func TestTokenTimeSeriesWeekStartsMonday(t *testing.T) {
	withTempLogDir(t)
	// 2026-03-11 is a Wednesday; its week starts Monday 2026-03-09.
	ts := time.Date(2026, 3, 11, 12, 0, 0, 0, time.Local).UnixMilli()
	AppendTaskLog(logAt("w1", ts, StatusSuccess, []string{"cook"}, "cli"))

	series := TokenTimeSeries(TokenTimeQuery{Granularity: GranularityWeek})
	wantStart := time.Date(2026, 3, 9, 0, 0, 0, 0, time.Local).UnixMilli()
	if series.Groups[0].Buckets[0].Start != wantStart {
		t.Fatalf("week bucket start = %d, want %d", series.Groups[0].Buckets[0].Start, wantStart)
	}
}

func TestDiskUsageAndCleanup(t *testing.T) {
	dir := withTempLogDir(t)
	d1 := time.Date(2026, 4, 1, 8, 0, 0, 0, time.Local).UnixMilli()
	d2 := time.Date(2026, 4, 2, 8, 0, 0, 0, time.Local).UnixMilli()
	AppendTaskLog(logAt("c1", d1, StatusSuccess, []string{"cook"}, "cli"))
	AppendTaskLog(logAt("c2", d2, StatusSuccess, []string{"cook"}, "cli"))
	// A non day-file entry must never be touched.
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	usage := DiskUsage()
	if usage.FileCount != 2 || usage.EntryCount != 2 {
		t.Fatalf("disk usage = %+v", usage)
	}
	if usage.OldestDay == nil || *usage.OldestDay != "2026-04-01" {
		t.Fatalf("oldestDay = %v", usage.OldestDay)
	}

	// Invalid day names are rejected: nothing is deleted.
	res := CleanupLogs(CleanupOptions{Days: []string{"../evil", "abc", "2026-04-01.jsonl"}})
	if len(res.Deleted) != 0 {
		t.Fatalf("invalid day names must be rejected, got %+v", res.Deleted)
	}

	dry := CleanupLogs(CleanupOptions{Before: strptr("2026-04-01"), DryRun: true})
	if len(dry.Deleted) != 0 {
		t.Fatalf("dry run must not delete, got %+v", dry.Deleted)
	}

	res = CleanupLogs(CleanupOptions{Before: strptr("2026-04-02")})
	if len(res.Deleted) != 1 || res.Deleted[0] != "2026-04-01.jsonl" {
		t.Fatalf("expected only the older day file to be deleted, got %+v", res.Deleted)
	}
	if res.FreedBytes <= 0 {
		t.Fatalf("freedBytes = %d", res.FreedBytes)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non day file must survive cleanup: %v", err)
	}

	res = CleanupLogs(CleanupOptions{Days: []string{"2026-04-02"}})
	if len(res.Deleted) != 1 || res.Deleted[0] != "2026-04-02.jsonl" {
		t.Fatalf("explicit day deletion failed: %+v", res.Deleted)
	}
	if u := DiskUsage(); u.FileCount != 0 {
		t.Fatalf("fileCount = %d", u.FileCount)
	}
}

func strptr(s string) *string { return &s }
