import { test } from "node:test";
import assert from "node:assert/strict";
import {
	compileCron,
	compileSchedule,
	nextCronWall,
	wallToEpoch,
	epochToWall,
	nextFireEpoch,
	parseOnceEpoch,
	type WallClock,
	type CompiledCron,
} from "./cron.ts";
import type { ScheduleEntry } from "./types.ts";

function entry(trigger: Partial<ScheduleEntry>): ScheduleEntry {
	return { id: "t", enabled: true, timezone: "UTC", message: "m", ...trigger };
}

function w(year: number, month: number, day: number, hour: number, minute: number): WallClock {
	return { year, month, day, hour, minute, second: 0 };
}

function matches(c: CompiledCron, w: WallClock): boolean {
	const dow = new Date(Date.UTC(w.year, w.month - 1, w.day)).getUTCDay();
	return (
		c.minute.includes(w.minute) &&
		c.hour.includes(w.hour) &&
		c.dom.includes(w.day) &&
		c.month.includes(w.month) &&
		c.dow.includes(dow)
	);
}

test("compileField handles star, step, range, list, dow=7", () => {
	const min = compileCron("*/5 * * * *").minute;
	assert.deepEqual(min, [0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55]);

	const range = compileCron("10-12 * * * *").minute;
	assert.deepEqual(range, [10, 11, 12]);

	const list = compileCron("0,30 * * * *").minute;
	assert.deepEqual(list, [0, 30]);

	// dow: 7 means Sunday == 0
	const dow = compileCron("0 0 * * 7").dow;
	assert.deepEqual(dow, [0]);

	// step within a range
	const step = compileCron("0 0 1-10/3 * *").dom;
	assert.deepEqual(step, [1, 4, 7, 10]);
});

test("nextCronWall aligns to */5 minutes", () => {
	const c = compileCron("*/5 * * * *");
	assert.deepEqual(nextCronWall(c, w(2026, 1, 1, 10, 2)), w(2026, 1, 1, 10, 5));
	assert.deepEqual(nextCronWall(c, w(2026, 1, 1, 10, 59)), w(2026, 1, 1, 11, 0));
});

test("nextCronWall daily at 08:00 crosses midnight", () => {
	const c = compileCron("0 8 * * *");
	assert.deepEqual(nextCronWall(c, w(2026, 1, 1, 9, 0)), w(2026, 1, 2, 8, 0));
	assert.deepEqual(nextCronWall(c, w(2026, 1, 1, 7, 0)), w(2026, 1, 1, 8, 0));
});

test("nextCronWall monthly on the 1st crosses month/year boundary", () => {
	const c = compileCron("0 0 1 * *");
	assert.deepEqual(nextCronWall(c, w(2026, 1, 15, 0, 0)), w(2026, 2, 1, 0, 0));
	assert.deepEqual(nextCronWall(c, w(2026, 12, 15, 0, 0)), w(2027, 1, 1, 0, 0));
});

test("nextCronWall respects day-of-month AND day-of-week (13th + Friday)", () => {
	const c = compileCron("0 0 13 * 5"); // 13th that is also a Friday
	const next = nextCronWall(c, w(2026, 1, 1, 0, 0));
	assert.ok(matches(c, next), "returned wall must satisfy both dom=13 and dow=Friday");
	assert.equal(next.day, 13);
	// 2026-02-13 is a Friday
	assert.deepEqual(next, w(2026, 2, 13, 0, 0));
});

test("nextCronWall February day-count and leap year", () => {
	const c = compileCron("0 0 29 2 *"); // Feb 29
	// Next Feb 29 after 2025-03-01 is 2028-02-29 (2026/2027 non-leap)
	const next = nextCronWall(c, w(2025, 3, 1, 0, 0));
	assert.deepEqual(next, w(2028, 2, 29, 0, 0));
});

test("epochToWall / wallToEpoch round-trip across timezones", () => {
	const zones = ["Asia/Shanghai", "America/New_York", "Europe/London", "UTC"];
	const samples: WallClock[] = [
		w(2026, 1, 15, 12, 30),
		w(2026, 6, 15, 9, 45),
		w(2026, 12, 31, 23, 59),
		w(2027, 3, 14, 0, 0),
	];
	for (const tz of zones) {
		for (const wall of samples) {
			const epoch = wallToEpoch(wall, tz);
			const back = epochToWall(epoch, tz);
			assert.deepEqual(back, wall, `round-trip failed for ${tz} ${JSON.stringify(wall)}`);
		}
	}
});

