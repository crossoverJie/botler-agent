package scheduler

import (
	"testing"
	"time"
)

func testWall(y, m, d, h, min int) WallClock {
	return WallClock{Year: y, Month: m, Day: d, Hour: h, Minute: min}
}

func assertWall(t *testing.T, got, want WallClock) {
	t.Helper()
	if got != want {
		t.Fatalf("wall mismatch: got %+v, want %+v", got, want)
	}
}

func mustCron(t *testing.T, expr string) Cron {
	t.Helper()
	c, err := CompileCron(expr)
	if err != nil {
		t.Fatalf("CompileCron(%q) failed: %v", expr, err)
	}
	return c
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCompileCronFields(t *testing.T) {
	if got := mustCron(t, "*/5 * * * *").Minute; !eqInts(got, []int{0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55}) {
		t.Fatalf("*/5 minute = %v", got)
	}
	if got := mustCron(t, "10-12 * * * *").Minute; !eqInts(got, []int{10, 11, 12}) {
		t.Fatalf("range minute = %v", got)
	}
	if got := mustCron(t, "0,30 * * * *").Minute; !eqInts(got, []int{0, 30}) {
		t.Fatalf("list minute = %v", got)
	}
	// dow: 7 means Sunday == 0
	if got := mustCron(t, "0 0 * * 7").Dow; !eqInts(got, []int{0}) {
		t.Fatalf("dow 7 = %v", got)
	}
	// step within a range
	if got := mustCron(t, "0 0 1-10/3 * *").Dom; !eqInts(got, []int{1, 4, 7, 10}) {
		t.Fatalf("stepped range dom = %v", got)
	}
}

func TestCompileCronRejectsMalformed(t *testing.T) {
	for _, expr := range []string{"", "0 8 * *", "0 8 * * * *", "99 8 * * *", "*/0 * * * *", "0 0 30 2 *", "0 0 31 4,6 *"} {
		if _, err := CompileCron(expr); err == nil {
			t.Fatalf("CompileCron(%q) should fail", expr)
		}
	}
	for _, expr := range []string{"0 0 29 2 *", "0 0 30,31 2,3 *"} {
		if _, err := CompileCron(expr); err != nil {
			t.Fatalf("CompileCron(%q) should be accepted: %v", expr, err)
		}
	}
}

func TestNextCronWall(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after WallClock
		want  WallClock
	}{
		{"align */5", "*/5 * * * *", testWall(2026, 1, 1, 10, 2), testWall(2026, 1, 1, 10, 5)},
		{"align */5 hour roll", "*/5 * * * *", testWall(2026, 1, 1, 10, 59), testWall(2026, 1, 1, 11, 0)},
		{"daily crosses midnight", "0 8 * * *", testWall(2026, 1, 1, 9, 0), testWall(2026, 1, 2, 8, 0)},
		{"daily same day", "0 8 * * *", testWall(2026, 1, 1, 7, 0), testWall(2026, 1, 1, 8, 0)},
		{"monthly crosses month", "0 0 1 * *", testWall(2026, 1, 15, 0, 0), testWall(2026, 2, 1, 0, 0)},
		{"monthly crosses year", "0 0 1 * *", testWall(2026, 12, 15, 0, 0), testWall(2027, 1, 1, 0, 0)},
		{"13th friday", "0 0 13 * 5", testWall(2026, 1, 1, 0, 0), testWall(2026, 2, 13, 0, 0)},
		{"feb 29 leap", "0 0 29 2 *", testWall(2025, 3, 1, 0, 0), testWall(2028, 2, 29, 0, 0)},
		{"31st skips february", "0 0 31 * *", testWall(2026, 1, 31, 0, 0), testWall(2026, 3, 31, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextCronWall(mustCron(t, tc.expr), tc.after)
			if err != nil {
				t.Fatalf("nextCronWall: %v", err)
			}
			assertWall(t, got, tc.want)
		})
	}
}

func TestEpochWallRoundTrip(t *testing.T) {
	zones := []string{"Asia/Shanghai", "America/New_York", "Europe/London", "UTC"}
	samples := []WallClock{
		testWall(2026, 1, 15, 12, 30),
		testWall(2026, 6, 15, 9, 45),
		testWall(2026, 12, 31, 23, 59),
		testWall(2027, 3, 14, 0, 0),
	}
	for _, tz := range zones {
		for _, w := range samples {
			epoch := WallToEpoch(w, tz)
			back := EpochToWall(epoch, tz)
			if back != w {
				t.Fatalf("round trip failed for %s %+v: got %+v", tz, w, back)
			}
		}
	}
}

