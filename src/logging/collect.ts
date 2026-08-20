/**
 * Collector: turns `agent.state.messages` (+ metadata) into the content portion of a {@link TaskLog}.
 *
 * Pure function, no I/O. The dispatcher owns `source` and final `status`, so we seed `status`
 * with "success" and let the dispatcher overwrite it.
 */

import { contentText, type Usage } from "@earendil-works/pi-ai";
import type { AgentMessage } from "@earendil-works/pi-agent-core";
import type {
	CallUsage,
	ConversationEntry,
	ModelCacheStats,
	TaskLog,
	TokenUsageLog,
	ToolCallLog,
} from "./types.ts";

/** Max characters for result text / thinking / conversation text (user msg & final reply are not truncated). */
const TEXT_MAX = 2000;

function truncate(s: string): string {
	return s.length > TEXT_MAX ? s.slice(0, TEXT_MAX) : s;
}

function sumUsage(list: Usage[]): TokenUsageLog {
	const acc: TokenUsageLog = {
		input: 0,
		output: 0,
		cacheRead: 0,
		cacheWrite: 0,
		reasoning: 0,
		total: 0,
		costUsd: 0,
	};
	for (const u of list) {
		if (!u) continue;
		acc.input += u.input;
		acc.output += u.output;
		acc.cacheRead += u.cacheRead;
		acc.cacheWrite += u.cacheWrite;
		// reasoning is a subset of output; accumulate for display only.
		if (typeof u.reasoning === "number") acc.reasoning = (acc.reasoning ?? 0) + u.reasoning;
		acc.total += u.totalTokens;
		acc.costUsd += u.cost.total;
	}
	return acc;
}

export interface CollectInput {
	id: string;
	taskId: string;
	phase: "execute" | "self-heal";
	source: string;
	provider: string;
	model: string;
	project: string | null;
	startedAt: number;
	endedAt: number;
	userMessage: string;
	replyText: string;
	images: string[];
	mutated: boolean;
	/** execute-phase agent.state.messages (empty for early-return paths). */
	messages: AgentMessage[];
	/** Routing LLM call usage (undefined when routing short-circuited or didn't run). */
	routingUsage?: Usage;
	routing?: { candidates: string[]; decision: string | null; prompt?: string };
	/** Rendered execution system prompt (execute phase). */
	systemPrompt?: string;
	modelCache: ModelCacheStats;
}

export function collectTaskLog(input: CollectInput): TaskLog {
	const {
		id,
		taskId,
		phase,
		source,
		provider,
		model,
		project,
		startedAt,
		endedAt,
		userMessage,
		replyText,
		images,
		mutated,
	messages,
	routingUsage,
	routing,
	systemPrompt,
	modelCache,
} = input;

	const tools: ToolCallLog[] = [];
	// Index tool calls by id so we can pair the result back when we hit a toolResult message.
	const toolIndex = new Map<string, ToolCallLog>();
	const conversation: ConversationEntry[] = [];
	const calls: CallUsage[] = [];

	function pushCall(m: Extract<AgentMessage, { role: "assistant" }>): void {
		const u = m.usage;
		if (!u) return;
		calls.push({
			timestamp: m.timestamp,
			stopReason: m.stopReason,
			input: u.input,
			output: u.output,
			cacheRead: u.cacheRead,
			cacheWrite: u.cacheWrite,
			reasoning: u.reasoning,
			total: u.totalTokens,
			costUsd: u.cost.total,
		});
	}

	for (const m of messages) {
		if (m.role === "user") {
			conversation.push({
				role: "user",
				timestamp: m.timestamp,
				text: contentText(m.content),
			});
		} else if (m.role === "assistant") {
			pushCall(m);
			const text = contentText(m.content);
			if (text) {
				conversation.push({
					role: "assistant",
					timestamp: m.timestamp,
					text,
					stopReason: m.stopReason,
				});
			}
			for (const block of m.content) {
				if (block.type === "thinking") {
					conversation.push({
						role: "assistant",
						timestamp: m.timestamp,
						thinking: truncate(block.thinking),
						chars: block.thinking.length,
						stopReason: m.stopReason,
					});
				} else if (block.type === "toolCall") {
					const t: ToolCallLog = {
						id: block.id,
						name: block.name,
						arguments: block.arguments,
						resultText: "",
						resultTruncated: false,
						isError: false,
						startedAt: m.timestamp,
						durationMs: 0,
					};
					tools.push(t);
					toolIndex.set(block.id, t);
				}
			}
		} else if (m.role === "toolResult") {
			const text = contentText(m.content);
			conversation.push({
				role: "toolResult",
				timestamp: m.timestamp,
				text: truncate(text),
				chars: text.length,
				toolName: m.toolName,
				isError: m.isError,
			});
			const t = toolIndex.get(m.toolCallId);
			if (t) {
				t.resultText = truncate(text);
				t.resultTruncated = text.length > TEXT_MAX;
				t.resultChars = text.length;
				t.isError = m.isError;
				t.details = m.details;
				t.durationMs = m.timestamp - t.startedAt;
			}
		}
	}

	const usage = sumUsage([
		...messages
			.filter((m): m is Extract<AgentMessage, { role: "assistant" }> => m.role === "assistant")
			.map((m) => m.usage),
		...(routingUsage ? [routingUsage] : []),
	]);

	const log: TaskLog = {
		id,
		taskId,
		phase,
		source,
		provider,
		model,
		project,
		status: "success",
		startedAt,
		endedAt,
		durationMs: endedAt - startedAt,
		userMessage,
		replyText,
		images,
		mutated,
		tools,
		conversation,
		usage,
		calls,
		systemPrompt,
		modelCache,
	};

	if (routing) {
		log.routing = {
			candidates: routing.candidates,
			decision: routing.decision,
			usage: sumUsage(routingUsage ? [routingUsage] : []),
			prompt: routing.prompt,
		};
	}

	return log;
}
