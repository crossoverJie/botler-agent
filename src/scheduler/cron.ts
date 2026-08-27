/**
 * Zero-dependency cron compiler + next-fire calculator + IANA timezone conversion.
 *
 * Supports a 5-field cron at minute granularity:
 *   min hour day-of-month month day-of-week
 * Supports wildcards, N-step (e.g. every 5 minutes), single values, ranges (a-b),
 * stepped ranges (a-b with step N), and comma lists (a,b,c) per field.
 * day-of-week: 0=Sunday..6=Saturday, 7 also accepted as Sunday.
 * When both day-of-month and day-of-week are restricted, the entry fires only when BOTH match (AND).
 *
 * Timezone conversion uses the built-in `Intl.DateTimeFormat` — no external deps.
 * China has no DST, so the single-correction `wallToEpoch` is exact enough for this use case.
 */

import type { ScheduleEntry } from "./types.ts";
import { isLegalWorkday, loadHolidays, type HolidayData } from "./holidays.ts";

/** A wall-clock time (no timezone); second is usually 0 for scheduling. */
export interface WallClock {
	year: number;
	month: number; // 1-12
	day: number; // 1-31
	hour: number;
	minute: number;
	second: number;
}

/** Compiled cron: one sorted allowed-value array per field. */
export interface CompiledCron {
	minute: number[];
	hour: number[];
	dom: number[];
	month: number[];
	dow: number[];
}

const DEFAULT_TZ = "Asia/Shanghai";

const DAYS_IN_MONTH = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

/** Days in a specific month of a specific year (Gregorian leap rule). */
function daysInMonth(year: number, month: number): number {
	if (month === 2) return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0 ? 29 : 28;
	return DAYS_IN_MONTH[month - 1];
}

/** Validate an IANA timezone string; throws nothing, returns false on invalid. */
export function isValidTimezone(tz: string): boolean {
	try {
		new Intl.DateTimeFormat("en-US", { timeZone: tz });
		return true;
	} catch {
		return false;
	}
}

// ---------------------------------------------------------------------------
// Field compilation
// ---------------------------------------------------------------------------

function compileField(spec: string, min: number, max: number, isDow = false): number[] {
	const out = new Set<number>();
	for (const partRaw of spec.split(",")) {
		const part = partRaw.trim();
		if (!part) continue;
		let step = 1;
		let range = part;
		if (part.includes("/")) {
			const [r, s] = part.split("/");
			range = r;
			step = Number(s);
			if (!Number.isInteger(step) || step <= 0) throw new Error(`invalid step in "${part}"`);
		}
		let lo = min;
		let hi = max;
		if (range === "*" || range === "?") {
			lo = min;
			hi = max;
		} else if (range.includes("-")) {
			const [a, b] = range.split("-");
			lo = Number(a);
			hi = Number(b);
			if (!Number.isInteger(lo) || !Number.isInteger(hi)) throw new Error(`invalid range "${range}"`);
		} else {
			const v = Number(range);
			if (!Number.isInteger(v)) throw new Error(`invalid field "${range}"`);
			lo = hi = v;
		}
		if (lo < min || hi > max) throw new Error(`value out of range [${min},${max}] in "${part}"`);
		if (lo > hi) throw new Error(`range start > end in "${part}"`);
		for (let v = lo; v <= hi; v += step) {
			if (isDow && v === 7) out.add(0);
			else out.add(v);
		}
	}
	return [...out].sort((a, b) => a - b);
}

/** Max day number a month can ever have (February counted as 29 for leap years). */
function maxDayOfMonth(month: number): number {
	return month === 2 ? 29 : DAYS_IN_MONTH[month - 1];
}

/**
 * Reject a cron whose day-of-month can never occur in any allowed month (e.g. "0 0 30 2 *").
 * Such an expression is syntactically valid but has no solution; without this check the
 * next-fire search would run to its bound on every scheduler round and log an error forever.
 */
function assertDayReachable(c: CompiledCron, expr: string): void {
	const reachable = c.month.some((m) => c.dom.some((d) => d <= maxDayOfMonth(m)));
	if (!reachable) {
		throw new Error(
			`cron "${expr}" has no valid date: day-of-month ${c.dom.join(",")} never occurs in month ${c.month.join(",")}`,
		);
	}
}