func TestWallToEpochMatchesTimezoneOffset(t *testing.T) {
	// 2026-06-15 12:30 America/New_York (EDT, UTC-4) -> 16:30 UTC
	got := WallToEpoch(testWall(2026, 6, 15, 12, 30), "America/New_York")
	want := time.Date(2026, 6, 15, 16, 30, 0, 0, time.UTC).UnixMilli()
	if got != want {
		t.Fatalf("NY wallToEpoch = %d, want %d", got, want)
	}
	// Asia/Shanghai (UTC+8) 12:30 -> 04:30 UTC same day
	got = WallToEpoch(testWall(2026, 6, 15, 12, 30), "Asia/Shanghai")
	want = time.Date(2026, 6, 15, 4, 30, 0, 0, time.UTC).UnixMilli()
	if got != want {
		t.Fatalf("Shanghai wallToEpoch = %d, want %d", got, want)
	}
}

func TestCompileScheduleIntervals(t *testing.T) {
	bad := []string{"90m", "25h", "2d", "0m", "5", "1w"}
	for _, iv := range bad {
		if _, err := CompileSchedule(Schedule{ID: "t", Interval: iv}); err == nil {
			t.Fatalf("interval %q should be rejected", iv)
		}
	}
	c, err := CompileSchedule(Schedule{ID: "t", Interval: "2h"})
	if err != nil {
		t.Fatalf("2h: %v", err)
	}
	if !eqInts(c.Hour, []int{0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22}) {
		t.Fatalf("2h hour = %v", c.Hour)
	}
	c, err = CompileSchedule(Schedule{ID: "t", Interval: "1d"})
	if err != nil {
		t.Fatalf("1d: %v", err)
	}
	if !eqInts(c.Minute, []int{0}) || !eqInts(c.Hour, []int{0}) {
		t.Fatalf("1d = %v/%v", c.Minute, c.Hour)
	}
	if _, err := CompileSchedule(Schedule{ID: "t"}); err == nil {
		t.Fatal("missing trigger should be rejected")
	}
}

func TestParseOnceEpoch(t *testing.T) {
	// Absolute forms with an explicit offset.
	for _, s := range []string{"2026-08-20T22:00:00+08:00", "2026-08-20T22:00+08:00", "2026-08-20T14:00:00Z"} {
		got, ok := ParseOnceEpoch(s, "UTC")
		if !ok {
			t.Fatalf("%q should parse", s)
		}
		want := time.Date(2026, 8, 20, 22, 0, 0, 0, time.FixedZone("", 8*3600)).UnixMilli()
		if s == "2026-08-20T14:00:00Z" {
			want = time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC).UnixMilli()
		}
		if got != want {
			t.Fatalf("%q = %d, want %d", s, got, want)
		}
	}
	// Naive local forms are interpreted in the entry timezone.
	if got, ok := ParseOnceEpoch("2026-06-15T12:30", "Asia/Shanghai"); !ok ||
		got != time.Date(2026, 6, 15, 4, 30, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("naive Shanghai = %d ok=%v", got, ok)
	}
	if got, ok := ParseOnceEpoch("2026-06-15 12:30", "Asia/Shanghai"); !ok ||
		got != time.Date(2026, 6, 15, 4, 30, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("space-separated naive = %d ok=%v", got, ok)
	}
	if got, ok := ParseOnceEpoch("2026-06-15T12:30", "America/New_York"); !ok ||
		got != time.Date(2026, 6, 15, 16, 30, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("naive NY = %d ok=%v", got, ok)
	}
	// Malformed / out-of-range / impossible calendar dates.
	for _, s := range []string{"", "not-a-date", "2026-13-40T25:99", "22:00", "2026-02-30T22:00",
		"2026-04-31T22:00", "2026-02-29T22:00", "2026-02-30T22:00:00Z", "2026-04-31T22:00:00+08:00",
		"2026-02-29T22:00:00+08:00"} {
		if _, ok := ParseOnceEpoch(s, "Asia/Shanghai"); ok {
			t.Fatalf("%q should be rejected", s)
		}
	}
	if _, ok := ParseOnceEpoch("2028-02-29T22:00", "Asia/Shanghai"); !ok {
		t.Fatal("2028-02-29 (leap year) should parse")
	}
}

