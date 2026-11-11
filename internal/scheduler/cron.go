package scheduler

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	// Embedded IANA timezone database: the scheduler must resolve "Asia/Shanghai" even in a
	// minimal container image that ships without /usr/share/zoneinfo.
	_ "time/tzdata"
)

// NoFire is the "inert / nothing pending" sentinel (v1 returns Infinity).
const NoFire int64 = math.MaxInt64

// WallClock is a naive calendar time (no timezone attached).
type WallClock struct {
	Year   int
	Month  int // 1-12
	Day    int // 1-31
	Hour   int
	Minute int
	Second int
}

// Cron is a compiled 5-field cron: one sorted allowed-value array per field.
type Cron struct {
	Minute []int
	Hour   []int
	Dom    []int
	Month  []int
	Dow    []int
}

// Next returns the next fire instant strictly after `from`, interpreted in from's location.
func (c Cron) Next(from time.Time) (time.Time, bool) {
	loc := from.Location()
	next, err := nextCronWall(c, epochToWall(from.UnixMilli(), loc))
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(wallToEpoch(next, loc)), true
}

var daysInMonthTable = [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// daysInMonth returns the number of days in a specific month of a specific year.
func daysInMonth(year, month int) int {
	if month == 2 {
		if (year%4 == 0 && year%100 != 0) || year%400 == 0 {
			return 29
		}
		return 28
	}
	return daysInMonthTable[month-1]
}

// maxDayOfMonth is the largest day number a month can ever have (February counted as 29).
func maxDayOfMonth(month int) int {
	if month == 2 {
		return 29
	}
	return daysInMonthTable[month-1]
}

// LoadTimezone validates and loads an IANA timezone.
func LoadTimezone(tz string) (*time.Location, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q", tz)
	}
	return loc, nil
}

// IsValidTimezone reports whether tz is a known IANA timezone name.
func IsValidTimezone(tz string) bool {
	_, err := time.LoadLocation(tz)
	return err == nil
}

// ---------------------------------------------------------------------------
// Field compilation
// ---------------------------------------------------------------------------

func addToSet(set map[int]bool, v int) { set[v] = true }

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func compileField(spec string, min, max int, isDow bool) ([]int, error) {
	set := map[int]bool{}
	for _, partRaw := range strings.Split(spec, ",") {
		part := strings.TrimSpace(partRaw)
		if part == "" {
			continue
		}
		step := 1
		rng := part
		if strings.Contains(part, "/") {
			bits := strings.Split(part, "/")
			rng = bits[0]
			n, err := strconv.Atoi(strings.TrimSpace(bits[1]))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case rng == "*" || rng == "?":
			lo, hi = min, max
		case strings.Contains(rng, "-"):
			a, b, ok := strings.Cut(rng, "-")
			if !ok {
				return nil, fmt.Errorf("invalid range %q", rng)
			}
			l, err1 := strconv.Atoi(strings.TrimSpace(a))
			h, err2 := strconv.Atoi(strings.TrimSpace(b))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid range %q", rng)
			}
			lo, hi = l, h
		default:
			v, err := strconv.Atoi(strings.TrimSpace(rng))
			if err != nil {
				return nil, fmt.Errorf("invalid field %q", rng)
			}
			lo, hi = v, v
		}
		if lo < min || hi > max {
			return nil, fmt.Errorf("value out of range [%d,%d] in %q", min, max, part)
		}
		if lo > hi {
			return nil, fmt.Errorf("range start > end in %q", part)
		}
		for v := lo; v <= hi; v += step {
			if isDow && v == 7 {
				addToSet(set, 0)
			} else {
				addToSet(set, v)
			}
		}
	}
	return sortedKeys(set), nil
}

