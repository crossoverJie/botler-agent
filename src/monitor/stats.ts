/**
 * In-process observability state (the monitoring layer).
 *
 * A pure in-memory singleton written to by instrumentation points across the codebase
 * (dispatcher / runner / scheduler / channels) and read only by the health endpoints
 * (src/monitor/health.ts). All helpers are synchronous, O(1), and self-clamping (never
 * produce negative counters even if calls arrive out of order). No I/O, no async.
 *
 * Zero new npm dependencies — kept deliberately tiny so the monitoring layer can never
 * become a failure source for the agent itself.
 */

/** Per-channel liveness snapshot (coarse-grained, process-local). */
export interface ChannelState {
	/** Whether the channel is currently considered connected / serving. */
	up: boolean;
	/** First time it came up (within this process; not reset on transient blips). */
	firstUpAt?: number;
	/** Most recent time it was marked up. */
	lastUpAt?: number;
	/** Timestamp of the last fatal error that took it down. */
	lastErrorAt?: number;
	/** Message of the last fatal error (process-local only; no secrets / user data). */
	lastError?: string;
}

/** All process-level counters and gauges exposed by the health endpoints. */
export interface Stats {
	/** Process start epoch (ms). */
	startedAt: number;
	// ---- dispatcher / runner ----
	/** Tasks queued but not yet executing on the serial chain. */
	queueDepth: number;
	/** Tasks currently executing on the serial chain (the chain allows at most 1). */
	activeTasks: number;
	/** Agent.prompt() instances currently in flight. */
	activeAgents: number;
	/** Non-duplicate dispatches that entered the execution chain. */
	totalDispatched: number;
	/** Messages dropped by the 5-minute dedup window. */
	duplicatesTotal: number;
	/** error + validation-failed task count (a save that didn't happen is still a failure). */
	totalFailed: number;
	/** Final task status buckets. */
	statusCounts: Record<string, number>;
	/** Epoch ms of the last dispatch that entered the chain. */
	lastDispatchAt: number;
	/** Duration (ms) of the most recently finished dispatch. */
	lastDispatchDurationMs: number;
	// ---- model-resolution cache (shared with task logs as single source of truth) ----
	modelCacheQueries: number;
	modelCacheHits: number;
	// ---- scheduler ----
	/** Total schedule entries in schedules.json. */
	schedulesTotal: number;
	/** Enabled schedule entries. */
	schedulesEnabled: number;
	/** Epoch ms of the next fire; 0 means nothing pending. */
	nextFireAt: number;
	/** Epoch ms of the most recent fire. */
	lastFireAt: number;
	/** Id of the most recently fired entry. */
	lastFireId: string;
	// ---- channels ----
	channels: Record<string, ChannelState>;
}

export const stats: Stats = {
	startedAt: Date.now(),
	queueDepth: 0,
	activeTasks: 0,
	activeAgents: 0,
	totalDispatched: 0,
	duplicatesTotal: 0,
	totalFailed: 0,
	statusCounts: {},
	lastDispatchAt: 0,
	lastDispatchDurationMs: 0,
	modelCacheQueries: 0,
	modelCacheHits: 0,
	schedulesTotal: 0,
	schedulesEnabled: 0,
	nextFireAt: 0,
	lastFireAt: 0,
	lastFireId: "",
	channels: {},
};

/** A task joined the serial queue (awaiting its turn). */
export function markEnqueued(): void {
	stats.queueDepth += 1;
}

/** A queued task actually started executing: leave the queue, enter the active slot. */
export function markStarted(): void {
	stats.queueDepth = Math.max(0, stats.queueDepth - 1);
	stats.activeTasks += 1;
}

/** The active task finished (success or failure). */
export function markFinished(): void {
	stats.activeTasks = Math.max(0, stats.activeTasks - 1);
}

/** Bucket a finished task by its final status. */
export function recordStatus(s: string): void {
	stats.statusCounts[s] = (stats.statusCounts[s] ?? 0) + 1;
}

/** Mark a message as dropped by dedup (does NOT count as a dispatch). */
export function markDuplicate(): void {
	stats.duplicatesTotal += 1;
}

/** Mark a dispatch that entered the execution chain. */
export function markDispatched(): void {
	stats.totalDispatched += 1;
	stats.lastDispatchAt = Date.now();
}

/** Mark a task as failed (error / validation-failed). */
export function markFailed(): void {
	stats.totalFailed += 1;
}

/** Increment the model-resolution cache counters. */
export function markModelCache(hit: boolean): void {
	stats.modelCacheQueries += 1;
	if (hit) stats.modelCacheHits += 1;
}

/** Increment the count of in-flight Agent instances. */
export function markAgentStart(): void {
	stats.activeAgents += 1;
}

/** Decrement the count of in-flight Agent instances. */
export function markAgentEnd(): void {
	stats.activeAgents = Math.max(0, stats.activeAgents - 1);
}

/** Channel recovered / confirmed serving. Does NOT reset firstUpAt. */
export function setChannelUp(name: string): void {
	const prev = stats.channels[name];
	stats.channels[name] = {
		up: true,
		firstUpAt: prev?.firstUpAt ?? Date.now(),
		lastUpAt: Date.now(),
		lastErrorAt: prev?.lastErrorAt,
		lastError: prev?.lastError,
	};
}

/** Channel hit a fatal error. */
export function setChannelDown(name: string, err: unknown): void {
	const prev = stats.channels[name] ?? {};
	stats.channels[name] = {
		up: false,
		firstUpAt: prev.firstUpAt,
		lastUpAt: prev.lastUpAt,
		lastErrorAt: Date.now(),
		lastError: err instanceof Error ? err.message : String(err),
	};
}

/** A single sampled point for the rolling trend buffer (fed by the sampler in health.ts). */
export interface Sample {
	ts: number;
	rss: number;
	heapUsed: number;
	queueDepth: number;
	activeTasks: number;
	lagP50: number;
	lagP99: number;
}

const HISTORY_CAPACITY = 720; // ~1 hour at 5s sampling
/** Rolling ring buffer of historical samples (read-only exposed via /healthz/history). */
export const history: Sample[] = [];

/**
 * Pure push: append a pre-assembled sample to the ring buffer, evicting the oldest when full.
 * This module never imports the event-loop-lag histogram (avoids a reverse dependency on
 * health.ts); the sampler assembles the Sample and calls this.
 */
export function pushSample(s: Sample): void {
	history.push(s);
	if (history.length > HISTORY_CAPACITY) history.shift();
}
