// Package scheduler is the in-process scheduler: it reads schedules.json, computes the next
// fire of each enabled entry, and fires due entries into the dispatch pipeline.
//
// A schedule is one of four mutually-exclusive trigger forms (cron / interval / at / once), one
// action (message → LLM pipeline, or script → direct run bypassing the LLM), plus an optional
// timezone, project hint, retry policy, do-not-disturb window and push recipient. The config is
// externalized to ~/.botler-agent/schedules.json (overridable via BOTLER_SCHEDULES_FILE).
package scheduler

import "github.com/crossoverJie/botler-agent/internal/push"

// DefaultTZ is the timezone used when an entry does not specify one.
const DefaultTZ = "Asia/Shanghai"

// HolidayModeWorkday gates a cron / interval / at trigger to China legal workdays.
const HolidayModeWorkday = "workday"

// Retry is the optional failure-retry policy of an entry.
type Retry struct {
	// Max is the number of retries after a failure (0 = no retry).
	Max int `json:"max"`
	// BackoffMs is the fixed backoff between retries.
	BackoffMs int64 `json:"backoffMs"`
}

// SilentHours is the optional do-not-disturb window ("HH:MM" in the entry timezone).
type SilentHours struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Schedule is one schedules.json entry.
//
// Field order matters: it mirrors v1's normalizeEntry output order so a read → write round trip
// produces the same key order as the TypeScript implementation.
type Schedule struct {
	// ID is the stable unique id (taskId source, dedup prefix, WebUI row key).
	ID string `json:"id"`
	// Enabled reports whether the entry is active (default true).
	Enabled bool `json:"enabled"`
	// Timezone is the IANA timezone the trigger is interpreted in (default Asia/Shanghai).
	Timezone string `json:"timezone"`
	// Script is the script path relative to DATA_ROOT (direct run, no LLM).
	Script string `json:"script,omitempty"`
	// ScriptArgs are forwarded verbatim to the interpreter (no shell).
	ScriptArgs []string `json:"scriptArgs,omitempty"`
	// Commit auto-commits the script's data changes on success (default true for script entries).
	Commit *bool `json:"commit,omitempty"`
	// Message is the instruction sent through the LLM pipeline when the entry fires.
	Message string `json:"message,omitempty"`
	// Cron is a 5-field cron expression.
	Cron string `json:"cron,omitempty"`
	// Interval is a simple interval: "5m" / "2h" / "1d" (minute granularity, wall-clock aligned).
	Interval string `json:"interval,omitempty"`
	// At is a daily fixed time "HH:MM" in the entry timezone.
	At string `json:"at,omitempty"`
	// Once is a one-shot absolute ISO 8601 instant; after firing the entry becomes inert.
	Once string `json:"once,omitempty"`
	// Project is an optional routing hint (a valid data subproject name).
	Project string `json:"project,omitempty"`
	// Retry is the optional failure-retry policy.
	Retry *Retry `json:"retry,omitempty"`
	// SilentHours defers fires landing inside the window to the window end.
	SilentHours *SilentHours `json:"silentHours,omitempty"`
	// Recipient is the optional push target (with channel fallback).
	Recipient *push.Recipient `json:"recipient,omitempty"`
	// HolidayMode is "workday" to fire only on China legal workdays (cron/interval/at only).
	HolidayMode string `json:"holidayMode,omitempty"`
}

// Trigger returns the entry's single trigger value (cron > interval > at > once).
func (s Schedule) Trigger() string {
	switch {
	case s.Cron != "":
		return s.Cron
	case s.Interval != "":
		return s.Interval
	case s.At != "":
		return s.At
	}
	return s.Once
}
