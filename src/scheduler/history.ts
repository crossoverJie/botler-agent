/**
 * Read-only scheduler history queries for the WebUI.
 *
 * V1 derives history from the existing task-log store: scheduler dispatches are written with
 * source "scheduler" and taskId "schedule:<id>:<epoch>". This keeps one execution record format
 * and avoids a second persistent history file.
 */

import { nextFireEpoch } from "./cron.ts";
import { loadSchedules } from "./store.ts";
import {
	queryLogs,
	scanLogs,
	scheduleIdFromTaskId,
	type LogQuery,
	type LogPage,
} from "../logging/store.ts";
import type { TaskLog } from "../logging/types.ts";
import type { ScheduleEntry } from "./types.ts";

export type ScheduleRunQuery = Pick<LogQuery, "from" | "to" | "q" | "limit" | "offset">;

export interface ScheduleRunStats {
	scheduleId: string;
	totalRuns: number;
	byStatus: Record<string, number>;
	firstRunAt: number | null;
	lastRunAt: number | null;
	avgDurationMs: number;
	token: { input: number; output: number; total: number };
}

export interface ScheduleLastRun {
	startedAt: number;
	status: TaskLog["status"];
	durationMs: number;
	project: string | null;
	tokenTotal: number;
}

export type ScheduleOverviewItem = ScheduleEntry & {
	nextFireAt: number | null;
	state: ScheduleState;
	lastRun: ScheduleLastRun | null;
};

export type RunLogVisitor = (visit: (log: TaskLog) => void) => void;

/** Lifecycle of a schedule as the UI should present it. */
export type ScheduleState = "active" | "paused" | "past";

/**
 * Derived from `enabled` + `nextFireAt` only — never from the trigger type — so it stays one
 * definition and generalizes to any future trigger that can become inert.
 *
 * - "paused": disabled. Deliberate and reversible; never fires.
 * - "past": enabled but no next fire (a spent `once`). Cannot fire again; history is all that's left.
 * - "active": enabled with a next fire.
 *
 * `nextFireAt` is also null for an unparseable trigger, but that never reaches here: loadSchedules()
 * -> normalizeEntry() compiles every trigger and skips the entries that fail.
 */
export function scheduleState(item: { enabled: boolean; nextFireAt: number | null }): ScheduleState {
	if (!item.enabled) return "paused";
	return item.nextFireAt === null ? "past" : "active";
}

function nextFireAt(e: ScheduleEntry): number | null {
	if (!e.enabled) return null;
	try {
		const next = nextFireEpoch(e, Date.now());
		return next === Infinity ? null : next;
	} catch {
		return null;
	}
}

export function scheduleRuns(id: string, q: ScheduleRunQuery = {}): LogPage {
	return queryLogs({ ...q, source: "scheduler", scheduleId: id });
}

export function aggregateRunStats(
	scheduleId: string,
	iterate: RunLogVisitor,
): ScheduleRunStats {
	const byStatus: Record<string, number> = {};
	const token = { input: 0, output: 0, total: 0 };
	let durationSum = 0;
	let firstRunAt: number | null = null;
	let lastRunAt: number | null = null;
	let totalRuns = 0;

	iterate((l) => {
		totalRuns += 1;
		byStatus[l.status] = (byStatus[l.status] ?? 0) + 1;
		token.input += l.usage.input;
		token.output += l.usage.output;
		token.total += l.usage.total;
		durationSum += l.durationMs;
		firstRunAt = firstRunAt === null ? l.startedAt : Math.min(firstRunAt, l.startedAt);
		lastRunAt = lastRunAt === null ? l.startedAt : Math.max(lastRunAt, l.startedAt);
	});

	return {
		scheduleId,
		totalRuns,
		byStatus,
		firstRunAt,
		lastRunAt,
		avgDurationMs: totalRuns ? Math.round(durationSum / totalRuns) : 0,
		token,
	};
}

export function scheduleRunStats(id: string): ScheduleRunStats {
	return aggregateRunStats(id, (visit) => {
		scanLogs({ source: "scheduler", scheduleId: id, phase: "execute" }, visit);
	});
}

function toLastRun(l: TaskLog | undefined): ScheduleLastRun | null {
	return l
		? {
				startedAt: l.startedAt,
				status: l.status,
				durationMs: l.durationMs,
				project: l.project,
				tokenTotal: l.usage.total,
			}
		: null;
}

function latestRunBySchedule(logs: Iterable<TaskLog>): Map<string, TaskLog> {
	const latest = new Map<string, TaskLog>();

	for (const l of logs) {
		const scheduleId = scheduleIdFromTaskId(l.taskId);
		if (!scheduleId) continue;
		const prev = latest.get(scheduleId);
		if (!prev || l.startedAt > prev.startedAt) latest.set(scheduleId, l);
	}

	return latest;
}

