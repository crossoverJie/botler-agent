package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// Limits enforced in exactly one place (both the schedule tool and the WebUI go through Normalize).
const (
	// MessageMaxChars caps a schedule message so a runaway write cannot bloat the config.
	MessageMaxChars = 10 * 1024
	// RetryMaxLimit bounds retry.max: an unbounded retry count would re-run a failing entry forever.
	RetryMaxLimit = 10
	// RetryBackoffMsLimit bounds retry.backoffMs (1 hour).
	RetryBackoffMsLimit int64 = 60 * 60 * 1000
	// maxScriptArgs / maxScriptArgLen bound the script argument list.
	maxScriptArgs   = 64
	maxScriptArgLen = 256
)

var (
	scriptPathRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.(py|js|mjs)$`)
	hmRe         = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	backupNameRe = regexp.MustCompile(`^(\.env|system-prompt\.md|schedules\.json)\.\d{8}T\d{6}$`)
	// maxConfigBackups is how many backups per config file are kept (shared convention with .env /
	// system-prompt.md), independent of the holidays.json.<epochMs> backups.
	maxConfigBackups = 10
)

// rawSchedule is the tolerant parse shape: presence of message/script/commit must be
// distinguishable from zero values, exactly like v1's `o.message !== undefined` checks.
type rawSchedule struct {
	ID          string          `json:"id"`
	Enabled     *bool           `json:"enabled"`
	Cron        string          `json:"cron"`
	Interval    string          `json:"interval"`
	At          string          `json:"at"`
	Once        string          `json:"once"`
	Timezone    string          `json:"timezone"`
	Message     *string         `json:"message"`
	Script      *string         `json:"script"`
	ScriptArgs  json.RawMessage `json:"scriptArgs"`
	Commit      json.RawMessage `json:"commit"`
	Project     string          `json:"project"`
	Retry       *Retry          `json:"retry"`
	SilentHours *SilentHours    `json:"silentHours"`
	HolidayMode string          `json:"holidayMode"`
	Recipient   *push.Recipient `json:"recipient"`
}

func parseHM(s string) (int, int, bool) {
	m := hmRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	if h > 23 || min > 59 {
		return 0, 0, false
	}
	return h, min, true
}

// normalizeRaw validates and normalizes one parsed entry. It returns an error describing the
// first fatal problem; the caller (load) skips the entry or (save) rejects the write.
func normalizeRaw(o rawSchedule) (Schedule, error) {
	if strings.TrimSpace(o.ID) == "" {
		return Schedule{}, fmt.Errorf("missing id")
	}
	id := o.ID

	msg := ""
	if o.Message != nil {
		msg = *o.Message
	}
	script := ""
	if o.Script != nil {
		script = *o.Script
	}
	hasMessage := strings.TrimSpace(msg) != ""
	hasScript := strings.TrimSpace(script) != ""
	if !hasMessage && !hasScript {
		return Schedule{}, fmt.Errorf("schedule %q needs a message or a script", id)
	}
	if hasMessage && hasScript {
		return Schedule{}, fmt.Errorf("schedule %q must specify exactly one of message/script", id)
	}
	if hasMessage && len(msg) > MessageMaxChars {
		return Schedule{}, fmt.Errorf("schedule %q message exceeds %d chars", id, MessageMaxChars)
	}

	var scriptArgs []string
	var commit *bool
	if hasScript {
		if strings.HasPrefix(script, "/") || strings.HasPrefix(script, "~") || strings.HasPrefix(script, "..") ||
			!scriptPathRe.MatchString(script) {
			return Schedule{}, fmt.Errorf("schedule %q invalid script path %q (relative path, .py/.js/.mjs only)", id, script)
		}
		if len(o.ScriptArgs) > 0 && strings.TrimSpace(string(o.ScriptArgs)) != "null" {
			var rawArgs []json.RawMessage
			if err := json.Unmarshal(o.ScriptArgs, &rawArgs); err != nil {
				return Schedule{}, fmt.Errorf("schedule %q invalid scriptArgs", id)
			}
			if len(rawArgs) > maxScriptArgs {
				return Schedule{}, fmt.Errorf("schedule %q too many scriptArgs", id)
			}
			for _, raw := range rawArgs {
				var a string
				if err := json.Unmarshal(raw, &a); err != nil || len(a) > maxScriptArgLen {
					return Schedule{}, fmt.Errorf("schedule %q invalid scriptArgs", id)
				}
				scriptArgs = append(scriptArgs, a)
			}
		}
		if len(o.Commit) > 0 {
			switch strings.TrimSpace(string(o.Commit)) {
			case "true":
				v := true
				commit = &v
			case "false":
				v := false
				commit = &v
			default:
				return Schedule{}, fmt.Errorf("schedule %q invalid commit", id)
			}
		}
	}

	hasCron := strings.TrimSpace(o.Cron) != ""
	hasInterval := strings.TrimSpace(o.Interval) != ""
	hasAt := strings.TrimSpace(o.At) != ""
	hasOnce := strings.TrimSpace(o.Once) != ""
	triggerCount := 0
	for _, has := range []bool{hasCron, hasInterval, hasAt, hasOnce} {
		if has {
			triggerCount++
		}
	}
	if triggerCount == 0 {
		return Schedule{}, fmt.Errorf("schedule %q needs one of cron/interval/at/once", id)
	}
	if triggerCount > 1 {
		return Schedule{}, fmt.Errorf("schedule %q must specify exactly one of cron/interval/at/once", id)
	}

	tz := strings.TrimSpace(o.Timezone)
	if tz == "" {
		tz = DefaultTZ
	}
	if !IsValidTimezone(tz) {
		return Schedule{}, fmt.Errorf("schedule %q invalid timezone %q", id, tz)
	}

	entry := Schedule{ID: id, Enabled: true, Timezone: tz}
	if o.Enabled != nil {
		entry.Enabled = *o.Enabled
	}
	if hasScript {
		entry.Script = script
		entry.ScriptArgs = scriptArgs
		entry.Commit = commit
	} else {
		entry.Message = msg
	}

	if hasCron {
		entry.Cron = o.Cron
	}
	if hasInterval {
		if !intervalRe.MatchString(o.Interval) {
			return Schedule{}, fmt.Errorf("schedule %q invalid interval %q", id, o.Interval)
		}
		unit := o.Interval[len(o.Interval)-1]
		n, _ := strconv.Atoi(o.Interval[:len(o.Interval)-1])
		if n < 1 || (unit == 'm' && n > 59) || (unit == 'h' && n > 24) || (unit == 'd' && n != 1) {
			return Schedule{}, fmt.Errorf("schedule %q invalid interval %q: minutes 1-59, hours 1-24, days only \"1d\"", id, o.Interval)
		}
		entry.Interval = o.Interval
	}
	if hasAt {
		if _, _, ok := parseHM(o.At); !ok {
			return Schedule{}, fmt.Errorf("schedule %q invalid at %q", id, o.At)
		}
		entry.At = o.At
	}
	if hasOnce {
		if _, ok := ParseOnceEpoch(o.Once, tz); !ok {
			return Schedule{}, fmt.Errorf("schedule %q invalid once %q", id, o.Once)
		}
		entry.Once = o.Once
	}
	if strings.TrimSpace(o.Project) != "" {
		entry.Project = o.Project
	}

	if o.Retry != nil {
		if o.Retry.Max < 0 {
			return Schedule{}, fmt.Errorf("schedule %q invalid retry.max", id)
		}
		if o.Retry.BackoffMs < 0 {
			return Schedule{}, fmt.Errorf("schedule %q invalid retry.backoffMs", id)
		}
		if o.Retry.Max > RetryMaxLimit {
			return Schedule{}, fmt.Errorf("schedule %q retry.max exceeds %d", id, RetryMaxLimit)
		}
		if o.Retry.BackoffMs > RetryBackoffMsLimit {
			return Schedule{}, fmt.Errorf("schedule %q retry.backoffMs exceeds %dms", id, RetryBackoffMsLimit)
		}
		entry.Retry = &Retry{Max: o.Retry.Max, BackoffMs: o.Retry.BackoffMs}
	}

	if o.SilentHours != nil {
		if _, _, ok := parseHM(o.SilentHours.From); !ok {
			return Schedule{}, fmt.Errorf("schedule %q invalid silentHours", id)
		}
		if _, _, ok := parseHM(o.SilentHours.To); !ok {
			return Schedule{}, fmt.Errorf("schedule %q invalid silentHours", id)
		}
		entry.SilentHours = &SilentHours{From: o.SilentHours.From, To: o.SilentHours.To}
	}

	if o.Recipient != nil {
		switch o.Recipient.Source {
		case push.SourceWeChat, push.SourceTelegram, push.SourceFeishu:
		default:
			return Schedule{}, fmt.Errorf("schedule %q invalid recipient.source", id)
		}
		if o.Recipient.UserID == "" {
			return Schedule{}, fmt.Errorf("schedule %q invalid recipient.userId", id)
		}
		r := *o.Recipient
		entry.Recipient = &r
	}

	// holidayMode ordering matters: accept the value first, then reject the incompatible
	// combination, then run the final sanity compile.
	if o.HolidayMode == HolidayModeWorkday {
		entry.HolidayMode = HolidayModeWorkday
	}
	if hasOnce && entry.HolidayMode == HolidayModeWorkday {
		return Schedule{}, fmt.Errorf("schedule %q holidayMode:\"workday\" cannot be combined with once", id)
	}
	if !hasOnce {
		if entry.HolidayMode == HolidayModeWorkday && entry.Cron != "" {
			if _, err := CompileWorkdayCron(entry.Cron); err != nil {
				return Schedule{}, fmt.Errorf("schedule %q %v", id, err)
			}
		} else if _, err := CompileSchedule(entry); err != nil {
			return Schedule{}, fmt.Errorf("schedule %q %v", id, err)
		}
	}
	return entry, nil
}

// Validate checks one entry without mutating it.
func Validate(s Schedule) error {
	_, err := Normalize(s)
	return err
}

// Normalize validates and normalizes an already-typed entry (defaults applied, unused fields
// dropped) so a read → write round trip is lossless.
func Normalize(s Schedule) (Schedule, error) {
	raw := rawSchedule{
		ID:          s.ID,
		Enabled:     &s.Enabled,
		Cron:        s.Cron,
		Interval:    s.Interval,
		At:          s.At,
		Once:        s.Once,
		Timezone:    s.Timezone,
		Project:     s.Project,
		Retry:       s.Retry,
		SilentHours: s.SilentHours,
		HolidayMode: s.HolidayMode,
		Recipient:   s.Recipient,
	}
	if s.Message != "" {
		msg := s.Message
		raw.Message = &msg
	}
	if s.Script != "" {
		script := s.Script
		raw.Script = &script
		if len(s.ScriptArgs) > 0 {
			encoded, err := json.Marshal(s.ScriptArgs)
			if err != nil {
				return Schedule{}, fmt.Errorf("schedule %q invalid scriptArgs", s.ID)
			}
			raw.ScriptArgs = encoded
		}
		if s.Commit != nil {
			raw.Commit = json.RawMessage(strconv.FormatBool(*s.Commit))
		}
	}
	return normalizeRaw(raw)
}

// ParseScheduleJSON parses and normalizes one raw schedules.json entry.
func ParseScheduleJSON(raw []byte) (Schedule, error) {
	var o rawSchedule
	if err := json.Unmarshal(raw, &o); err != nil {
		return Schedule{}, err
	}
	return normalizeRaw(o)
}

// NormalizeSchedules validates a list, rejecting duplicate ids (a write is all-or-nothing).
func NormalizeSchedules(items []Schedule) ([]Schedule, error) {
	ids := map[string]bool{}
	out := make([]Schedule, 0, len(items))
	for _, item := range items {
		e, err := Normalize(item)
		if err != nil {
			return nil, err
		}
		if ids[e.ID] {
			return nil, fmt.Errorf("duplicate schedule id %q", e.ID)
		}
		ids[e.ID] = true
		out = append(out, e)
	}
	return out, nil
}

// LoadSchedules reads and normalizes schedules.json. A missing/invalid file yields an empty list;
// individual malformed entries are skipped with a warning so one bad entry cannot break the loop.
func LoadSchedules() ([]Schedule, error) {
	file := config.C().SchedulesFile
	raw, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return []Schedule{}, nil
		}
		warnf("failed to read schedules.json: %v", err)
		return []Schedule{}, nil
	}
	var doc struct {
		Schedules []json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		warnf("failed to read schedules.json: %v", err)
		return []Schedule{}, nil
	}
	if doc.Schedules == nil {
		warnf("schedules.json missing 'schedules' array")
		return []Schedule{}, nil
	}
	out := make([]Schedule, 0, len(doc.Schedules))
	ids := map[string]bool{}
	for _, item := range doc.Schedules {
		e, err := ParseScheduleJSON(item)
		if err != nil {
			warnf("skipping invalid entry: %v", err)
			continue
		}
		if ids[e.ID] {
			warnf("duplicate schedule id %q, skipping", e.ID)
			continue
		}
		ids[e.ID] = true
		out = append(out, e)
	}
	return out, nil
}

var (
	savedMu       sync.RWMutex
	savedListener func()
)

// SetSavedListener registers a callback fired after a successful schedules.json write; the engine
// registers its reload there so a schedule write wakes the firing loop immediately.
func SetSavedListener(fn func()) {
	savedMu.Lock()
	savedListener = fn
	savedMu.Unlock()
}

func fireSaved() {
	savedMu.RLock()
	fn := savedListener
	savedMu.RUnlock()
	if fn != nil {
		fn()
	}
}

func backupTimestamp() string {
	return time.Now().Format("20060102T150405")
}

func configBackupDir() string {
	return filepath.Join(config.UserConfigDir(), ".backups")
}

// pruneConfigBackups keeps only the newest maxConfigBackups backups of one config file. Holiday
// backups (holidays.json.<epochMs>) are matched by a different pattern and are never pruned here.
func pruneConfigBackups(name string) {
	dir := configBackupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var matched []string
	prefix := name + "."
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		matched = append(matched, e.Name())
	}
	sort.Strings(matched) // ascending → oldest first
	for len(matched) > maxConfigBackups {
		old := matched[0]
		matched = matched[1:]
		_ = os.Remove(filepath.Join(dir, old))
	}
}

// backupConfigFile snapshots the current file into <config>/.backups/<name>.<YYYYMMDDTHHMMSS>.
func backupConfigFile(name, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	dir := configBackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name+"."+backupTimestamp()), body, 0o600); err != nil {
		warnf("backup of %s failed: %v", name, err)
		return
	}
	pruneConfigBackups(name)
}

// writeConfigFile writes content atomically: backup → tmp → rename.
func writeConfigFile(name, path, content string) error {
	backupConfigFile(name, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixMilli())
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SaveSchedules validates + normalizes then atomically writes schedules.json, and wakes the
// firing loop. Writing the normalized entries keeps "what is written" == "what is read back".
func SaveSchedules(items []Schedule) ([]Schedule, error) {
	entries, err := NormalizeSchedules(items)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(map[string]any{"schedules": entries}, "", "  ")
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	if err := writeConfigFile("schedules.json", config.C().SchedulesFile, string(payload)); err != nil {
		return nil, err
	}
	fireSaved()
	return entries, nil
}

// FindSchedule returns the entry with the given id.
func FindSchedule(id string) (Schedule, bool) {
	all, _ := LoadSchedules()
	for _, s := range all {
		if s.ID == id {
			return s, true
		}
	}
	return Schedule{}, false
}
