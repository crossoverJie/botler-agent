package logs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// dayFileRe is the strict day-file name (YYYY-MM-DD.jsonl). Used to reject path traversal and
// off-directory deletions.
var dayFileRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

// maxLineSize bounds a single JSONL line; a rendered system prompt plus a large tool result can
// easily exceed bufio's default 64KB token limit.
const maxLineSize = 32 << 20

func logDir() string { return config.C().LogDir }

// fileForDay renders the day-file name of a timestamp in local time.
func fileForDay(ts int64) string {
	return time.UnixMilli(ts).Format("2006-01-02") + ".jsonl"
}

// AppendTaskLog appends one log as a JSON line. The dispatcher serializes writes through its
// sequential queue, so lines never interleave. Failures are logged only: logging must never
// break the main flow.
func AppendTaskLog(entry TaskLog) {
	if err := appendTaskLog(entry); err != nil {
		warnf("[logging] appendTaskLog failed: %v", err)
	}
}

func appendTaskLog(entry TaskLog) error {
	dir := logDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, fileForDay(entry.StartedAt)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(body, '\n')); err != nil {
		return err
	}
	return nil
}

// forEachLine streams one day file, skipping corrupt lines instead of failing the whole query.
func forEachLine(path string, visit func(TaskLog)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l TaskLog
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			continue
		}
		visit(l)
	}
}

