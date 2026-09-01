import { statSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { Agent } from "@earendil-works/pi-agent-core";
import { createModels } from "@earendil-works/pi-ai";
import { anthropicProvider } from "@earendil-works/pi-ai/providers/anthropic";
import type { AssistantMessage, ImageContent, Model, Usage } from "@earendil-works/pi-ai";
// Model is generic; here we use any to denote a model of any provider API
type AnyModel = Model<any>;
import { dataTools, fileTools } from "./tools/index.ts";
import { setTaskContext } from "./tools/task-context.ts";
import {
	parseRoute,
	shouldClearConversationForRoute,
	shouldRecordConversationTurn,
} from "./runner/decisions.ts";
import { safePath } from "./tools/paths.ts";
import { persistInboundImage, type InboundImage } from "./channels/wechat/download.ts";
import {
	loadSystemPrompt,
	buildRoutePrompt,
	listProjectDirs,
	SCHEDULER_VIRTUAL_PROJECT,
	RESET_CONTEXT_DECISION,
} from "./prompts/system-prompt.ts";
import {
	isGreeting,
	greetingReply,
	fallbackUnknownReply,
	contextResetReply,
} from "./greeting.ts";
import { CONFIG } from "./config.ts";
import { buildCustomProvider } from "./providers.ts";
import { collectTaskLog, type CollectInput } from "./logging/collect.ts";
import type { Recipient } from "./push/types.ts";
import type { ModelCacheStats, TaskLog } from "./logging/types.ts";
import {
	formatRecentTurns,
	loadRecentTurns,
	clearSession,
	shouldLoadRecentTurns,
	type ConversationTurn,
} from "./conversation/store.ts";
import { markModelCache, markAgentStart, markAgentEnd, stats } from "./monitor/stats.ts";

const models = createModels();
// Custom providers (from ~/.botler-agent/providers.json, with a legacy CUSTOM_* env fallback) take
// priority; otherwise fall back to the built-in anthropic provider. Multiple providers can coexist.
if (CONFIG.customProviders.length > 0) {
	for (const p of CONFIG.customProviders) {
		models.setProvider(buildCustomProvider(p));
	}
} else {
	models.setProvider(anthropicProvider());
}

let cachedModel: AnyModel | undefined;

/** Resolve and cache the model used for this run; on misconfiguration give a useful error instead of crashing. */
function resolveModel(): AnyModel {
	if (cachedModel) {
		markModelCache(true);
		return cachedModel;
	}
	markModelCache(false);
	const model = models.getModel(CONFIG.provider, CONFIG.model);
	if (!model) {
		const available = models
			.getModels()
			.map((m) => `${m.provider}/${m.id}`)
			.join(", ");
		throw new Error(
			`Model not found: ${CONFIG.provider}/${CONFIG.model}.\nAvailable models: ${available}\nCheck PI_PROVIDER / PI_MODEL in .env.`,
		);
	}
	cachedModel = model;
	return model;
}

/** Snapshot of the model-resolution cache counters for a task log (single source of truth = stats). */
function snapshotModelCache(): ModelCacheStats {
	return {
		queries: stats.modelCacheQueries,
		hits: stats.modelCacheHits,
		hitRate: stats.modelCacheQueries ? stats.modelCacheHits / stats.modelCacheQueries : 0,
	};
}

export interface RunLogContext {
	/** Dispatch-level id shared by the main execute log and its self-heal retry. */
	taskId: string;
	/** Source channel (telegram / feishu / wechat / cli / scheduler / unknown). */
	source: string;
	/** "execute" for the main run, "self-heal" for the validation self-heal retry. */
	phase: "execute" | "self-heal";
	/** Optional routing hint (a valid data subproject name) — skips the routing LLM call. */
	projectHint?: string;
	/** Optional recipient (the message sender); injected into the task context for tools like schedule. */
	recipient?: Recipient;
	/** Inbound images (decoded bytes) to feed the model as vision input AND persist under the target subproject. */
	inboundImages?: InboundImage[];
	/** Fixed conversation session key for IM messages; absent for scheduler / CLI / self-heal. */
	sessionKey?: string;
}

export interface TaskResult {
	/** Final reply text (taken from the body of the last assistant message), image refs stripped. */
	text: string;
	/** Images to send alongside the text: absolute paths inside DATA_ROOT, or https URLs. */
	images: string[];
	/** Whether this task called write / edit (i.e. modified a data file). */
	mutated: boolean;
	/** Collected task log; the dispatcher overrides `status` then appends it (undefined only on early throw). */
	log?: TaskLog;
	/** False for deterministic short-circuit replies that should not consume a conversation slot. */
	recordConversationTurn?: boolean;
}

const IMAGE_REF_RE = /!\[[^\]]*\]\(([^)]*)\)/g;
const IMAGE_EXT_RE = /\.(png|jpe?g|gif|webp|bmp)$/i;