func TestNextFireEpochOnce(t *testing.T) {
	onceEpoch := time.Date(2026, 8, 20, 22, 0, 0, 0, time.FixedZone("", 8*3600)).UnixMilli()
	after := time.Date(2026, 8, 20, 10, 0, 0, 0, time.FixedZone("", 8*3600)).UnixMilli()
	e := Schedule{ID: "t", Enabled: true, Timezone: "Asia/Shanghai", Once: "2026-08-20T22:00:00+08:00", Message: "m"}

	if got, err := NextFireEpoch(e, after); err != nil || got != onceEpoch {
		t.Fatalf("future once = %d (err %v), want %d", got, err, onceEpoch)
	}
	if got, err := NextFireEpoch(e, onceEpoch); err != nil || got != NoFire {
		t.Fatalf("equal watermark should be inert: got %d (err %v)", got, err)
	}
	if got, err := NextFireEpoch(e, onceEpoch+1000); err != nil || got != NoFire {
		t.Fatalf("past once should be inert: got %d (err %v)", got, err)
	}
	// silentHours is intentionally not applied to `once`.
	e.SilentHours = &SilentHours{From: "22:00", To: "07:00"}
	e.Once = "2026-08-20T23:30:00+08:00"
	want := time.Date(2026, 8, 20, 23, 30, 0, 0, time.FixedZone("", 8*3600)).UnixMilli()
	if got, err := NextFireEpoch(e, after); err != nil || got != want {
		t.Fatalf("once with silentHours = %d (err %v), want %d", got, err, want)
	}
	if _, err := NextFireEpoch(Schedule{ID: "t", Once: "not-a-date"}, 0); err == nil {
		t.Fatal("invalid once should error")
	}
}

func TestNextFireEpochAppliesSilentHours(t *testing.T) {
	// Daily 23:30 with DND 22:00-07:00 -> deferred to the next day 07:00.
	e := Schedule{ID: "t", Enabled: true, Timezone: "Asia/Shanghai", Cron: "30 23 * * *",
		Message: "m", SilentHours: &SilentHours{From: "22:00", To: "07:00"}}
	after := WallToEpoch(testWall(2026, 1, 1, 0, 0), "Asia/Shanghai")
	got, err := NextFireEpoch(e, after)
	if err != nil {
		t.Fatalf("NextFireEpoch: %v", err)
	}
	wall := EpochToWall(got, "Asia/Shanghai")
	assertWall(t, wall, testWall(2026, 1, 2, 7, 0))
}

// workdayData models a statutory holiday on a Monday and a 调休 makeup workday on a Sunday.
func workdayData() HolidayData {
	return HolidayData{
		"2026": {
			Holidays: []string{"2026-01-05"}, // Monday
			Workdays: []string{"2026-01-11"}, // Sunday (补班)
		},
	}
}

func TestIsLegalWorkday(t *testing.T) {
	data := workdayData()
	if IsLegalWorkday(data, 2026, 1, 5) {
		t.Fatal("statutory holiday on a Monday must not be a workday")
	}
	if !IsLegalWorkday(data, 2026, 1, 11) {
		t.Fatal("makeup workday on a Sunday must be a workday")
	}
	if !IsLegalWorkday(data, 2026, 1, 7) {
		t.Fatal("plain Wednesday must be a workday")
	}
	if IsLegalWorkday(data, 2026, 1, 10) {
		t.Fatal("plain Saturday must not be a workday")
	}
	// Missing year data degrades to Mon-Fri.
	empty := HolidayData{}
	if !IsLegalWorkday(empty, 2026, 1, 5) || IsLegalWorkday(empty, 2026, 1, 10) {
		t.Fatal("empty calendar must degrade to Mon-Fri")
	}
}

func TestCompileWorkdayCronIgnoresDateFields(t *testing.T) {
	c, err := CompileWorkdayCron("0 18 31 2 *")
	if err != nil {
		t.Fatalf("workday cron with an impossible date field should compile: %v", err)
	}
	if !eqInts(c.Minute, []int{0}) || !eqInts(c.Hour, []int{18}) {
		t.Fatalf("compiled fields = %+v", c)
	}
	if _, err := CompileWorkdayCron("99 18 * * *"); err == nil {
		t.Fatal("out-of-range hour must be rejected")
	}
}

