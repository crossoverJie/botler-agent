import { CONFIG } from "./config.ts";
import { dispatch } from "./dispatcher.ts";
import { startTelegram } from "./channels/telegram.ts";
import { startFeishu } from "./channels/feishu.ts";
import { startWechat, wechatLogin, wechatLogout } from "./channels/wechat/index.ts";
import { startWebui } from "./webui/server.ts";
import { startScheduler } from "./scheduler/engine.ts";
import { startHealthServer } from "./monitor/health.ts";

async function main(): Promise<void> {
	const positional = process.argv.slice(2).find((a) => !a.startsWith("--"));

	// WeChat CLI subcommands (before generic CLI mode)
	if (positional === "wechat-login") {
		await wechatLogin();
		process.exit(0);
	}
	if (positional === "wechat-logout") {
		wechatLogout();
		console.log("✅ WeChat session logged out");
		process.exit(0);
	}

	// CLI mode: pass a single message and run directly (no persistent channel started). Good for local debugging / end-to-end testing.
	if (positional) {
		const reply = await dispatch(positional);
		console.log(reply.text);
		if (reply.images.length) console.log(`[images] ${reply.images.join(", ")}`);
		process.exit(0);
	}

	// Persistent channels: start per .env config
	let started = false;

	// Start the local health/metrics server first (independent of WebUI; ops requirement).
	if (CONFIG.monitorEnabled) {
		startHealthServer();
	}
	if (CONFIG.telegramToken) {
		try {
			startTelegram();
			started = true;
		} catch (e) {
			console.error("[main] Failed to start Telegram:", e instanceof Error ? e.message : e);
		}
	}
	if (CONFIG.feishuAppId && CONFIG.feishuAppSecret) {
		try {
			startFeishu();
			started = true;
		} catch (e) {
			console.error("[main] Failed to start Feishu:", e instanceof Error ? e.message : e);
		}
	}
	if (CONFIG.wechatEnabled) {
		try {
			startWechat();
			started = true;
		} catch (e) {
			console.error("[main] Failed to start WeChat:", e instanceof Error ? e.message : e);
		}
	}

	if (!started) {
		console.error(
			"No channel configured (TELEGRAM_BOT_TOKEN / FEISHU_* / WECHAT_ENABLED).\n" +
				"Run in CLI mode: npm start -- \"your message\"\n" +
				"Or copy .env.example to .env and fill in the channel credentials.",
		);
		process.exit(1);
	}

	if (CONFIG.webuiEnabled) {
		startWebui();
		started = true;
	}

	if (CONFIG.schedulerEnabled) {
		startScheduler();
		started = true;
	}

	console.log("[main] botler-agent running. Press Ctrl+C to exit.");
}

main().catch((e) => {
	console.error("Startup failed:", e);
	process.exit(1);
});