/** Compile a 5-field cron expression into allowed-value arrays. Throws on malformed input. */
export function compileCron(expr: string): CompiledCron {
	const fields = expr.trim().split(/\s+/);
	if (fields.length !== 5) throw new Error(`cron requires 5 fields, got ${fields.length}: "${expr}"`);
	const c: CompiledCron = {
		minute: compileField(fields[0], 0, 59),
		hour: compileField(fields[1], 0, 23),
		dom: compileField(fields[2], 1, 31),
		month: compileField(fields[3], 1, 12),
		dow: compileField(fields[4], 0, 7, true),
	};
	assertDayReachable(c, expr);
	return c;
}

/**
 * Neutralize a workday-mode cron's date fields: keep minute/hour, replace day-of-month / month /
 * day-of-week with `*`. A workday schedule fires on "every legal workday at HH:MM", so the date
 * fields are meaningless and must not be validated (e.g. "0 18 31 2 *" — Feb 31 never occurs — is
 * accepted in workday mode and would otherwise be rejected by `assertDayReachable`).
 */
function neutralizeWorkdayCron(cron: string): string {
	const f = cron.trim().split(/\s+/);
	if (f.length !== 5) throw new Error(`cron requires 5 fields, got ${f.length}: "${cron}"`);
	return `${f[0]} ${f[1]} * * *`;
}

/**
 * Compile a workday-mode cron: validates only minute/hour (via the neutralized expression);
 * date fields are ignored. Returns a `CompiledCron` so the time fields can be reused directly.
 */
export function compileWorkdayCron(cron: string): CompiledCron {
	return compileCron(neutralizeWorkdayCron(cron));
}

/**
 * Normalize a simple interval to a cron. Fails loudly rather than silently degrading:
 * a 5-field cron can only express steps *within* a field, so out-of-range steps would
 * collapse to a single value ("90m" -> minute [0] = hourly, "2d" -> daily).
 */
function intervalToCron(s: string): string {
	const m = /^(\d+)(m|h|d)$/.exec(s);
	if (!m) throw new Error(`invalid interval "${s}"`);
	const n = Number(m[1]);
	const u = m[2];
	if (n < 1) throw new Error(`invalid interval "${s}": step must be >= 1`);
	if (u === "m") {
		if (n > 59) throw new Error(`invalid interval "${s}": minutes must be 1-59, use "1h"/"2h" or a cron for longer`);
		return `*/${n} * * * *`; // every n minutes, wall-clock aligned
	}
	if (u === "h") {
		if (n > 24) throw new Error(`invalid interval "${s}": hours must be 1-24, use "1d" or a cron for longer`);
		return `0 */${n} * * *`; // every n hours, aligned to even hours at minute 0
	}
	if (n !== 1) {
		throw new Error(
			`invalid interval "${s}": a 5-field cron cannot express "every ${n} days"; use cron/at instead`,
		);
	}
	return `0 0 * * *`; // daily at 00:00
}

function atToCron(s: string): string {
	const m = /^(\d{1,2}):(\d{2})$/.exec(s);
	if (!m) throw new Error(`invalid at "${s}"`);
	const h = Number(m[1]);
	const min = Number(m[2]);
	if (h > 23 || min > 59) throw new Error(`invalid at "${s}"`);
	return `${min} ${h} * * *`;
}

/**
 * Parse a `once` trigger value to epoch ms.
 *
 * Accepted forms:
 * - absolute time with explicit offset: "2026-08-20T22:00:00+08:00" / "2026-08-20T14:00:00Z"
 * - naive local form (no offset): "2026-08-20T22:00:00" or "2026-08-20 22:00", interpreted in `tz`
 *
 * Returns null on malformed input. Does not check whether the time is in the future.
 */
