import test from "node:test";
import assert from "node:assert/strict";
import {
	aggregateRunStats,
	scheduleOverviewFromLogs,
} from "./history.ts";
import type { TaskLog } from "../logging/types.ts";
import type { ScheduleEntry } from "./types.ts";

function makeLog(overrides: Partial<TaskLog> = {}): TaskLog {
	return {
		id: "log-1",
		taskId: "schedule:foo:1720000000000",
		phase: "execute",
		source: "scheduler",
		provider: "test",
		model: "test",
		project: "notes",
		status: "success",
		startedAt: 1720000000000,
		endedAt: 1720000001000,
		durationMs: 1000,
		userMessage: "run schedule",
		replyText: "ok",
		images: [],
		mutated: false,
		tools: [],
		conversation: [],
		usage: { input: 10, output: 1, cacheRead: 0, cacheWrite: 0, total: 11, costUsd: 0 },
		calls: [],
		modelCache: { queries: 1, hits: 0, hitRate: 0 },
		...overrides,
	};
}

function makeEntry(overrides: Partial<ScheduleEntry> = {}): ScheduleEntry {
	return {
		id: "foo",
		enabled: false,
		timezone: "Asia/Shanghai",
		message: "run it",
		interval: "1h",
		project: "notes",
		retry: { max: 2, backoffMs: 1000 },
		silentHours: { from: "22:00", to: "07:00" },
		holidayMode: "workday",
		recipient: { source: "telegram", userId: "chat-1" },
		...overrides,
	};
}

test("aggregateRunStats handles an empty log stream", () => {
	const stats = aggregateRunStats("empty", (visit) => {
		// No logs visited.
	});

	assert.equal(stats.scheduleId, "empty");
	assert.equal(stats.totalRuns, 0);
	assert.deepEqual(stats.byStatus, {});
	assert.equal(stats.firstRunAt, null);
	assert.equal(stats.lastRunAt, null);
	assert.equal(stats.avgDurationMs, 0);
	assert.deepEqual(stats.token, { input: 0, output: 0, total: 0 });
});

test("aggregateRunStats sums tokens, durations, and statuses", () => {
	const logs = [
		makeLog({
			id: "a",
			status: "success",
			startedAt: 1720000000000,
			durationMs: 100,
			usage: { input: 10, output: 2, cacheRead: 0, cacheWrite: 0, total: 12, costUsd: 0 },
		}),
		makeLog({
			id: "b",
			status: "error",
			startedAt: 1720000100000,
			durationMs: 250,
			usage: { input: 20, output: 3, cacheRead: 0, cacheWrite: 0, total: 23, costUsd: 0 },
		}),
		makeLog({
			id: "c",
			status: "validation-failed",
			startedAt: 1720000050000,
			durationMs: 400,
			usage: { input: 30, output: 4, cacheRead: 0, cacheWrite: 0, total: 34, costUsd: 0 },
		}),
	];

	const stats = aggregateRunStats("foo", (visit) => logs.forEach(visit));

	assert.equal(stats.totalRuns, 3);
	assert.deepEqual(stats.byStatus, {
		success: 1,
		error: 1,
		"validation-failed": 1,
	});
	assert.equal(stats.firstRunAt, 1720000000000);
	assert.equal(stats.lastRunAt, 1720000100000);
	assert.equal(stats.avgDurationMs, 250);
	assert.deepEqual(stats.token, { input: 60, output: 9, total: 69 });
});

test("scheduleOverviewFromLogs preserves entry fields and picks the latest run", () => {
	const entries = [
		makeEntry({ id: "foo" }),
		makeEntry({ id: "bar", enabled: false, timezone: "UTC", message: "bar" }),
	];
	const logs = [
		makeLog({
			id: "older",
			taskId: "schedule:foo:1720000000000",
			status: "error",
			startedAt: 1720000000000,
			durationMs: 900,
			project: "notes",
			usage: { input: 1, output: 2, cacheRead: 0, cacheWrite: 0, total: 3, costUsd: 0 },
		}),
		makeLog({
			id: "newer",
			taskId: "schedule:foo:1720000100000",
			status: "success",
			startedAt: 1720000100000,
			durationMs: 100,
			project: "notes",
			usage: { input: 4, output: 5, cacheRead: 0, cacheWrite: 0, total: 9, costUsd: 0 },
		}),
		makeLog({
			id: "other",
			taskId: "telegram:123:456",
			status: "success",
			startedAt: 1720000200000,
		}),
	];

	const overview = scheduleOverviewFromLogs(entries, logs);
	const foo = overview.find((e) => e.id === "foo");
	const bar = overview.find((e) => e.id === "bar");

	assert.ok(foo);
	assert.equal(foo.interval, "1h");
	assert.equal(foo.project, "notes");
	assert.deepEqual(foo.retry, { max: 2, backoffMs: 1000 });
	assert.deepEqual(foo.silentHours, { from: "22:00", to: "07:00" });
	assert.equal(foo.holidayMode, "workday");
	assert.deepEqual(foo.recipient, { source: "telegram", userId: "chat-1" });
	assert.equal(foo.nextFireAt, null);
	assert.deepEqual(foo.lastRun, {
		startedAt: 1720000100000,
		status: "success",
		durationMs: 100,
		project: "notes",
		tokenTotal: 9,
	});

	assert.ok(bar);
	assert.equal(bar.lastRun, null);
});
