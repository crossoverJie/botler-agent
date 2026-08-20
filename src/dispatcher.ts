import { randomUUID } from "node:crypto";
import { runTask, type TaskResult } from "./runner.ts";
import { validateState } from "./safety/validate.ts";
import { commitIfChanged } from "./safety/git.ts";
import { appendTaskLog } from "./logging/store.ts";
import { CONFIG } from "./config.ts";
import type { Recipient } from "./push/types.ts";
import type { TaskLog, TaskStatus, TokenUsageLog } from "./logging/types.ts";
import {
	markEnqueued,
	markStarted,
	markFinished,
	recordStatus,
	markDuplicate,
	markDispatched,
	markFailed,
	stats,
} from "./monitor/stats.ts";

export interface DispatchOptions {
	/** Dedup key (e.g. `${chatId}:${messageId}`). Once provided, duplicate messages within a short window are ignored. */
	id?: string;
	/** Source channel, used for logging only. */
	source?: string;
	/** Optional routing hint (a valid data subproject). Scheduler entries use it to skip the routing LLM call. */
	projectHint?: string;
	/** Optional recipient: the sender of this message. Tools like schedule inject it as the push target. */
	recipient?: Recipient;
}

export interface DispatchResult {
	/** Reply text to send to the user. May be empty when the reply is images only. */
	text: string;
	/** Images to send: absolute paths inside DATA_ROOT, or https URLs. Channels without image support ignore these. */
	images: string[];
	/** Final task status (mirrors the appended TaskLog.status). Used by the scheduler to decide on retry. */
	status: TaskStatus;
}

/** Returned as `text` when a message is dropped as a duplicate; channels must not echo it. */
export const DUPLICATE_SENTINEL = "(duplicate message ignored)";

const DEDUP_MS = 5 * 60 * 1000;
const seen = new Map<string, number>();

/** Sequential queue: only one task writes to the data directory at a time, avoiding concurrent corruption of files. */
let chain: Promise<unknown> = Promise.resolve();

function truncate(s: string, n = 60): string {
	const flat = s.replace(/\s+/g, " ").trim();
	return flat.length > n ? `${flat.slice(0, n)}…` : flat;
}

const ZERO_USAGE: TokenUsageLog = {
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	reasoning: 0,
	total: 0,
	costUsd: 0,
};

/** Construct a minimal TaskLog for paths that never ran an Agent (duplicate / error). */
function minimalLog(opts: {
	taskId: string;
	source: string;
	startedAt: number;
	status: TaskLog["status"];
	userMessage: string;
	replyText: string;
}): TaskLog {
	return {
		id: randomUUID(),
		taskId: opts.taskId,
		phase: "execute",
		source: opts.source,
		provider: CONFIG.provider,
		model: CONFIG.model,
		project: null,
		status: opts.status,
		startedAt: opts.startedAt,
		endedAt: Date.now(),
		durationMs: Date.now() - opts.startedAt,
		userMessage: opts.userMessage,
		replyText: opts.replyText,
		images: [],
		mutated: false,
		tools: [],
		conversation: [],
		usage: ZERO_USAGE,
		calls: [],
		modelCache: { queries: 0, hits: 0, hitRate: 0 },
	};
}

/**
 * Process a single user message:
 * 1. Dedup (by id, short time window)
 * 2. Run in sequence (mutually exclusive writes)
 * 3. Read-only tasks return directly; write tasks are validated first, and on failure retried with a self-contained fix instruction (at most once)
 * 4. On success, commit the changed subprojects
 *
 * Never rejects — any failure is turned into a readable message returned to the channel, and a
 * task log is appended (best-effort) for the WebUI.
 */
