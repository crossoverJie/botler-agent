package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
	"github.com/crossoverJie/botler-agent/internal/scheduler"
)

// withScheduleStore points the schedule store at a fresh temp file and clears the task context.
// The schedule tool deliberately takes no path argument (it always writes schedules.json through
// saveSchedules), so the only way to reach it is via the externalized config dir.
func withScheduleStore(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOTLER_CONFIG_DIR", cfgDir)
	config.Set(&config.Config{
		DataRoot:      dir,
		SchedulesFile: filepath.Join(cfgDir, "schedules.json"),
		HolidaysFile:  filepath.Join(cfgDir, "holidays.json"),
		LogDir:        filepath.Join(cfgDir, "task-logs"),
	})
	SetTaskContext(nil)
	t.Cleanup(func() { SetTaskContext(nil) })
	return cfgDir
}

// runSchedule invokes the tool the way the agent loop does: raw JSON in, ToolResult out.
func runSchedule(t *testing.T, args string) (agentrt.ToolResult, error) {
	t.Helper()
	return newScheduleTool().Execute(context.Background(), json.RawMessage(args))
}

func mustSchedule(t *testing.T, args string) agentrt.ToolResult {
	t.Helper()
	res, err := runSchedule(t, args)
	if err != nil {
		t.Fatalf("unexpected error for %s: %v", args, err)
	}
	return res
}

// loadEntry re-reads schedules.json so assertions are about what was persisted, not just the reply.
func loadEntry(t *testing.T, id string) (scheduler.Schedule, bool) {
	t.Helper()
	return scheduler.FindSchedule(id)
}

func futureOnce(t *testing.T, d time.Duration) string {
	t.Helper()
	return time.Now().Add(d).In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
}

func TestScheduleMetadata(t *testing.T) {
	tool := newScheduleTool()
	if tool.Name() != "schedule" {
		t.Fatalf("unexpected name %q", tool.Name())
	}
	if tool.Label() == "" || tool.Description() == "" {
		t.Fatal("label and description must be non-empty")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("unexpected schema type %v", schema["type"])
	}
}

