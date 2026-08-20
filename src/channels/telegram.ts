import { Bot, type BotConfig } from "grammy";
import { HttpsProxyAgent } from "https-proxy-agent";
import { CONFIG } from "../config.ts";
import { dispatch } from "../dispatcher.ts";
import { recordContact } from "../push/contacts.ts";
import { setChannelUp, setChannelDown } from "../monitor/stats.ts";

/** Module-level bot instance (set by startTelegram); used by push delivery. */
let bot: Bot | null = null;

/** Send a plain-text message to a chat. Used by push delivery (deliver.ts). */
export async function sendTelegramMessage(chatId: number | string, text: string): Promise<void> {
	if (!bot) throw new Error("Telegram bot not started");
	await bot.api.sendMessage(chatId, text);
}

/**
 * Resolve the proxy address: prefer TG_PROXY, fall back to the generic HTTPS_PROXY / HTTP_PROXY.
 * On the mainland China network, the Telegram API is blocked, so a proxy is required to connect.
 */
function resolveProxy(): string | undefined {
	return (
		process.env.TG_PROXY ||
		process.env.HTTPS_PROXY ||
		process.env.https_proxy ||
		process.env.HTTP_PROXY ||
		process.env.http_proxy ||
		undefined
	);
}

/**
 * Telegram channel: grammy + default long polling.
 * Long polling naturally sidesteps Telegram's 60s webhook timeout, which is the simplest setup.
 * Receive message → dispatch → reply.
 */
export function startTelegram(): void {
	const token = CONFIG.telegramToken;
	if (!token) {
		throw new Error("Starting Telegram requires TELEGRAM_BOT_TOKEN");
	}

	const proxy = resolveProxy();
	const botConfig: BotConfig<any> = {};
	if (proxy) {
		botConfig.client = {
			baseFetchConfig: { agent: new HttpsProxyAgent(proxy) },
		};
		console.log(`[telegram] Using proxy: ${proxy}`);
	}

	bot = new Bot(token, botConfig);

	// Diagnostics: log every received update to confirm Telegram is delivering messages
	bot.use(async (ctx, next) => {
		const chatId = ctx.chat?.id;
		const text = ctx.message?.text;
		const from = ctx.from?.username ?? ctx.from?.id;
		const updateType = Object.keys(ctx.update)[0] ?? "unknown";
		console.log(`[tg] update=${updateType} chat=${chatId} from=${from} text=${JSON.stringify((text ?? "").slice(0, 60))}`);
		await next();
	});

	bot.on("message", async (ctx) => {
		const text = ctx.message?.text;
		const messageId = ctx.message?.message_id;
		const chatId = ctx.chat?.id;
		if (!text || messageId === undefined || chatId === undefined) {
			console.log("[tg] Ignored non-text / no-chat message");
			return;
		}
		console.log(`[tg] Processing chat=${chatId} msg=${messageId}: ${JSON.stringify(text.slice(0, 60))}`);
		try {
			// Remember this address so push delivery can fall back to Telegram when the primary channel fails.
			recordContact("telegram", String(chatId));
			const reply = await dispatch(text, {
				id: `${chatId}:${messageId}`,
				source: "telegram",
				recipient: { source: "telegram", userId: String(chatId) },
			});
			// Text-only channel: images the agent produced are not sent here, so an
			// images-only reply still needs a non-empty body (Telegram rejects empty text).
			const body = reply.text || "(image generated, but the Telegram channel does not support sending images yet)";
			console.log(`[tg] Reply length=${body.length}, sending…`);
			await ctx.reply(body);
			// A successful reply means the channel is reachable — recover from any transient blip.
			setChannelUp("telegram");
			console.log(`[tg] Replied to chat=${chatId}`);
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			console.error("[telegram] Error processing message:", msg);
			await ctx.reply("⚠️ Something went wrong; please try again later.").catch(() => {});
		}
	});

	bot.catch((err) => {
		// grammy's catch fires on every polling/processing exception — do NOT mark the channel
		// down here (it would stay down permanently on transient errors); just log.
		console.error("[telegram] Unhandled error:", err);
	});

	console.log("[telegram] Starting long polling…");
	bot
		.start({
			onStart: (info) => {
				console.log(`[telegram] @${info.username} online`);
				setChannelUp("telegram");
			},
		})
		.catch((e) => {
			// Long-poll startup / fatal failure: only now is the channel truly down.
			setChannelDown("telegram", e);
		});
}
