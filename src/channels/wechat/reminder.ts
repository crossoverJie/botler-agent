/**
 * WeChat session renewal reminder loop.
 *
 * The WeChat context_token is tied to a rolling 24h window; each inbound message refreshes it.
 * This loop periodically checks ONLY the account owner: if they have been quiet for
 * `WECHAT_REMINDER_HOURS` and this round hasn't reminded them yet, it sends a reminder
 * (primary WeChat channel, falling back to other channels via deliver). It must not spam:
 * lastRemindedAt is updated whether the send succeeded or failed, so each quiet stretch is
 * reminded at most once per tick.
 *
 * The owner is re-resolved from account.json on every tick (never cached), so a re-login that
 * changes the account takes effect immediately. The loop runs on a setTimeout chain with a
 * re-entrancy guard so a slow deliver never overlaps ticks; it does not block the monitor's
 * long-poll.
 */

import { CONFIG } from "../../config.ts";
import { deliver } from "../../push/deliver.ts";
import { resolveAccount } from "./account.ts";
import { getContext, markReminded } from "./context.ts";

/** Renewal-reminder check interval. Exported so the WebUI can display it. */
export const REMINDER_TICK_MS = 10 * 60 * 1000;

/** Process-local loop state, surfaced via reminderStatus() for the WebUI. */
let started = false;
let lastTickAt: number | null = null;
let nextTickAt: number | null = null;

/** Current renewal-reminder loop state (read-only, for the WebUI status panel). */
export function reminderStatus(): {
	started: boolean;
	tickMs: number;
	lastTickAt: number | null;
	nextTickAt: number | null;
} {
	return { started, tickMs: REMINDER_TICK_MS, lastTickAt, nextTickAt };
}

/** Reminder copy modeled after openilink-hub; values are defensively clamped (negative hours / over 24 → remaining hours = 0). */
function reminderText(elapsedHours: number): string {
	const h = Math.max(0, Math.round(elapsedHours));
	const remaining = Math.max(0, 24 - h);
	return (
		`[System reminder] Your bot has not received a message for over ${h} hours; the session expires in about ${remaining} hours.` +
		`Please reply with any message in WeChat to refresh the 24-hour window.`
	);
}

let running = false;

async function tick(): Promise<void> {
	if (running) return; // a slow deliver must not overlap the next tick
	running = true;
	lastTickAt = Date.now();
	try {
		const hours = CONFIG.wechatReminderHours;
		if (hours < 1) return; // disabled (0) or misconfigured — config clamps to [1,24] anyway
		const owner = resolveAccount().userId;
		if (!owner) return;
		const ctx = getContext(owner);
		if (!ctx) return; // owner never messaged the bot (no context yet) — nothing to renew
		const now = Date.now();
		const quiet = now - ctx.lastMsgAt;
		if (quiet < hours * 3600e3) return; // still inside the window
		const alreadyReminded = ctx.lastRemindedAt != null && ctx.lastRemindedAt >= ctx.lastMsgAt;
		if (alreadyReminded) return; // this quiet stretch was already nudged
		const result = await deliver(
			{ text: reminderText(quiet / 3600e3), images: [] },
			{ source: "wechat", userId: owner },
		);
		if (!result.ok) {
			console.error(`[wechat-reminder] deliver failed: ${result.error ?? "unknown"}`);
		}
		// Record the attempt regardless of outcome so we don't retry-spam on the same quiet stretch.
		markReminded(owner);
	} catch (e) {
		console.error(`[wechat-reminder] tick error: ${e instanceof Error ? e.message : String(e)}`);
	} finally {
		running = false;
	}
}

function loop(): void {
	void tick().then(() => {
		nextTickAt = Date.now() + REMINDER_TICK_MS;
		setTimeout(loop, REMINDER_TICK_MS);
	});
}

/** Start the renewal reminder loop (fire-and-forget). No-op unless wechatReminderHours >= 1. */
export function startWechatReminderLoop(): void {
	if (CONFIG.wechatReminderHours < 1) return;
	started = true;
	console.log(`[wechat-reminder] started (threshold ${CONFIG.wechatReminderHours}h, tick ${REMINDER_TICK_MS / 60_000}min)`);
	loop();
}
