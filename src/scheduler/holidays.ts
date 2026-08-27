/**
 * China legal-holiday calendar for `holidayMode:"workday"` schedules.
 *
 * The cached data lives in `~/.botler-agent/holidays.json` (overridable via BOTLER_HOLIDAYS_FILE).
 * It is keyed by year and records, per year, which dates are statutory holidays (法定假日, to skip)
 * and which are 调休 makeup workdays (补班, to fire even on a weekend). The source is fetched fresh at
 * startup (when missing) and every 24h from BOTLER_HOLIDAY_API_URL (default NateScarlet/holiday-cn).
 *
 * Any outage keeps the cached data and never crashes: `refreshHolidaysYear` swallows all errors,
 * `ensureHolidays` never throws, and `isLegalWorkday` degrades to plain Mon–Fri when a year is absent.
 *
 * This module imports only `config.ts` + `node:fs` + `global.fetch` — never cron/engine — so there is
 * no import cycle.
 */

import { existsSync, mkdirSync, readFileSync, writeFileSync, renameSync, copyFileSync, readdirSync, unlinkSync } from "node:fs";
import { dirname, join } from "node:path";
import { CONFIG } from "../config.ts";

/** Cached calendar, keyed by 4-digit year; each year lists its holidays and makeup workdays (YYYY-MM-DD). */
export type HolidayData = Record<string, { holidays: string[]; workdays: string[] }>;

const FETCH_TIMEOUT_MS = 10_000;

function dateKey(y: number, m: number, d: number): string {
	return `${y}-${String(m).padStart(2, "0")}-${String(d).padStart(2, "0")}`;
}

function dayOfWeek(y: number, m: number, d: number): number {
	return new Date(Date.UTC(y, m - 1, d)).getUTCDay(); // 0=Sun..6=Sat
}

/**
 * Is `y-m-d` a China legal workday?
 * - no data / missing year → fall back to plain Mon–Fri (graceful degradation).
 * - date in `holidays` → false (skip 法定假日).
 * - date in `workdays` → true (fire 补班 even on a weekend).
 * - otherwise → Mon–Fri.
 */
export function isLegalWorkday(data: HolidayData, y: number, m: number, d: number): boolean {
	const year = data[String(y)];
	if (!year) return dayOfWeek(y, m, d) >= 1 && dayOfWeek(y, m, d) <= 5;
	const key = dateKey(y, m, d);
	if (year.holidays.includes(key)) return false;
	if (year.workdays.includes(key)) return true;
	return dayOfWeek(y, m, d) >= 1 && dayOfWeek(y, m, d) <= 5;
}

/** Read the cached holiday calendar; returns `{}` on missing/invalid/malformed file (never throws). */
export function loadHolidays(): HolidayData {
	try {
		if (!existsSync(CONFIG.holidaysFile)) return {};
		const raw = JSON.parse(readFileSync(CONFIG.holidaysFile, "utf8")) as Record<string, unknown>;
		if (!raw || typeof raw !== "object" || Array.isArray(raw)) return {};
		// Validate shape: every year entry must carry holidays/workdays arrays. A malformed but
		// parseable file (e.g. {"2026": {}} or {"2026": {"holidays": 123}}) would otherwise make
		// isLegalWorkday throw when it does `(123).includes(...)` during fire / preview — silently
		// breaking every workday entry. Treat such files as "no data" and fall back to Mon–Fri.
		for (const v of Object.values(raw)) {
			if (
				!v || typeof v !== "object" || Array.isArray(v) ||
				!Array.isArray((v as HolidayData[string]).holidays) ||
				!Array.isArray((v as HolidayData[string]).workdays)
			) {
				console.warn("[scheduler] holidays.json has malformed year entry; ignoring cached calendar");
				return {};
			}
		}
		return raw as HolidayData;
	} catch {
		return {};
	}
}

let savedListener: (() => void) | null = null;
/** Module-level listener invoked by `saveHolidays`; the engine registers `reloadSchedules`. */
export function setHolidaysSavedListener(fn: (() => void) | null): void {
	savedListener = fn;
}

const MAX_HOLIDAYS_BACKUPS = 10;

/** Keep only the most recent MAX_HOLIDAYS_BACKUPS holiday-calendar backups. */
function pruneHolidaysBackups(backupDir: string): void {
	try {
		const matched = readdirSync(backupDir)
			.filter((f) => f.startsWith("holidays.json."))
			.sort(); // ascending (oldest) → keep the tail
		while (matched.length > MAX_HOLIDAYS_BACKUPS) {
			const old = matched.shift();
			if (!old) break;
			unlinkSync(join(backupDir, old));
		}
	} catch {
		// best-effort
	}
}

/** Atomic write (tmp + rename, with a pre-write backup) + fire the saved listener. */
export function saveHolidays(data: HolidayData): void {
	const target = CONFIG.holidaysFile;
	const dir = dirname(target);
	mkdirSync(dir, { recursive: true });
	// Pre-write backup so a corrupt write is recoverable.
	try {
		if (existsSync(target)) {
			const backupDir = join(dir, ".backups");
			mkdirSync(backupDir, { recursive: true, mode: 0o700 });
			copyFileSync(target, join(backupDir, `holidays.json.${Date.now()}`));
			pruneHolidaysBackups(backupDir);
		}
	} catch {
		// best-effort
	}
	const tmp = `${target}.tmp.${process.pid}.${Date.now()}`;
	writeFileSync(tmp, JSON.stringify(data, null, 2) + "\n", "utf8");
	renameSync(tmp, target);
	savedListener?.();
}

/**
 * Fetch and merge one year's calendar. Returns true on success, false on any failure
 * (network/outage/invalid JSON) — never throws, so a refresh can't break startup.
 * The load → merge → save is one synchronous block, so a parallel refresh can't lose updates.
 */
export async function refreshHolidaysYear(year: number): Promise<boolean> {
	try {
		const url = CONFIG.holidayApiUrl.replace("{year}", String(year));
		const ctrl = new AbortController();
		const timer = setTimeout(() => ctrl.abort(), FETCH_TIMEOUT_MS);
		let text: string;
		try {
			const resp = await fetch(url, { signal: ctrl.signal });
			if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
			text = await resp.text();
		} finally {
			clearTimeout(timer);
		}
		const json = JSON.parse(text) as { days?: Array<{ date: string; isOffDay?: boolean }> };
		const holidays: string[] = [];
		const workdays: string[] = [];
		for (const day of json.days ?? []) {
			if (typeof day.date !== "string") continue;
			if (day.isOffDay) holidays.push(day.date);
			else workdays.push(day.date);
		}
		// Synchronous block: read, merge, write — no await in between.
		const cur = loadHolidays();
		cur[String(year)] = { holidays, workdays };
		saveHolidays(cur);
		return true;
	} catch (err) {
		console.warn(`[scheduler] holiday calendar refresh for ${year} failed:`, err instanceof Error ? err.message : err);
		return false;
	}
}

let inFlight = false;
/**
 * Refresh the current and next year's calendars. Serializes the two refreshes (not Promise.all)
 * and is guarded by `inFlight` so a slow startup refresh can't overlap the 24h tick. Never throws.
 */
export async function ensureHolidays(): Promise<void> {
	if (inFlight) return;
	inFlight = true;
	try {
		const year = new Date().getFullYear(); // recomputed each call so the 24h tick rolls into the new year
		await refreshHolidaysYear(year);
		await refreshHolidaysYear(year + 1);
	} catch {
		// unreachable (refreshHolidaysYear swallows), but keep the guard honest
	} finally {
		inFlight = false;
	}
}