/**
 * Split the reply into text + image references (markdown `![alt](path)`).
 *
 * Local paths go through safePath (DATA_ROOT allowlist + symlink escape check) and must be an
 * existing image file — the agent supplies the path, but the framework decides whether it is
 * allowed, so a hallucinated path cannot leak an arbitrary file. Invalid refs are dropped
 * silently: the text reply still gets through. Image markdown is always stripped from the text,
 * so channels that cannot render it (telegram / CLI) don't show raw markdown.
 */
function extractImages(text: string): { text: string; images: string[] } {
	const images: string[] = [];
	for (const m of text.matchAll(IMAGE_REF_RE)) {
		const ref = (m[1] ?? "").trim();
		if (!ref) continue;
		if (/^https:\/\//i.test(ref)) {
			if (!images.includes(ref)) images.push(ref);
			continue;
		}
		if (!IMAGE_EXT_RE.test(ref)) continue;
		try {
			const abs = safePath(ref);
			if (statSync(abs).isFile() && !images.includes(abs)) images.push(abs);
		} catch {
			// Out of bounds / missing / unreadable: skip this image, keep the reply text
			console.warn(`[runner] ignoring unusable image reference: ${ref}`);
		}
	}
	return { text: text.replace(IMAGE_REF_RE, "").trim(), images };
}

/** InboundImage[] -> ImageContent[] for the model (base64 inline vision). */
function toImageContent(inboundImages: InboundImage[]): ImageContent[] {
	return inboundImages.map((i) => ({
		type: "image",
		data: i.buffer.toString("base64"),
		mimeType: i.mimeType,
	}));
}

/** Take the body of the last assistant message from state; returns empty string if none or on failure. */
function lastAssistantText(state: { messages: unknown[] }): string {
	const msgs = state.messages;
	const last = msgs[msgs.length - 1] as { role?: string; stopReason?: string; content?: unknown } | undefined;
	if (!last || last.role !== "assistant") return "";
	if (last.stopReason === "error" || last.stopReason === "aborted") return "";
	const content = last.content;
	if (!Array.isArray(content)) return "";
	return content
		.filter((c) => (c as { type?: string }).type === "text")
		.map((c) => (c as { text: string }).text)
		.join("")
		.trim();
}

/** Count "tool turns": the number of assistant messages containing at least one toolCall content block (a message with multiple toolCalls still counts as 1 turn). */
function countToolTurns(messages: readonly { role?: unknown; content?: unknown }[]): number {
	let n = 0;
	for (const m of messages) {
		if (
			m.role === "assistant" &&
			Array.isArray(m.content) &&
			m.content.some((c) => (c as { type?: string }).type === "toolCall")
		) {
			n++;
		}
	}
	return n;
}

/** Hint injected to the model as it approaches the tool-turn cap, asking it to wrap up. */
const TOOL_TURN_CAP_HINT = [
	"Note: this task's tool-call count is approaching the limit. Please stop calling any tools immediately (do not use read / write / edit / run / schedule again).",
	"Based on the reads/writes you have already completed, give your final summary reply directly.",
	"If the task is not finished, state honestly what is done, where it is stuck, and why it could not be completed.",
].join("\n");

