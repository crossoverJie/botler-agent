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
	lastRun: ScheduleLastRun | null;
};

export type RunLogVisitor = (visit: (log: TaskLog) => void) => void;

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
	return entries.map((e) => ({
		...e,
		nextFireAt: nextFireAt(e),
		lastRun: toLastRun(latest.get(e.id)),
	}));
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
