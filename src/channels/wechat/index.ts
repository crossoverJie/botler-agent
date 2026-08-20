import { CONFIG } from "../../config.ts";
import { clearAccount, resolveAccount } from "./account.ts";
import { runWechatMonitor } from "./monitor.ts";
import { startWechatReminderLoop } from "./reminder.ts";
import { setChannelUp } from "../../monitor/stats.ts";

export { wechatLogin } from "./login.ts";

/** Log out: remove stored credentials and sync cursor. */
export function wechatLogout(): void {
	clearAccount();
}

/**
 * Start the WeChat long-poll monitor (fire-and-forget, like telegram bot.start()).
 * Requires WECHAT_ENABLED=1 and a completed `npm start -- wechat-login`.
 */
export function startWechat(): void {
	if (!CONFIG.wechatEnabled) {
		throw new Error("Starting WeChat requires WECHAT_ENABLED=1");
	}
	if (!resolveAccount().configured) {
		throw new Error("WeChat account not logged in; please run: npm start -- wechat-login");
	}
	void runWechatMonitor().catch((e) => {
		console.error("[wechat] monitor exited:", e instanceof Error ? e.message : String(e));
	});
	console.log("[wechat] long polling listening");
	setChannelUp("wechat");
	startWechatReminderLoop();
}
