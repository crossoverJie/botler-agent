/**
 * JSONL task-log store.
 *
 * Append-only, one file per day (`YYYY-MM-DD.jsonl`) under CONFIG.logDir. The directory lives
 * outside DATA_ROOT (default ~/.botler-agent/task-logs), so it never enters git or the data
 * separation boundary. All reads tolerate corrupt lines; the only mutating operation is
 * {@link cleanupLogs}, which is locked to DAY_FILE_RE-named files inside logDir.
 */

import { appendFileSync, existsSync, mkdirSync, readdirSync, readFileSync, statSync, unlinkSync } from "node:fs";
import { join } from "node:path";
import { CONFIG } from "../config.ts";
import type { ModelCacheStats, TaskLog } from "./types.ts";

/** Strict day-file name: YYYY-MM-DD.jsonl. Used to reject path-traversal / off-dir deletions. */
const DAY_FILE_RE = /^\d{4}-\d{2}-\d{2}\.jsonl$/;

function fileForDay(ts: number): string {
	const d = new Date(ts);
	const mm = String(d.getMonth() + 1).padStart(2, "0");
	const dd = String(d.getDate()).padStart(2, "0");
	return `${d.getFullYear()}-${mm}-${dd}.jsonl`;
}

/**
 * Append one log as a JSON line. Writes under a per-day file; the dispatcher serializes writes
 * through its sequential queue, so lines never interleave.
 */
export function appendTaskLog(entry: TaskLog): void {
	try {
		mkdirSync(CONFIG.logDir, { recursive: true });
		const file = join(CONFIG.logDir, fileForDay(entry.startedAt));
		appendFileSync(file, JSON.stringify(entry) + "\n", "utf8");
	} catch (e) {
		// Logging must never break the main flow.
		console.error("[logging] appendTaskLog failed:", e instanceof Error ? e.message : e);
	}
}

export interface LogQuery {
	from?: number;
	to?: number;
	project?: string;
	source?: string;
	scheduleId?: string;
	phase?: TaskLog["phase"];
	q?: string;
	limit?: number;
	offset?: number;
}

export interface LogPage {
	logs: TaskLog[];
	total: number;
}

/**
 * Scheduler dispatches use taskId `schedule:<id>:<epoch>`. Use a greedy capture so ids that
 * themselves contain ":" are extracted correctly.
 */
export function scheduleIdFromTaskId(taskId: string): string | undefined {
	const m = /^schedule:(.+):\d+$/.exec(taskId);
	return m?.[1];
}

export function matchesLogQuery(log: TaskLog, q: LogQuery): boolean {
	if (q.project && log.project !== q.project) return false;
	if (q.source && log.source !== q.source) return false;
	if (q.phase && log.phase !== q.phase) return false;
	if (q.scheduleId && scheduleIdFromTaskId(log.taskId) !== q.scheduleId) return false;
	if (q.q) {
		const needle = q.q.toLowerCase();
		if (
			!log.userMessage.toLowerCase().includes(needle) &&
			!log.replyText.toLowerCase().includes(needle) &&
			!(log.project ?? "").toLowerCase().includes(needle)
		) {
			return false;
		}
	}
	return true;
}

export type LogScan = Omit<LogQuery, "limit" | "offset">;

function loadDayFile(file: string): TaskLog[] {
	const abs = join(CONFIG.logDir, file);
	const raw = readFileSync(abs, "utf8");
	const out: TaskLog[] = [];
	for (const line of raw.split("\n")) {
		const t = line.trim();
		if (!t) continue;
		try {
			out.push(JSON.parse(t) as TaskLog);
		} catch {
			// Skip corrupt lines rather than fail the whole query.
		}
	}
	return out;
}

/** List day-files whose name falls within [from, to] (by date prefix). Returns sorted desc. */
function dayFilesInRange(from?: number, to?: number): string[] {
	if (!existsSync(CONFIG.logDir)) return [];
	let files = readdirSync(CONFIG.logDir).filter((f) => DAY_FILE_RE.test(f));
	if (from !== undefined) {
		const lo = fileForDay(from); // YYYY-MM-DD; files with date >= lo
		files = files.filter((f) => f >= lo);
	}
	if (to !== undefined) {
		const hi = fileForDay(to);
		files = files.filter((f) => f <= hi);
	}
	return files.sort().reverse();
}

