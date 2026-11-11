package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// configDir is the temp BOTLER_CONFIG_DIR installed by newTestConfig.
func configDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("BOTLER_CONFIG_DIR")
	if dir == "" {
		t.Fatal("BOTLER_CONFIG_DIR is not set by the test")
	}
	return dir
}

// newTestConfig points every scheduler path at a fresh temp directory.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfgDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOTLER_CONFIG_DIR", cfgDir)
	// Mirror the production layout: schedules.json / holidays.json / task-logs all live in the
	// externalized config directory, so their backups share <config>/.backups.
	c := &config.Config{
		Provider:      "custom",
		Model:         "test-model",
		SchedulesFile: filepath.Join(cfgDir, "schedules.json"),
		HolidaysFile:  filepath.Join(cfgDir, "holidays.json"),
		LogDir:        filepath.Join(cfgDir, "task-logs"),
	}
	config.Set(c)
	return c
}

func boolPtr(v bool) *bool { return &v }

func TestNormalizeAcceptsValidEntry(t *testing.T) {
	e, err := Normalize(Schedule{ID: "t1", Enabled: true, Timezone: "Asia/Shanghai", Message: "m", At: "08:00"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if e.ID != "t1" || e.Message != "m" || e.At != "08:00" || e.Timezone != "Asia/Shanghai" || !e.Enabled {
		t.Fatalf("unexpected entry: %+v", e)
	}
}

func TestNormalizeDefaultsEnabledAndTimezone(t *testing.T) {
	raw := []byte(`{"id":"x","message":"m","cron":"0 8 * * *"}`)
	e, err := ParseScheduleJSON(raw)
	if err != nil {
		t.Fatalf("ParseScheduleJSON: %v", err)
	}
	if !e.Enabled || e.Timezone != DefaultTZ {
		t.Fatalf("defaults not applied: %+v", e)
	}
	// An explicit false must survive.
	raw = []byte(`{"id":"x","enabled":false,"message":"m","cron":"0 8 * * *"}`)
	e, err = ParseScheduleJSON(raw)
	if err != nil || e.Enabled {
		t.Fatalf("explicit enabled:false lost: %+v (err %v)", e, err)
	}
}

func TestNormalizeRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"no id", `{"message":"m","at":"08:00"}`, "missing id"},
		{"no trigger", `{"id":"x","message":"m"}`, "needs one of cron/interval/at/once"},
		{"two triggers", `{"id":"x","message":"m","at":"08:00","cron":"0 8 * * *"}`, "exactly one of cron/interval/at/once"},
		{"invalid timezone", `{"id":"x","message":"m","at":"08:00","timezone":"Mars/Olympus"}`, "invalid timezone"},
		{"bad interval", `{"id":"x","message":"m","interval":"90m"}`, "invalid interval"},
		{"bad at", `{"id":"x","message":"m","at":"25:00"}`, "invalid at"},
		{"neither action", `{"id":"x","at":"08:00"}`, "needs a message or a script"},
		{"blank message", `{"id":"x","message":"   ","at":"08:00"}`, "needs a message or a script"},
		{"both actions", `{"id":"x","message":"m","script":"a/b.py","at":"08:00"}`, "exactly one of message/script"},
		{"traversal script", `{"id":"x","script":"../escape.py","at":"08:00"}`, "invalid script path"},
		{"shell script", `{"id":"x","script":"a/b.sh","at":"08:00"}`, "invalid script path"},
		{"bad scriptArgs", `{"id":"x","script":"a/b.py","at":"08:00","scriptArgs":"nope"}`, "invalid scriptArgs"},
		{"non-bool commit", `{"id":"x","script":"a/b.py","at":"08:00","commit":"yes"}`, "invalid commit"},
		{"retry max", `{"id":"x","message":"m","at":"08:00","retry":{"max":11,"backoffMs":0}}`, "retry.max"},
		{"retry backoff", `{"id":"x","message":"m","at":"08:00","retry":{"max":1,"backoffMs":3600001}}`, "retry.backoffMs"},
		{"bad silentHours", `{"id":"x","message":"m","at":"08:00","silentHours":{"from":"9","to":"07:00"}}`, "invalid silentHours"},
		{"bad recipient", `{"id":"x","message":"m","at":"08:00","recipient":{"source":"sms","userId":"u"}}`, "invalid recipient.source"},
		{"empty recipient user", `{"id":"x","message":"m","at":"08:00","recipient":{"source":"wechat","userId":""}}`, "invalid recipient.userId"},
		{"workday + once", `{"id":"x","message":"m","once":"2026-08-20T22:00:00+08:00","holidayMode":"workday"}`, "cannot be combined with once"},
		{"bad workday cron time", `{"id":"x","message":"m","cron":"99 18 * * *","holidayMode":"workday"}`, "out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseScheduleJSON([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNormalizeAcceptsScriptEntryAndForwardsFields(t *testing.T) {
	raw := []byte(`{"id":"x","script":"notes/scripts/build.py","at":"08:00","scriptArgs":["--all"],"commit":false}`)
	e, err := ParseScheduleJSON(raw)
	if err != nil {
		t.Fatalf("ParseScheduleJSON: %v", err)
	}
	if e.Script != "notes/scripts/build.py" || e.Message != "" {
		t.Fatalf("unexpected actions: %+v", e)
	}
	if len(e.ScriptArgs) != 1 || e.ScriptArgs[0] != "--all" {
		t.Fatalf("scriptArgs = %v", e.ScriptArgs)
	}
	if e.Commit == nil || *e.Commit {
		t.Fatalf("commit = %v, want false", e.Commit)
	}
}

func TestNormalizeAcceptsWorkdayImpossibleDateField(t *testing.T) {
	raw := []byte(`{"id":"x","message":"m","cron":"0 18 31 2 *","holidayMode":"workday"}`)
	if _, err := ParseScheduleJSON(raw); err != nil {
		t.Fatalf("workday cron with impossible date field should be accepted: %v", err)
	}
}

func TestNormalizeMessageLengthCap(t *testing.T) {
	long := strings.Repeat("x", MessageMaxChars+1)
	if _, err := ParseScheduleJSON([]byte(`{"id":"x","message":"` + long + `","at":"08:00"}`)); err == nil ||
		!strings.Contains(err.Error(), "message exceeds") {
		t.Fatalf("over-cap message should be rejected, got %v", err)
	}
	exact := strings.Repeat("x", MessageMaxChars)
	e, err := ParseScheduleJSON([]byte(`{"id":"x","message":"` + exact + `","at":"08:00"}`))
	if err != nil {
		t.Fatalf("exactly-cap message should be accepted: %v", err)
	}
	if len(e.Message) != MessageMaxChars {
		t.Fatalf("message length = %d", len(e.Message))
	}
}

func TestNormalizeKeepsRecipientAndRetry(t *testing.T) {
	raw := []byte(`{"id":"x","message":"m","at":"08:00","recipient":{"source":"wechat","userId":"o9cq@im.wechat"},"retry":{"max":2,"backoffMs":1500}}`)
	e, err := ParseScheduleJSON(raw)
	if err != nil {
		t.Fatalf("ParseScheduleJSON: %v", err)
	}
	if e.Recipient == nil || e.Recipient.Source != push.SourceWeChat || e.Recipient.UserID != "o9cq@im.wechat" {
		t.Fatalf("recipient = %+v", e.Recipient)
	}
	if e.Retry == nil || e.Retry.Max != 2 || e.Retry.BackoffMs != 1500 {
		t.Fatalf("retry = %+v", e.Retry)
	}
}

func TestNormalizeSchedulesRejectsDuplicateIDs(t *testing.T) {
	items := []Schedule{
		{ID: "dup", Enabled: true, Timezone: DefaultTZ, Message: "a", At: "08:00"},
		{ID: "dup", Enabled: true, Timezone: DefaultTZ, Message: "b", At: "09:00"},
	}
	if _, err := NormalizeSchedules(items); err == nil || !strings.Contains(err.Error(), "duplicate schedule id") {
		t.Fatalf("duplicate ids should be rejected, got %v", err)
	}
}

func TestLoadSchedulesSkipsInvalidEntries(t *testing.T) {
	c := newTestConfig(t)
	body := `{"schedules":[
		{"id":"ok1","message":"m","at":"08:00"},
		{"id":"bad1","at":"08:00"},
		{"id":"ok2","message":"m","cron":"0 9 * * *"}
	]}`
	if err := os.WriteFile(c.SchedulesFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := LoadSchedules()
	if err != nil {
		t.Fatalf("LoadSchedules: %v", err)
	}
	if len(list) != 2 || list[0].ID != "ok1" || list[1].ID != "ok2" {
		t.Fatalf("loaded = %+v", list)
	}
}

func TestLoadSchedulesMissingFile(t *testing.T) {
	newTestConfig(t)
	list, err := LoadSchedules()
	if err != nil {
		t.Fatalf("LoadSchedules on a missing file: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected an empty list, got %+v", list)
	}
}

func TestSaveSchedulesWritesNormalizedAndBacksUp(t *testing.T) {
	newTestConfig(t)

	listenerCalls := 0
	SetSavedListener(func() { listenerCalls++ })
	defer SetSavedListener(nil)

	items := []Schedule{
		{ID: "a", Enabled: true, Timezone: "", Message: "m", At: "08:00"},
		{ID: "b", Enabled: false, Timezone: "UTC", Message: "m", Cron: "0 9 * * *"},
	}
	saved, err := SaveSchedules(items)
	if err != nil {
		t.Fatalf("SaveSchedules: %v", err)
	}
	if len(saved) != 2 || saved[0].Timezone != DefaultTZ || saved[1].Timezone != "UTC" {
		t.Fatalf("saved = %+v", saved)
	}
	if listenerCalls != 1 {
		t.Fatalf("saved listener calls = %d, want 1", listenerCalls)
	}
	// Round trip: what is written must read back identically.
	loaded, err := LoadSchedules()
	if err != nil {
		t.Fatalf("LoadSchedules: %v", err)
	}
	if len(loaded) != 2 || loaded[0].ID != "a" || loaded[1].ID != "b" || loaded[1].Enabled {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	// The first save has nothing to back up; the second one snapshots the previous version.
	saved[0].Message = "changed"
	if _, err := SaveSchedules(saved); err != nil {
		t.Fatalf("SaveSchedules: %v", err)
	}
	backupDir := filepath.Join(configDir(t), ".backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "schedules.json.") {
			backups = append(backups, e.Name())
		}
	}
	if len(backups) != 1 {
		t.Fatalf("expected exactly 1 backup, got %v", backups)
	}
	if !config.ConfigBackupNameRe.MatchString(backups[0]) {
		t.Fatalf("backup name %q does not match the config-backup convention", backups[0])
	}
}

func TestSaveSchedulesRejectsInvalidInput(t *testing.T) {
	c := newTestConfig(t)
	if _, err := SaveSchedules([]Schedule{{ID: "x", Enabled: true, Message: "m"}}); err == nil {
		t.Fatal("saving an entry without a trigger should fail")
	}
	if _, err := os.Stat(c.SchedulesFile); err == nil {
		t.Fatal("a failed save must not write the file")
	}
}

func TestLoadHolidaysRejectsMalformedCalendar(t *testing.T) {
	c := newTestConfig(t)
	for _, bad := range []string{
		`{"2026":{}}`,
		`{"2026":{"holidays":[]}}`,
		`{"2026":{"holidays":"x","workdays":[]}}`,
		`{"2026":[1,2]}`,
		`not json`,
	} {
		if err := os.WriteFile(c.HolidaysFile, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := LoadHolidays(); len(got) != 0 {
			t.Fatalf("malformed calendar %q should be ignored, got %+v", bad, got)
		}
	}
	// A well-formed file (even with no special days) is accepted.
	if err := os.WriteFile(c.HolidaysFile, []byte(`{"2026":{"holidays":[],"workdays":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadHolidays()
	if len(got) != 1 {
		t.Fatalf("well-formed calendar rejected: %+v", got)
	}
	if _, ok := got["2026"]; !ok {
		t.Fatalf("year key lost: %+v", got)
	}
}

func TestSaveHolidaysUsesIndependentBackupNaming(t *testing.T) {
	newTestConfig(t)
	if err := SaveHolidays(HolidayData{"2026": {Holidays: []string{"2026-10-01"}, Workdays: []string{}}}); err != nil {
		t.Fatalf("SaveHolidays: %v", err)
	}
	if err := SaveHolidays(HolidayData{"2026": {Holidays: []string{"2026-10-02"}, Workdays: []string{}}}); err != nil {
		t.Fatalf("SaveHolidays: %v", err)
	}
	// The config-backup prune must never delete holiday backups, and vice versa.
	config.PruneConfigBackups("schedules.json")
	entries, err := os.ReadDir(filepath.Join(configDir(t), ".backups"))
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	holidayBackups := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "holidays.json.") {
			holidayBackups++
			if config.ConfigBackupNameRe.MatchString(e.Name()) {
				t.Fatalf("holiday backup %q must not match the config-backup pattern", e.Name())
			}
		}
	}
	if holidayBackups != 1 {
		t.Fatalf("holiday backups = %d, want 1", holidayBackups)
	}
	// The calendar itself round-trips.
	got := LoadHolidays()
	if len(got["2026"].Holidays) != 1 || got["2026"].Holidays[0] != "2026-10-02" {
		t.Fatalf("calendar round trip failed: %+v", got)
	}
}

func TestSchedulesFileShapeIsObjectWithSchedulesArray(t *testing.T) {
	c := newTestConfig(t)
	if _, err := SaveSchedules([]Schedule{{ID: "a", Enabled: true, Timezone: DefaultTZ, Message: "m", At: "08:00"}}); err != nil {
		t.Fatalf("SaveSchedules: %v", err)
	}
	raw, err := os.ReadFile(c.SchedulesFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schedules.json is not a JSON object: %v", err)
	}
	if _, ok := doc["schedules"]; !ok {
		t.Fatalf("schedules.json must be an object with a schedules array, got %s", raw)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatal("schedules.json should end with a newline")
	}
}
