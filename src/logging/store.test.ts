import test from "node:test";
import assert from "node:assert/strict";
import { filterAndPageLogs, matchesLogQuery, scheduleIdFromTaskId } from "./store.ts";
import type { TaskLog } from "./types.ts";

function makeLog(overrides: Partial<TaskLog> = {}): TaskLog {
	return {
		id: "log-1",
		taskId: "schedule:foo:1720000000000",
		phase: "execute",
		source: "scheduler",
		provider: "test",
		model: "test",
		project: null,
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

test("scheduleIdFromTaskId extracts a normal schedule id", () => {
	assert.equal(scheduleIdFromTaskId("schedule:drink-water:1720000000000"), "drink-water");
});

test("scheduleIdFromTaskId supports colon in the schedule id", () => {
	assert.equal(scheduleIdFromTaskId("schedule:foo:bar:1720000000000"), "foo:bar");
});

test("scheduleIdFromTaskId ignores non-scheduler task ids", () => {
	assert.equal(scheduleIdFromTaskId("telegram:123:456"), undefined);
	assert.equal(scheduleIdFromTaskId("random-task-id"), undefined);
});

test("matchesLogQuery filters scheduler logs by scheduleId", () => {
	const q = { source: "scheduler", scheduleId: "foo:bar" };
	assert.equal(matchesLogQuery(makeLog({ taskId: "schedule:foo:bar:1720000000000" }), q), true);
	assert.equal(matchesLogQuery(makeLog({ taskId: "schedule:other:1720000000000" }), q), false);
});

test("matchesLogQuery filters by phase", () => {
	const q = { source: "scheduler", scheduleId: "foo", phase: "execute" as const };
	assert.equal(matchesLogQuery(makeLog({ phase: "execute" }), q), true);
	assert.equal(matchesLogQuery(makeLog({ phase: "self-heal" }), q), false);
});

test("filterAndPageLogs filters by scheduleId and handles ids containing a colon", () => {
	const logs = [
		makeLog({ taskId: "schedule:foo:bar:1720000003000", startedAt: 1720000003000 }),
		makeLog({ taskId: "schedule:foo:bar:1720000001000", startedAt: 1720000001000 }),
		makeLog({ taskId: "schedule:other:1720000002000", startedAt: 1720000002000 }),
	];

	const page = filterAndPageLogs(logs, {
		source: "scheduler",
		scheduleId: "foo:bar",
		limit: 1,
		offset: 0,
	});

	assert.equal(page.total, 2);
	assert.equal(page.logs.length, 1);
	assert.equal(page.logs[0].taskId, "schedule:foo:bar:1720000003000");
});

test("filterAndPageLogs pages past the first match without changing total", () => {
	const logs = [
		makeLog({ taskId: "schedule:foo:1720000003000", startedAt: 1720000003000 }),
		makeLog({ taskId: "schedule:foo:1720000002000", startedAt: 1720000002000 }),
		makeLog({ taskId: "schedule:foo:1720000001000", startedAt: 1720000001000 }),
	];

	const page = filterAndPageLogs(logs, {
		source: "scheduler",
		scheduleId: "foo",
		limit: 1,
		offset: 1,
	});

	assert.equal(page.total, 3);
	assert.equal(page.logs.length, 1);
	assert.equal(page.logs[0].taskId, "schedule:foo:1720000002000");
});
