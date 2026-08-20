/**
 * Scheduler config store: read/validate/write `schedules.json`.
 *
 * This module only does file I/O and validation — it never touches the engine state
 * (compiled cache / mtime dirty flag live in engine.ts). On read, malformed entries are
 * skipped individually (with a warning) so a single bad entry can't take down the whole
 * scheduler. On write, `saveSchedules` normalizes through the same `normalizeEntry` used on read
 * (so a round-trip is lossless) and delegates the atomic-write-and-backup to `config-store`,
 * keeping backup logic in one place.
 */

import { existsSync, readFileSync } from "node:fs";
import { CONFIG } from "../config.ts";
import { compileSchedule, isValidTimezone, parseOnceEpoch } from "./cron.ts";
import { writeConfigFile } from "../webui/config-store.ts";
import type { Recipient } from "../push/types.ts";
import type { ScheduleEntry } from "./types.ts";

const DEFAULT_TZ = "Asia/Shanghai";

/** Max length of a schedule message (prevents a runaway write from bloating the config). */
const MESSAGE_MAX_CHARS = 10 * 1024;

/**
 * Saved-listener: set by the scheduler engine (engine.ts) at module load so a schedule write
 * wakes the loop immediately. Kept here instead of importing the engine to avoid the
 * engine → dispatcher → runner → tools → schedule → store → engine cycle.
 */
let onSaved: (() => void) | null = null;
export function setSchedulesSavedListener(fn: (() => void) | null): void {
	onSaved = fn;
}

function parseHM(s: unknown): [number, number] | null {
	if (typeof s !== "string") return null;
	const m = /^(\d{1,2}):(\d{2})$/.exec(s);
	if (!m) return null;
	const h = Number(m[1]);
	const min = Number(m[2]);
	if (h > 23 || min > 59) return null;
	return [h, min];
}

/**
 * Validate and normalize one raw schedule object into a {@link ScheduleEntry}.
 * Throws on any fatal problem so the caller (load/save) can skip or reject it.
 */
export function normalizeEntry(item: unknown): ScheduleEntry {
	if (typeof item !== "object" || item === null) throw new Error("entry must be an object");
	const o = item as Record<string, unknown>;

	const id = o.id;
	if (typeof id !== "string" || !id) throw new Error("missing id");
	const message = o.message;
	if (typeof message !== "string" || !message.trim()) throw new Error(`schedule "${id}" missing message`);
	if (message.length > MESSAGE_MAX_CHARS) {
		throw new Error(`schedule "${id}" message exceeds ${MESSAGE_MAX_CHARS} chars`);
	}

	const hasCron = typeof o.cron === "string" && o.cron.trim().length > 0;
	const hasInterval = typeof o.interval === "string" && o.interval.trim().length > 0;
	const hasAt = typeof o.at === "string" && o.at.trim().length > 0;
	const hasOnce = typeof o.once === "string" && o.once.trim().length > 0;
	const triggerCount = [hasCron, hasInterval, hasAt, hasOnce].filter(Boolean).length;
	if (triggerCount === 0) {
		throw new Error(`schedule "${id}" needs one of cron/interval/at/once`);
	}
	if (triggerCount > 1) {
		throw new Error(`schedule "${id}" must specify exactly one of cron/interval/at/once`);
	}

	const tz = typeof o.timezone === "string" && o.timezone ? o.timezone : DEFAULT_TZ;
	if (!isValidTimezone(tz)) throw new Error(`schedule "${id}" invalid timezone "${tz}"`);

	const entry: ScheduleEntry = {
		id,
		enabled: o.enabled !== false, // default true unless explicitly false
		timezone: tz,
		message,
	};

	if (hasCron) entry.cron = o.cron as string;
	if (hasInterval) {
		if (!/^\d+(m|h|d)$/.test(o.interval as string)) {
			throw new Error(`schedule "${id}" invalid interval "${o.interval}"`);
		}
		// Range-check here so a bad value fails on write/load with the same readable message it
		// would get from the cron compiler — not silently later. ("90m" would otherwise compile
		// to minute [0] = hourly.)
		const n = Number((o.interval as string).slice(0, -1));
		const u = (o.interval as string).at(-1);
		if (n < 1 || (u === "m" && n > 59) || (u === "h" && n > 24) || (u === "d" && n !== 1)) {
			throw new Error(
				`schedule "${id}" invalid interval "${o.interval}": minutes 1-59, hours 1-24, days only "1d"`,
			);
		}
		entry.interval = o.interval as string;
	}
	if (hasAt) {
		if (!parseHM(o.at)) throw new Error(`schedule "${id}" invalid at "${o.at}"`);
		entry.at = o.at as string;
	}
	if (hasOnce) {
		if (parseOnceEpoch(o.once as string, tz) === null) {
			throw new Error(`schedule "${id}" invalid once "${o.once}"`);
		}
		entry.once = o.once as string;
	}
	if (typeof o.project === "string" && o.project) entry.project = o.project;

	if (o.retry !== undefined) {
		const r = o.retry as Record<string, unknown>;
		const max = Number(r.max);
		const backoffMs = Number(r.backoffMs);
		if (!Number.isInteger(max) || max < 0) throw new Error(`schedule "${id}" invalid retry.max`);
		if (!Number.isInteger(backoffMs) || backoffMs < 0) throw new Error(`schedule "${id}" invalid retry.backoffMs`);
		entry.retry = { max, backoffMs };
	}

	if (o.silentHours !== undefined) {
		const s = o.silentHours as Record<string, unknown>;
		if (!parseHM(s.from) || !parseHM(s.to)) {
			throw new Error(`schedule "${id}" invalid silentHours`);
		}
		entry.silentHours = { from: s.from as string, to: s.to as string };
	}

	if (o.recipient !== undefined) {
		const r = o.recipient as Record<string, unknown>;
		const source = r?.source;
		const userId = r?.userId;
		if (source !== "wechat" && source !== "telegram" && source !== "feishu") {
			throw new Error(`schedule "${id}" invalid recipient.source`);
		}
		if (typeof userId !== "string" || !userId) {
			throw new Error(`schedule "${id}" invalid recipient.userId`);
		}
		entry.recipient = { source, userId } as Recipient;
	}

	// Final sanity: cron/interval/at entries must compile. once entries are resolved directly
	// by nextFireEpoch and do not go through the cron compiler.
	if (!hasOnce) compileSchedule(entry);
	return entry;
}

