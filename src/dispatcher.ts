import { randomUUID } from "node:crypto";
import { runTask, type TaskResult } from "./runner.ts";
import { validateState } from "./safety/validate.ts";
import { commitIfChanged } from "./safety/git.ts";
import { appendTaskLog } from "./logging/store.ts";
import { CONFIG } from "./config.ts";
import { appendTurn, shouldRecordTurn } from "./conversation/store.ts";
import { projectOf } from "./tools/paths.ts";
import { runScript } from "./tools/run.ts";
import type { Recipient } from "./push/types.ts";
import type { TaskLog, TaskStatus, TokenUsageLog } from "./logging/types.ts";
import type { InboundImage } from "./channels/wechat/download.ts";
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
	/** Optional plural routing hint (valid data subproject names); used by the self-heal retry. */
	projectsHint?: string[];
	/** Optional recipient: the sender of this message. Tools like schedule inject it as the push target. */
	recipient?: Recipient;
	/**
	 * Inbound images (decoded bytes) to feed the model as vision input AND persist under the
	 * target subproject. WeChat-only for now. Distinct from DispatchResult.images (outbound).
	 */
	inboundImages?: InboundImage[];
	/** Fixed conversation session key for IM channels; omitted for scheduler / CLI. */
	sessionKey?: string;
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