function buildScheduleOverview(
	entries: ScheduleEntry[],
	latest: Map<string, TaskLog>,
): ScheduleOverviewItem[] {
	return entries.map((e) => {
		const next = nextFireAt(e);
		return {
			...e,
			nextFireAt: next,
			state: scheduleState({ enabled: e.enabled, nextFireAt: next }),
			lastRun: toLastRun(latest.get(e.id)),
		};
	});
}

export function scheduleOverviewFromLogs(
	entries: ScheduleEntry[],
	logs: Iterable<TaskLog>,
): ScheduleOverviewItem[] {
	return buildScheduleOverview(entries, latestRunBySchedule(logs));
}

export function scheduleOverview(): ScheduleOverviewItem[] {
	const entries = loadSchedules();
	const latest = new Map<string, TaskLog>();

	scanLogs({ source: "scheduler", phase: "execute" }, (l) => {
		const scheduleId = scheduleIdFromTaskId(l.taskId);
		if (!scheduleId) return;
		const prev = latest.get(scheduleId);
		if (!prev || l.startedAt > prev.startedAt) latest.set(scheduleId, l);
	});

	return buildScheduleOverview(entries, latest);
}

/** Bounds the index payload for a pathological log directory; newest ids win. */
const MAX_HISTORY_IDS = 200;
/** A schedule message can be up to 10 KB, so cap the retained copy. */
const HISTORY_MESSAGE_CHARS = 200;

/**
 * Run-derived summary for one schedule id. Everything comes from the task logs, so it survives
 * deletion of the config entry: there is no scheduleId column on TaskLog, and the link is
 * `taskId` = "schedule:<id>:<epoch>" parsed by scheduleIdFromTaskId.
 */
export interface ScheduleHistoryEntry {
	scheduleId: string;
	runCount: number;
	firstRunAt: number;
	lastRunAt: number;
	lastStatus: TaskLog["status"];
	lastProject: string | null;
	/** userMessage of the latest run — the only context left once the config spec is gone. */
	lastMessage: string;
}

interface HistoryAcc {
	count: number;
	first: number;
	last: number;
	log: TaskLog;
}

function pushHistory(acc: Map<string, HistoryAcc>, l: TaskLog): void {
	const id = scheduleIdFromTaskId(l.taskId);
	if (!id) return;
	const prev = acc.get(id);
	if (!prev) {
		acc.set(id, { count: 1, first: l.startedAt, last: l.startedAt, log: l });
		return;
	}
	prev.count += 1;
	if (l.startedAt < prev.first) prev.first = l.startedAt;
	if (l.startedAt > prev.last) {
		prev.last = l.startedAt;
		prev.log = l;
	}
}

function finishHistory(
	acc: Map<string, HistoryAcc>,
	exclude: ReadonlySet<string>,
): ScheduleHistoryEntry[] {
	const out: ScheduleHistoryEntry[] = [];
	for (const [scheduleId, a] of acc) {
		if (exclude.has(scheduleId)) continue;
		out.push({
			scheduleId,
			runCount: a.count,
			firstRunAt: a.first,
			lastRunAt: a.last,
			lastStatus: a.log.status,
			lastProject: a.log.project,
			lastMessage: a.log.userMessage.slice(0, HISTORY_MESSAGE_CHARS),
		});
	}
	return out.sort((x, y) => y.lastRunAt - x.lastRunAt).slice(0, MAX_HISTORY_IDS);
}

/**
 * Pure: every schedule id found in `logs` that is no longer in `entries`, newest last-run first.
 * Pass an empty `entries` to index every id.
 */
export function orphanHistoryIndex(
	entries: ScheduleEntry[],
	logs: Iterable<TaskLog>,
): ScheduleHistoryEntry[] {
	const acc = new Map<string, HistoryAcc>();
	for (const l of logs) pushHistory(acc, l);
	return finishHistory(acc, new Set(entries.map((e) => e.id)));
}

/**
 * Schedules that were removed from schedules.json but still have runs in the task logs. The diff
 * is done server-side because the client has no test harness, and it is read-only: the saved
 * listener would wake the firing loop on any write.
 */
export function scheduleHistoryIndex(): ScheduleHistoryEntry[] {
	const acc = new Map<string, HistoryAcc>();
	// phase "execute" only, matching scheduleRunStats() so runCount agrees with the Runs figure
	// in the history drawer's stats cards (self-heal retries are excluded in both).
	scanLogs({ source: "scheduler", phase: "execute" }, (l) => pushHistory(acc, l));
	return finishHistory(acc, new Set(loadSchedules().map((e) => e.id)));
}
