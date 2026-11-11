package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/scheduler"
)

var scheduleSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list","create","update","delete","enable","disable"],
      "description": "list=view all scheduled tasks; create=create; update=modify; delete=delete; enable/disable=enable/disable"},
    "id": {"type": "string", "description": "Scheduled task id (short, e.g. drink-water). Required for update/delete/enable/disable; auto-generated if omitted on create"},
    "message": {"type": "string", "description": "Message content sent to the Agent when the task fires (exactly one of message/script)"},
    "script": {"type": "string", "description": "Run an existing in-project script directly when the task fires, bypassing the LLM (exactly one of message/script). Path relative to DATA_ROOT, e.g. 'notes/scripts/build.py' (.py/.js/.mjs only)."},
    "scriptArgs": {"type": "array", "items": {"type": "string"}, "description": "Arguments passed to the script (optional), e.g. ['--all']"},
    "commit": {"type": "boolean", "description": "Auto git-commit the script's data changes on success (default true; set false for read-only scripts)"},
    "retryMax": {"type": "integer", "description": "Failure retry count (0 = no retry, the default). For idempotent scripts set >0 so transient failures self-recover; for non-idempotent scripts keep 0."},
    "retryBackoffMs": {"type": "integer", "description": "Fixed backoff between retries, in ms (used only when retryMax > 0)"},
    "cron": {"type": "string", "description": "5-field cron expression, e.g. '0 8 * * *' (recurring; one of four for create)"},
    "interval": {"type": "string", "description": "Simple interval, e.g. '5m' / '2h' / '1d' (recurring; one of four for create)"},
    "at": {"type": "string", "description": "Fixed DAILY time 'HH:MM' — repeats every day. Do NOT use for one-off reminders; use once instead."},
    "once": {"type": "string", "description": "ONE-SHOT absolute date-time in ISO 8601, e.g. '2026-08-20T22:00:00+08:00' — fires exactly once then becomes inert. MUST include the full date; compute it from today's date."},
    "timezone": {"type": "string", "description": "IANA timezone, default Asia/Shanghai"},
    "project": {"type": "string", "description": "Routing hint (data subproject name); when set, routing is skipped on fire and the project runs directly"},
    "holidayMode": {"type": "string", "enum": ["workday","off"], "description": "China legal-workday gating for cron/interval/at triggers. 'workday' = fire only on China legal workdays (skip statutory holidays, include makeup workdays; cron date fields are ignored). Cannot be combined with 'once'."}
  },
  "required": ["action"]
}`)

type scheduleArgs struct {
	Action         string   `json:"action"`
	ID             string   `json:"id"`
	Message        string   `json:"message"`
	Script         string   `json:"script"`
	ScriptArgs     []string `json:"scriptArgs"`
	Commit         *bool    `json:"commit"`
	RetryMax       *int     `json:"retryMax"`
	RetryBackoffMs *int     `json:"retryBackoffMs"`
	Cron           string   `json:"cron"`
	Interval       string   `json:"interval"`
	At             string   `json:"at"`
	Once           string   `json:"once"`
	Timezone       string   `json:"timezone"`
	Project        *string  `json:"project"`
	HolidayMode    string   `json:"holidayMode"`
}

type scheduleTool struct{}

func newScheduleTool() agentrt.Tool { return scheduleTool{} }

func (scheduleTool) Name() string  { return "schedule" }
func (scheduleTool) Label() string { return "Manage scheduled tasks" }
func (scheduleTool) Description() string {
	return "Create / view / modify / delete / enable / disable scheduled tasks (writes the fixed schedule config, takes effect immediately). Use when the user says things like 'remind me every morning at 8am…', 'summarize every 2 hours…', 'remind me in X minutes…' that contain scheduling / reminder / calendar intent. create must specify exactly one trigger plus exactly one action: a non-empty `message` OR a `script` (an existing in-project script path relative to DATA_ROOT, run directly on fire; optionally `scriptArgs` and `commit`). Four trigger types: (1) cron — 5-field cron for complex recurring schedules; (2) interval — simple '5m'/'2h'/'1d'; (3) at — a DAILY fixed time 'HH:MM' that REPEATS EVERY DAY; (4) once — a ONE-SHOT absolute date-time (ISO 8601, MUST include the full date). CRITICAL DISTINCTION: for one-off reminders like 'tonight 10pm' or 'tomorrow 3pm' you MUST use 'once'; use 'at' only when the user explicitly says 'every day'. After creating or modifying, confirm the next fire time to the user."
}

func (scheduleTool) Schema() json.RawMessage { return scheduleSchema }

func triggerOf(e scheduler.Schedule) string {
	switch {
	case e.Cron != "":
		return "cron " + e.Cron
	case e.Interval != "":
		return "interval " + e.Interval
	case e.At != "":
		return "at " + e.At + " (daily)"
	case e.Once != "":
		return "once " + e.Once
	}
	return "(no trigger)"
}

func fmtFire(epochMs int64, tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		loc = time.UTC
	}
	return time.UnixMilli(epochMs).In(loc).Format("1/2/2006, 15:04:05")
}

func nextFireOf(e scheduler.Schedule) string {
	next, err := scheduler.NextFireEpoch(e, time.Now().UnixMilli())
	if err != nil || next >= scheduler.NoFire {
		return ""
	}
	return fmtFire(next, e.Timezone)
}

func textResult(text string, details any) (agentrt.ToolResult, error) {
	if details == nil {
		details = map[string]any{}
	}
	return agentrt.ToolResult{Text: text, Details: details}, nil
}

func (scheduleTool) Execute(_ context.Context, raw json.RawMessage) (agentrt.ToolResult, error) {
	var args scheduleArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agentrt.ToolResult{}, err
	}
	entries, err := scheduler.LoadSchedules()
	if err != nil {
		return agentrt.ToolResult{}, err
	}

	switch args.Action {
	case "list":
		if len(entries) == 0 {
			return textResult("There are no scheduled tasks right now.", map[string]any{"count": 0})
		}
		lines := make([]string, 0, len(entries))
		for _, e := range entries {
			fire := ""
			if !e.Enabled {
				fire = " disabled"
			} else if next := nextFireOf(e); next != "" {
				fire = " next-fire=" + next
			} else if e.Once != "" {
				fire = " (expired)"
			}
			enabled := "enabled"
			if !e.Enabled {
				enabled = "disabled"
			}
			actionDesc := "msg=" + e.Message
			if e.Script != "" {
				actionDesc = "▶ " + e.Script
			}
			rec := ""
			if e.Recipient != nil {
				rec = " push=" + string(e.Recipient.Source) + ":" + e.Recipient.UserID
			}
			proj := ""
			if e.Project != "" {
				proj = " project=" + e.Project
			}
			hm := ""
			if e.HolidayMode != "" {
				hm = " holidayMode=" + e.HolidayMode
			}
			rt := ""
			if e.Retry != nil {
				rt = " retry=" + strconv.Itoa(e.Retry.Max)
			}
			lines = append(lines, fmt.Sprintf("- %s [%s] %s %s%s tz=%s%s%s%s%s",
				e.ID, enabled, triggerOf(e), actionDesc, rt, e.Timezone, proj, rec, hm, fire))
		}
		return textResult(fmt.Sprintf("Scheduled tasks (%d):\n%s", len(entries), strings.Join(lines, "\n")),
			map[string]any{"count": len(entries)})

	case "create":
		id := strings.TrimSpace(args.ID)
		if id == "" {
			id = "sched-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
		}
		message := strings.TrimSpace(args.Message)
		script := strings.TrimSpace(args.Script)
		if message == "" && script == "" {
			return agentrt.ToolResult{}, fmt.Errorf("create requires a non-empty message OR script")
		}
		if message != "" && script != "" {
			return agentrt.ToolResult{}, fmt.Errorf("create must specify exactly one of message/script")
		}
		tz := args.Timezone
		if tz == "" {
			tz = scheduler.DefaultTZ
		}
		if !scheduler.IsValidTimezone(tz) {
			return agentrt.ToolResult{}, fmt.Errorf("invalid timezone %q", tz)
		}
		triggers := make([][2]string, 0, 4)
		if args.Cron != "" {
			triggers = append(triggers, [2]string{"cron", args.Cron})
		}
		if args.Interval != "" {
			triggers = append(triggers, [2]string{"interval", args.Interval})
		}
		if args.At != "" {
			triggers = append(triggers, [2]string{"at", args.At})
		}
		if args.Once != "" {
			triggers = append(triggers, [2]string{"once", args.Once})
		}
		if len(triggers) != 1 {
			return agentrt.ToolResult{}, fmt.Errorf("create must specify exactly one of cron / interval / at / once")
		}
		if triggers[0][0] == "once" {
			t, ok := scheduler.ParseOnceEpoch(triggers[0][1], tz)
			if !ok {
				return agentrt.ToolResult{}, fmt.Errorf("invalid once value %q; use ISO 8601 date-time, e.g. 2026-08-20T22:00:00+08:00", triggers[0][1])
			}
			if t <= time.Now().UnixMilli() {
				return agentrt.ToolResult{}, fmt.Errorf("once time must be in the future (got %s); use a full date-time, e.g. 2026-08-20T22:00:00+08:00", triggers[0][1])
			}
		}
		for _, e := range entries {
			if e.ID == id {
				return agentrt.ToolResult{}, fmt.Errorf("scheduled task id already exists: %s. Use update to modify it", id)
			}
		}
		entry := scheduler.Schedule{ID: id, Enabled: true, Timezone: tz}
		switch triggers[0][0] {
		case "cron":
			entry.Cron = triggers[0][1]
		case "interval":
			entry.Interval = triggers[0][1]
		case "at":
			entry.At = triggers[0][1]
		case "once":
			entry.Once = triggers[0][1]
		}
		if script != "" {
			entry.Script = script
			entry.ScriptArgs = args.ScriptArgs
			entry.Commit = args.Commit
		} else {
			entry.Message = message
		}
		retryMax := 0
		retryBackoff := 0
		if args.RetryMax != nil {
			retryMax = *args.RetryMax
		}
		if args.RetryBackoffMs != nil {
			retryBackoff = *args.RetryBackoffMs
		}
		if retryMax > 0 {
			entry.Retry = &scheduler.Retry{Max: retryMax, BackoffMs: int64(retryBackoff)}
		}
		if args.Project != nil {
			if p := strings.TrimSpace(*args.Project); p != "" {
				entry.Project = p
			}
		}
		if args.HolidayMode == scheduler.HolidayModeWorkday {
			entry.HolidayMode = scheduler.HolidayModeWorkday
		}
		// The push recipient is injected by the framework from the current message sender.
		if ctx := GetTaskContext(); ctx != nil && ctx.Recipient != nil {
			r := *ctx.Recipient
			entry.Recipient = &r
		}
		saved, err := scheduler.SaveSchedules(append(entries, entry))
		if err != nil {
			return agentrt.ToolResult{}, err
		}
		var created *scheduler.Schedule
		for i := range saved {
			if saved[i].ID == id {
				created = &saved[i]
			}
		}
		if created == nil {
			return agentrt.ToolResult{}, fmt.Errorf("failed to create: saved schedule %s not found", id)
		}
		next := nextFireOf(*created)
		suffix := ""
		if next != "" {
			suffix = ", next fire: " + next
		}
		return textResult(fmt.Sprintf(`Created scheduled task "%s": %s (timezone %s)%s`, created.ID, triggerOf(*created), created.Timezone, suffix),
			map[string]any{"id": created.ID})

	case "delete", "enable", "disable", "update":
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return agentrt.ToolResult{}, fmt.Errorf("%s requires an id", args.Action)
		}
		idx := -1
		for i := range entries {
			if entries[i].ID == id {
				idx = i
			}
		}
		if idx == -1 {
			return agentrt.ToolResult{}, fmt.Errorf("scheduled task %s not found", id)
		}
		if args.Action == "delete" {
			removed := entries[idx]
			rest := append(append([]scheduler.Schedule{}, entries[:idx]...), entries[idx+1:]...)
			if _, err := scheduler.SaveSchedules(rest); err != nil {
				return agentrt.ToolResult{}, err
			}
			return textResult(fmt.Sprintf(`Deleted scheduled task "%s".`, removed.ID), map[string]any{"id": id})
		}
		if args.Action == "enable" || args.Action == "disable" {
			entries[idx].Enabled = args.Action == "enable"
			if _, err := scheduler.SaveSchedules(entries); err != nil {
				return agentrt.ToolResult{}, err
			}
			msg := "Disabled scheduled task \"" + id + "\"."
			if args.Action == "enable" {
				msg = "Enabled scheduled task \"" + id + "\"."
				if next := nextFireOf(entries[idx]); next != "" {
					msg += " next fire: " + next
				}
			}
			return textResult(msg, map[string]any{"id": id})
		}
		return scheduleUpdate(entries, idx, args)
	}
	return agentrt.ToolResult{}, fmt.Errorf("unknown action %q", args.Action)
}

func scheduleUpdate(entries []scheduler.Schedule, idx int, args scheduleArgs) (agentrt.ToolResult, error) {
	current := &entries[idx]
	id := current.ID
	newMsg := strings.TrimSpace(args.Message)
	newScript := strings.TrimSpace(args.Script)
	if newMsg != "" && newScript != "" {
		return agentrt.ToolResult{}, fmt.Errorf("update must not set both message and script; pick exactly one action")
	}
	wasScript := current.Script != ""
	if newScript != "" {
		current.Script = newScript
		current.Message = ""
		current.ScriptArgs = nil
		current.Commit = nil
		if !wasScript {
			current.Retry = nil
		}
	} else if newMsg != "" {
		current.Message = newMsg
		current.Script = ""
		current.ScriptArgs = nil
		current.Commit = nil
		if wasScript {
			current.Retry = nil
		}
	}
	if current.Script != "" {
		if args.ScriptArgs != nil {
			current.ScriptArgs = args.ScriptArgs
		}
		if args.Commit != nil {
			current.Commit = args.Commit
		}
	} else {
		current.ScriptArgs = nil
		current.Commit = nil
	}
	if args.RetryMax != nil || args.RetryBackoffMs != nil {
		rmax := 0
		rms := 0
		if current.Retry != nil {
			rmax = current.Retry.Max
			rms = int(current.Retry.BackoffMs)
		}
		if args.RetryMax != nil {
			rmax = *args.RetryMax
		}
		if args.RetryBackoffMs != nil {
			rms = *args.RetryBackoffMs
		}
		if rmax > 0 {
			current.Retry = &scheduler.Retry{Max: rmax, BackoffMs: int64(rms)}
		} else {
			current.Retry = nil
		}
	}
	if args.Timezone != "" {
		if !scheduler.IsValidTimezone(args.Timezone) {
			return agentrt.ToolResult{}, fmt.Errorf("invalid timezone %q", args.Timezone)
		}
		current.Timezone = args.Timezone
	}
	// v1 clears the project when the client sends an explicit empty string.
	if args.Project != nil {
		current.Project = strings.TrimSpace(*args.Project)
	}
	if args.HolidayMode != "" {
		if args.HolidayMode == scheduler.HolidayModeWorkday {
			current.HolidayMode = scheduler.HolidayModeWorkday
		} else {
			current.HolidayMode = ""
		}
	}
	var onceToCheck string
	switch {
	case args.Cron != "":
		current.Cron, current.Interval, current.At, current.Once = args.Cron, "", "", ""
	case args.Interval != "":
		current.Interval, current.Cron, current.At, current.Once = args.Interval, "", "", ""
	case args.At != "":
		current.At, current.Cron, current.Interval, current.Once = args.At, "", "", ""
	case args.Once != "":
		current.Once, current.Cron, current.Interval, current.At = args.Once, "", "", ""
		onceToCheck = args.Once
	}
	if onceToCheck != "" {
		tz := current.Timezone
		if tz == "" {
			tz = scheduler.DefaultTZ
		}
		t, ok := scheduler.ParseOnceEpoch(onceToCheck, tz)
		if !ok {
			return agentrt.ToolResult{}, fmt.Errorf("invalid once value %q; use ISO 8601 date-time, e.g. 2026-08-20T22:00:00+08:00", onceToCheck)
		}
		if t <= time.Now().UnixMilli() {
			return agentrt.ToolResult{}, fmt.Errorf("once time must be in the future (got %s)", onceToCheck)
		}
	}
	if current.Message == "" && current.Script == "" {
		return agentrt.ToolResult{}, fmt.Errorf("schedule must have a message or script")
	}
	saved, err := scheduler.SaveSchedules(entries)
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	var updated *scheduler.Schedule
	for i := range saved {
		if saved[i].ID == id {
			updated = &saved[i]
		}
	}
	msg := "Updated scheduled task \"" + id + "\""
	if updated != nil {
		msg += ": " + triggerOf(*updated)
		if next := nextFireOf(*updated); next != "" {
			msg += ", next fire: " + next
		}
	}
	return textResult(msg, map[string]any{"id": id})
}