// allDayFiles lists every day file, sorted ascending.
func allDayFiles() []string {
	entries, err := os.ReadDir(logDir())
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !dayFileRe.MatchString(e.Name()) {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	return files
}

// dayFilesInRange lists day files whose date falls within [from, to], sorted descending.
func dayFilesInRange(from, to *int64) []string {
	files := allDayFiles()
	if from != nil {
		lo := fileForDay(*from)
		kept := files[:0]
		for _, f := range files {
			if f >= lo {
				kept = append(kept, f)
			}
		}
		files = kept
	}
	if to != nil {
		hi := fileForDay(*to)
		kept := files[:0]
		for _, f := range files {
			if f <= hi {
				kept = append(kept, f)
			}
		}
		files = kept
	}
	desc := make([]string, len(files))
	for i, f := range files {
		desc[len(files)-1-i] = f
	}
	return desc
}

// LogQuery selects task logs. Limit <= 0 falls back to 50 (the WebUI default page size).
type LogQuery struct {
	From *int64
	To   *int64
	// Project matches either the plural `projects` set or the legacy singular `project`.
	Project string
	Source  string
	// ScheduleID matches dispatches whose taskId is "schedule:<id>:<epoch>".
	ScheduleID string
	Phase      string
	// Q is a case-insensitive keyword searched in userMessage, replyText and the project names.
	Q      string
	Limit  int
	Offset int
}

// LogPage is one page of query results.
type LogPage struct {
	Logs  []TaskLog `json:"logs"`
	Total int       `json:"total"`
}

// scheduleIDRe extracts the schedule id from taskId "schedule:<id>:<epoch>" (greedy so ids
// containing ":" still work).
var scheduleIDRe = regexp.MustCompile(`^schedule:(.+):\d+$`)

// ScheduleIDFromTaskID extracts the schedule id from a scheduler task id.
func ScheduleIDFromTaskID(taskID string) (string, bool) {
	m := scheduleIDRe.FindStringSubmatch(taskID)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// MatchesLogQuery reports whether a log satisfies the filter part of q.
func MatchesLogQuery(l TaskLog, q LogQuery) bool {
	if q.Project != "" {
		matched := false
		if len(l.Projects) > 0 {
			for _, p := range l.Projects {
				if p == q.Project {
					matched = true
					break
				}
			}
		} else if l.Project != nil && *l.Project == q.Project {
			matched = true
		}
		if !matched {
			return false
		}
	}
	if q.Source != "" && l.Source != q.Source {
		return false
	}
	if q.Phase != "" && l.Phase != q.Phase {
		return false
	}
	if q.ScheduleID != "" {
		id, ok := ScheduleIDFromTaskID(l.TaskID)
		if !ok || id != q.ScheduleID {
			return false
		}
	}
	if q.Q != "" {
		needle := strings.ToLower(q.Q)
		names := make([]string, 0, len(l.Projects)+1)
		if l.Project != nil && *l.Project != "" {
			names = append(names, *l.Project)
		}
		names = append(names, l.Projects...)
		projectText := strings.ToLower(strings.Join(names, " "))
		if !strings.Contains(strings.ToLower(l.UserMessage), needle) &&
			!strings.Contains(strings.ToLower(l.ReplyText), needle) &&
			!strings.Contains(projectText, needle) {
			return false
		}
	}
	return true
}

// FilterAndPageLogs filters, sorts (newest first) and pages an in-memory log slice.
func FilterAndPageLogs(all []TaskLog, q LogQuery) LogPage {
	filtered := make([]TaskLog, 0, len(all))
	for _, l := range all {
		if MatchesLogQuery(l, q) {
			filtered = append(filtered, l)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].StartedAt > filtered[j].StartedAt })
	total := len(filtered)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if offset >= len(filtered) {
		return LogPage{Logs: []TaskLog{}, Total: total}
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return LogPage{Logs: filtered[offset:end], Total: total}
}

// QueryLogs streams every day file in the requested range and returns one page.
func QueryLogs(q LogQuery) LogPage {
	var matched []TaskLog
	for _, f := range dayFilesInRange(q.From, q.To) {
		forEachLine(filepath.Join(logDir(), f), func(l TaskLog) {
			if MatchesLogQuery(l, q) {
				matched = append(matched, l)
			}
		})
	}
	return FilterAndPageLogs(matched, q)
}

// GetLog finds one log by id, scanning newest files first. Line-by-line, never a whole-file read.
func GetLog(id string) (*TaskLog, bool) {
	if id == "" {
		return nil, false
	}
	files := allDayFiles()
	for i := len(files) - 1; i >= 0; i-- {
		var found *TaskLog
		forEachLine(filepath.Join(logDir(), files[i]), func(l TaskLog) {
			if found == nil && l.ID == id {
				cp := l
				found = &cp
			}
		})
		if found != nil {
			return found, true
		}
	}
	return nil, false
}

// TaskSummary is the aggregate shown by the WebUI summary cards.
type TaskSummary struct {
	TotalTasks    int             `json:"totalTasks"`
	ByStatus      map[string]int  `json:"byStatus"`
	ByProject     map[string]int  `json:"byProject"`
	BySource      map[string]int  `json:"bySource"`
	Token         SummaryToken    `json:"token"`
	ModelCache    ModelCacheStats `json:"modelCache"`
	AvgDurationMs int             `json:"avgDurationMs"`
}

// SummaryToken is the token aggregate of a Summary.
type SummaryToken struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	CacheRead int `json:"cacheRead"`
	Total     int `json:"total"`
}

// Summary aggregates every log file (no range filter, matching v1).
func Summary() TaskSummary {
	byStatus := map[string]int{}
	byProject := map[string]int{}
	bySource := map[string]int{}
	token := SummaryToken{}
	var durationSum int64
	total := 0
	modelCache := ModelCacheStats{}

	for _, f := range allDayFiles() {
		forEachLine(filepath.Join(logDir(), f), func(l TaskLog) {
			total++
			byStatus[string(l.Status)]++
			names := l.Projects
			if len(names) == 0 {
				if l.Project != nil && *l.Project != "" {
					names = []string{*l.Project}
				} else {
					names = []string{"(none)"}
				}
			}
			for _, p := range names {
				byProject[p]++
			}
			bySource[l.Source]++
			token.Input += l.Usage.Input
			token.Output += l.Usage.Output
			token.CacheRead += l.Usage.CacheRead
			token.Total += l.Usage.Total
			durationSum += l.DurationMs
			// Latest snapshot wins (process-level counter).
			modelCache = l.ModelCache
		})
	}

	avg := int64(0)
	if total > 0 {
		avg = (durationSum + int64(total)/2) / int64(total)
	}
	return TaskSummary{
		TotalTasks:    total,
		ByStatus:      byStatus,
		ByProject:     byProject,
		BySource:      bySource,
		Token:         token,
		ModelCache:    modelCache,
		AvgDurationMs: int(avg),
	}
}

// Granularities for TokenTimeQuery.
const (
	GranularityHour = "hour"
	GranularityDay  = "day"
	GranularityWeek = "week"
)

// TokenTimeQuery selects a token time series.
type TokenTimeQuery struct {
	From *int64
	To   *int64
	// Granularity is hour | day | week (default day).
	Granularity string
	// GroupBy is project | source (empty = a single "(all)" group).
	GroupBy string
}

// TokenBucket is one time bucket of token usage.
type TokenBucket struct {
	Start      int64   `json:"start"`
	End        int64   `json:"end"`
	Tasks      int     `json:"tasks"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Total      int     `json:"total"`
	CostUSD    float64 `json:"costUsd"`
}

// TokenGroup is one group's bucket sequence.
type TokenGroup struct {
	Key     string        `json:"key"`
	Buckets []TokenBucket `json:"buckets"`
}

// TokenSeries is the response of the token-stats endpoint. (v1 calls this type and the builder
// below the same name; Go cannot, so the type keeps the data shape and the builder is the
// function `TokenTimeSeries`.)
type TokenSeries struct {
	Granularity string       `json:"granularity"`
	TZ          string       `json:"tz"`
	From        int64        `json:"from"`
	To          int64        `json:"to"`
	Groups      []TokenGroup `json:"groups"`
}

// tzName reports the server-local IANA zone for display. time.Local often reports the literal
// "Local", so fall back to the /etc/localtime symlink (the same source the OS uses).
func tzName() string {
	if v := os.Getenv("TZ"); v != "" {
		return v
	}
	name := time.Local.String()
	if name != "" && name != "Local" {
		return name
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.Index(target, "zoneinfo/"); idx >= 0 {
			if zone := target[idx+len("zoneinfo/"):]; zone != "" {
				return zone
			}
		}
	}
	return name
}

// bucketStart truncates a timestamp to the start of its hour / day / week (Monday) in local time.
func bucketStart(ts int64, granularity string) int64 {
	t := time.UnixMilli(ts).In(time.Local)
	switch granularity {
	case GranularityHour:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local).UnixMilli()
	case GranularityWeek:
		dow := (int(t.Weekday()) + 6) % 7 // Monday = 0 ... Sunday = 6
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		return day.AddDate(0, 0, -dow).UnixMilli()
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).UnixMilli()
	}
}

func stepMs(granularity string) int64 {
	switch granularity {
	case GranularityHour:
		return 3_600_000
	case GranularityWeek:
		return 7 * 86_400_000
	default:
		return 86_400_000
	}
}

// TokenTimeSeries aggregates token usage into continuous time buckets. Every group gets the same
// bucket sequence so the frontend can align its X axis without handling gaps. Without GroupBy at
// least one "(all)" group is returned so the UI never renders a blank state.
func TokenTimeSeries(q TokenTimeQuery) TokenSeries {
	g := q.Granularity
	switch g {
	case GranularityHour, GranularityDay, GranularityWeek:
	default:
		g = GranularityDay
	}

	var selected []TaskLog
	for _, f := range dayFilesInRange(q.From, q.To) {
		forEachLine(filepath.Join(logDir(), f), func(l TaskLog) {
			selected = append(selected, l)
		})
	}

	lo := int64(time.Now().UnixMilli())
	hi := lo
	if q.From != nil {
		lo = *q.From
	} else if len(selected) > 0 {
		lo = selected[0].StartedAt
		for _, l := range selected {
			if l.StartedAt < lo {
				lo = l.StartedAt
			}
		}
	}
	if q.To != nil {
		hi = *q.To
	} else if len(selected) > 0 {
		hi = selected[0].StartedAt
		for _, l := range selected {
			if l.StartedAt > hi {
				hi = l.StartedAt
			}
		}
	}

	step := stepMs(g)
	first := bucketStart(lo, g)
	var starts []int64
	for t := first; t <= hi; t += step {
		starts = append(starts, t)
	}
	if len(starts) == 0 {
		starts = append(starts, first)
	}
	newBuckets := func() []TokenBucket {
		out := make([]TokenBucket, len(starts))
		for i, s := range starts {
			end := s + step
			if i+1 < len(starts) {
				end = starts[i+1]
			}
			out[i] = TokenBucket{Start: s, End: end}
		}
		return out
	}

	groupOrder := make([]string, 0, 4)
	groups := make(map[string][]TokenBucket, 4)
	groupOf := func(key string) []TokenBucket {
		if arr, ok := groups[key]; ok {
			return arr
		}
		arr := newBuckets()
		groups[key] = arr
		groupOrder = append(groupOrder, key)
		return arr
	}
	if q.GroupBy == "" {
		groupOf("(all)")
	}

	for _, l := range selected {
		if l.StartedAt < lo || l.StartedAt > hi {
			continue
		}
		idx := int((l.StartedAt - first) / step)
		if idx < 0 || idx >= len(starts) {
			continue
		}
		var keys []string
		switch q.GroupBy {
		case "project":
			if len(l.Projects) > 0 {
				keys = l.Projects
			} else if l.Project != nil && *l.Project != "" {
				keys = []string{*l.Project}
			} else {
				keys = []string{"(none)"}
			}
		case "source":
			keys = []string{l.Source}
		default:
			keys = []string{"(all)"}
		}
		for _, k := range keys {
			b := &groupOf(k)[idx]
			b.Tasks++
			b.Input += l.Usage.Input
			b.Output += l.Usage.Output
			b.CacheRead += l.Usage.CacheRead
			b.CacheWrite += l.Usage.CacheWrite
			b.Total += l.Usage.Total
			b.CostUSD += l.Usage.CostUSD
		}
	}

	out := TokenSeries{
		Granularity: g,
		TZ:          tzName(),
		From:        lo,
		To:          hi,
		Groups:      make([]TokenGroup, 0, len(groupOrder)),
	}
	for _, k := range groupOrder {
		out.Groups = append(out.Groups, TokenGroup{Key: k, Buckets: groups[k]})
	}
	return out
}

// LogFileStat is one day-file's size and entry count.
type LogFileStat struct {
	Day     string `json:"day"`
	File    string `json:"file"`
	Bytes   int64  `json:"bytes"`
	Entries int    `json:"entries"`
}

// DiskUsageReport describes the log directory's footprint. (The builder function below keeps
// v1's name `DiskUsage`, so the type cannot use it too.)
type DiskUsageReport struct {
	Dir        string        `json:"dir"`
	TotalBytes int64         `json:"totalBytes"`
	FileCount  int           `json:"fileCount"`
	EntryCount int           `json:"entryCount"`
	OldestDay  *string       `json:"oldestDay"`
	NewestDay  *string       `json:"newestDay"`
	Files      []LogFileStat `json:"files"`
}

// DiskUsage scans the log directory (day files only).
func DiskUsage() DiskUsageReport {
	out := DiskUsageReport{Dir: logDir(), Files: []LogFileStat{}}
	files := allDayFiles()
	for _, f := range files {
		path := filepath.Join(logDir(), f)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		entries := 0
		forEachRawLine(path, func(string) { entries++ })
		out.TotalBytes += st.Size()
		out.EntryCount += entries
		out.Files = append(out.Files, LogFileStat{
			Day:     strings.TrimSuffix(f, ".jsonl"),
			File:    f,
			Bytes:   st.Size(),
			Entries: entries,
		})
	}
	out.FileCount = len(out.Files)
	if len(files) > 0 {
		oldest := strings.TrimSuffix(files[0], ".jsonl")
		newest := strings.TrimSuffix(files[len(files)-1], ".jsonl")
		out.OldestDay = &oldest
		out.NewestDay = &newest
	}
	return out
}

func forEachRawLine(path string, visit func(line string)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			visit(sc.Text())
		}
	}
}

// CleanupOptions selects day files to delete.
type CleanupOptions struct {
	// Before is a "YYYY-MM-DD" day; files strictly older are deleted.
	Before *string
	// Days is an explicit day list (each must be a valid YYYY-MM-DD).
	Days []string
	// DryRun reports what would be deleted without deleting.
	DryRun bool
}

// CleanupResult reports what a cleanup did.
type CleanupResult struct {
	Deleted        []string `json:"deleted"`
	FreedBytes     int64    `json:"freedBytes"`
	RemainingBytes int64    `json:"remainingBytes"`
}

func validDay(day string) bool { return dayFileRe.MatchString(day + ".jsonl") }

// CleanupLogs deletes day files inside the log directory only. Names are strictly validated;
// deletion is never recursive and never outside the log directory.
func CleanupLogs(opts CleanupOptions) CleanupResult {
	usage := DiskUsage()
	candidates := make(map[string]struct{})

	if opts.Before != nil {
		if !validDay(*opts.Before) {
			return CleanupResult{Deleted: []string{}, FreedBytes: 0, RemainingBytes: usage.TotalBytes}
		}
		for _, f := range usage.Files {
			// Fixed-width ISO dates compare lexically.
			if f.Day < *opts.Before {
				candidates[f.File] = struct{}{}
			}
		}
	}
	for _, d := range opts.Days {
		if !validDay(d) {
			continue
		}
		name := d + ".jsonl"
		for _, f := range usage.Files {
			if f.File == name {
				candidates[name] = struct{}{}
			}
		}
	}

	deleted := make([]string, 0, len(candidates))
	var freed int64
	for name := range candidates {
		var bytes int64
		for _, f := range usage.Files {
			if f.File == name {
				bytes = f.Bytes
				break
			}
		}
		if !opts.DryRun {
			if err := os.Remove(filepath.Join(logDir(), name)); err != nil {
				warnf("[logging] cleanup failed for %s: %v", name, err)
				continue
			}
		}
		deleted = append(deleted, name)
		freed += bytes
	}
	sort.Strings(deleted)

	remaining := usage.TotalBytes
	if !opts.DryRun {
		remaining = usage.TotalBytes - freed
	}
	return CleanupResult{Deleted: deleted, FreedBytes: freed, RemainingBytes: remaining}
}

var warnf = func(format string, args ...any) {}

// SetWarnf replaces the warning sink (tests).
func SetWarnf(f func(format string, args ...any)) { warnf = f }

// FormatDayFile renders the day-file name of a timestamp (exported for tests / tooling).
func FormatDayFile(ts int64) string { return fileForDay(ts) }
