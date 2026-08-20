/**
 * Process health / metrics HTTP endpoints (the monitoring layer).
 *
 * A zero-dependency `node:http` server bound to 127.0.0.1 only — never 0.0.0.0 (the Feishu
 * webhook binds 0.0.0.0 because it is an external receiver; the health surface is local-only).
 * Read-only, no secrets / user data / data-directory access. Runs independently of the WebUI
 * (which is optional); the WebUI merely renders this data into a "🩺 Monitoring" view.
 *
 * Routes:
 *   GET /healthz           → 200 JSON process snapshot (for humans / liveness probes)
 *   GET /metrics           → 200 text/plain Prometheus exposition (for scrapers)
 *   GET /healthz/history   → 200 JSON rolling trend samples (for the WebUI charts)
 *
 * Design rules (from plan/MONITORING.md):
 *   - The event-loop-lag histogram is enabled only inside startHealthServer(), so CLI mode and
 *     MONITOR_ENABLED=0 don't pay for sampling.
 *   - server.listen() errors (e.g. EADDRINUSE) are logged, never thrown — monitoring must not
 *     be able to kill the agent.
 *   - The request handler is wrapped in try/catch → 500, so the monitor can't crash the process.
 */

import { createServer, type ServerResponse } from "node:http";
import { monitorEventLoopDelay } from "node:perf_hooks";
import { CONFIG } from "../config.ts";
import { stats, history, pushSample, type Sample } from "./stats.ts";

const EVENT_LOOP_RESOLUTION = 20; // ms; histogram bucket resolution
const SAMPLE_INTERVAL_MS = 5000;
const HISTORY_CAPACITY = 720;

const eventLoopLag = monitorEventLoopDelay({ resolution: EVENT_LOOP_RESOLUTION });

/** Convert a histogram percentile (ns) into milliseconds. */
function lagMs(p: number): number {
	return eventLoopLag.percentile(p) / 1e6;
}

function sendJson(res: ServerResponse, data: unknown, status = 200): void {
	const body = JSON.stringify(data);
	res.writeHead(status, { "Content-Type": "application/json; charset=utf-8" });
	res.end(body);
}

/** Build the instantaneous process snapshot returned by /healthz. Exported so the WebUI server can serve it on its own port (same process, no cross-port CORS). */
export function buildSnapshot() {
	const mem = process.memoryUsage();
	return {
		ok: true,
		pid: process.pid,
		uptimeSec: Math.floor(process.uptime()),
		startedAt: stats.startedAt,
		memory: {
			rss: mem.rss,
			heapUsed: mem.heapUsed,
			heapTotal: mem.heapTotal,
			external: mem.external,
			arrayBuffers: mem.arrayBuffers,
		},
		eventLoopLagMs: {
			p50: Number(lagMs(50).toFixed(3)),
			p90: Number(lagMs(90).toFixed(3)),
			p99: Number(lagMs(99).toFixed(3)),
		},
		queueDepth: stats.queueDepth,
		activeTasks: stats.activeTasks,
		activeAgents: stats.activeAgents,
		totalDispatched: stats.totalDispatched,
		duplicatesTotal: stats.duplicatesTotal,
		totalFailed: stats.totalFailed,
		statusCounts: stats.statusCounts,
		modelCache: {
			queries: stats.modelCacheQueries,
			hits: stats.modelCacheHits,
			hitRate: stats.modelCacheQueries ? stats.modelCacheHits / stats.modelCacheQueries : 0,
		},
		schedulesTotal: stats.schedulesTotal,
		schedulesEnabled: stats.schedulesEnabled,
		nextFireAtSec: stats.nextFireAt ? Math.round(stats.nextFireAt / 1000) : 0,
		lastFireAtSec: stats.lastFireAt ? Math.round(stats.lastFireAt / 1000) : 0,
		lastFireId: stats.lastFireId,
		channels: stats.channels,
	};
}

/** Emit one Prometheus metric line group (# HELP + # TYPE + sample). */
function emit(
	lines: string[],
	name: string,
	type: "gauge" | "counter",
	help: string,
	value: number,
	labels?: Record<string, string>,
): void {
	const labelStr = labels
		? `{${Object.entries(labels)
				.map(([k, v]) => `${k}="${v}"`)
				.join(",")}}`
		: "";
	lines.push(`# HELP ${name} ${help}`);
	lines.push(`# TYPE ${name} ${type}`);
	lines.push(`${name}${labelStr} ${value}`);
}