func TestNextWorkdayFireEpoch(t *testing.T) {
	data := workdayData()
	e := Schedule{ID: "wd", Enabled: true, Timezone: "Asia/Shanghai", Cron: "0 18 * * *",
		HolidayMode: HolidayModeWorkday, Message: "m"}

	// Starting just before a weekday holiday lands on the next workday.
	after := WallToEpoch(testWall(2026, 1, 4, 0, 0), "Asia/Shanghai")
	next, err := NextWorkdayFireEpoch(e, after, data)
	if err != nil {
		t.Fatalf("NextWorkdayFireEpoch: %v", err)
	}
	wall := EpochToWall(next, "Asia/Shanghai")
	assertWall(t, wall, testWall(2026, 1, 6, 18, 0))

	// A makeup workday on a Sunday fires.
	after = WallToEpoch(testWall(2026, 1, 10, 0, 0), "Asia/Shanghai")
	next, err = NextWorkdayFireEpoch(e, after, data)
	if err != nil {
		t.Fatalf("NextWorkdayFireEpoch: %v", err)
	}
	wall = EpochToWall(next, "Asia/Shanghai")
	if wall.Year != 2026 || wall.Month != 1 || wall.Day != 11 {
		t.Fatalf("makeup workday fire = %+v", wall)
	}

	// Multiple times of day: from 12:00 the next slot is 18:00 the same day, then 08:00 next day.
	e.Cron = "0 8,18 * * *"
	after = WallToEpoch(testWall(2026, 1, 6, 12, 0), "Asia/Shanghai")
	first, err := NextWorkdayFireEpoch(e, after, data)
	if err != nil {
		t.Fatalf("NextWorkdayFireEpoch: %v", err)
	}
	fw := EpochToWall(first, "Asia/Shanghai")
	if fw.Hour != 18 || fw.Day != 6 {
		t.Fatalf("first slot = %+v", fw)
	}
	second, err := NextWorkdayFireEpoch(e, first, data)
	if err != nil {
		t.Fatalf("NextWorkdayFireEpoch: %v", err)
	}
	sw := EpochToWall(second, "Asia/Shanghai")
	if sw.Hour != 8 || sw.Day != 7 {
		t.Fatalf("second slot = %+v", sw)
	}

	// An impossible date field is ignored in workday mode.
	e.Cron = "0 18 31 2 *"
	after = WallToEpoch(testWall(2026, 1, 1, 0, 0), "Asia/Shanghai")
	next, err = NextWorkdayFireEpoch(e, after, data)
	if err != nil {
		t.Fatalf("NextWorkdayFireEpoch with impossible date field: %v", err)
	}
	wall = EpochToWall(next, "Asia/Shanghai")
	if wall.Hour != 18 {
		t.Fatalf("fire hour = %+v", wall)
	}
	if key := dateKey(wall.Year, wall.Month, wall.Day); key == "2026-01-05" {
		t.Fatalf("fired on a statutory holiday: %s", key)
	}
}

func TestNextWorkdayFireEpochSkipsDeferralIntoNonWorkday(t *testing.T) {
	data := HolidayData{"2026": {Holidays: []string{}, Workdays: []string{}}}
	e := Schedule{ID: "wd", Enabled: true, Timezone: "Asia/Shanghai", Cron: "0 23 * * *",
		HolidayMode: HolidayModeWorkday, Message: "m", SilentHours: &SilentHours{From: "22:00", To: "07:00"}}
	// Fri 2026-01-09 20:00: the Fri 23:00 fire defers to Sat 07:00 (not a workday) -> Mon 23:00
	// deferring to Tue 07:00.
	after := WallToEpoch(testWall(2026, 1, 9, 20, 0), "Asia/Shanghai")
	next, err := NextWorkdayFireEpoch(e, after, data)
	if err != nil {
		t.Fatalf("NextWorkdayFireEpoch: %v", err)
	}
	wall := EpochToWall(next, "Asia/Shanghai")
	assertWall(t, wall, testWall(2026, 1, 13, 7, 0))
}

func TestPollDelay(t *testing.T) {
	const now = int64(1_790_128_171_897) // 2026-09-23
	far := int64(1_797_782_400_000)      // ~88 days ahead
	if got := PollDelay(far, now); got.Milliseconds() != MaxSleepMs {
		t.Fatalf("far-future delay = %v, want %d", got, MaxSleepMs)
	}
	if got := PollDelay(now+5*60_000, now); got.Milliseconds() != 5*60_000 {
		t.Fatalf("near delay = %v", got)
	}
	if got := PollDelay(now, now); got.Milliseconds() != MinSleepMs {
		t.Fatalf("past delay = %v", got)
	}
	if got := PollDelay(now-10_000, now); got.Milliseconds() != MinSleepMs {
		t.Fatalf("past delay = %v", got)
	}
	if got := PollDelay(NoFire, now); got.Milliseconds() != IdlePollMs {
		t.Fatalf("idle delay = %v", got)
	}
}

func TestCronNextMethod(t *testing.T) {
	c := mustCron(t, "0 9 * * *")
	loc := time.FixedZone("CST", 8*3600)
	from := time.Date(2026, 11, 3, 10, 0, 0, 0, loc)
	next, ok := c.Next(from)
	if !ok {
		t.Fatal("Cron.Next reported no match")
	}
	want := time.Date(2026, 11, 4, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("Cron.Next = %v, want %v", next, want)
	}
}