export function parseOnceEpoch(s: string, tz: string): number | null {
	const trimmed = s.trim();
	// Explicit offset (Z or +/-HH:MM) -> let Date.parse resolve the absolute instant.
	// Validate the calendar date ourselves first: Date.parse silently rolls over impossible
	// dates (e.g. "2026-02-30..." -> Mar 2), so we'd otherwise violate the "reject impossible
	// calendar dates" guarantee that the naive form already provides.
	if (/[zZ]$|[+-]\d{2}:?\d{2}$/.test(trimmed)) {
		const ymd = /^(\d{4})-(\d{2})-(\d{2})/.exec(trimmed);
		if (ymd) {
			const y = Number(ymd[1]);
			const mo = Number(ymd[2]);
			const d = Number(ymd[3]);
			if (mo < 1 || mo > 12 || d < 1 || d > daysInMonth(y, mo)) return null;
		}
		const t = Date.parse(trimmed);
		return Number.isNaN(t) ? null : t;
	}
	// Naive local form: interpret in the entry's timezone.
	const m = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{1,2}):(\d{2})(?::(\d{2}))?$/.exec(trimmed);
	if (!m) return null;
	const year = Number(m[1]);
	const month = Number(m[2]);
	const day = Number(m[3]);
	const hour = Number(m[4]);
	const minute = Number(m[5]);
	const second = m[6] !== undefined ? Number(m[6]) : 0;
	if (
		month < 1 || month > 12 ||
		day < 1 || day > daysInMonth(year, month) ||
		hour > 23 || minute > 59 || second > 59
	) return null;
	return wallToEpoch({ year, month, day, hour, minute, second }, tz);
}

/**
 * Compile a schedule entry to a cron. One of cron / interval / at is used
 * (priority cron > interval > at); `once` is a one-shot trigger resolved directly by
 * nextFireEpoch and never reaches this function. Throws if none (or an invalid form)
 * is present. The original entry keeps its user-written value; this returns a fresh
 * compiled object.
 */
export function compileSchedule(e: ScheduleEntry): CompiledCron {
	let expr: string;
	if (e.cron) expr = e.cron;
	else if (e.interval) expr = intervalToCron(e.interval);
	else if (e.at) expr = atToCron(e.at);
	else throw new Error("schedule must specify one of cron/interval/at (or once for one-shot)");
	return compileCron(expr);
}

// ---------------------------------------------------------------------------
// Wall-clock helpers (operate in naive calendar space, independent of timezone)
// ---------------------------------------------------------------------------

function wallMs(w: WallClock): number {
	return Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute, w.second);
}

export function addMinutes(w: WallClock, m: number): WallClock {
	const d = new Date(wallMs(w) + m * 60_000);
	return {
		year: d.getUTCFullYear(),
		month: d.getUTCMonth() + 1,
		day: d.getUTCDate(),
		hour: d.getUTCHours(),
		minute: d.getUTCMinutes(),
		second: d.getUTCSeconds(),
	};
}

export function addDays(w: WallClock, n: number): WallClock {
	return addMinutes(w, n * 24 * 60);
}

/** Day of week (0=Sunday..6=Saturday) of a wall date; timezone-independent. */
export function dayOfWeek(w: WallClock): number {
	return new Date(Date.UTC(w.year, w.month - 1, w.day)).getUTCDay();
}

// ---------------------------------------------------------------------------
// IANA timezone conversion (built-in Intl only)
// ---------------------------------------------------------------------------

/** epoch ms -> wall clock in the given IANA timezone. */
export function epochToWall(ts: number, tz: string): WallClock {
	const d = new Date(ts);
	const parts = new Intl.DateTimeFormat("en-US", {
		timeZone: tz,
		year: "numeric",
		month: "2-digit",
		day: "2-digit",
		hour: "2-digit",
		minute: "2-digit",
		second: "2-digit",
		hour12: false,
	}).formatToParts(d);
	const get = (t: string): string => parts.find((p) => p.type === t)?.value ?? "0";
	let hour = Number(get("hour"));
	if (hour === 24) hour = 0; // some environments emit 24 for midnight
	return {
		year: Number(get("year")),
		month: Number(get("month")),
		day: Number(get("day")),
		hour,
		minute: Number(get("minute")),
		second: Number(get("second")),
	};
}