// assertDayReachable rejects a cron whose day-of-month can never occur in any allowed month
// (e.g. "0 0 30 2 *"), which would otherwise make the next-fire search run to its bound forever.
func assertDayReachable(c Cron, expr string) error {
	for _, m := range c.Month {
		for _, d := range c.Dom {
			if d <= maxDayOfMonth(m) {
				return nil
			}
		}
	}
	return fmt.Errorf("cron %q has no valid date: day-of-month %s never occurs in month %s",
		expr, joinInts(c.Dom), joinInts(c.Month))
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// CompileCron compiles a 5-field cron expression (min hour day-of-month month day-of-week).
func CompileCron(expr string) (Cron, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("cron requires 5 fields, got %d: %q", len(fields), expr)
	}
	minute, err := compileField(fields[0], 0, 59, false)
	if err != nil {
		return Cron{}, err
	}
	hour, err := compileField(fields[1], 0, 23, false)
	if err != nil {
		return Cron{}, err
	}
	dom, err := compileField(fields[2], 1, 31, false)
	if err != nil {
		return Cron{}, err
	}
	month, err := compileField(fields[3], 1, 12, false)
	if err != nil {
		return Cron{}, err
	}
	dow, err := compileField(fields[4], 0, 7, true)
	if err != nil {
		return Cron{}, err
	}
	c := Cron{Minute: minute, Hour: hour, Dom: dom, Month: month, Dow: dow}
	if err := assertDayReachable(c, expr); err != nil {
		return Cron{}, err
	}
	return c, nil
}

// neutralizeWorkdayCron keeps minute/hour and replaces the date fields with `*`.
func neutralizeWorkdayCron(expr string) (string, error) {
	f := strings.Fields(strings.TrimSpace(expr))
	if len(f) != 5 {
		return "", fmt.Errorf("cron requires 5 fields, got %d: %q", len(f), expr)
	}
	return f[0] + " " + f[1] + " * * *", nil
}

// CompileWorkdayCron compiles a workday-mode cron: only minute/hour are validated/used.
func CompileWorkdayCron(expr string) (Cron, error) {
	neutral, err := neutralizeWorkdayCron(expr)
	if err != nil {
		return Cron{}, err
	}
	return CompileCron(neutral)
}

var (
	intervalRe = regexp.MustCompile(`^(\d+)(m|h|d)$`)
	atRe       = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
)