func TestScheduleListEmpty(t *testing.T) {
	withScheduleStore(t)
	res := mustSchedule(t, `{"action":"list"}`)
	if !strings.Contains(res.Text, "no scheduled tasks") {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
	details := res.Details.(map[string]any)
	if details["count"] != 0 {
		t.Fatalf("unexpected count: %v", details["count"])
	}
}

func TestScheduleCreateMessageWithCron(t *testing.T) {
	withScheduleStore(t)
	res := mustSchedule(t, `{"action":"create","id":"water","cron":"0 8 * * *","message":"drink water"}`)
	if !strings.Contains(res.Text, `Created scheduled task "water"`) {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
	if !strings.Contains(res.Text, "cron 0 8 * * *") {
		t.Fatalf("reply must name the trigger: %q", res.Text)
	}
	// The reply confirms the next fire time, which is what the model reports back to the user.
	if !strings.Contains(res.Text, "next fire:") {
		t.Fatalf("reply must include the next fire time: %q", res.Text)
	}
	if res.Details.(map[string]any)["id"] != "water" {
		t.Fatalf("unexpected details: %v", res.Details)
	}

	entry, ok := loadEntry(t, "water")
	if !ok {
		t.Fatal("entry was not persisted")
	}
	if entry.Message != "drink water" || entry.Cron != "0 8 * * *" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if !entry.Enabled {
		t.Fatal("a created entry must be enabled")
	}
	if entry.Timezone != scheduler.DefaultTZ {
		t.Fatalf("expected the default timezone, got %q", entry.Timezone)
	}
}

func TestScheduleCreateGeneratesID(t *testing.T) {
	withScheduleStore(t)
	res := mustSchedule(t, `{"action":"create","interval":"45m","message":"stretch"}`)
	id, _ := res.Details.(map[string]any)["id"].(string)
	if !strings.HasPrefix(id, "sched-") {
		t.Fatalf("expected a generated id, got %q", id)
	}
	if _, ok := loadEntry(t, id); !ok {
		t.Fatalf("generated entry %q was not persisted", id)
	}
}

func TestScheduleCreateScriptEntryKeepsRuntimeOptions(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"build","script":"cook/scripts/build.py",
		"scriptArgs":["--all"],"commit":false,"retryMax":2,"retryBackoffMs":1500,
		"cron":"30 7 * * *","project":"cook","holidayMode":"workday"}`)

	entry, ok := loadEntry(t, "build")
	if !ok {
		t.Fatal("script entry was not persisted")
	}
	if entry.Script != "cook/scripts/build.py" {
		t.Fatalf("unexpected script: %q", entry.Script)
	}
	if len(entry.ScriptArgs) != 1 || entry.ScriptArgs[0] != "--all" {
		t.Fatalf("scriptArgs lost: %v", entry.ScriptArgs)
	}
	if entry.Commit == nil || *entry.Commit {
		t.Fatal("commit=false must be preserved")
	}
	if entry.Retry == nil || entry.Retry.Max != 2 || entry.Retry.BackoffMs != 1500 {
		t.Fatalf("retry policy lost: %+v", entry.Retry)
	}
	if entry.Project != "cook" {
		t.Fatalf("project lost: %q", entry.Project)
	}
	if entry.HolidayMode != scheduler.HolidayModeWorkday {
		t.Fatalf("holidayMode lost: %q", entry.HolidayMode)
	}
}

// The push recipient is never guessed by the model: the framework injects it from the sender.
func TestScheduleCreateInjectsRecipientFromTaskContext(t *testing.T) {
	withScheduleStore(t)
	SetTaskContext(&TaskContext{Recipient: &push.Recipient{Source: push.SourceWeChat, UserID: "wx-1"}})
	mustSchedule(t, `{"action":"create","id":"wx","at":"08:00","message":"hi"}`)

	entry, ok := loadEntry(t, "wx")
	if !ok {
		t.Fatal("entry was not persisted")
	}
	if entry.Recipient == nil || entry.Recipient.UserID != "wx-1" || entry.Recipient.Source != push.SourceWeChat {
		t.Fatalf("recipient not injected: %+v", entry.Recipient)
	}
}

func TestScheduleCreateRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"no action", `{"action":"create","cron":"0 8 * * *"}`, "message OR script"},
		{"message and script", `{"action":"create","cron":"0 8 * * *","message":"m","script":"a/b.py"}`, "exactly one of message/script"},
		{"no trigger", `{"action":"create","message":"m"}`, "exactly one of cron / interval / at / once"},
		{"two triggers", `{"action":"create","cron":"0 8 * * *","at":"08:00","message":"m"}`, "exactly one of cron / interval / at / once"},
		{"bad timezone", `{"action":"create","cron":"0 8 * * *","message":"m","timezone":"Mars/Olympus"}`, "invalid timezone"},
		{"bad once", `{"action":"create","once":"tomorrow","message":"m"}`, "invalid once value"},
		{"past once", `{"action":"create","once":"2020-01-01T08:00:00+08:00","message":"m"}`, "must be in the future"},
		{"bad cron", `{"action":"create","cron":"nope","message":"m"}`, "cron"},
		{"bad interval", `{"action":"create","interval":"90m","message":"m"}`, "interval"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withScheduleStore(t)
			_, err := runSchedule(t, c.args)
			if err == nil {
				t.Fatalf("expected an error for %s", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestScheduleCreateRejectsDuplicateID(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"dup","cron":"0 8 * * *","message":"m"}`)
	_, err := runSchedule(t, `{"action":"create","id":"dup","cron":"0 9 * * *","message":"m2"}`)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected a duplicate-id error, got %v", err)
	}
}

func TestScheduleCreateAcceptsFutureOnce(t *testing.T) {
	withScheduleStore(t)
	once := futureOnce(t, 2*time.Hour)
	mustSchedule(t, `{"action":"create","id":"one-shot","once":"`+once+`","message":"ping"}`)

	entry, ok := loadEntry(t, "one-shot")
	if !ok {
		t.Fatal("entry was not persisted")
	}
	if entry.Once != once {
		t.Fatalf("once not preserved: %q", entry.Once)
	}
	// A future one-shot must report its next fire time, unlike an expired one.
	res := mustSchedule(t, `{"action":"list"}`)
	if !strings.Contains(res.Text, "next-fire=") {
		t.Fatalf("list must show the next fire time: %q", res.Text)
	}
}