/** wall clock (interpreted in the given IANA timezone) -> epoch ms. */
export function wallToEpoch(w: WallClock, tz: string): number {
	let epoch = wallMs(w); // guess: treat the wall time as if it were UTC
	// Correct the guess: re-derive the wall the guess maps to, then shift by the difference.
	for (let i = 0; i < 3; i++) {
		const w2 = epochToWall(epoch, tz);
		epoch -= wallMs(w2) - wallMs(w);
	}
	return epoch;
}

// ---------------------------------------------------------------------------
// Workday-mode (China legal workday) next-fire calculation
// ---------------------------------------------------------------------------

/**
 * Resolve the (hour, minute) fields of a workday entry. For a cron we compile the neutralized
 * expression (date fields ignored, validating only minute/hour); for interval/at we fall back to
 * the standard compile. Returns the raw sorted arrays so callers can enumerate every time-of-day.
 */
function workdayTimeFields(e: ScheduleEntry): { hour: number[]; minute: number[] } {
	if (e.cron) {
		const c = compileWorkdayCron(e.cron);
		return { hour: c.hour, minute: c.minute };
	}
	const c = compileSchedule(e);
	return { hour: c.hour, minute: c.minute };
}

/**
 * Next fire epoch for a `holidayMode:"workday"` entry: strictly after `afterEpoch`, on a China legal
 * workday, at one of the entry's time-of-day slots. Enumerates ALL (h,m) pairs (sorted by time-of-day)
 * so a cron like `"0 8,18 * * *"` fires at both 08:00 and 18:00. After applying silent-hours, re-checks
 * the deferred day: if the deferral crossed into a non-workday, the slot is skipped and the search
 * continues. `data` defaults to the live cached calendar (re-read every call), so a manual edit to
 * holidays.json takes effect on the next recompute. Throws if no match within 3 years.
 */
export function nextWorkdayFireEpoch(
	e: ScheduleEntry,
	afterEpoch: number,
	data: HolidayData = loadHolidays(),
): number {
	const tz = e.timezone || DEFAULT_TZ;
	const { hour, minute } = workdayTimeFields(e);
	const times: Array<{ h: number; m: number }> = [];
	for (const h of hour) for (const m of minute) times.push({ h, m });
	times.sort((a, b) => a.h * 60 + a.m - (b.h * 60 + b.m));

	const start = epochToWall(afterEpoch, tz);
	for (let i = 0; i < 366 * 3; i++) {
		const dw = i === 0 ? start : addDays(start, i);
		if (!isLegalWorkday(data, dw.year, dw.month, dw.day)) continue;
		for (const { h, m } of times) {
			const cand = wallToEpoch({ ...dw, hour: h, minute: m, second: 0 }, tz);
			if (cand <= afterEpoch) continue;
			const final = e.silentHours ? applySilentHours(e, cand, tz) : cand;
			const fw = epochToWall(final, tz);
			// A silent-hours deferral may have crossed into a non-workday; if so, do not fire there.
			if (!isLegalWorkday(data, fw.year, fw.month, fw.day)) continue;
			return final;
		}
	}
	throw new Error("nextWorkdayFireEpoch: no match within 3 years");
}

// ---------------------------------------------------------------------------
// Next-fire calculation
// ---------------------------------------------------------------------------

function dayMatches(c: CompiledCron, year: number, month: number, day: number): boolean {
	return (
		c.dom.includes(day) &&
		c.dow.includes(dayOfWeek({ year, month, day, hour: 0, minute: 0, second: 0 }))
	);
}

/**
 * Smallest minute-of-day satisfying the hour+minute fields at or after `floorMin`, or null
 * if this day has no such slot left. Both field arrays are sorted, so the first hit is minimal.
 */
function firstTimeOfDay(c: CompiledCron, floorMin: number): number | null {
	for (const h of c.hour) {
		if (h * 60 + 59 < floorMin) continue;
		for (const m of c.minute) {
			const tod = h * 60 + m;
			if (tod >= floorMin) return tod;
		}
	}
	return null;
}

/**
 * Next wall-clock time (strictly after `after`, minute granularity) satisfying the compiled cron.
 * Timezone-independent: the caller converts to/from epoch.
 *
 * Advances a day at a time (skipping whole disallowed months) and picks the time-of-day
 * arithmetically, instead of scanning minute by minute — a Feb-29 cron resolves in ~120 steps
 * rather than ~1.5M. Throws if no match within a 5-year bound.
 */