/** Append the user-visible turn when the execute phase completed with a recordable status. */
function appendVisibleTurn(
	sessionKey: string | undefined,
	message: string,
	result: TaskResult | undefined,
	status: TaskStatus,
): void {
	if (!CONFIG.conversationContextEnabled) return;
	if (!shouldRecordTurn("execute", status, sessionKey, result?.recordConversationTurn !== false)) return;
	const log = result?.log;
	if (!log) return;
	// Store the clean user-facing reply. For auto-fixed runs `log.replyText` carries the
	// framework decoration "(auto-fixed and saved)", which is task-log metadata, not the
	// visible assistant turn that should be reused as conversation context.
	const assistant = (result?.text || log.replyText || "").trim();
	if (!assistant) return;
	appendTurn(
		sessionKey!,
		{
			ts: log.startedAt,
			project: log.project,
			projects: log.projects,
			// The model sees an additional image-persisted note appended by the framework;
			// history deliberately stores only the original user text, not that transport note.
			user: message,
			assistant,
		},
		CONFIG.conversationContextTurns,
	);
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
		projects: [],
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
				inboundImages: opts.inboundImages,
				sessionKey: opts.sessionKey,
			});
			const log = result.log;
			if (!log) {
				// Collection unexpectedly missing; don't crash the channel.
				finalStatus = "success";
				return out(result.text, result.images, "success");
			}

			if (!result.mutated) {
				// Read-only task (query) — or unrouted/no-project (empty selected set).
				log.status = log.projects && log.projects.length === 0 ? "unknown-project" : "success";
				log.replyText = result.text;
				appendTaskLog(log);
				finalStatus = log.status;
				return out(result.text, result.images, log.status);
			}

			let validation = validateState(log.projects);
			if (!validation.ok) {
				// A fresh Agent reads the file and self-heals in place.
				console.log(`[dispatch] Validation failed; self-heal retry: ${validation.fix}`);
				const heal = await runTask(validation.fix!, { taskId, source, phase: "self-heal", projectsHint: log.projects });
				validation = validateState(log.projects);
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
				commitIfChanged(`agent: ${truncate(message)}`, log.projects);
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

			commitIfChanged(`agent: ${truncate(message)}`, log.projects);
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
			appendVisibleTurn(opts.sessionKey, message, result, finalStatus);
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

export interface ScriptTaskOptions {
	/** Task id (already namespaced, e.g. `schedule:<id>:<epoch>`). Used as the dedup/log taskId source. */
	id: string;
	/** Script path relative to DATA_ROOT (e.g. "notes/scripts/build.py"). */
	script: string;
	/** Args passed to the script (optional). */
	scriptArgs?: string[];
	/** Auto git-commit the script's data changes on success (defaults to true). */
	commit?: boolean;
	/** Source label for logging (defaults to "scheduler"). */
	source?: string;
	/** Recipient echoed through for stats only; actual delivery stays in the engine. */
	recipient?: Recipient;
}

/**
 * Build a history-compatible TaskLog for a direct script run. Mirrors {@link minimalLog} but fills
 * in the script's own project so WebUI history / per-schedule stats / recent runs stay accurate.
 */
function scriptTaskLog(o: {
	taskId: string;
	source: string;
	startedAt: number;
	status: TaskStatus;
	script: string;
	replyText: string;
	project: string | null;
	ran: boolean;
}): TaskLog {
	return {
		id: randomUUID(),
		taskId: o.taskId,
		phase: "execute",
		source: o.source,
		provider: CONFIG.provider,
		model: CONFIG.model,
		project: o.project,
		projects: o.project ? [o.project] : [],
		status: o.status,
		startedAt: o.startedAt,
		endedAt: Date.now(),
		durationMs: Date.now() - o.startedAt,
		userMessage: `run ${o.script}`,
		replyText: o.replyText,
		images: [],
		// For scripts, `mutated` means "the script executed, so it MAY have changed data" — not
		// merely "it succeeded". A script that errors or writes invalid JSON may still have written
		// partial data, so `ran` (not `status === "success"`) drives this flag.
		mutated: o.ran,
		tools: [],
		conversation: [],
		usage: ZERO_USAGE,
		calls: [],
		modelCache: { queries: 0, hits: 0, hitRate: 0 },
	};
}

/**
 * Run a scheduled script directly (bypassing the LLM) on the same sequential `chain` as
 * {@link dispatch}, so it never races a chat task's writes and shares the same status accounting.
 *
 * Behaviour:
 * - Pre-condition rejection (bad path / missing file / wrong type): the script never ran, so no
 *   data was touched — reported as `error`, no validation. `ran` is false.
 * - Any script that actually started (success OR non-zero exit) may have written data, so its
 *   project is ALWAYS validated (`validation-failed` if JSON is broken, data left as-is).
 * - On success, auto git-commit is scoped to the script's own subproject (unless `commit:false`).
 * - A `validation-failed` run is reported and logged but returns that status (the engine does NOT
 *   retry it) — re-running would repeat the script's side effects.
 */
export async function dispatchScriptedTask(opts: ScriptTaskOptions): Promise<DispatchResult> {
	const startedAt = Date.now();
	const source = opts.source ?? "scheduler";
	markEnqueued();
	markDispatched();
	const out = (text: string, status: TaskStatus): DispatchResult => ({ text, images: [], status });
	const run = chain.then(async (): Promise<DispatchResult> => {
		markStarted();
		let finalStatus: TaskStatus = "success";
		let text = "";
		let project: string | null = null;
		let ran = false;
		try {
			const res = runScript(opts.script, opts.scriptArgs ?? []);
			text = res.text;
			// Resolve the project from the script's cwd on BOTH success and failure, so the task log
			// (and thus history filtering by project) always has attribution.
			project = res.cwd ? projectOf(res.cwd) : null;
			// `ran` = the script actually executed (success OR runtime failure), as opposed to a
			// pre-condition rejection (bad path / missing file / wrong type) that never started it.
			ran = res.ok || res.errorKind === "runtime";
			// Pre-condition errors: the script never ran, so no data was touched — no validation needed.
			if (!res.ok && res.errorKind === "precondition") {
				finalStatus = "error";
				return out(text, "error");
			}
			// EVERY script that actually started (success OR non-zero exit) may have written data,
			// so ALWAYS validate its project — even when the script also failed. This keeps the JSON
			// safety boundary regardless of the script's exit status.
			const v = validateState(project ? [project] : []);
			if (!v.ok) {
				finalStatus = "validation-failed";
				// If the script also exited non-zero, combine its output with the data-safety warning
				// so the pushed / recorded message shows BOTH. The data-safety status wins for history
				// and stats (it is the more actionable signal), but the script error is not lost.
				const scriptErr = !res.ok ? `Script failed: ${text}\n` : "";
				text = `⚠️ ${scriptErr}Script produced invalid data (left as-is, not committed): ${v.fix ?? ""}`;
				return out(text, "validation-failed");
			}
			if (!res.ok) {
				// Ran, wrote valid data, but exited non-zero.
				finalStatus = "error";
				return out(text, "error");
			}
			// Ran successfully with valid data.
			if (opts.commit ?? true) {
				commitIfChanged(`agent: scheduled script ${opts.id}`, project ? [project] : undefined);
			}
			return out(text, "success");
		} catch (e) {
			finalStatus = "error";
			text = `⚠️ Script task error: ${e instanceof Error ? e.message : String(e)}`;
			return out(text, "error");
		} finally {
			// Mirrors dispatch()'s finally AND appends a history-compatible TaskLog so script runs
			// appear in the WebUI schedule history / recent runs / stats.
			appendTaskLog(
				scriptTaskLog({
					taskId: opts.id,
					source,
					startedAt,
					status: finalStatus,
					script: opts.script,
					replyText: text,
					project,
					ran,
				}),
			);
			markFinished();
			recordStatus(finalStatus);
			stats.lastDispatchDurationMs = Date.now() - startedAt;
			if (finalStatus === "error" || finalStatus === "validation-failed") markFailed();
		}
	});

	// Keep the queue moving even on error (same pattern as dispatch).
	chain = run.then(
		() => undefined,
		() => undefined,
	);
	return run;
}