func TestScheduleListRendersEveryField(t *testing.T) {
	cfgDir := withScheduleStore(t)
	commit := false
	seed := []scheduler.Schedule{
		{
			ID: "enabled", Enabled: true, Cron: "0 8 * * *", Message: "m", Timezone: scheduler.DefaultTZ,
			Project: "cook", Recipient: &push.Recipient{Source: push.SourceTelegram, UserID: "42"},
			Retry: &scheduler.Retry{Max: 3, BackoffMs: 500}, HolidayMode: scheduler.HolidayModeWorkday,
		},
		{ID: "off", Enabled: false, At: "09:30", Message: "m", Timezone: scheduler.DefaultTZ},
		// holidayMode:"workday" cannot be combined with once, so the expired one-shot stays plain.
		{ID: "expired", Enabled: true, Once: "2020-01-01T08:00:00+08:00", Message: "m", Timezone: scheduler.DefaultTZ},
		{
			ID: "script", Enabled: true, Interval: "2h", Script: "cook/scripts/run.py",
			ScriptArgs: []string{"-v"}, Commit: &commit, Timezone: scheduler.DefaultTZ,
		},
	}
	writeScheduleFile(t, filepath.Join(cfgDir, "schedules.json"), seed)

	res := mustSchedule(t, `{"action":"list"}`)
	for _, want := range []string{
		"Scheduled tasks (4):",
		"- enabled [enabled] cron 0 8 * * * msg=m retry=3 tz=Asia/Shanghai project=cook push=telegram:42 holidayMode=workday",
		"- off [disabled] at 09:30 (daily) msg=m tz=Asia/Shanghai disabled",
		"- expired [enabled] once 2020-01-01T08:00:00+08:00 msg=m tz=Asia/Shanghai (expired)",
		"\u25b6 cook/scripts/run.py",
	} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("list output missing %q:\n%s", want, res.Text)
		}
	}
	if res.Details.(map[string]any)["count"] != 4 {
		t.Fatalf("unexpected count: %v", res.Details)
	}
}

