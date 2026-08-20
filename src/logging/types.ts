/**
 * Task-log schema for the WebUI.
 *
 * One {@link TaskLog} is written per dispatch execution (the main execute phase) and, when a
 * self-heal retry happens, one extra {@link TaskLog} with `phase: "self-heal"`. The logs are
 * append-only JSONL; see {@link ./store.ts}.
 */

/** A single tool call paired with its result. */
export interface ToolCallLog {
	/** toolCallId, matches the corresponding ToolResultMessage.toolCallId. */
	id: string;
	name: string;
	/** Tool arguments (may contain data structures). */
	arguments: Record<string, unknown>;
	/** Tool return text (after truncation). */
	resultText: string;
	resultTruncated: boolean;
	/** Full length (in characters) of the tool return before truncation. */
	resultChars?: number;
	isError: boolean;
	/** Timestamp of the assistant message that issued this tool call. */
	startedAt: number;
	/** toolResult.timestamp - startedAt (approximate; see collector note). */
	durationMs: number;
	/** Tool `details` (path / bytes / script / cwd …). */
	details?: unknown;
}

/** One entry in the conversation timeline. */
export interface ConversationEntry {
	role: "user" | "assistant" | "toolResult";
	timestamp: number;
	/** user text / assistant reply text / toolResult text. */
	text?: string;
	/** assistant thinking (ThinkingContent). */
	thinking?: string;
	/** Full length (in characters) of text/thinking before truncation. */
	chars?: number;
	/** assistant stopReason. */
	stopReason?: string;
	/** toolResult toolName. */
	toolName?: string;
	/** toolResult isError. */
	isError?: boolean;
}

/** Token usage for a single LLM call (or an aggregate of several). */
export interface TokenUsageLog {
	input: number;
	output: number;
	/** Provider-side prompt-cache read tokens. */
	cacheRead: number;
	cacheWrite: number;
	/** Subset of `output` (reasoning tokens); do NOT re-add to output in the UI. */
	reasoning?: number;
	total: number;
	/** usage.cost.total */
	costUsd: number;
}

/** Per-LLM-call usage detail, one entry per assistant message (in conversation order). */
export interface CallUsage {
	timestamp: number;
	stopReason?: string;
	input: number;
	output: number;
	cacheRead: number;
	cacheWrite: number;
	reasoning?: number;
	total: number;
	costUsd: number;
}

/** Process-lifetime model-resolution cache statistics. */
export interface ModelCacheStats {
	/** Cumulative resolveModel() calls. */
	queries: number;
	/** Times the cached model was reused. */
	hits: number;
	/** hits / queries. */
	hitRate: number;
}

export type TaskStatus =
	| "success"
	| "auto-fixed"
	| "validation-failed"
	| "error"
	| "duplicate"
	| "unknown-project";

export interface TaskLog {
	/** Per-log id (randomUUID). */
	id: string;
	/** Dispatch-level id shared by the main execute log and its self-heal retry. */
	taskId: string;
	phase: "execute" | "self-heal";
	source: string;
	provider: string;
	model: string;
	/** Routed subproject; null = not routed / no project. */
	project: string | null;
	status: TaskStatus;
	startedAt: number;
	endedAt: number;
	durationMs: number;
	userMessage: string;
	replyText: string;
	images: string[];
	mutated: boolean;
	tools: ToolCallLog[];
	conversation: ConversationEntry[];
	/** Aggregate usage of execute + routing calls. */
	usage: TokenUsageLog;
	/** Per-LLM-call usage in the execute phase, in conversation order. Empty for early-return paths. */
	calls: CallUsage[];
	/** Rendered execution system prompt (execute phase); absent for early-return paths. */
	systemPrompt?: string;
	/** Process-level snapshot at log time. */
	modelCache: ModelCacheStats;
	/** Present only on the main execute log when routing ran. */
	routing?: {
		candidates: string[];
		decision: string | null;
		usage: TokenUsageLog;
		/** Routing prompt text. */
		prompt?: string;
	};
}
