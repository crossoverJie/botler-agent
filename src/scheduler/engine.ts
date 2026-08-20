/**
 * In-process scheduler engine.
 *
 * A single setTimeout-driven loop: each round, re-read schedules.json (if changed), compute the
 * soonest next-fire among enabled entries, sleep until then, fire the due ones via dispatch,
 * then recompute. The engine only *triggers* — all execution, dedup, queueing, validation,
 * commit and logging is the existing dispatch pipeline, reused verbatim.
 *
 * Design notes:
 * - A single sleep is cancellable (stored resolve) so reloadSchedules() can wake the loop
 *   immediately after a config edit ("instant effect").
 * - Empty / no-enabled-entry case is guarded (a "soonest" of Infinity would hang the sleep).
 * - Each fired entry gets a unique id (`schedule:<id>:<epochMs>`) so it bypasses the 5-min dedup
 *   and is traceable in the task log. source is "scheduler".
 * - Due-ness is measured from a per-entry watermark (last fire / first sighting), never from `now`:
 *   `nextFireEpoch` is strictly after its argument, so `next <= now` would never hold.
 * - By default NO catch-up: on (re)start, missed fires are skipped (the watermark starts at now).
 *   A fire that comes due while the loop is blocked on a long dispatch is late, but not lost.
 * - Retry here means re-running the whole schedule entry once; it is independent of dispatch's
 *   internal validation self-heal. Only error / validation-failed statuses are retried.
 */

import { statSync } from "node:fs";
import { CONFIG } from "../config.ts";
import { dispatch } from "../dispatcher.ts";
import { deliver } from "../push/deliver.ts";
import { loadSchedules, setSchedulesSavedListener } from "./store.ts";
import { nextFireEpoch } from "./cron.ts";
import type { ScheduleEntry } from "./types.ts";
import { stats } from "../monitor/stats.ts";

const IDLE_POLL_MS = 60_000;

let lastMtime = -1;
let wakeSleep: (() => void) | null = null;

// A schedule write (via the schedule tool or the WebUI) wakes the loop immediately.
// Kept one-way (store.ts never imports engine) to avoid a dependency cycle.
setSchedulesSavedListener(reloadSchedules);

/**
 * Per-entry watermark: the instant from which the entry's next fire is measured (its last fire,
 * or when it was first seen). A fire is due when `nextFireEpoch(entry, watermark) <= now`.
 * Measuring from `now` instead would never be due, because `nextFireEpoch` returns a time
 * strictly after its argument.
 */
const lastFire = new Map<string, number>();

/** Watermark for an entry; a first sighting (startup / newly added) starts at `now` = no catch-up. */
function watermarkOf(id: string, now: number): number {
	const prev = lastFire.get(id);
	if (prev !== undefined) return prev;
	lastFire.set(id, now);
	return now;
}

/** Forget watermarks of entries that are gone or disabled, so re-enabling never back-fires. */
function pruneWatermarks(entries: ScheduleEntry[]): void {
	const live = new Set(entries.map((e) => e.id));
	for (const id of [...lastFire.keys()]) if (!live.has(id)) lastFire.delete(id);
}

function sleep(ms: number): Promise<void> {
	return new Promise((resolve) => {
		const t = setTimeout(() => {
			wakeSleep = null;
			resolve();
		}, ms);
		wakeSleep = () => {
			clearTimeout(t);
			wakeSleep = null;
			resolve();
		};
	});
}

/** Wake the loop to re-read schedules.json immediately (called after a WebUI edit). */
export function reloadSchedules(): void {
	lastMtime = -1;
	wakeSleep?.();
}

function fileMtime(): number {
	try {
		return statSync(CONFIG.schedulesFile).mtimeMs;
	} catch {
		return 0; // missing file → treat as "no file", never throw ENOENT
	}
}

function configChanged(): boolean {
	return fileMtime() !== lastMtime;
}

async function fire(e: ScheduleEntry, attempt = 0): Promise<void> {
	const result = await dispatch(e.message, {
		id: `schedule:${e.id}:${Date.now()}`,
		source: "scheduler",
		projectHint: e.project,
	});
	const failed = result.status === "error" || result.status === "validation-failed";
	if (failed && attempt < (e.retry?.max ?? 0)) {
		await sleep(e.retry!.backoffMs);
		return fire(e, attempt + 1);
	}
	// A no-project reminder (e.g. "remind user to drink water") has no data subproject to route to, so routing
	// returns the generic "couldn't decide a project" text. For such entries the schedule's own
	// message IS the reminder — push it as-is instead of the routing-failure boilerplate.
	const text = result.status === "unknown-project" ? e.message : result.text;
	// Push the result back to the entry's recipient (if any). Failure is only logged, not
	// retried here: a dead token won't recover inside the retry backoff and deliver() already
	// tried the channel fallback chain.
	if (e.recipient) {
		const push = await deliver({ text, images: result.images }, e.recipient);
		if (!push.ok) {
			console.error(`[scheduler] ${e.id} push failed: ${push.error ?? "unknown"}`);
		}
	}
	// Record the most recent fire (for the health endpoint).
	stats.lastFireAt = Date.now();
	stats.lastFireId = e.id;
	console.log(`[scheduler] ${e.id} -> ${result.status}`);
}

async function loop(): Promise<void> {
	while (true) {
		if (configChanged()) lastMtime = fileMtime();
		const all = loadSchedules();
		const entries = all.filter((e) => e.enabled);
		pruneWatermarks(entries);
		// Expose schedule counts to the health endpoint each round.
		stats.schedulesTotal = all.length;
		stats.schedulesEnabled = entries.length;

		// Soonest upcoming fire across all entries; Infinity = nothing scheduled (or all broken).
		let soonest = Infinity;
		for (const e of entries) {
			try {
				const now = Date.now();
				let next = nextFireEpoch(e, watermarkOf(e.id, now));
				if (next <= now) {
					// Advance the watermark before firing: dispatch is awaited (it shares the global
					// queue and can take a while), and the next fire must be measured from this point.
					lastFire.set(e.id, now);
					await fire(e);
					next = nextFireEpoch(e, Date.now());
				}
				if (next < soonest) soonest = next;
			} catch (err) {
				// A single bad entry must never take down the whole loop.
				console.error(`[scheduler] entry "${e.id}" failed:`, err instanceof Error ? err.message : err);
			}
		}

		// Record the next fire instant (Infinity → 0 means "nothing pending").
		stats.nextFireAt = soonest === Infinity ? 0 : soonest;

		const pollMs = soonest === Infinity ? IDLE_POLL_MS : soonest - Date.now();
		await sleep(Math.max(1000, pollMs));
	}
}

/** Start the scheduler. No-op unless SCHEDULER_ENABLED=1. CLI mode never calls this. */
export function startScheduler(): void {
	if (!CONFIG.schedulerEnabled) return;
	console.log(`[scheduler] started (config: ${CONFIG.schedulesFile})`);
	void loop();
}