func TestScheduleDelete(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m"}`)
	mustSchedule(t, `{"action":"create","id":"b","cron":"0 9 * * *","message":"m"}`)

	res := mustSchedule(t, `{"action":"delete","id":"a"}`)
	if !strings.Contains(res.Text, `Deleted scheduled task "a"`) {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
	if _, ok := loadEntry(t, "a"); ok {
		t.Fatal("entry a must be gone")
	}
	if _, ok := loadEntry(t, "b"); !ok {
		t.Fatal("entry b must survive")
	}
}

func TestScheduleEnableDisable(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m"}`)

	res := mustSchedule(t, `{"action":"disable","id":"a"}`)
	if !strings.Contains(res.Text, `Disabled scheduled task "a"`) {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
	if e, _ := loadEntry(t, "a"); e.Enabled {
		t.Fatal("entry must be disabled")
	}

	res = mustSchedule(t, `{"action":"enable","id":"a"}`)
	if !strings.Contains(res.Text, `Enabled scheduled task "a"`) {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
	// Enabling recomputes the next fire time so the model can report it.
	if !strings.Contains(res.Text, "next fire:") {
		t.Fatalf("enable must report the next fire time: %q", res.Text)
	}
	if e, _ := loadEntry(t, "a"); !e.Enabled {
		t.Fatal("entry must be enabled")
	}
}

func TestScheduleMutationRequiresExistingID(t *testing.T) {
	withScheduleStore(t)
	if _, err := runSchedule(t, `{"action":"delete"}`); err == nil || !strings.Contains(err.Error(), "requires an id") {
		t.Fatalf("expected an id error, got %v", err)
	}
	for _, action := range []string{"delete", "enable", "disable", "update"} {
		_, err := runSchedule(t, `{"action":"`+action+`","id":"missing"}`)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("%s: expected not-found, got %v", action, err)
		}
	}
}

func TestScheduleUpdateSwitchesActionType(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m","retryMax":2,"retryBackoffMs":100}`)

	// message -> script: the message is cleared and the previous retry policy is dropped, because
	// it described the other action type.
	mustSchedule(t, `{"action":"update","id":"a","script":"cook/scripts/a.js","scriptArgs":["--x"],"commit":false}`)
	entry, _ := loadEntry(t, "a")
	if entry.Script != "cook/scripts/a.js" || entry.Message != "" {
		t.Fatalf("action type not switched: %+v", entry)
	}
	if len(entry.ScriptArgs) != 1 || entry.ScriptArgs[0] != "--x" {
		t.Fatalf("scriptArgs not applied: %v", entry.ScriptArgs)
	}
	if entry.Commit == nil || *entry.Commit {
		t.Fatal("commit=false not applied")
	}
	if entry.Retry != nil {
		t.Fatalf("retry must be cleared when switching action type: %+v", entry.Retry)
	}

	// script -> message: script-only fields are cleared again.
	mustSchedule(t, `{"action":"update","id":"a","message":"new message"}`)
	entry, _ = loadEntry(t, "a")
	if entry.Message != "new message" || entry.Script != "" || len(entry.ScriptArgs) != 0 || entry.Commit != nil {
		t.Fatalf("script fields not cleared: %+v", entry)
	}
}

func TestScheduleUpdateSwitchesTrigger(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m"}`)

	once := futureOnce(t, 30*time.Minute)
	mustSchedule(t, `{"action":"update","id":"a","once":"`+once+`"}`)
	entry, _ := loadEntry(t, "a")
	if entry.Once != once {
		t.Fatalf("once not set: %q", entry.Once)
	}
	if entry.Cron != "" || entry.Interval != "" || entry.At != "" {
		t.Fatalf("the other triggers must be cleared: %+v", entry)
	}

	// once -> at: switching away from once must clear it again.
	mustSchedule(t, `{"action":"update","id":"a","at":"07:15"}`)
	entry, _ = loadEntry(t, "a")
	if entry.At != "07:15" || entry.Once != "" {
		t.Fatalf("trigger switch failed: %+v", entry)
	}
}

func TestScheduleUpdateRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"message and script", `{"action":"update","id":"a","message":"m","script":"b/c.py"}`, "must not set both"},
		{"bad timezone", `{"action":"update","id":"a","timezone":"Nowhere/Zone"}`, "invalid timezone"},
		{"past once", `{"action":"update","id":"a","once":"2020-01-01T08:00:00+08:00"}`, "must be in the future"},
		{"bad once", `{"action":"update","id":"a","once":"later"}`, "invalid once value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withScheduleStore(t)
			mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m"}`)
			_, err := runSchedule(t, c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected %q, got %v", c.want, err)
			}
		})
	}
}

func TestScheduleUpdateRetryPolicy(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","script":"cook/scripts/a.py","cron":"0 8 * * *"}`)

	mustSchedule(t, `{"action":"update","id":"a","retryMax":3}`)
	if e, _ := loadEntry(t, "a"); e.Retry == nil || e.Retry.Max != 3 {
		t.Fatalf("retry not set: %+v", e.Retry)
	}
	// Backoff-only updates keep the existing Max.
	mustSchedule(t, `{"action":"update","id":"a","retryBackoffMs":250}`)
	if e, _ := loadEntry(t, "a"); e.Retry == nil || e.Retry.Max != 3 || e.Retry.BackoffMs != 250 {
		t.Fatalf("backoff not merged: %+v", e.Retry)
	}
	// retryMax=0 removes the policy entirely.
	mustSchedule(t, `{"action":"update","id":"a","retryMax":0}`)
	if e, _ := loadEntry(t, "a"); e.Retry != nil {
		t.Fatalf("retry must be cleared: %+v", e.Retry)
	}
}

func TestScheduleUpdateProjectAndHolidayMode(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m","project":"cook","holidayMode":"workday"}`)

	// An explicit empty project clears it (the model's way of un-pinning the routing hint).
	mustSchedule(t, `{"action":"update","id":"a","project":""}`)
	entry, _ := loadEntry(t, "a")
	if entry.Project != "" {
		t.Fatalf("project not cleared: %q", entry.Project)
	}

	// Any non-workday value turns the gating off.
	mustSchedule(t, `{"action":"update","id":"a","holidayMode":"off"}`)
	entry, _ = loadEntry(t, "a")
	if entry.HolidayMode != "" {
		t.Fatalf("holidayMode not cleared: %q", entry.HolidayMode)
	}
}