/** Load and normalize schedules from disk. Missing/invalid file → []; per-entry errors are skipped with a warning. */
export function loadSchedules(): ScheduleEntry[] {
	const file = CONFIG.schedulesFile;
	if (!existsSync(file)) return [];
	try {
		const raw = JSON.parse(readFileSync(file, "utf8")) as { schedules?: unknown };
		if (!raw || !Array.isArray(raw.schedules)) {
			console.warn("[scheduler] schedules.json missing 'schedules' array");
			return [];
		}
		const out: ScheduleEntry[] = [];
		const ids = new Set<string>();
		for (const item of raw.schedules) {
			try {
				const e = normalizeEntry(item);
				if (ids.has(e.id)) {
					console.warn(`[scheduler] duplicate schedule id "${e.id}", skipping`);
					continue;
				}
				ids.add(e.id);
				out.push(e);
			} catch (err) {
				console.warn(`[scheduler] skipping invalid entry:`, err instanceof Error ? err.message : err);
			}
		}
		return out;
	} catch (e) {
		console.warn("[scheduler] failed to read schedules.json:", e);
		return [];
	}
}

/**
 * Validate and normalize a raw list, rejecting duplicate ids. Throws with a readable message
 * on the first bad entry — unlike {@link loadSchedules}, a write must be all-or-nothing.
 */
export function normalizeSchedules(items: unknown[]): ScheduleEntry[] {
	const ids = new Set<string>();
	const out: ScheduleEntry[] = [];
	for (const item of items) {
		const e = normalizeEntry(item);
		if (ids.has(e.id)) throw new Error(`duplicate schedule id "${e.id}"`);
		ids.add(e.id);
		out.push(e);
	}
	return out;
}

/**
 * Validate + normalize then atomically write schedules.json (pre-write backup via config-store).
 * Writing the *normalized* entries is what keeps "what is written" == "what is read back":
 * otherwise a field only checked on the read path (retry / silentHours / project / enabled)
 * could be persisted and then silently dropped by the next load.
 */
export function saveSchedules(items: unknown[]): void {
	const entries = normalizeSchedules(items);
	writeConfigFile("schedules.json", JSON.stringify({ schedules: entries }, null, 2) + "\n");
	onSaved?.();
}