// intervalToCron normalizes a simple interval to a cron, failing loudly instead of silently
// degrading (a 5-field cron can only express steps within a field).
func intervalToCron(s string) (string, error) {
	m := intervalRe.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("invalid interval %q", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := m[2]
	if n < 1 {
		return "", fmt.Errorf("invalid interval %q: step must be >= 1", s)
	}
	switch unit {
	case "m":
		if n > 59 {
			return "", fmt.Errorf("invalid interval %q: minutes must be 1-59, use \"1h\"/\"2h\" or a cron for longer", s)
		}
		return fmt.Sprintf("*/%d * * * *", n), nil
	case "h":
		if n > 24 {
			return "", fmt.Errorf("invalid interval %q: hours must be 1-24, use \"1d\" or a cron for longer", s)
		}
		return fmt.Sprintf("0 */%d * * *", n), nil
	}
	if n != 1 {
		return "", fmt.Errorf("invalid interval %q: a 5-field cron cannot express \"every %d days\"; use cron/at instead", s, n)
	}
	return "0 0 * * *", nil
}

func atToCron(s string) (string, error) {
	m := atRe.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("invalid at %q", s)
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	if h > 23 || min > 59 {
		return "", fmt.Errorf("invalid at %q", s)
	}
	return fmt.Sprintf("%d %d * * *", min, h), nil
}

// CompileSchedule compiles a cron / interval / at entry (priority cron > interval > at).
// `once` is resolved directly by NextFireEpoch and never reaches this function.
func CompileSchedule(s Schedule) (Cron, error) {
	var (
		expr string
		err  error
	)
	switch {
	case s.Cron != "":
		expr = s.Cron
	case s.Interval != "":
		expr, err = intervalToCron(s.Interval)
		if err != nil {
			return Cron{}, err
		}
	case s.At != "":
		expr, err = atToCron(s.At)
		if err != nil {
			return Cron{}, err
		}
	default:
		return Cron{}, errors.New("schedule must specify one of cron/interval/at (or once for one-shot)")
	}
	return CompileCron(expr)
}

// ---------------------------------------------------------------------------
// Wall-clock helpers (naive calendar space, independent of timezone)
// ---------------------------------------------------------------------------

func wallMs(w WallClock) int64 {
	return time.Date(w.Year, time.Month(w.Month), w.Day, w.Hour, w.Minute, w.Second, 0, time.UTC).UnixMilli()
}

func wallFromTime(t time.Time) WallClock {
	return WallClock{Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
		Hour: t.Hour(), Minute: t.Minute(), Second: t.Second()}
}

// AddMinutes shifts a wall clock by n minutes (calendar arithmetic, timezone independent).
func AddMinutes(w WallClock, n int) WallClock {
	return wallFromTime(time.UnixMilli(wallMs(w) + int64(n)*60_000).UTC())
}

// AddDays shifts a wall clock by n days.
func AddDays(w WallClock, n int) WallClock { return AddMinutes(w, n*24*60) }

// DayOfWeek returns 0=Sunday..6=Saturday of a wall date (timezone independent).
func DayOfWeek(w WallClock) int {
	return int(time.Date(w.Year, time.Month(w.Month), w.Day, 0, 0, 0, 0, time.UTC).Weekday())
}

// EpochToWall converts an epoch instant to a wall clock in the given IANA timezone.
func EpochToWall(ts int64, tz string) WallClock {
	loc, err := LoadTimezone(tz)
	if err != nil {
		loc = time.UTC
	}
	return wallFromTime(time.UnixMilli(ts).In(loc))
}

// WallToEpoch converts a wall clock interpreted in the given IANA timezone to an epoch instant.
func WallToEpoch(w WallClock, tz string) int64 {
	loc, err := LoadTimezone(tz)
	if err != nil {
		loc = time.UTC
	}
	return time.Date(w.Year, time.Month(w.Month), w.Day, w.Hour, w.Minute, w.Second, 0, loc).UnixMilli()
}

func epochToWall(ts int64, loc *time.Location) WallClock {
	return wallFromTime(time.UnixMilli(ts).In(loc))
}

func wallToEpoch(w WallClock, loc *time.Location) int64 {
	return time.Date(w.Year, time.Month(w.Month), w.Day, w.Hour, w.Minute, w.Second, 0, loc).UnixMilli()
}

// ---------------------------------------------------------------------------
// once (one-shot) parsing
// ---------------------------------------------------------------------------

var (
	naiveFormRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[T ](\d{1,2}):(\d{2})(?::(\d{2}))?$`)
	ymdRe       = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})`)
	offsetRe    = regexp.MustCompile(`(?i)[zZ]$|[+-]\d{2}:?\d{2}$`)
	// absoluteFormLayouts cover the offset forms accepted by Date.parse for the v1 contract.
	absoluteFormLayouts = []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04Z07:00",
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04Z0700",
		"2006-01-02 15:04:05Z0700",
		"2006-01-02 15:04Z0700",
	}
)

// ParseOnceEpoch parses a `once` trigger. Absolute forms with an explicit offset (Z / ±HH:MM) and
// naive local forms (interpreted in tz) are accepted; impossible calendar dates are rejected.
// ok=false means malformed input.
func ParseOnceEpoch(s, tz string) (int64, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, false
	}
	if offsetRe.MatchString(trimmed) {
		// Validate the calendar date ourselves: Go's time.Parse also rolls over impossible dates.
		if ymd := ymdRe.FindStringSubmatch(trimmed); ymd != nil {
			y, _ := strconv.Atoi(ymd[1])
			mo, _ := strconv.Atoi(ymd[2])
			d, _ := strconv.Atoi(ymd[3])
			if mo < 1 || mo > 12 || d < 1 || d > daysInMonth(y, mo) {
				return 0, false
			}
		}
		for _, layout := range absoluteFormLayouts {
			if t, err := time.Parse(layout, trimmed); err == nil {
				return t.UnixMilli(), true
			}
		}
		return 0, false
	}
	m := naiveFormRe.FindStringSubmatch(trimmed)
	if m == nil {
		return 0, false
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	second := 0
	if m[6] != "" {
		second, _ = strconv.Atoi(m[6])
	}
	if month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) ||
		hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	return WallToEpoch(WallClock{Year: year, Month: month, Day: day, Hour: hour, Minute: minute, Second: second}, tz), true
}

