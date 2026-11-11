// Package webui serves the single-file task-log UI plus the JSON API it talks to.
//
// The HTML is embedded verbatim (it is the v1 UI, unchanged), so only the API has to be
// reimplemented. Everything here is standard library only, binds config.WebUIHost
// (127.0.0.1 by default; 0.0.0.0 in containers), never reads DATA_ROOT and never touches the
// data subprojects.
package webui

// SchedulesBackend is the scheduler surface the WebUI needs. It is injected by the wiring layer
// (cmd/botler) so this package stays independent of the scheduler engine and can be unit-tested
// without compiling the whole runtime.
//
// The JSON shapes are pass-through: the scheduler package already produces the v1 shapes
// (ScheduleEntry array, ScheduleOverviewItem array, ScheduleHistoryEntry array, LogPage,
// ScheduleRunStats), so the WebUI only routes requests and serializes the result.
type SchedulesBackend struct {
	// Load returns the raw schedule entries from schedules.json (the WebUI renders them as-is).
	Load func() ([]any, error)
	// Save validates, normalizes and atomically writes the entries.
	Save func(items []any) error
	// Reload wakes the scheduling loop so a save takes effect immediately.
	Reload func()
	// NextFire computes the next fire epoch (ms) for an unsaved candidate entry.
	// hasNext=false means the entry can never fire again (a spent `once`).
	NextFire func(spec map[string]any, afterMs int64) (epochMs int64, hasNext bool, err error)
	// Overview returns []ScheduleOverviewItem.
	Overview func() any
	// HistoryIndex returns []ScheduleHistoryEntry (schedules still present in the logs only).
	HistoryIndex func() any
	// Runs returns one LogPage of a schedule's runs.
	Runs func(id string, limit, offset int) any
	// Stats returns the ScheduleRunStats of one schedule.
	Stats func(id string) any
}
