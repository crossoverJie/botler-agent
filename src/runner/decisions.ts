/**
 * Pure routing/execution decisions extracted from runner.ts so they can be
 * tested without constructing a model or Agent.
 */

import { RESET_CONTEXT_DECISION, SCHEDULER_VIRTUAL_PROJECT } from "../prompts/system-prompt.ts";

const CLEAR_CONVERSATION_TOOL = "clear_conversation_context";
const RESET_CONTEXT_PATTERN = new RegExp(`^${RESET_CONTEXT_DECISION}\\b`, "i");

export interface RouteDecision {
	project: string | null;
	resetContext: boolean;
}

/**
 * Parse the routing output into a project name; returns null if undetermined.
 * The candidate list includes the virtual `__scheduler__` project. Aliases are
 * matched only AFTER real-project matches, so a message targeting a data project
 * is never stolen by the scheduler; "task" alone is deliberately not an alias.
 */
export function parseRoute(output: string, projects: string[]): RouteDecision {
	const t = output.replace(/^[-*\s]+/, "").replace(/[\/\s]+$/, "").trim();
	if (!t) return { project: null, resetContext: false };
	if (RESET_CONTEXT_PATTERN.test(t)) {
		return { project: null, resetContext: true };
	}
	if (/unknown|无法确定|不确定|无法判断|不明确|多个|both/i.test(t)) {
		return { project: null, resetContext: false };
	}
	if (projects.includes(t)) return { project: t, resetContext: false };
	for (const p of projects) if (t.includes(p)) return { project: p, resetContext: false };
	if (projects.includes(SCHEDULER_VIRTUAL_PROJECT) && (t === "scheduler" || /定时|提醒|日程/.test(t)))
		return { project: SCHEDULER_VIRTUAL_PROJECT, resetContext: false };
	return { project: null, resetContext: false };
}

/**
 * Decide whether an execution should append a user-visible turn.
 *
 * A request that only called clear_conversation_context (with no data tool and
 * no inbound-image persistence) is intentionally not written back into history:
 * writing it would immediately repopulate the session that was just cleared.
 */
export function shouldRecordConversationTurn(
	toolNames: readonly string[],
	savedImageCount = 0,
): boolean {
	const clearToolCalled = toolNames.includes(CLEAR_CONVERSATION_TOOL);
	const nonClearToolCalled = toolNames.some((name) => name !== CLEAR_CONVERSATION_TOOL);
	return !(clearToolCalled && !nonClearToolCalled && savedImageCount === 0);
}

/** Whether a route decision should clear the active IM conversation session. */
export function shouldClearConversationForRoute(
	resetContext: boolean,
	isImSession: boolean,
	sessionKey?: string,
): boolean {
	return resetContext && isImSession && Boolean(sessionKey);
}