/**
 * Single-pass visitor for aggregation paths. It still reads each day file, but does not keep
 * every matching TaskLog in memory.
 *
 * V1 deliberately reuses loadDayFile(), which currently materializes one day's file as a
 * TaskLog[] via readFileSync. That caps memory at the largest single-day file, not the whole
 * history, and matches the existing WebUI log query. If a future install produces very large
 * single-day files, replace this with a line-streaming reader without changing call sites.
 */
export function scanLogs(q: LogScan, visit: (log: TaskLog) => void): void {
	for (const f of dayFilesInRange(q.from, q.to)) {
		for (const l of loadDayFile(f)) {
			if (matchesLogQuery(l, q)) visit(l);
		}
	}
}

/**
 * Pure filter/sort/page helper. Kept separate from the file reading so the schedule filtering
 * logic can be tested with fixture TaskLog[] values without touching CONFIG.logDir.
 */
export function filterAndPageLogs(all: TaskLog[], q: LogQuery): LogPage {
	const filtered = all.filter((l) => matchesLogQuery(l, q));

	// Already sorted desc by file; stable sort by startedAt desc to be safe across a day boundary.
	filtered.sort((a, b) => b.startedAt - a.startedAt);

	const total = filtered.length;
	const offset = q.offset ?? 0;
	const limit = q.limit ?? 50;
	const logs = filtered.slice(offset, offset + limit);
	return { logs, total };
}

export function queryLogs(q: LogQuery): LogPage {
	const files = dayFilesInRange(q.from, q.to);
	const all: TaskLog[] = [];
	for (const f of files) all.push(...loadDayFile(f));
	return filterAndPageLogs(all, q);
}

export function getLog(id: string): TaskLog | undefined {
	if (!existsSync(CONFIG.logDir)) return undefined;
	for (const f of readdirSync(CONFIG.logDir).filter((x) => DAY_FILE_RE.test(x))) {
		for (const l of loadDayFile(f)) {
			if (l.id === id) return l;
		}
	}
	return undefined;
}

export interface TaskSummary {
	totalTasks: number;
	byStatus: Record<string, number>;
	byProject: Record<string, number>;
	bySource: Record<string, number>;
	token: { input: number; output: number; cacheRead: number; total: number };
	modelCache: ModelCacheStats;
	avgDurationMs: number;
}

export function summary(): TaskSummary {
	const files = existsSync(CONFIG.logDir)
		? readdirSync(CONFIG.logDir).filter((f) => DAY_FILE_RE.test(f))
		: [];
	const all: TaskLog[] = [];
	for (const f of files) all.push(...loadDayFile(f));

	const byStatus: Record<string, number> = {};
	const byProject: Record<string, number> = {};
	const bySource: Record<string, number> = {};
	const token = { input: 0, output: 0, cacheRead: 0, total: 0 };
	let durationSum = 0;

	for (const l of all) {
		byStatus[l.status] = (byStatus[l.status] ?? 0) + 1;
		const proj = l.project ?? "(none)";
		byProject[proj] = (byProject[proj] ?? 0) + 1;
		bySource[l.source] = (bySource[l.source] ?? 0) + 1;
		token.input += l.usage.input;
		token.output += l.usage.output;
		token.cacheRead += l.usage.cacheRead;
		token.total += l.usage.total;
		durationSum += l.durationMs;
	}

	// Latest model-cache snapshot across logs (process-level; last writer wins).
	let modelCache: ModelCacheStats = { queries: 0, hits: 0, hitRate: 0 };
	if (all.length) modelCache = all[all.length - 1].modelCache;

	return {
		totalTasks: all.length,
		byStatus,
		byProject,
		bySource,
		token,
		modelCache,
		avgDurationMs: all.length ? Math.round(durationSum / all.length) : 0,
	};
}

// ---- Token time-series aggregation (read-only, no new dependencies) ----

export type Granularity = "hour" | "day" | "week";