/**
 * Phase 1: decide which data subproject the user message targets.
 * Single project returns directly; multiple projects route via one lightweight LLM call; undetermined returns null (caller notifies the user).
 * The routing Agent is a single clean LLM call, so its `usage` (and the routing prompt) is returned alongside the decision.
 *
 * `includeScheduler` controls whether the virtual `__scheduler__` project is a routing candidate.
 * It is excluded for scheduler-fired entries without a projectHint (a fired reminder is a
 * notification, never a schedule-management request), so a message like "remind user to drink water" can't be
 * misread as "create/manage a schedule" and instead lands in the engine's unknown-project fallback.
 */
async function routeProject(
	userMessage: string,
	model: AnyModel,
	includeScheduler: boolean,
	images?: ImageContent[],
	hasImages = false,
	recentTurns: readonly ConversationTurn[] = [],
	allowResetContext = false,
): Promise<{ project: string | null; resetContext: boolean; usage?: Usage; prompt?: string; candidates: string[] }> {
	const projects = listProjectDirs();
	if (projects.length === 0) return { project: null, resetContext: false, candidates: [] };
	if (projects.length === 1) return { project: projects[0], resetContext: false, candidates: [...projects] };

	// The virtual scheduler project is a routing candidate but NOT a data subproject; the
	// single-project shortcut above intentionally ignores it (the schedule tool stays available
	// in any execution context, so single-project setups still work).
	const candidates = includeScheduler ? [...projects, SCHEDULER_VIRTUAL_PROJECT] : projects;
	const prompt = buildRoutePrompt(userMessage, includeScheduler, hasImages, recentTurns, allowResetContext);
	const agent = new Agent({
		initialState: {
			systemPrompt: prompt,
			model,
			tools: [], // Routing phase needs no tools, only outputs the project name
		},
		streamFn: models.streamSimple.bind(models),
	});
	await agent.prompt(userMessage, images);
	const decision = parseRoute(lastAssistantText(agent.state), candidates);
	// Routing state is [user, assistant]; the assistant message carries a single clean usage.
	const assistant = agent.state.messages[agent.state.messages.length - 1];
	const usage = assistant && assistant.role === "assistant" ? assistant.usage : undefined;
	return { project: decision.project, resetContext: decision.resetContext, usage, prompt, candidates };
}

/**
 * Build a minimal TaskLog for early-return paths (no projects / unknown project / error) where we
 * have no agent.state to collect from. `status` is seeded "success" and overridden by the dispatcher.
 */
function buildMinimalLog(opts: {
	ctx: RunLogContext;
	startedAt: number;
	endedAt: number;
	userMessage: string;
	replyText: string;
	project: string | null;
	routingUsage?: Usage;
	routing?: { candidates: string[]; decision: string | null; prompt?: string };
}): TaskLog {
	const base: CollectInput = {
		id: randomUUID(),
		taskId: opts.ctx.taskId,
		phase: opts.ctx.phase,
		source: opts.ctx.source,
		provider: CONFIG.provider,
		model: CONFIG.model,
		project: opts.project,
		startedAt: opts.startedAt,
		endedAt: opts.endedAt,
		userMessage: opts.userMessage,
		replyText: opts.replyText,
		images: [],
		mutated: false,
		inboundImageCount: opts.ctx.inboundImages?.length ?? 0,
		messages: [],
		routingUsage: opts.routingUsage,
		routing: opts.routing,
		modelCache: snapshotModelCache(),
	};
	return collectTaskLog(base);
}

/**
 * Run a task (two phases):
 *   1. Route: decide which subproject the message belongs to; if undetermined → return a hint and do not execute.
 *   2. Execute: concatenate only that subproject's conventions, create a fresh independent Agent to complete the task.
 *
 * Collects a {@link TaskLog} (via `logCtx`) describing the run; the dispatcher finalizes `status`
 * and appends it. On an unexpected throw before collection, `log` is undefined (the dispatcher
 * emits its own error log).
 */
