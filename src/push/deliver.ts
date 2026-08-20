/**
 * Push delivery with channel fallback.
 *
 * `deliver` sends a payload to a recipient on its primary channel; if that throws, it degrades
 * to the other configured channels in a fixed order (`telegram → feishu → wechat`), only
 * trying channels that (a) are configured and (b) have recorded contact addresses — each
 * recorded address is tried until one succeeds, so a fallback never fires at an unknown
 * address or logs a misleading "bot not started" line for a channel the user never enabled.
 *
 * Images are only sent on the primary WeChat channel; degraded channels get plain text
 * (matching the current per-channel behavior).
 */

import { CONFIG } from "../config.ts";
import { getContacts } from "./contacts.ts";
import type { ChannelSource, PushPayload, Recipient } from "./types.ts";
import { sendTelegramMessage } from "../channels/telegram.ts";
import { replyFeishu } from "../channels/feishu.ts";
import { resolveAccount } from "../channels/wechat/account.ts";
import { getContext } from "../channels/wechat/context.ts";
import {
	markdownToPlainText,
	sendMessageWeixin,
	sendImageMessageWeixin,
} from "../channels/wechat/send-media.ts";
import { downloadRemoteImageToTemp, uploadImageToWeixin } from "../channels/wechat/upload.ts";

const FALLBACK_ORDER: ChannelSource[] = ["telegram", "feishu", "wechat"];

function channelConfigured(ch: ChannelSource): boolean {
	switch (ch) {
		case "telegram":
			return Boolean(CONFIG.telegramToken);
		case "feishu":
			return Boolean(CONFIG.feishuAppId && CONFIG.feishuAppSecret);
		case "wechat":
			return resolveAccount().configured;
	}
}

/**
 * Send one payload to a single channel address. Throws on any failure so the caller can
 * fall back. WeChat requires a cached context_token (refreshed by inbound messages); without
 * one it throws like the other channels.
 */
async function deliverToChannel(
	ch: ChannelSource,
	payload: PushPayload,
	address: string,
	withImages: boolean,
): Promise<void> {
	switch (ch) {
		case "telegram":
			await sendTelegramMessage(address, payload.text);
			return;
		case "feishu":
			await replyFeishu(address, payload.text);
			return;
		case "wechat": {
			const ctx = getContext(address);
			if (!ctx?.token) throw new Error("wechat: no cached context_token for this user");
			const { baseUrl, token } = resolveAccount();
			if (!baseUrl) throw new Error("wechat: account not configured");
			if (payload.text) {
				// Same as the normal reply path (monitor.ts): strip markdown before sending —
				// a scheduled result may contain **bold**, `code`, [links] and table pipes.
				const plain = markdownToPlainText(payload.text).trim();
				if (plain) {
					await sendMessageWeixin({ to: address, text: plain, baseUrl, token, contextToken: ctx.token });
				}
			}
			if (withImages) {
				for (const img of payload.images) {
					const localPath = /^https:\/\//i.test(img) ? await downloadRemoteImageToTemp(img) : img;
					const uploaded = await uploadImageToWeixin({
						filePath: localPath,
						toUserId: address,
						opts: { baseUrl, token },
					});
					await sendImageMessageWeixin({
						to: address,
						uploaded,
						baseUrl,
						token,
						contextToken: ctx.token,
					});
				}
			}
			return;
		}
	}
}

export async function deliver(
	payload: PushPayload,
	recipient: Recipient,
): Promise<{ ok: boolean; via?: ChannelSource; error?: string }> {
	const primary = recipient.source;
	try {
		await deliverToChannel(primary, payload, recipient.userId, primary === "wechat");
		return { ok: true, via: primary };
	} catch (e) {
		const primaryErr = e instanceof Error ? e.message : String(e);
		console.error(`[push] primary ${primary} failed: ${primaryErr}`);
		// Degraded channels are plain-text only (current per-channel behavior).
		const textOnly = { text: payload.text, images: [] };
		for (const ch of FALLBACK_ORDER) {
			if (ch === primary) continue;
			if (!channelConfigured(ch)) continue;
			const addresses = getContacts(ch);
			if (addresses.length === 0) continue;
			// Try every recorded address for that channel; succeed on the first one that works.
			for (const addr of addresses) {
				try {
					await deliverToChannel(ch, textOnly, addr, false);
					console.log(`[push] degraded to ${ch} -> ${addr}`);
					return { ok: true, via: ch };
				} catch (e2) {
					console.error(`[push] fallback ${ch} -> ${addr} failed: ${e2 instanceof Error ? e2.message : String(e2)}`);
				}
			}
		}
		return { ok: false, error: primaryErr };
	}
}