func TestScheduleUpdateTimezone(t *testing.T) {
	withScheduleStore(t)
	mustSchedule(t, `{"action":"create","id":"a","cron":"0 8 * * *","message":"m"}`)
	mustSchedule(t, `{"action":"update","id":"a","timezone":"UTC"}`)
	if e, _ := loadEntry(t, "a"); e.Timezone != "UTC" {
		t.Fatalf("timezone not updated: %q", e.Timezone)
	}
}

func TestScheduleUnknownAction(t *testing.T) {
	withScheduleStore(t)
	if _, err := runSchedule(t, `{"action":"frobnicate"}`); err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("expected unknown-action error, got %v", err)
	}
}

func TestScheduleRejectsInvalidJSON(t *testing.T) {
	withScheduleStore(t)
	if _, err := runSchedule(t, `{"action":`); err == nil {
		t.Fatal("expected a JSON error")
	}
}

func TestScheduleListSurvivesMalformedStore(t *testing.T) {
	// A corrupt schedules.json must not wedge the tool: LoadSchedules degrades to an empty list.
	cfgDir := withScheduleStore(t)
	writeRawFile(t, filepath.Join(cfgDir, "schedules.json"), "{ not json")
	res := mustSchedule(t, `{"action":"list"}`)
	if !strings.Contains(res.Text, "no scheduled tasks") {
		t.Fatalf("unexpected reply: %q", res.Text)
	}
}

func TestTriggerOfCoversEveryForm(t *testing.T) {
	cases := []struct {
		entry scheduler.Schedule
		want  string
	}{
		{scheduler.Schedule{Cron: "0 8 * * *"}, "cron 0 8 * * *"},
		{scheduler.Schedule{Interval: "2h"}, "interval 2h"},
		{scheduler.Schedule{At: "07:30"}, "at 07:30 (daily)"},
		{scheduler.Schedule{Once: "2026-08-20T22:00:00+08:00"}, "once 2026-08-20T22:00:00+08:00"},
		{scheduler.Schedule{}, "(no trigger)"},
	}
	for _, c := range cases {
		if got := triggerOf(c.entry); got != c.want {
			t.Fatalf("triggerOf(%+v) = %q, want %q", c.entry, got, c.want)
		}
	}
}

func TestNextFireOfExpiredOnceIsEmpty(t *testing.T) {
	// An expired once entry is inert: no next fire, and the list renders it as "(expired)".
	entry := scheduler.Schedule{
		ID: "old", Enabled: true, Once: "2020-01-01T08:00:00+08:00",
		Message: "m", Timezone: scheduler.DefaultTZ,
	}
	if got := nextFireOf(entry); got != "" {
		t.Fatalf("expected no next fire, got %q", got)
	}
}

// fmtFire falls back to UTC for an unknown timezone instead of failing the whole reply.
func TestFmtFireFallsBackToUTC(t *testing.T) {
	epoch := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC).UnixMilli()
	if got := fmtFire(epoch, "Not/AZone"); got != "3/4/2026, 05:06:07" {
		t.Fatalf("unexpected fallback format: %q", got)
	}
	if got := fmtFire(epoch, "Asia/Shanghai"); got != "3/4/2026, 13:06:07" {
		t.Fatalf("unexpected local format: %q", got)
	}
}

func TestTextResultDefaultsDetails(t *testing.T) {
	res, err := textResult("hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Details.(map[string]any); !ok {
		t.Fatalf("nil details must become an empty map, got %T", res.Details)
	}
}

// writeScheduleFile seeds schedules.json directly, bypassing the tool, so the read path can be
// exercised with entries the tool would reject (an already-expired once, for instance).
func writeScheduleFile(t *testing.T, path string, entries []scheduler.Schedule) {
	t.Helper()
	payload, err := json.MarshalIndent(map[string]any{"schedules": entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeRawFile(t, path, string(payload))
}

func writeRawFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
