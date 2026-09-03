import test from "node:test";
import assert from "node:assert/strict";
import {
	aggregateRunStats,
	orphanHistoryIndex,
	scheduleOverviewFromLogs,
	scheduleState,
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

/** makeEntry() defaults to interval:"1h", which must go first — triggers are mutually exclusive. */
function makeOnceEntry(id: string, once: string, overrides: Partial<ScheduleEntry> = {}): ScheduleEntry {
	const base = makeEntry({ id, enabled: true, ...overrides });
	delete base.interval;
	return { ...base, once };
}

test("scheduleState splits enabled/disabled and spent one-time schedules", () => {
	// Disabled wins even with a trigger that would otherwise fire: it is deliberate and reversible.
	assert.equal(scheduleState({ enabled: false, nextFireAt: 1700000000000 }), "paused");
	assert.equal(scheduleState({ enabled: false, nextFireAt: null }), "paused");
	assert.equal(scheduleState({ enabled: true, nextFireAt: 1700000000000 }), "active");
	assert.equal(scheduleState({ enabled: true, nextFireAt: null }), "past");
	// An unparseable trigger also yields a null nextFireAt and so lands in "past", but that case
	// never reaches the UI: loadSchedules() -> normalizeEntry() skips entries that fail to compile.
});

test("scheduleOverviewFromLogs reports state per entry", () => {
	const entries = [
		makeOnceEntry("future", "2099-01-01T00:00:00+08:00"),
		makeOnceEntry("spent", "2020-01-01T00:00:00+08:00"),
		makeEntry({ id: "off", enabled: false }),
	];
	const logs = [makeLog({ id: "run", taskId: "schedule:spent:1720000000000" })];

	const overview = scheduleOverviewFromLogs(entries, logs);
	const byId = new Map(overview.map((e) => [e.id, e]));

	assert.equal(byId.get("future")?.state, "active");
	assert.equal(byId.get("spent")?.state, "past");
	assert.equal(byId.get("off")?.state, "paused");
});

test("a spent once is only distinguishable as fired by its last run", () => {
	const spent = [makeOnceEntry("spent", "2020-01-01T00:00:00+08:00")];

	// Fired: the run was recorded, so the UI shows the run rather than a "missed" warning.
	const fired = scheduleOverviewFromLogs(spent, [
		makeLog({ id: "run", taskId: "schedule:spent:1720000000000" }),
	]);
	assert.equal(fired[0]?.state, "past");
	assert.notEqual(fired[0]?.lastRun, null);

	// Missed: the time passed with no run at all (the process was down, or the watermark was
	// reseeded to now by a restart before it could fire).
	const missed = scheduleOverviewFromLogs(spent, []);
	assert.equal(missed[0]?.state, "past");
	assert.equal(missed[0]?.lastRun, null);
});

test("orphanHistoryIndex returns an empty index for no logs", () => {
	assert.deepEqual(orphanHistoryIndex([], []), []);
});

test("orphanHistoryIndex aggregates runs per id and keeps only the newest log's fields", () => {
	const logs = [
		// Deliberately out of order: first/last must be min/max, and the retained fields must come
		// from the newest run rather than the last one visited.
		makeLog({
			id: "newer", taskId: "schedule:foo:1720000200000", startedAt: 1720000200000,
			status: "error", project: "cook", userMessage: "newest message",
		}),
		makeLog({
			id: "older", taskId: "schedule:foo:1720000100000", startedAt: 1720000100000,
			status: "success", project: "notes", userMessage: "oldest message",
		}),
		makeLog({
			id: "middle", taskId: "schedule:foo:1720000150000", startedAt: 1720000150000,
			status: "success", project: "notes", userMessage: "middle message",
		}),
		// Not a scheduler dispatch — no schedule id to recover.
		makeLog({ id: "chat", taskId: "telegram:123:456", startedAt: 1720000300000 }),
	];

	const [foo] = orphanHistoryIndex([], logs);

	assert.equal(foo.runCount, 3);
	assert.equal(foo.firstRunAt, 1720000100000);
	assert.equal(foo.lastRunAt, 1720000200000);
	assert.equal(foo.lastStatus, "error");
	assert.equal(foo.lastProject, "cook");
	assert.equal(foo.lastMessage, "newest message");
});

test("orphanHistoryIndex excludes ids still present in the config", () => {
	const entries = [makeEntry({ id: "foo" })];
	const logs = [
		makeLog({ id: "a", taskId: "schedule:foo:1720000100000", startedAt: 1720000100000 }),
		makeLog({ id: "b", taskId: "schedule:gone:1720000200000", startedAt: 1720000200000 }),
	];

	const index = orphanHistoryIndex(entries, logs);

	assert.deepEqual(index.map((i) => i.scheduleId), ["gone"]);
});

test("orphanHistoryIndex sorts newest first and truncates a long message", () => {
	const long = "x".repeat(500);
	const logs = [
		makeLog({ id: "a", taskId: "schedule:old:1720000100000", startedAt: 1720000100000 }),
		makeLog({ id: "b", taskId: "schedule:new:1720000200000", startedAt: 1720000200000, userMessage: long }),
	];

	const index = orphanHistoryIndex([], logs);

	assert.deepEqual(index.map((i) => i.scheduleId), ["new", "old"]);
	assert.equal(index[0]?.lastMessage.length, 200);
});

test("orphanHistoryIndex caps the index at 200 ids, keeping the newest", () => {
	const logs = [];
	for (let i = 0; i < 201; i++) {
		logs.push(
			makeLog({ id: `s${i}`, taskId: `schedule:id-${i}:1`, startedAt: 1720000000000 + i }),
		);
	}

	const index = orphanHistoryIndex([], logs);

	assert.equal(index.length, 200);
	assert.equal(index[0]?.scheduleId, "id-200");
	assert.ok(!index.some((i) => i.scheduleId === "id-0"));
});

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
