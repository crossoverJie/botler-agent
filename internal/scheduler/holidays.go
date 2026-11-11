package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// HolidayYear lists one year's statutory holidays (法定假日, to skip) and 调休 makeup workdays
// (补班, to fire even on a weekend), both as "YYYY-MM-DD".
type HolidayYear struct {
	Holidays []string `json:"holidays"`
	Workdays []string `json:"workdays"`
}

// HolidayData is the cached calendar keyed by 4-digit year.
type HolidayData map[string]HolidayYear

// holidayFetchTimeout bounds one calendar fetch.
const holidayFetchTimeout = 10 * time.Second

// maxHolidayBackups is how many holiday-calendar backups are kept (independent of the config
// backups naming, so the config prune must never touch them).
const maxHolidayBackups = 10

func dateKey(y, m, d int) string {
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

// IsLegalWorkday reports whether y-m-d is a China legal workday. With no data for the year it
// degrades gracefully to plain Mon–Fri.
func IsLegalWorkday(data HolidayData, y, m, d int) bool {
	year, ok := data[strconv.Itoa(y)]
	if !ok {
		dow := DayOfWeek(WallClock{Year: y, Month: m, Day: d})
		return dow >= 1 && dow <= 5
	}
	key := dateKey(y, m, d)
	for _, h := range year.Holidays {
		if h == key {
			return false
		}
	}
	for _, w := range year.Workdays {
		if w == key {
			return true
		}
	}
	dow := DayOfWeek(WallClock{Year: y, Month: m, Day: d})
	return dow >= 1 && dow <= 5
}

// LoadHolidays reads the cached calendar. A missing/invalid/malformed file degrades to an empty
// calendar (never an error): a broken calendar must not break the scheduler.
func LoadHolidays() HolidayData {
	raw, err := os.ReadFile(config.C().HolidaysFile)
	if err != nil {
		return HolidayData{}
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return HolidayData{}
	}
	out := make(HolidayData, len(parsed))
	for year, body := range parsed {
		var y HolidayYear
		if err := json.Unmarshal(body, &y); err != nil {
			warnf("holidays.json has malformed year entry; ignoring cached calendar")
			return HolidayData{}
		}
		if y.Holidays == nil || y.Workdays == nil {
			warnf("holidays.json has malformed year entry; ignoring cached calendar")
			return HolidayData{}
		}
		out[year] = y
	}
	return out
}

var (
	holidayListenerMu sync.RWMutex
	holidayListener   func()
)

// SetHolidaysSavedListener registers a callback fired after the calendar is written; the engine
// registers its reload here so a 24h refresh immediately recomputes workday schedules.
func SetHolidaysSavedListener(fn func()) {
	holidayListenerMu.Lock()
	holidayListener = fn
	holidayListenerMu.Unlock()
}

func fireHolidaysSaved() {
	holidayListenerMu.RLock()
	fn := holidayListener
	holidayListenerMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// pruneHolidayBackups keeps only the newest maxHolidayBackups holiday-calendar backups.
func pruneHolidayBackups(backupDir string) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return
	}
	var matched []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "holidays.json.") {
			matched = append(matched, e.Name())
		}
	}
	sort.Strings(matched) // ascending (oldest first)
	for len(matched) > maxHolidayBackups {
		old := matched[0]
		matched = matched[1:]
		_ = os.Remove(filepath.Join(backupDir, old))
	}
}

// SaveHolidays writes the calendar atomically (pre-write backup under <config>/.backups/
// holidays.json.<epochMs>, max 10) and then fires the saved listener.
func SaveHolidays(data HolidayData) error {
	target := config.C().HolidaysFile
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Pre-write backup so a corrupt write is recoverable. Best-effort only.
	if _, err := os.Stat(target); err == nil {
		backupDir := filepath.Join(dir, ".backups")
		if err := os.MkdirAll(backupDir, 0o700); err == nil {
			body, rerr := os.ReadFile(target)
			if rerr == nil {
				_ = os.WriteFile(filepath.Join(backupDir, fmt.Sprintf("holidays.json.%d", time.Now().UnixMilli())), body, 0o600)
				pruneHolidayBackups(backupDir)
			}
		}
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := config.WriteFileAtomic(target, payload, 0o644); err != nil {
		return err
	}
	fireHolidaysSaved()
	return nil
}

// RefreshHolidaysYear fetches and merges one year's calendar. It never returns an error: an
// outage keeps the cached data (returns false).
func RefreshHolidaysYear(ctx context.Context, year int) bool {
	url := strings.ReplaceAll(config.C().HolidayAPIURL, "{year}", strconv.Itoa(year))
	reqCtx, cancel := context.WithTimeout(ctx, holidayFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		warnf("holiday calendar refresh for %d failed: %v", year, err)
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		warnf("holiday calendar refresh for %d failed: %v", year, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		warnf("holiday calendar refresh for %d failed: HTTP %d", year, resp.StatusCode)
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		warnf("holiday calendar refresh for %d failed: %v", year, err)
		return false
	}
	var doc struct {
		Days []struct {
			Date  string `json:"date"`
			IsOff bool   `json:"isOffDay"`
		} `json:"days"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		warnf("holiday calendar refresh for %d failed: %v", year, err)
		return false
	}
	holidays := []string{}
	workdays := []string{}
	for _, d := range doc.Days {
		if d.Date == "" {
			continue
		}
		if d.IsOff {
			holidays = append(holidays, d.Date)
		} else {
			workdays = append(workdays, d.Date)
		}
	}
	// Read → merge → write in one synchronous block so a parallel refresh cannot lose updates.
	cur := LoadHolidays()
	cur[strconv.Itoa(year)] = HolidayYear{Holidays: holidays, Workdays: workdays}
	if err := SaveHolidays(cur); err != nil {
		warnf("holiday calendar refresh for %d failed: %v", year, err)
		return false
	}
	return true
}

var holidaysInFlight sync.Mutex

// EnsureHolidays refreshes the current and next year's calendars serially. It never blocks twice
// concurrently and never panics.
func EnsureHolidays(ctx context.Context) {
	if !holidaysInFlight.TryLock() {
		return
	}
	defer holidaysInFlight.Unlock()
	year := time.Now().Year()
	RefreshHolidaysYear(ctx, year)
	RefreshHolidaysYear(ctx, year+1)
}
