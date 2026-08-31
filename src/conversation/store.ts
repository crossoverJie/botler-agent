/**
 * Persistent recent-conversation store.
 *
 * The framework keeps only the user-visible turns (user message + final Bot reply) for the
 * shared IM session. This file lives outside DATA_ROOT and is intentionally small: no agent
 * thinking, tool calls, or tool results are stored here; those remain in task-logs.
 */

import { closeSync, fsyncSync, mkdirSync, openSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { CONFIG } from "../config.ts";

/** Fixed conversation key used by Telegram / Feishu / WeChat. */
export const IM_SESSION_KEY = "im";

export interface ConversationTurn {
	/** Epoch ms of the turn's start. */
	ts: number;
	/** Routed data subproject; null when the reply did not target a project. */
	project: string | null;
	/** User-visible input text. */
	user: string;
	/** User-visible final Bot reply text. */
	assistant: string;
}

const SESSION_KEY_RE = /^[a-z0-9_-]+$/i;

function sessionFile(sessionKey: string): string {
	if (!SESSION_KEY_RE.test(sessionKey)) throw new Error(`invalid conversation session key: ${sessionKey}`);
	return join(CONFIG.conversationDir, `${sessionKey}.json`);
}

function normalizeProject(value: unknown): string | null {
	return typeof value === "string" && value ? value : null;
}

function normalizeTurn(value: unknown): ConversationTurn | null {
	if (!value || typeof value !== "object") return null;
	const raw = value as Record<string, unknown>;
	const ts = Number(raw.ts);
	if (!Number.isFinite(ts)) return null;
	const user = typeof raw.user === "string" ? raw.user.slice(0, CONFIG.conversationTurnMaxChars) : "";
	const assistant = typeof raw.assistant === "string" ? raw.assistant.slice(0, CONFIG.conversationTurnMaxChars) : "";
	if (!user && !assistant) return null;
	return {
		ts,
		project: normalizeProject(raw.project),
		user,
		assistant,
	};
}

/** Read the full persisted turn list. Corrupt/missing files degrade to an empty list. */
function readTurns(sessionKey: string): ConversationTurn[] {
	try {
		const raw = JSON.parse(readFileSync(sessionFile(sessionKey), "utf8")) as { turns?: unknown };
		if (!raw || !Array.isArray(raw.turns)) return [];
		return raw.turns
			.map(normalizeTurn)
			.filter((turn): turn is ConversationTurn => turn !== null);
	} catch {
		return [];
	}
}

/** Load the newest `maxTurns` turns, ordered oldest -> newest. */
export function loadRecentTurns(sessionKey: string, maxTurns: number): ConversationTurn[] {
	if (!Number.isInteger(maxTurns) || maxTurns <= 0) return [];
	return readTurns(sessionKey).slice(-maxTurns);
}

/** Append a normalized turn and keep only the newest `maxTurns` entries. */
export function appendTurn(sessionKey: string, turn: ConversationTurn, maxTurns: number): void {
	if (!Number.isInteger(maxTurns) || maxTurns <= 0) return;
	try {
		const normalized = normalizeTurn(turn);
		if (!normalized) return;
		const turns = readTurns(sessionKey);
		turns.push(normalized);
		const kept = turns.slice(-maxTurns);
		const file = sessionFile(sessionKey);
		mkdirSync(CONFIG.conversationDir, { recursive: true, mode: 0o700 });
		const tmp = `${file}.${process.pid}.${Date.now()}.tmp`;
		writeFileSync(tmp, JSON.stringify({ turns: kept }, null, 2) + "\n", {
			encoding: "utf8",
			mode: 0o600,
		});
		// fsync before rename so a hard kill cannot leave a zero-length `.tmp` file that
		// looks like the intended destination while the original was already removed.
		const fd = openSync(tmp, "r+");
		try {
			fsyncSync(fd);
		} finally {
			closeSync(fd);
		}
		renameSync(tmp, file);
	} catch (e) {
		// Conversation history must never break the main dispatch flow.
		console.error("[conversation] appendTurn failed:", e instanceof Error ? e.message : e);
	}
}

/** Remove a conversation session file (best-effort). */
export function clearSession(sessionKey: string): void {
	try {
		unlinkSync(sessionFile(sessionKey));
	} catch (e) {
		const err = e as NodeJS.ErrnoException;
		if (err.code !== "ENOENT") {
			console.error("[conversation] clearSession failed:", e instanceof Error ? e.message : e);
		}
	}
}

/**
 * Render recent turns for prompt injection. `includeProject` is useful in the routing phase,
 * where the model should preferentially continue the most recent routed project.
 */
export function formatRecentTurns(
	turns: readonly ConversationTurn[],
	opts: { includeProject?: boolean; maxChars?: number } = {},
): string {
	if (turns.length === 0) return "";
	const maxChars = opts.maxChars ?? CONFIG.conversationContextMaxChars;
	if (!Number.isFinite(maxChars) || maxChars <= 0) return "";
	const separator = "\n\n---\n\n";
	const rendered = turns.map((turn) => {
		const project = opts.includeProject && turn.project ? `（项目：${turn.project}）` : "";
		return `${project}用户：${turn.user}\nBot：${turn.assistant}`;
	});
	// Keep the newest turns within the whole-window cap, dropping the oldest turns first.
	const kept: string[] = [];
	let used = 0;
	for (let i = rendered.length - 1; i >= 0; i--) {
		const piece = rendered[i];
		const add = kept.length === 0 ? piece : `${separator}${piece}`;
		if (used + add.length > maxChars) {
			if (kept.length === 0) kept.push(piece.slice(0, maxChars));
			break;
		}
		kept.unshift(piece);
		used += add.length;
	}
	return kept.join(separator);
}

export type RecordableTaskStatus = "success" | "auto-fixed" | "unknown-project";

/** Whether a completed execution should be appended to the shared IM history. */
export function shouldRecordTurn(
	phase: "execute" | "self-heal",
	status: string,
	sessionKey?: string,
	recordable = true,
): status is RecordableTaskStatus {
	if (!recordable || phase !== "execute" || !sessionKey) return false;
	return status === "success" || status === "auto-fixed" || status === "unknown-project";
}

export interface RecentTurnLoadContext {
	enabled: boolean;
	phase: "execute" | "self-heal";
	source: string;
	sessionKey?: string;
}

/** Whether the runner should load the shared IM conversation for this run. */
export function shouldLoadRecentTurns(ctx: RecentTurnLoadContext): boolean {
	return (
		ctx.enabled &&
		ctx.phase === "execute" &&
		ctx.source !== "scheduler" &&
		ctx.source !== "cli" &&
		Boolean(ctx.sessionKey)
	);
}