// ---------------------------------------------------------------------------
// Workday-mode (China legal workday) next-fire calculation
// ---------------------------------------------------------------------------

func workdayTimeFields(s Schedule) (hour []int, minute []int, err error) {
	if s.Cron != "" {
		c, cerr := CompileWorkdayCron(s.Cron)
		if cerr != nil {
			return nil, nil, cerr
		}
		return c.Hour, c.Minute, nil
	}
	c, cerr := CompileSchedule(s)
	if cerr != nil {
		return nil, nil, cerr
	}
	return c.Hour, c.Minute, nil
}

type timeOfDay struct{ h, m int }

// NextWorkdayFireEpoch returns the next fire strictly after `after` on a China legal workday,
// enumerating every (hour, minute) slot so "0 8,18 * * *" fires twice a day. A silent-hours
// deferral that crosses into a non-workday is skipped and the search continues.
func NextWorkdayFireEpoch(s Schedule, after int64, data HolidayData) (int64, error) {
	tz := s.Timezone
	if tz == "" {
		tz = DefaultTZ
	}
	hour, minute, err := workdayTimeFields(s)
	if err != nil {
		return 0, err
	}
	times := make([]timeOfDay, 0, len(hour)*len(minute))
	for _, h := range hour {
		for _, m := range minute {
			times = append(times, timeOfDay{h: h, m: m})
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].h*60+times[i].m < times[j].h*60+times[j].m })

	start := EpochToWall(after, tz)
	for i := 0; i < 366*3; i++ {
		dw := start
		if i > 0 {
			dw = AddDays(start, i)
		}
		if !IsLegalWorkday(data, dw.Year, dw.Month, dw.Day) {
			continue
		}
		for _, t := range times {
			cand := WallToEpoch(WallClock{Year: dw.Year, Month: dw.Month, Day: dw.Day, Hour: t.h, Minute: t.m}, tz)
			if cand <= after {
				continue
			}
			final := cand
			if s.SilentHours != nil {
				final = applySilentHours(s, cand, tz)
			}
			fw := EpochToWall(final, tz)
			if !IsLegalWorkday(data, fw.Year, fw.Month, fw.Day) {
				continue
			}
			return final, nil
		}
	}
	return 0, errors.New("NextWorkdayFireEpoch: no match within 3 years")
}

// ---------------------------------------------------------------------------
// Next-fire calculation
// ---------------------------------------------------------------------------

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func dayMatches(c Cron, year, month, day int) bool {
	return containsInt(c.Dom, day) &&
		containsInt(c.Dow, DayOfWeek(WallClock{Year: year, Month: month, Day: day}))
}

// firstTimeOfDay returns the smallest minute-of-day satisfying hour+minute at or after floorMin.
func firstTimeOfDay(c Cron, floorMin int) (int, bool) {
	for _, h := range c.Hour {
		if h*60+59 < floorMin {
			continue
		}
		for _, m := range c.Minute {
			tod := h*60 + m
			if tod >= floorMin {
				return tod, true
			}
		}
	}
	return 0, false
}

// nextCronWall returns the next wall-clock time strictly after `after` (minute granularity).
// It advances a day at a time and picks the time-of-day arithmetically instead of scanning
// minute by minute, so a Feb-29 cron resolves in ~120 steps rather than ~1.5M.
func nextCronWall(c Cron, after WallClock) (WallClock, error) {
	start := AddMinutes(WallClock{Year: after.Year, Month: after.Month, Day: after.Day,
		Hour: after.Hour, Minute: after.Minute, Second: 0}, 1)
	year, month, day := start.Year, start.Month, start.Day
	floorMin := start.Hour*60 + start.Minute

	for i := 0; i < 5*366; i++ {
		if !containsInt(c.Month, month) {
			month++
			if month > 12 {
				month = 1
				year++
			}
			day = 1
			floorMin = 0
			continue
		}
		if dayMatches(c, year, month, day) {
			if tod, ok := firstTimeOfDay(c, floorMin); ok {
				return WallClock{Year: year, Month: month, Day: day, Hour: tod / 60, Minute: tod % 60}, nil
			}
		}
		day++
		floorMin = 0
		if day > daysInMonth(year, month) {
			day = 1
			month++
			if month > 12 {
				month = 1
				year++
			}
		}
	}
	return WallClock{}, errors.New("nextCronWall: no match found within 5 years")
}