export interface TokenTimeQuery {
	from?: number;
	to?: number;
	/** Default "day". */
	granularity?: Granularity;
	/** Default = no grouping (single group "(all)"). */
	groupBy?: "project" | "source";
}

export interface TokenBucket {
	/** Bucket start epoch ms (inclusive). */
	start: number;
	/** Bucket end epoch ms (exclusive) = next bucket's start (last bucket = start + step); only for interval completeness; the UI shows start only. */
	end: number;
	/** Number of task-log entries that fall into this bucket (including self-heal retries). */
	tasks: number;
	input: number;
	output: number;
	cacheRead: number;
	cacheWrite: number;
	total: number;
	costUsd: number;
}

export interface TokenGroup {
	/** Group key: project name / source name / "(all)". */
	key: string;
	buckets: TokenBucket[];
}

export interface TokenTimeSeries {
	granularity: Granularity;
	/** Server-local IANA timezone (display only; buckets are computed in server-local time). */
	tz: string;
	from: number;
	to: number;
	groups: TokenGroup[];
}

/** Bucket start: hours/days truncated to local time; weeks start at Monday 00:00 (getDay() Sunday=0 adjusted). */
function bucketStart(ts: number, g: Granularity): number {
	const d = new Date(ts);
	if (g === "hour") {
		d.setMinutes(0, 0, 0);
	} else if (g === "day") {
		d.setHours(0, 0, 0, 0);
	} else {
		const dow = (d.getDay() + 6) % 7; // Mon=0 … Sun=6
		d.setDate(d.getDate() - dow);
		d.setHours(0, 0, 0, 0);
	}
	return d.getTime();
}

function stepMs(g: Granularity): number {
	return g === "hour" ? 3_600_000 : g === "day" ? 86_400_000 : 7 * 86_400_000;
}

/**
 * Aggregate token usage into time buckets across the task logs in [from, to].
 * Pure read; reuses `dayFilesInRange` / `loadDayFile` (tolerant of corrupt lines).
 * Every group gets the same continuous bucket array, so the frontend can align its X axis
 * without handling missing buckets. No grouping returns at least one "(all)" group (empty
 * bucket sequence) so the UI never renders a blank state.
 */
export function tokenTimeSeries(q: TokenTimeQuery = {}): TokenTimeSeries {
	const g = q.granularity ?? "day";
	const logs: TaskLog[] = [];
	for (const f of dayFilesInRange(q.from, q.to)) logs.push(...loadDayFile(f));

	// Effective range: prefer the requested range; default to the data's first/last (no data degenerates to a single bucket at the current time).
	const lo = q.from ?? (logs.length ? logs.reduce((m, l) => Math.min(m, l.startedAt), Infinity) : Date.now());
	const hi = q.to ?? (logs.length ? logs.reduce((m, l) => Math.max(m, l.startedAt), -Infinity) : Date.now());

	// Generate continuous empty buckets (spanning [lo, hi], including the bucket containing hi) so the frontend X axis is continuous with no gaps.
	const step = stepMs(g);
	const first = bucketStart(lo, g);
	const starts: number[] = [];
	for (let t = first; t <= hi; t += step) starts.push(t);
	const buckets: TokenBucket[] = starts.map((s, i) => ({
		start: s,
		end: i + 1 < starts.length ? starts[i + 1] : s + step,
		tasks: 0,
		input: 0,
		output: 0,
		cacheRead: 0,
		cacheWrite: 0,
		total: 0,
		costUsd: 0,
	}));

	const groupKey = (l: TaskLog): string =>
		q.groupBy === "project" ? (l.project ?? "(none)") : q.groupBy === "source" ? l.source : "(all)";

	const groups = new Map<string, TokenBucket[]>();
	const groupOf = (k: string): TokenBucket[] => {
		let arr = groups.get(k);
		if (!arr) {
			arr = buckets.map((b) => ({ ...b }));
			groups.set(k, arr);
		}
		return arr;
	};

	// When no grouping, always create a group: 0 logs in range still returns at least one "(all)" group (empty bucket sequence) so the frontend never shows a blank screen.
	if (!q.groupBy) groupOf("(all)");

	for (const l of logs) {
		if (l.startedAt < lo || l.startedAt > hi) continue;
		const idx = Math.floor((l.startedAt - first) / step);
		if (idx < 0 || idx >= buckets.length) continue;
		const b = groupOf(groupKey(l))[idx];
		b.tasks += 1;
		b.input += l.usage.input;
		b.output += l.usage.output;
		b.cacheRead += l.usage.cacheRead;
		b.cacheWrite += l.usage.cacheWrite;
		b.total += l.usage.total;
		b.costUsd += l.usage.costUsd;
	}

	return {
		granularity: g,
		tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
		from: lo,
		to: hi,
		groups: [...groups.entries()].map(([key, buckets]) => ({ key, buckets })),
	};
}

