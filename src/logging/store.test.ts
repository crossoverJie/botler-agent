import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { TaskLog } from "./types.ts";

let store: typeof import("./store.ts");

before(async () => {
	const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-logstore-")));
	process.env.BOTLER_CONFIG_DIR = tmp;
	process.env.BOTLER_LOG_DIR = path.join(tmp, "logs");
	store = await import("./store.ts");
});

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
	assert.equal(store.scheduleIdFromTaskId("schedule:drink-water:1720000000000"), "drink-water");
});

test("scheduleIdFromTaskId supports colon in the schedule id", () => {
	assert.equal(store.scheduleIdFromTaskId("schedule:foo:bar:1720000000000"), "foo:bar");
});

test("scheduleIdFromTaskId ignores non-scheduler task ids", () => {
	assert.equal(store.scheduleIdFromTaskId("telegram:123:456"), undefined);
	assert.equal(store.scheduleIdFromTaskId("random-task-id"), undefined);
});

test("matchesLogQuery filters scheduler logs by scheduleId", () => {
	const q = { source: "scheduler", scheduleId: "foo:bar" };
	assert.equal(store.matchesLogQuery(makeLog({ taskId: "schedule:foo:bar:1720000000000" }), q), true);
	assert.equal(store.matchesLogQuery(makeLog({ taskId: "schedule:other:1720000000000" }), q), false);
});

test("matchesLogQuery filters by phase", () => {
	const q = { source: "scheduler", scheduleId: "foo", phase: "execute" as const };
	assert.equal(store.matchesLogQuery(makeLog({ phase: "execute" }), q), true);
	assert.equal(store.matchesLogQuery(makeLog({ phase: "self-heal" }), q), false);
});

test("matchesLogQuery matches a multi-project log by any selected project", () => {
	const log = makeLog({ project: null, projects: ["cook", "vocab"] });
	assert.equal(store.matchesLogQuery(log, { project: "cook" }), true);
	assert.equal(store.matchesLogQuery(log, { project: "vocab" }), true);
	assert.equal(store.matchesLogQuery(log, { project: "notes" }), false);
});

test("matchesLogQuery falls back to the legacy project for old single-project logs", () => {
	const log = makeLog({ project: "cook" });
	assert.equal(store.matchesLogQuery(log, { project: "cook" }), true);
	assert.equal(store.matchesLogQuery(log, { project: "vocab" }), false);
});

test("matchesLogQuery full-text search hits on a selected project name", () => {
	const log = makeLog({ project: null, projects: ["cook", "vocab"], userMessage: "add", replyText: "done" });
	assert.equal(store.matchesLogQuery(log, { q: "cook" }), true);
	assert.equal(store.matchesLogQuery(log, { q: "vocab" }), true);
	assert.equal(store.matchesLogQuery(log, { q: "nothing" }), false);
});

test("filterAndPageLogs filters by scheduleId and handles ids containing a colon", () => {
	const logs = [
		makeLog({ taskId: "schedule:foo:bar:1720000003000", startedAt: 1720000003000 }),
		makeLog({ taskId: "schedule:foo:bar:1720000001000", startedAt: 1720000001000 }),
		makeLog({ taskId: "schedule:other:1720000002000", startedAt: 1720000002000 }),
	];

	const page = store.filterAndPageLogs(logs, {
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

	const page = store.filterAndPageLogs(logs, {
		source: "scheduler",
		scheduleId: "foo",
		limit: 1,
		offset: 1,
	});

	assert.equal(page.total, 3);
	assert.equal(page.logs.length, 1);
	assert.equal(page.logs[0].taskId, "schedule:foo:1720000002000");
});

test("summary counts a multi-project log under every selected project", () => {
	store.appendTaskLog(
		makeLog({
			id: "sum-multi",
			taskId: "telegram:summulti:1",
			source: "telegram",
			project: null,
			projects: ["sumcook", "sumvocab"],
		}),
	);
	store.appendTaskLog(
		makeLog({
			id: "sum-single",
			taskId: "telegram:sumsingle:1",
			source: "telegram",
			project: "sumcook",
			projects: ["sumcook"],
		}),
	);

	const s = store.summary();
	assert.equal(s.byProject["sumcook"], 2);
	assert.equal(s.byProject["sumvocab"], 1);
});