// NextFireEpoch returns the epoch ms of the next fire of an entry strictly after `fromMs`,
// with the optional silent-hours window applied. It returns NoFire when the entry is inert
// (a spent `once`) and an error for an unparseable trigger.
func NextFireEpoch(s Schedule, fromMs int64) (next int64, err error) {
	// A single bad entry must never take down the caller: normalize panics into errors.
	defer func() {
		if r := recover(); r != nil {
			next = NoFire
			err = fmt.Errorf("next fire calculation failed: %v", r)
		}
	}()
	tz := s.Timezone
	if tz == "" {
		tz = DefaultTZ
	}
	// One-shot: handled BEFORE CompileSchedule (which has no `once` case). silentHours is
	// intentionally not applied: a one-shot reminder is an explicit, exact instant.
	if s.Once != "" {
		t, ok := ParseOnceEpoch(s.Once, tz)
		if !ok {
			return NoFire, fmt.Errorf("schedule %q invalid once %q", s.ID, s.Once)
		}
		if t > fromMs {
			return t, nil
		}
		return NoFire, nil
	}
	if s.HolidayMode == HolidayModeWorkday {
		return NextWorkdayFireEpoch(s, fromMs, LoadHolidays())
	}
	cron, cerr := CompileSchedule(s)
	if cerr != nil {
		return NoFire, cerr
	}
	nextWall, werr := nextCronWall(cron, EpochToWall(fromMs, tz))
	if werr != nil {
		return NoFire, werr
	}
	next = WallToEpoch(nextWall, tz)
	if s.SilentHours != nil {
		next = applySilentHours(s, next, tz)
	}
	return next, nil
}

// applySilentHours defers an instant inside the window to the window end. `from == to` is
// treated as "no window". Bounded to 366 iterations; if it cannot resolve, the window is ignored.
func applySilentHours(s Schedule, epoch int64, tz string) int64 {
	sh := s.SilentHours
	if sh == nil {
		return epoch
	}
	parseHM := func(v string) (int, int, bool) {
		m := atRe.FindStringSubmatch(strings.TrimSpace(v))
		if m == nil {
			return 0, 0, false
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h > 23 || mi > 59 {
			return 0, 0, false
		}
		return h, mi, true
	}
	fh, fm, ok1 := parseHM(sh.From)
	th, tm, ok2 := parseHM(sh.To)
	if !ok1 || !ok2 {
		return epoch
	}
	fromMin := fh*60 + fm
	toMin := th*60 + tm
	if fromMin == toMin {
		return epoch // degenerate window → ignore
	}
	cur := epoch
	for i := 0; i < 366; i++ {
		w := EpochToWall(cur, tz)
		cm := w.Hour*60 + w.Minute
		inWindow := cm >= fromMin && cm < toMin
		if fromMin > toMin {
			inWindow = cm >= fromMin || cm < toMin
		}
		if !inWindow {
			return cur
		}
		if fromMin < toMin {
			cur = WallToEpoch(WallClock{Year: w.Year, Month: w.Month, Day: w.Day, Hour: th, Minute: tm}, tz)
		} else {
			nd := AddDays(w, 1)
			cur = WallToEpoch(WallClock{Year: nd.Year, Month: nd.Month, Day: nd.Day, Hour: th, Minute: tm}, tz)
		}
	}
	warnf("[scheduler] silentHours for %q did not resolve; ignoring DND", s.ID)
	return epoch
}