export interface LogFileStat {
	day: string;
	file: string;
	bytes: number;
	entries: number;
}

export interface DiskUsage {
	dir: string;
	totalBytes: number;
	fileCount: number;
	entryCount: number;
	oldestDay: string | null;
	newestDay: string | null;
	files: LogFileStat[];
}

export function diskUsage(): DiskUsage {
	const dir = CONFIG.logDir;
	if (!existsSync(dir)) {
		return { dir, totalBytes: 0, fileCount: 0, entryCount: 0, oldestDay: null, newestDay: null, files: [] };
	}
	const files = readdirSync(dir)
		.filter((f) => DAY_FILE_RE.test(f))
		.sort();
	let totalBytes = 0;
	let entryCount = 0;
	const stats: LogFileStat[] = [];
	for (const f of files) {
		const abs = join(dir, f);
		const bytes = statSync(abs).size;
		const entries = readFileSync(abs, "utf8").split("\n").filter((l) => l.trim()).length;
		totalBytes += bytes;
		entryCount += entries;
		stats.push({ day: f.replace(/\.jsonl$/, ""), file: f, bytes, entries });
	}
	return {
		dir,
		totalBytes,
		fileCount: files.length,
		entryCount,
		oldestDay: files.length ? files[0].replace(/\.jsonl$/, "") : null,
		newestDay: files.length ? files[files.length - 1].replace(/\.jsonl$/, "") : null,
		files: stats,
	};
}

export interface CleanupOptions {
	/** "YYYY-MM-DD": delete files with day strictly less than this (fixed-width string compare). */
	before?: string;
	/** Explicit day list to delete (each must match DAY_FILE_RE). */
	days?: string[];
	dryRun?: boolean;
}

export interface CleanupResult {
	deleted: string[];
	freedBytes: number;
	remainingBytes: number;
}

function validateDay(day: string): boolean {
	return DAY_FILE_RE.test(`${day}.jsonl`);
}

/**
 * Delete day-files inside logDir only. Names are strictly validated; deletion uses
 * join(logDir, name) + unlinkSync — never recursive, never outside logDir, never non-jsonl.
 */
export function cleanupLogs(opts: CleanupOptions): CleanupResult {
	const usage = diskUsage();
	const candidates = new Set<string>();

	if (opts.before !== undefined) {
		if (!validateDay(opts.before)) {
			return { deleted: [], freedBytes: 0, remainingBytes: usage.totalBytes };
		}
		for (const f of usage.files) {
			// f.day strictly less than before (fixed-width ISO date compares lexically).
			if (f.day < opts.before) candidates.add(f.file);
		}
	}
	if (opts.days) {
		for (const d of opts.days) {
			if (!validateDay(d)) continue; // reject "../../.env", "abc", etc.
			const name = `${d}.jsonl`;
			if (usage.files.some((f) => f.file === name)) candidates.add(name);
		}
	}

	const deleted: string[] = [];
	let freedBytes = 0;
	for (const name of candidates) {
		const stat = usage.files.find((f) => f.file === name);
		if (!stat) continue;
		if (!opts.dryRun) {
			try {
				unlinkSync(join(CONFIG.logDir, name));
			} catch (e) {
				console.error("[logging] cleanup failed for", name, e);
				continue;
			}
		}
		deleted.push(name);
		freedBytes += stat.bytes;
	}

	const remainingBytes = opts.dryRun ? usage.totalBytes : usage.totalBytes - freedBytes;
	return { deleted, freedBytes, remainingBytes };
}