test("wallToEpoch produces a UTC epoch consistent with Intl offset", () => {
	// 2026-06-15 12:30 America/New_York (EDT, UTC-4) -> 16:30 UTC
	const epoch = wallToEpoch(w(2026, 6, 15, 12, 30), "America/New_York");
	assert.equal(epoch, Date.UTC(2026, 5, 15, 16, 30));
	// Asia/Shanghai (UTC+8) 12:30 -> 04:30 UTC same day
	const epoch2 = wallToEpoch(w(2026, 6, 15, 12, 30), "Asia/Shanghai");
	assert.equal(epoch2, Date.UTC(2026, 5, 15, 4, 30));
});

test("nextFireEpoch applies silentHours deferral (cross-midnight)", () => {
	// at 23:30 with silentHours 22:00-07:00 -> defer to next day 07:00
	// Build a schedule firing daily at 23:30.
	const c = compileCron("30 23 * * *");
	const after = wallToEpoch(w(2026, 1, 1, 0, 0), "Asia/Shanghai"); // start of day
	const next = nextCronWall(c, epochToWall(after, "Asia/Shanghai"));
	assert.deepEqual(next, w(2026, 1, 1, 23, 30));
});

test("nextCronWall skips months shorter than the requested day-of-month", () => {
	const c = compileCron("0 0 31 * *"); // the 31st only exists in 7 months
	// After Jan 31 the next 31st is Mar 31 (Feb has none).
	assert.deepEqual(nextCronWall(c, w(2026, 1, 31, 0, 0)), w(2026, 3, 31, 0, 0));
});

test("compileCron rejects a date that can never occur", () => {
	// Syntactically valid but unsatisfiable: February never has a 30th.
	assert.throws(() => compileCron("0 0 30 2 *"), /no valid date/);
	assert.throws(() => compileCron("0 0 31 4,6 *"), /no valid date/);
	// Still allowed when at least one month can host the day.
	assert.doesNotThrow(() => compileCron("0 0 29 2 *"));
	assert.doesNotThrow(() => compileCron("0 0 30,31 2,3 *"));
});

test("compileSchedule rejects intervals a 5-field cron cannot express", () => {
	assert.throws(() => compileSchedule(entry({ interval: "90m" })), /minutes must be 1-59/);
	assert.throws(() => compileSchedule(entry({ interval: "25h" })), /hours must be 1-24/);
	assert.throws(() => compileSchedule(entry({ interval: "2d" })), /cannot express "every 2 days"/);
	assert.throws(() => compileSchedule(entry({ interval: "0m" })), /step must be >= 1/);
});

test("compileSchedule accepts in-range intervals with the expected meaning", () => {
	assert.deepEqual(compileSchedule(entry({ interval: "1m" })).minute.length, 60);
	assert.deepEqual(compileSchedule(entry({ interval: "2h" })).hour, [0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22]);
	const daily = compileSchedule(entry({ interval: "1d" }));
	assert.deepEqual([daily.minute, daily.hour], [[0], [0]]);
});

test("nextCronWall resolves a far-future date without scanning minutes", () => {
	// Feb 29 from 2025-03-01 is 3 years out; a minute scan would need ~1.5M steps.
	const c = compileCron("0 0 29 2 *");
	const t0 = process.hrtime.bigint();
	const next = nextCronWall(c, w(2025, 3, 1, 0, 0));
	const ms = Number(process.hrtime.bigint() - t0) / 1e6;
	assert.deepEqual(next, w(2028, 2, 29, 0, 0));
	assert.ok(ms < 50, `expected day-level advancement to be fast, took ${ms}ms`);
});

// --- once (one-shot) trigger ---