export function nextCronWall(c: CompiledCron, after: WallClock): WallClock {
	const start = addMinutes({ ...after, second: 0 }, 1);
	let { year, month, day } = start;
	// Only the first candidate day is constrained by `after`; later days start at 00:00.
	let floorMin = start.hour * 60 + start.minute;

	for (let i = 0; i < 5 * 366; i++) {
		if (!c.month.includes(month)) {
			month++;
			if (month > 12) {
				month = 1;
				year++;
			}
			day = 1;
			floorMin = 0;
			continue;
		}
		if (dayMatches(c, year, month, day)) {
			const tod = firstTimeOfDay(c, floorMin);
			if (tod !== null) {
				return { year, month, day, hour: Math.floor(tod / 60), minute: tod % 60, second: 0 };
			}
		}
		day++;
		floorMin = 0;
		if (day > daysInMonth(year, month)) {
			day = 1;
			month++;
			if (month > 12) {
				month = 1;
				year++;
			}
		}
	}
	throw new Error("nextCronWall: no match found within 5 years");
}

/**
 * Convenience: epoch ms of the next fire of an entry strictly after `afterEpoch`,
 * with the optional silent-hours window applied (defer to window end).
 */
export function nextFireEpoch(e: ScheduleEntry, afterEpoch: number): number {
	// One-shot trigger: must be handled BEFORE compileSchedule(), which has no `once` case and
	// would reject a once-only entry as missing a trigger. Returns the target instant when it is
	// still in the future relative to the watermark, or Infinity once it has fired/passed.
	// silentHours is intentionally NOT applied: a one-shot reminder is an explicit, exact instant.
	if (e.once) {
		const t = parseOnceEpoch(e.once, e.timezone || DEFAULT_TZ);
		if (t === null) throw new Error(`schedule "${e.id}" invalid once "${e.once}"`);
		return t > afterEpoch ? t : Infinity;
	}
	// Workday mode: fire only on China legal workdays. The cron's date fields are ignored — only
	// its hour:minute(s) matter — and holidays / makeup workdays are supplied by the cached calendar.
	if (e.holidayMode === "workday") return nextWorkdayFireEpoch(e, afterEpoch);
	const cron = compileSchedule(e);
	const tz = e.timezone || DEFAULT_TZ;
	const wall = epochToWall(afterEpoch, tz);
	const nextWall = nextCronWall(cron, wall);
	let next = wallToEpoch(nextWall, tz);
	if (e.silentHours) next = applySilentHours(e, next, tz);
	return next;
}

/**
 * If `epoch` falls inside the silent-hours window, defer it to the window end.
 * `from === to` is treated as "no window" (return as-is). Bounded to <=366 iterations;
 * if it cannot resolve, the window is ignored and a warning is logged.
 */
function applySilentHours(e: ScheduleEntry, epoch: number, tz: string): number {
	const sh = e.silentHours!;
	const [fh, fm] = sh.from.split(":").map(Number);
	const [th, tm] = sh.to.split(":").map(Number);
	const fromMin = fh * 60 + fm;
	const toMin = th * 60 + tm;
	if (fromMin === toMin) return epoch; // degenerate window → ignore

	let cur = epoch;
	for (let i = 0; i < 366; i++) {
		const w = epochToWall(cur, tz);
		const cm = w.hour * 60 + w.minute;
		const inWindow =
			fromMin < toMin ? cm >= fromMin && cm < toMin : cm >= fromMin || cm < toMin;
		if (!inWindow) return cur;
		// Advance to the window end.
		if (fromMin < toMin) {
			cur = wallToEpoch({ ...w, hour: th, minute: tm, second: 0 }, tz);
		} else {
			const nd = addDays(w, 1);
			cur = wallToEpoch({ ...nd, hour: th, minute: tm, second: 0 }, tz);
		}
	}
	console.warn(`[scheduler] silentHours for "${e.id}" did not resolve; ignoring DND`);
	return epoch;
}
