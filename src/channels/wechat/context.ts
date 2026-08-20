/**
 * WeChat context_token persistence.
 *
 * context_token is the protocol handle the server expects echoed back on replies. It is tied
 * to a 24h rolling window: each inbound message refreshes it. We persist per-user token +
 * last message time (+ optional last reminder time) so the renewal reminder loop can detect
 * a quiet owner and nudge them before the session expires.
 *
 * This module deliberately does NOT import account.ts (no getOwnerContext()): the owner
 * resolution lives in the caller (reminder.ts), keeping the dependency direction
 * account.ts → context.ts one-way (no cycle).
 */

import { existsSync, mkdirSync, readFileSync, writeFileSync, unlinkSync, chmodSync } from "node:fs";
import { join } from "node:path";
import { USER_CONFIG_DIR } from "../../config.ts";

export interface WechatContextEntry {
	token: string;
	lastMsgAt: number;
	lastRemindedAt?: number;
}

type ContextsMap = Record<string, WechatContextEntry>;

function contextsPath(): string {
	return join(USER_CONFIG_DIR, "wechat", "contexts.json");
}

function loadContexts(): ContextsMap {
	try {
		if (existsSync(contextsPath())) {
			const raw = JSON.parse(readFileSync(contextsPath(), "utf8")) as ContextsMap;
			if (raw && typeof raw === "object") return raw;
		}
	} catch {
		// ignore → empty map
	}
	return {};
}

function saveContexts(map: ContextsMap): void {
	mkdirSync(join(USER_CONFIG_DIR, "wechat"), { recursive: true });
	writeFileSync(contextsPath(), JSON.stringify(map, null, 2), "utf8");
	try {
		chmodSync(contextsPath(), 0o600);
	} catch {
		// best-effort
	}
}

/** Refresh a user's token and last-message time (preserving lastRemindedAt). */
export function updateContext(userId: string, token: string): void {
	if (!userId || !token) return;
	const map = loadContexts();
	const prev = map[userId];
	map[userId] = {
		token,
		lastMsgAt: Date.now(),
		lastRemindedAt: prev?.lastRemindedAt,
	};
	saveContexts(map);
}

/** Mark that a renewal reminder was sent for this user at now (lastMsgAt untouched). */
export function markReminded(userId: string): void {
	const map = loadContexts();
	const prev = map[userId];
	if (!prev) return;
	map[userId] = { ...prev, lastRemindedAt: Date.now() };
	saveContexts(map);
}

export function getContext(userId: string): WechatContextEntry | undefined {
	return loadContexts()[userId];
}

export function clearContexts(): void {
	try {
		unlinkSync(contextsPath());
	} catch {
		// ignore (already absent)
	}
}
