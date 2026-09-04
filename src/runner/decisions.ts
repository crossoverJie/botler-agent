/**
 * Pure routing/execution decisions extracted from runner.ts so they can be
 * tested without constructing a model or Agent.
 */

import { RESET_CONTEXT_DECISION, SCHEDULER_VIRTUAL_PROJECT } from "../prompts/system-prompt.ts";

const CLEAR_CONVERSATION_TOOL = "clear_conversation_context";
const RESET_CONTEXT_PATTERN = new RegExp(`^${RESET_CONTEXT_DECISION}\\b`, "i");
const UNKNOWN_PATTERN = /unknown|无法确定|不确定|无法判断|不明确/i;
const SCHEDULER_ALIAS_PATTERN = /定时|提醒|日程/;

export interface RouteDecision {
	/** Ordered, deduplicated selected subprojects; [] = unknown (no project). */
	projects: string[];
	/** For image-bearing messages, the single selected project where the original is saved; null otherwise. */
	attachmentProject: string | null;
	resetContext: boolean;
}

export function emptyRoute(): RouteDecision {
	return { projects: [], attachmentProject: null, resetContext: false };
}

/** Strip surrounding markdown code fences and trim whitespace. */
function stripCodeFences(s: string): string {
	return s
		.replace(/^```[a-zA-Z]*\s*\n?/, "")
		.replace(/\n?```\s*$/, "")
		.trim();
}

/** Slice a string from the first `{` so prose prepended before JSON still parses. */
function sliceFromFirstBrace(s: string): string {
	const idx = s.indexOf("{");
	return idx === -1 ? s : s.slice(idx);
}

function validateRoute(value: unknown, candidates: readonly string[]): RouteDecision {
	if (!value || typeof value !== "object" || Array.isArray(value)) return emptyRoute();
	const obj = value as Record<string, unknown>;
	if (!Array.isArray(obj.projects)) return emptyRoute();

	const projects: string[] = [];
	for (const p of obj.projects) {
		if (typeof p !== "string" || !candidates.includes(p)) return emptyRoute();
		if (!projects.includes(p)) projects.push(p);
	}
	if (projects.length === 0) return emptyRoute();

	// The scheduler virtual project is exclusive: it must be the sole entry, never mixed with data projects.
	if (projects.includes(SCHEDULER_VIRTUAL_PROJECT) && projects.length > 1) return emptyRoute();

	const rawAtt = obj.attachmentProject;
	let attachmentProject: string | null = null;
	if (rawAtt !== undefined && rawAtt !== null && rawAtt !== "null") {
		if (typeof rawAtt !== "string") return emptyRoute();
		if (rawAtt === SCHEDULER_VIRTUAL_PROJECT) return emptyRoute();
		if (!projects.includes(rawAtt)) return emptyRoute();
		attachmentProject = rawAtt;
	}

	return { projects, attachmentProject, resetContext: false };
}

/** Exact (whole-token) match of a candidate name in free text, tolerant of punctuation. */
function exactNameMatches(text: string, candidates: readonly string[]): string[] {
	const tokens = text.split(/[^A-Za-z0-9_-]+/).filter(Boolean);
	return candidates.filter((c) => tokens.includes(c));
}

/**
 * Parse the routing output (structured JSON) into an ordered set of selected subprojects.
 * The candidate list includes the virtual `__scheduler__` project. Aliases are matched only
 * AFTER real-project matches, so a message targeting a data project is never stolen by the
 * scheduler; "task" alone is deliberately not an alias.
 */
export function parseRoute(output: string, candidates: string[]): RouteDecision {
	const t = stripCodeFences(output.replace(/^[-*\s]+/, "").trim());
	if (!t) return emptyRoute();
	if (RESET_CONTEXT_PATTERN.test(t)) return { ...emptyRoute(), resetContext: true };
	if (UNKNOWN_PATTERN.test(t)) return emptyRoute();

	const parsed = (() => {
		try {
			return JSON.parse(sliceFromFirstBrace(t)) as unknown;
		} catch {
			return undefined;
		}
	})();
	if (parsed !== undefined) return validateRoute(parsed, candidates);

	// Tolerant fallback for a model that ignores the JSON instruction and replies with a bare
	// project name: route only when exactly one candidate name appears (whole-token match).
	const matches = exactNameMatches(t, candidates);
	if (matches.length === 1) {
		return { projects: [matches[0]], attachmentProject: null, resetContext: false };
	}
	if (
		matches.length === 0 &&
		candidates.includes(SCHEDULER_VIRTUAL_PROJECT) &&
		(t === "scheduler" || SCHEDULER_ALIAS_PATTERN.test(t))
	) {
		return { projects: [SCHEDULER_VIRTUAL_PROJECT], attachmentProject: null, resetContext: false };
	}
	return emptyRoute();
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
