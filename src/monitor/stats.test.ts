import { test } from "node:test";
import assert from "node:assert/strict";
import {
	stats,
	history,
	markEnqueued,
	markStarted,
	markFinished,
	recordStatus,
	markDuplicate,
	markDispatched,
	markFailed,
	markModelCache,
	markAgentStart,
	markAgentEnd,
	setChannelUp,
	setChannelDown,
	pushSample,
} from "./stats.ts";

// The stats object is a process-wide singleton; reset the numeric/object fields we touch before
// each test so cases don't leak into one another.
function reset(): void {
	stats.queueDepth = 0;
	stats.activeTasks = 0;
	stats.activeAgents = 0;
	stats.totalDispatched = 0;
	stats.duplicatesTotal = 0;
	stats.totalFailed = 0;
	stats.statusCounts = {};
	stats.lastDispatchAt = 0;
	stats.lastDispatchDurationMs = 0;
	stats.modelCacheQueries = 0;
	stats.modelCacheHits = 0;
	stats.schedulesTotal = 0;
	stats.schedulesEnabled = 0;
	stats.nextFireAt = 0;
	stats.lastFireAt = 0;
	stats.lastFireId = "";
	stats.channels = {};
	history.length = 0;
}

// ---------------------------------------------------------------------------
// queue / active-task lifecycle + negative clamping
// ---------------------------------------------------------------------------

test("markEnqueued increments queueDepth", () => {
	reset();
	markEnqueued();
	markEnqueued();
	assert.equal(stats.queueDepth, 2);
});

test("markStarted moves a task from queue to active (queueDepth-1, activeTasks+1)", () => {
	reset();
	stats.queueDepth = 3;
	markStarted();
	assert.equal(stats.queueDepth, 2);
	assert.equal(stats.activeTasks, 1);
});

test("markStarted never drives queueDepth below 0 (clamp)", () => {
	reset();
	stats.queueDepth = 0;
	markStarted();
	assert.equal(stats.queueDepth, 0, "queueDepth must not go negative");
	assert.equal(stats.activeTasks, 1);
});

test("markFinished decrements activeTasks and clamps at 0", () => {
	reset();
	stats.activeTasks = 1;
	markFinished();
	assert.equal(stats.activeTasks, 0);
	markFinished();
	assert.equal(stats.activeTasks, 0, "activeTasks must not go negative");
});

// ---------------------------------------------------------------------------
// status / counters
// ---------------------------------------------------------------------------

test("recordStatus buckets statuses independently and accumulates", () => {
	reset();
	recordStatus("success");
	recordStatus("success");
	recordStatus("error");
	assert.deepEqual(stats.statusCounts, { success: 2, error: 1 });
});

test("markDispatched increments totalDispatched and stamps lastDispatchAt", () => {
	reset();
	markDispatched();
	markDispatched();
	assert.equal(stats.totalDispatched, 2);
	assert.ok(stats.lastDispatchAt > 0);
});

test("markDuplicate only bumps duplicatesTotal (not totalDispatched)", () => {
	reset();
	markDuplicate();
	assert.equal(stats.duplicatesTotal, 1);
	assert.equal(stats.totalDispatched, 0);
});

test("markFailed increments totalFailed only", () => {
	reset();
	markFailed();
	markFailed();
	assert.equal(stats.totalFailed, 2);
	assert.equal(stats.queueDepth, 0);
	assert.equal(stats.activeTasks, 0);
});

// ---------------------------------------------------------------------------
// model cache
// ---------------------------------------------------------------------------

test("markModelCache increments queries and hits only on a hit", () => {
	reset();
	markModelCache(false);
	markModelCache(true);
	markModelCache(true);
	assert.equal(stats.modelCacheQueries, 3);
	assert.equal(stats.modelCacheHits, 2);
});

// ---------------------------------------------------------------------------
// agent lifecycle
// ---------------------------------------------------------------------------

test("markAgentStart/End track in-flight agents and clamp at 0", () => {
	reset();
	markAgentStart();
	markAgentStart();
	assert.equal(stats.activeAgents, 2);
	markAgentEnd();
	assert.equal(stats.activeAgents, 1);
	markAgentEnd();
	markAgentEnd();
	assert.equal(stats.activeAgents, 0, "activeAgents must not go negative");
});

// ---------------------------------------------------------------------------
// channel up/down
// ---------------------------------------------------------------------------

test("setChannelUp records firstUpAt once and updates lastUpAt on later calls", async () => {
	reset();
	setChannelUp("telegram");
	const first = stats.channels["telegram"].firstUpAt;
	assert.ok(first);
	assert.equal(stats.channels["telegram"].up, true);
	// Simulate time passing, then a recovery (transient blip) — firstUpAt must be preserved.
	await new Promise((r) => setTimeout(r, 5));
	setChannelUp("telegram");
	assert.equal(stats.channels["telegram"].firstUpAt, first, "firstUpAt must not be reset");
	assert.ok(stats.channels["telegram"].lastUpAt! >= first!);
});

test("setChannelDown marks the channel down with an error message", () => {
	reset();
	setChannelUp("wechat");
	setChannelDown("wechat", new Error("session expired"));
	const c = stats.channels["wechat"];
	assert.equal(c.up, false);
	assert.ok(c.lastErrorAt);
	assert.equal(c.lastError, "session expired");
});

// ---------------------------------------------------------------------------
// history ring buffer
// ---------------------------------------------------------------------------

test("pushSample appends and never exceeds capacity (oldest evicted)", () => {
	reset();
	// HISTORY_CAPACITY is 720; push well beyond it.
	for (let i = 0; i < 750; i++) {
		pushSample({ ts: i, rss: i, heapUsed: i, queueDepth: 0, activeTasks: 0, lagP50: 0, lagP99: 0 });
	}
	assert.equal(history.length, 720, "ring buffer must cap at capacity");
	// Oldest (ts=0..29) should have been evicted; newest (ts=749) present.
	assert.equal(history[0].ts, 30, "oldest sample should be evicted first");
	assert.equal(history[history.length - 1].ts, 749);
});