export async function dispatch(message: string, opts: DispatchOptions = {}): Promise<DispatchResult> {
	const dispatchStartedAt = Date.now();
	const taskId = opts.id ?? randomUUID();
	const source = opts.source ?? "cli";

	if (opts.id) {
		const now = Date.now();
		const prev = seen.get(opts.id);
		if (prev !== undefined && now - prev < DEDUP_MS) {
			console.log(`[dispatch] Ignoring duplicate message ${opts.id}`);
			markDuplicate();
			appendTaskLog(
				minimalLog({
					taskId,
					source,
					startedAt: dispatchStartedAt,
					status: "duplicate",
					userMessage: message,
					replyText: DUPLICATE_SENTINEL,
				}),
			);
			return { text: DUPLICATE_SENTINEL, images: [], status: "duplicate" };
		}
		seen.set(opts.id, now);
	}

	// A non-duplicate dispatch entered the execution chain (queued, awaiting its turn).
	markEnqueued();
	markDispatched();

	const run = chain.then(async (): Promise<DispatchResult> => {
		markStarted();
		const out = (text: string, images: string[], status: TaskStatus): DispatchResult => ({ text, images, status });
		let result: TaskResult | undefined;
		// Lifted to the callback scope so the single `finally` below can record the final status
		// regardless of which of the 5 exit points was taken.
		let finalStatus: TaskStatus = "success";
		try {
			result = await runTask(message, {
				taskId,
				source,
				phase: "execute",
				projectHint: opts.projectHint,
				recipient: opts.recipient,
			});
			const log = result.log;
			if (!log) {
				// Collection unexpectedly missing; don't crash the channel.
				finalStatus = "success";
				return out(result.text, result.images, "success");
			}

			if (!result.mutated) {
				// Read-only task (query) — or unrouted/no-project (project === null).
				log.status = log.project === null ? "unknown-project" : "success";
				log.replyText = result.text;
				appendTaskLog(log);
				finalStatus = log.status;
				return out(result.text, result.images, log.status);
			}

			let validation = validateState();
			if (!validation.ok) {
				// A fresh Agent reads the file and self-heals in place.
				console.log(`[dispatch] Validation failed; self-heal retry: ${validation.fix}`);
				const heal = await runTask(validation.fix!, { taskId, source, phase: "self-heal" });
				validation = validateState();
				if (!validation.ok) {
					const errText = `⚠️ Could not save safely; please check the data: ${validation.fix ?? ""}`;
					log.status = "validation-failed";
					log.replyText = errText;
					appendTaskLog(log);
					if (heal.log) {
						heal.log.status = "validation-failed";
						heal.log.replyText = errText;
						appendTaskLog(heal.log);
					}
					finalStatus = "validation-failed";
					return out(errText, [], "validation-failed");
				}
				commitIfChanged(`agent: ${truncate(message)}`);
				const finalText = `${result.text}\n\n(auto-fixed and saved)`;
				log.status = "auto-fixed";
				log.replyText = finalText;
				appendTaskLog(log);
				if (heal.log) {
					heal.log.status = "success";
					appendTaskLog(heal.log);
				}
				finalStatus = "auto-fixed";
				return out(finalText, result.images, "auto-fixed");
			}

			commitIfChanged(`agent: ${truncate(message)}`);
			log.status = "success";
			log.replyText = result.text;
			appendTaskLog(log);
			finalStatus = "success";
			return out(result.text, result.images, "success");
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			console.error(`[dispatch] Task error:`, msg);
			const errText = `⚠️ Task execution error: ${msg}`;
			const log =
				result?.log ??
				minimalLog({
					taskId,
					source,
					startedAt: dispatchStartedAt,
					status: "error",
					userMessage: message,
					replyText: errText,
				});
			log.status = "error";
			log.replyText = errText;
			appendTaskLog(log);
			finalStatus = "error";
			return out(errText, [], "error");
		} finally {
			// Single unified exit accounting: always fires, regardless of which branch returned.
			markFinished();
			recordStatus(finalStatus);
			stats.lastDispatchDurationMs = Date.now() - dispatchStartedAt;
			if (finalStatus === "error" || finalStatus === "validation-failed") markFailed();
		}
	});

	// Keep the queue moving forward even if this run errored, so later tasks are not blocked
	chain = run.then(
		() => undefined,
		() => undefined,
	);
	return run;
}