/** Build the Prometheus text exposition returned by /metrics. Exported for the same reason as buildSnapshot(). */
export function buildMetrics(): string {
	const lines: string[] = [];
	const mem = process.memoryUsage();

	emit(lines, "botler_uptime_seconds", "gauge", "Process uptime in seconds", Math.floor(process.uptime()));
	emit(lines, "botler_memory_rss_bytes", "gauge", "Resident set size in bytes", mem.rss);
	emit(lines, "botler_memory_heap_used_bytes", "gauge", "V8 heap used in bytes", mem.heapUsed);
	emit(lines, "botler_memory_heap_total_bytes", "gauge", "V8 heap total in bytes", mem.heapTotal);
	emit(lines, "botler_memory_external_bytes", "gauge", "External memory in bytes", mem.external);
	emit(lines, "botler_memory_array_buffers_bytes", "gauge", "ArrayBuffer memory in bytes", mem.arrayBuffers);
	emit(lines, "botler_event_loop_lag_p50_ms", "gauge", "Event loop lag p50 in ms", Number(lagMs(50).toFixed(3)));
	emit(lines, "botler_event_loop_lag_p90_ms", "gauge", "Event loop lag p90 in ms", Number(lagMs(90).toFixed(3)));
	emit(lines, "botler_event_loop_lag_p99_ms", "gauge", "Event loop lag p99 in ms", Number(lagMs(99).toFixed(3)));

	emit(lines, "botler_queue_depth", "gauge", "Tasks queued awaiting execution", stats.queueDepth);
	emit(lines, "botler_active_tasks", "gauge", "Tasks currently executing", stats.activeTasks);
	emit(lines, "botler_active_agents", "gauge", "Agent.prompt() instances in flight", stats.activeAgents);

	emit(lines, "botler_dispatches_total", "counter", "Non-duplicate dispatches", stats.totalDispatched);
	emit(lines, "botler_duplicates_total", "counter", "Messages dropped by dedup", stats.duplicatesTotal);
	emit(lines, "botler_failures_total", "counter", "Failed tasks (error + validation-failed)", stats.totalFailed);
	emit(lines, "botler_model_cache_queries_total", "counter", "Model resolution cache queries", stats.modelCacheQueries);
	emit(lines, "botler_model_cache_hits_total", "counter", "Model resolution cache hits", stats.modelCacheHits);

	for (const [s, n] of Object.entries(stats.statusCounts)) {
		emit(lines, "botler_tasks_total", "counter", "Tasks by final status", n, { status: s });
	}

	emit(lines, "botler_schedules_total", "gauge", "Total schedule entries", stats.schedulesTotal);
	emit(lines, "botler_schedules_enabled_total", "gauge", "Enabled schedule entries", stats.schedulesEnabled);
	emit(
		lines,
		"botler_scheduler_next_fire_epoch_seconds",
		"gauge",
		"Epoch seconds of next scheduled fire (0 = none)",
		stats.nextFireAt ? Math.round(stats.nextFireAt / 1000) : 0,
	);
	emit(
		lines,
		"botler_scheduler_last_fire_epoch_seconds",
		"gauge",
		"Epoch seconds of last scheduled fire (0 = none)",
		stats.lastFireAt ? Math.round(stats.lastFireAt / 1000) : 0,
	);

	// Only emit labels for channels that have actually appeared (configured + started).
	for (const [name, c] of Object.entries(stats.channels)) {
		emit(lines, "botler_channel_up", "gauge", "Channel online (1) or down (0)", c.up ? 1 : 0, { channel: name });
	}

	return lines.join("\n") + "\n";
}

function route(req: { url?: string | null }, res: ServerResponse): void {
	const url = new URL(req.url ?? "/", "http://localhost");
	const path = url.pathname;
	if (path === "/healthz/history") {
		sendJson(res, {
			intervalSec: SAMPLE_INTERVAL_MS / 1000,
			capacity: HISTORY_CAPACITY,
			samples: history,
		});
		return;
	}
	if (path === "/metrics") {
		res.writeHead(200, { "Content-Type": "text/plain; version=0.0.4; charset=utf-8" });
		res.end(buildMetrics());
		return;
	}
	if (path === "/healthz" || path === "/") {
		sendJson(res, buildSnapshot());
		return;
	}
	res.writeHead(404, { "Content-Type": "application/json" });
	res.end(JSON.stringify({ ok: false, error: "not found" }));
}

/** Enable the event-loop-lag histogram + the rolling history sampler exactly once.
 *
 * Decoupled from the 8899 HTTP port on purpose: it is invoked by both `startHealthServer()` and
 * the WebUI server, so the WebUI "🩺 Monitoring" view keeps showing real lag/history even when
 * MONITOR_ENABLED=0 (port off) but WEBUI_ENABLED=1. Idempotent — safe to call from both.
 *
 * CLI mode never starts either server, so the histogram is never armed there.
 */
let samplingStarted = false;
export function ensureMonitorSampling(): void {
	if (samplingStarted) return;
	samplingStarted = true;
	eventLoopLag.enable();

	// Rolling trend sampler: assembled here (reads event-loop lag + memory), pushed into the
	// pure ring buffer in stats.ts. Lives on the server side so closing/refreshing the WebUI
	// never loses history and multiple clients don't double-sample.
	const sampler = setInterval(() => {
		const sample: Sample = {
			ts: Date.now(),
			rss: process.memoryUsage().rss,
			heapUsed: process.memoryUsage().heapUsed,
			queueDepth: stats.queueDepth,
			activeTasks: stats.activeTasks,
			lagP50: Number(lagMs(50).toFixed(3)),
			lagP99: Number(lagMs(99).toFixed(3)),
		};
		pushSample(sample);
	}, SAMPLE_INTERVAL_MS);
	// Don't keep the process alive solely for sampling.
	sampler.unref?.();
}

/** Start the local health/metrics server. Never throws — listen errors are logged only. */
export function startHealthServer(): void {
	ensureMonitorSampling();

	const server = createServer((req, res) => {
		try {
			route(req, res);
		} catch (e) {
			res.writeHead(500, { "Content-Type": "application/json" });
			res.end(JSON.stringify({ ok: false, error: e instanceof Error ? e.message : String(e) }));
		}
	});

	server.on("error", (e) => {
		// EADDRINUSE etc. must not propagate to main() (which would `process.exit(1)` and kill the bot).
		console.error(`[monitor] health server error: ${e instanceof Error ? e.message : e}`);
	});

	server.listen(CONFIG.monitorPort, "127.0.0.1", () => {
		console.log(`[monitor] health server on http://127.0.0.1:${CONFIG.monitorPort}`);
	});
}

/** Rolling history payload (same shape as GET /healthz/history). Exported for the WebUI server. */
export function getHistory() {
	return {
		intervalSec: SAMPLE_INTERVAL_MS / 1000,
		capacity: HISTORY_CAPACITY,
		samples: history,
	};
}