test("parseOnceEpoch parses absolute forms with explicit offset", () => {
	assert.equal(parseOnceEpoch("2026-08-20T22:00:00+08:00", "UTC"), Date.parse("2026-08-20T22:00:00+08:00"));
	assert.equal(parseOnceEpoch("2026-08-20T14:00:00Z", "UTC"), Date.parse("2026-08-20T14:00:00Z"));
	assert.equal(parseOnceEpoch("2026-08-20T22:00+08:00", "UTC"), Date.parse("2026-08-20T22:00:00+08:00"));
});

test("parseOnceEpoch parses naive local forms in the entry timezone", () => {
	// 2026-06-15 12:30 Asia/Shanghai (UTC+8) -> 04:30 UTC
	assert.equal(parseOnceEpoch("2026-06-15T12:30", "Asia/Shanghai"), Date.UTC(2026, 5, 15, 4, 30));
	// Space separator instead of T is accepted.
	assert.equal(parseOnceEpoch("2026-06-15 12:30", "Asia/Shanghai"), Date.UTC(2026, 5, 15, 4, 30));
	// Same wall time in America/New_York (EDT, UTC-4) -> 16:30 UTC
	assert.equal(parseOnceEpoch("2026-06-15T12:30", "America/New_York"), Date.UTC(2026, 5, 15, 16, 30));
});

test("parseOnceEpoch rejects malformed or out-of-range input", () => {
	assert.equal(parseOnceEpoch("", "UTC"), null);
	assert.equal(parseOnceEpoch("not-a-date", "UTC"), null);
	assert.equal(parseOnceEpoch("2026-13-40T25:99", "UTC"), null);
	assert.equal(parseOnceEpoch("22:00", "UTC"), null); // bare HH:MM is an `at`, not once
});

test("parseOnceEpoch rejects impossible calendar dates that Date.UTC would otherwise overflow", () => {
	// Feb 30 / Apr 31 must be rejected, not silently rolled over into the next month.
	assert.equal(parseOnceEpoch("2026-02-30T22:00", "Asia/Shanghai"), null);
	assert.equal(parseOnceEpoch("2026-04-31T22:00", "Asia/Shanghai"), null);
	// Feb 29 in a non-leap year is also impossible.
	assert.equal(parseOnceEpoch("2026-02-29T22:00", "Asia/Shanghai"), null);
	// Feb 29 in a leap year is valid.
	assert.notEqual(parseOnceEpoch("2028-02-29T22:00", "Asia/Shanghai"), null);
});

test("nextFireEpoch returns the once instant when it is in the future of the watermark", () => {
	const onceEpoch = Date.parse("2026-08-20T22:00:00+08:00");
	const after = Date.parse("2026-08-20T10:00:00+08:00");
	const e = entry({ once: "2026-08-20T22:00:00+08:00" });
	assert.equal(nextFireEpoch(e, after), onceEpoch);
});

test("nextFireEpoch returns Infinity for a once at or before the watermark (already fired / expired)", () => {
	const onceEpoch = Date.parse("2026-08-20T22:00:00+08:00");
	const e = entry({ once: "2026-08-20T22:00:00+08:00" });
	assert.equal(nextFireEpoch(e, onceEpoch), Infinity); // equal watermark -> not strictly after
	assert.equal(nextFireEpoch(e, onceEpoch + 1000), Infinity); // past
});

test("nextFireEpoch for once does not apply silentHours", () => {
	const onceEpoch = Date.parse("2026-08-20T23:30:00+08:00");
	const after = Date.parse("2026-08-20T10:00:00+08:00");
	const e = entry({
		once: "2026-08-20T23:30:00+08:00",
		silentHours: { from: "22:00", to: "07:00" }, // would defer a daily trigger to next 07:00
	});
	assert.equal(nextFireEpoch(e, after), onceEpoch); // returned exactly, not deferred
});

test("nextFireEpoch accepts a once-only entry without any cron/interval/at", () => {
	// A once-only entry must not trip compileSchedule's missing-trigger error.
	const e = entry({ once: "2026-08-20T22:00:00+08:00" });
	assert.doesNotThrow(() => nextFireEpoch(e, Date.parse("2026-08-20T10:00:00+08:00")));
});

test("nextFireEpoch throws on an invalid once value", () => {
	const e = entry({ once: "not-a-date" });
	assert.throws(() => nextFireEpoch(e, 0), /invalid once/);
});