export async function runTask(userMessage: string, logCtx?: RunLogContext): Promise<TaskResult> {
	const ctx: RunLogContext = logCtx ?? { taskId: randomUUID(), source: "cli", phase: "execute" };
	const startedAt = Date.now();
	const inboundImages = ctx.inboundImages ?? [];
	const isImSession =
		ctx.phase === "execute" &&
		ctx.source !== "scheduler" &&
		ctx.source !== "cli" &&
		Boolean(ctx.sessionKey);

	const recentTurns = shouldLoadRecentTurns({
		enabled: CONFIG.conversationContextEnabled,
		phase: ctx.phase,
		source: ctx.source,
		sessionKey: ctx.sessionKey,
	})
		? loadRecentTurns(ctx.sessionKey!, CONFIG.conversationContextTurns)
		: [];
	// Base64-encode each inbound image exactly once and reuse it for routing + execution.
	const imagesContent = toImageContent(inboundImages);
	const model = resolveModel();
	const projects = listProjectDirs();
	if (projects.length === 0) {
		const replyText = "⚠️ No data subprojects under DATA_ROOT; please check your config.";
		return {
			text: replyText,
			images: [],
			mutated: false,
			log: buildMinimalLog({ ctx, startedAt, endedAt: Date.now(), userMessage, replyText, project: null }),
			recordConversationTurn: false,
		};
	}

	// Greeting short-circuit: a bare greeting has nothing to route to, so skip the routing
	// LLM call entirely and reply with a friendly welcome (zero extra LLM cost). Programmatic
	// messages that carry a projectHint, and scheduler-fired tasks, never take this path.
	// A message that carries an image is NEVER a greeting — skip the check so the image is not swallowed.
	if (!ctx.projectHint && ctx.source !== "scheduler" && inboundImages.length === 0 && isGreeting(userMessage)) {
		const replyText = greetingReply();
		return {
			text: replyText,
			images: [],
			mutated: false,
			log: buildMinimalLog({ ctx, startedAt, endedAt: Date.now(), userMessage, replyText, project: null }),
			recordConversationTurn: false,
		};
	}

	// Phase 1 (routing): a valid projectHint skips the routing LLM call entirely.
	const hint = ctx.projectHint;
	let project: string | null;
	let routingUsage: Usage | undefined;
	let routingPrompt: string | undefined;
	let routingCandidates: string[];
	let resetContext = false;
	if (hint && projects.includes(hint)) {
		project = hint;
		routingUsage = undefined;
		routingPrompt = undefined;
		routingCandidates = [...projects, SCHEDULER_VIRTUAL_PROJECT];
	} else {
		const routed = await routeProject(
			userMessage,
			model,
			ctx.source !== "scheduler",
			imagesContent,
			inboundImages.length > 0,
			recentTurns,
			isImSession,
		);
		resetContext = routed.resetContext;
		project = routed.resetContext ? null : routed.project;
		routingUsage = routed.usage;
		routingPrompt = routed.prompt;
		routingCandidates = routed.candidates;
	}
	if (shouldClearConversationForRoute(resetContext, isImSession, ctx.sessionKey)) {
		clearSession(ctx.sessionKey!);
		const replyText = contextResetReply();
		return {
			text: replyText,
			images: [],
			mutated: false,
			log: buildMinimalLog({
				ctx,
				startedAt,
				endedAt: Date.now(),
				userMessage,
				replyText,
				project: null,
				routingUsage,
				routing: { candidates: routingCandidates, decision: RESET_CONTEXT_DECISION, prompt: routingPrompt },
			}),
			recordConversationTurn: false,
		};
	}
	if (!project) {
		const replyText = fallbackUnknownReply(inboundImages.length > 0);
		return {
			text: replyText,
			images: [],
			mutated: false,
			log: buildMinimalLog({
				ctx,
				startedAt,
				endedAt: Date.now(),
				userMessage,
				replyText,
				project: null,
				routingUsage,
				routing: { candidates: routingCandidates, decision: null, prompt: routingPrompt },
			}),
		};
	}

	// Persist originals under DATA_ROOT/<project>/photos, then tell the agent where they are.
	// Persistence happens only after a subproject is known — unrouted / unknown-project messages
	// (returned above) never write a photo into DATA_ROOT, and the virtual __scheduler__ project
	// is not a data subproject (nothing to persist, avoids a spurious warn). The file is written
	// by the framework (a transport concern), validated through safePath + an image-extension
	// whitelist; the agent only ever receives the relative path as text to cite in its record.
	const savedPaths: string[] = [];
	if (inboundImages.length > 0 && project !== SCHEDULER_VIRTUAL_PROJECT) {
		for (const img of inboundImages) {
			try {
				savedPaths.push(await persistInboundImage(img, project!));
			} catch (e) {
				console.warn(`[runner] persist image failed: ${String(e)}`);
			}
		}
	}
	const attachmentNote = savedPaths.length
		? `\n[图片已保存到] ${savedPaths.join(", ")}（可在记录中引用这些相对路径）`
		: "";
	const execText = `${userMessage}${attachmentNote}`.trim();

	// Phase 2: execution. Recent visible turns are injected through the system prompt rather
	// than reconstructed as Agent messages, avoiding internal tool-call metadata.
	const historyBlock = formatRecentTurns(recentTurns);
	const baseSystemPrompt = historyBlock
		? `${loadSystemPrompt(project)}\n\n# 最近对话\n${historyBlock}\n\n以上是最近若干轮用户可见对话。当前消息如果明显是新的独立任务，请忽略旧历史并按新任务处理；如果它是上一轮任务的确认、补充或纠正，请结合历史继续处理。无法确定时向用户确认。`
		: loadSystemPrompt(project);
	const canControlConversation = isImSession && project !== SCHEDULER_VIRTUAL_PROJECT;
	const clearToolInstruction = canControlConversation
		? "\n\n# 会话上下文控制\n你可以使用 clear_conversation_context 工具。仅当用户明确要求清空 / 忽略 / 重置之前的上下文（例如「新任务」「忽略上文」「重置上下文」，或等价表达）时，在处理当前请求前调用该工具一次。调用后，如果用户还带有具体任务，继续完成该任务；如果没有具体任务，只需简短确认已清空。"
		: "";
	const systemPrompt = `${baseSystemPrompt}${clearToolInstruction}`;
	const executionTools = canControlConversation ? fileTools : dataTools;
	// Flag set when the tool-turn cap is hit and the run is hard-stopped; used after prompt() to recognize it and return the fallback copy.
	let toolTurnCapHit = false;
	const agent = new Agent({
		initialState: {
			systemPrompt,
			model,
			tools: executionTools,
		},
		streamFn: models.streamSimple.bind(models),
		// Tool-turn cap: counted from context.messages after each turn.
		// At max-1, inject the wrap-up hint (the next LLM request will see it);
		// at max, hard-stop. The routing Agent (tools: []) is unaffected.
		prepareNextTurnWithContext: (ctx) => {
			if (
				CONFIG.maxToolTurns >= 2 &&
				countToolTurns(ctx.context.messages) === CONFIG.maxToolTurns - 1
			) {
				return {
					context: {
						...ctx.context,
						messages: [
							...ctx.context.messages,
							{ role: "user", content: [{ type: "text", text: TOOL_TURN_CAP_HINT }], timestamp: Date.now() },
						],
					},
				};
			}
			return undefined;
		},
		shouldStopAfterTurn: (ctx) => {
			const hit = countToolTurns(ctx.context.messages) >= CONFIG.maxToolTurns;
			if (hit) toolTurnCapHit = true;
			return hit;
		},
	});

	// Inject the sender as the task context (for tools like schedule to auto-fill the push
	// recipient), and allow the clear-conversation tool only for IM execute runs. Clear the
	// context once the run ends — whether it succeeded or not.
	setTaskContext(
		isImSession || ctx.recipient
			? {
					recipient: ctx.recipient,
					conversationSessionKey: canControlConversation ? ctx.sessionKey : undefined,
				}
			: null,
	);
	try {
		markAgentStart();
		// Inline vision (decoded bytes) + the saved-path note, so the model can both see the
		// image and reference its persisted location in the data file.
		await agent.prompt(execText, imagesContent);
	} finally {
		markAgentEnd();
		setTaskContext(null);
	}

	// prompt() returns Promise<void>, the result is not in the return value; take the last assistant body from state.messages.
	// Do not concatenate all text_delta — the thinking before a tool call is also a text_delta, and concatenating it would mix into the reply.
	const state = agent.state;
	const last = state.messages[state.messages.length - 1];
	const toolNames = state.messages
		.filter((m) => m.role === "toolResult")
		.map((m) => (m as { toolName?: string }).toolName)
		.filter((name): name is string => typeof name === "string");

	let text = "⚠️ No reply generated";
	let images: string[] = [];
	if (last && last.role === "assistant") {
		const msg = last as AssistantMessage;
		// On LLM call failure the last message is error/aborted and content may be empty — return an error message instead of an empty string
		if (msg.stopReason === "error" || msg.stopReason === "aborted") {
			text = `⚠️ Task did not complete successfully: ${msg.errorMessage ?? msg.stopReason}`;
		} else {
			const raw = msg.content
				.filter((c) => c.type === "text")
				.map((c) => (c as { text: string }).text)
				.join("");
			({ text, images } = extractImages(raw));
		}
	} else if (last && last.role === "toolResult" && toolTurnCapHit) {
		// Hard-stopped at the tool-turn cap: the last message is a toolResult with no final reply, so return fallback copy
		const tl = last as { toolName?: unknown; isError?: unknown; content?: unknown };
		const toolName = typeof tl.toolName === "string" ? tl.toolName : "unknown tool";
		const isErr = tl.isError === true;
		const resultText = Array.isArray(tl.content)
			? tl.content
					.filter((c) => (c as { type?: string }).type === "text")
					.map((c) => (c as { text: string }).text)
					.join("")
					.trim()
			: "";
		text =
			`⚠️ Reached the tool-call limit (${CONFIG.maxToolTurns} turns); the task was aborted with no final reply.\n\n` +
			`Last step: ${toolName}${isErr ? " (errored)" : ""}\n` +
			`Last result: ${resultText || "(no text result)"}\n\n` +
			`To raise the limit, set MAX_TOOL_TURNS in .env (default 20).`;
	}

	// Whether a data file was modified: scan toolResult messages for the toolName, or whether
	// inbound images were persisted (so the photo is committed promptly rather than riding the
	// next commit of this subproject).
	const mutated =
		savedPaths.length > 0 ||
		state.messages.some(
			(m) =>
				m.role === "toolResult" &&
				typeof (m as { toolName?: string }).toolName === "string" &&
				["write", "edit"].includes((m as { toolName: string }).toolName),
		);

	const endedAt = Date.now();
	const log = collectTaskLog({
		id: randomUUID(),
		taskId: ctx.taskId,
		phase: ctx.phase,
		source: ctx.source,
		provider: CONFIG.provider,
		model: CONFIG.model,
		project,
		startedAt,
		endedAt,
		userMessage,
		replyText: text,
		images,
		mutated,
		inboundImageCount: inboundImages.length,
		messages: state.messages,
		routingUsage,
		routing: { candidates: routingCandidates, decision: project, prompt: routingPrompt },
		systemPrompt,
		modelCache: snapshotModelCache(),
	});

	return {
		text,
		images,
		mutated,
		log,
		recordConversationTurn: shouldRecordConversationTurn(toolNames, savedPaths.length),
	};
}
