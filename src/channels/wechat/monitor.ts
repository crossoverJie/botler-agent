import { CONFIG } from "../../config.ts";
import { dispatch, DUPLICATE_SENTINEL } from "../../dispatcher.ts";
import { recordContact } from "../../push/contacts.ts";
import { getUpdates } from "./api.ts";
import { loadSyncBuf, saveSyncBuf, resolveAccount } from "./account.ts";
import { updateContext } from "./context.ts";
import {
	markdownToPlainText,
	sendImageMessageWeixin,
	sendMessageWeixin,
} from "./send-media.ts";
import { downloadRemoteImageToTemp, uploadImageToWeixin } from "./upload.ts";
import { downloadInboundImage, type InboundImage } from "./download.ts";
import { ImageBatchCoordinator } from "./image-batch.ts";
import { MessageItemType, MessageType, type MessageItem, type WeixinMessage } from "./types.ts";
import { setChannelUp, setChannelDown } from "../../monitor/stats.ts";

const DEFAULT_LONG_POLL_TIMEOUT_MS = 35_000;
const MAX_CONSECUTIVE_FAILURES = 3;
const BACKOFF_DELAY_MS = 30_000;
const RETRY_DELAY_MS = 2_000;
const SESSION_EXPIRED_ERRCODE = -14;

/** Image batching window from config, in ms (WECHAT_IMAGE_BATCH_SECONDS × 1000; 0 = disabled). */
const IMAGE_BATCH_WINDOW_MS = CONFIG.wechatImageBatchSeconds * 1000;

/**
 * Image batching state machine (pure): owns the per-sender merge / flush decisions.
 * WeChat delivers a selected photo immediately, while the user may still be typing the text
 * ("选图即发,文字后到"). The coordinator buffers image-bearing messages for a short window; a
 * following text from the same sender joins the batch and dispatches as one task, otherwise the
 * image(s) alone are dispatched when the window expires.
 */
const batchCoordinator = new ImageBatchCoordinator(IMAGE_BATCH_WINDOW_MS);

/** Flush timers per sender, owned by the monitor (the coordinator stays pure). */
const batchTimers = new Map<string, NodeJS.Timeout>();

/** (Re)arm the flush timer for a sender; the window extends whenever a new image joins. */
function armBatchTimer(sender: string, opts: { baseUrl: string; token?: string }): void {
	const existing = batchTimers.get(sender);
	if (existing) clearTimeout(existing);
	const timer = setTimeout(() => {
		batchTimers.delete(sender);
		void flushBatch(sender, opts);
	}, IMAGE_BATCH_WINDOW_MS);
	// Do not let a pending flush timer keep the process alive on shutdown.
	timer.unref?.();
	batchTimers.set(sender, timer);
}

/** Cancel a sender's flush timer (called when a caption joins and dispatches immediately). */
function cancelBatchTimer(sender: string): void {
	const timer = batchTimers.get(sender);
	if (timer) {
		clearTimeout(timer);
		batchTimers.delete(sender);
	}
}

/** Flush a sender's buffered batch (timer fired with no caption): dispatch as one task, send the reply. */
async function flushBatch(sender: string, opts: { baseUrl: string; token?: string }): Promise<void> {
	const dispatch = batchCoordinator.flush(sender);
	if (!dispatch) return;
	console.log(
		`[wechat] flushed image batch from ${sender}: ${dispatch.images.length} image(s), text=${JSON.stringify(dispatch.text.slice(0, 40))}`,
	);
	await dispatchAndReply({
		text: dispatch.text,
		inboundImages: dispatch.images,
		contextToken: dispatch.contextToken,
		fromUserId: sender,
		opts,
		id: dispatch.id,
	});
}

/** Extract plain text from item_list: first TEXT item, else VOICE-to-text. */
function bodyFromItemList(itemList?: MessageItem[]): string {
	if (!itemList?.length) return "";
	for (const item of itemList) {
		if (item.type === MessageItemType.TEXT && item.text_item?.text != null) {
			return String(item.text_item.text);
		}
	}
	for (const item of itemList) {
		if (item.type === MessageItemType.VOICE && item.voice_item?.text) {
			return String(item.voice_item.text);
		}
	}
	return "";
}

/**
 * Allowlist: the account owner (the person who scanned the QR) is always allowed;
 * additional ilink_user_id values come from WECHAT_ALLOW_FROM (comma-separated).
 */
function isAllowed(sender: string, ownerUserId?: string): boolean {
	if (ownerUserId && sender === ownerUserId) return true;
	const extra = (CONFIG.wechatAllowFrom ?? "")
		.split(",")
		.map((s) => s.trim())
		.filter(Boolean);
	return extra.includes(sender);
}

/** Process a single inbound message: allowlist → extract text → dispatch → send reply. */
async function processOneMessage(
	full: WeixinMessage,
	opts: { baseUrl: string; token?: string; ownerUserId?: string },
): Promise<void> {
	const fromUserId = full.from_user_id ?? "";
	const contextToken = full.context_token;
	const text = bodyFromItemList(full.item_list);

	// First-deploy observability: log raw metadata so we can confirm what real inbound
	// messages look like (message_type / from_user_id) before trusting any skip rule.
	console.log(
		`[wechat] inbound: from=${fromUserId || "(empty)"} type=${full.message_type ?? "?"} text=${JSON.stringify(text.slice(0, 40))}`,
	);

	// Robust skip: no sender. Our own outbound echoes are built with from_user_id=""
	// (see send-media.ts buildMessageReq), so an empty sender is never a real user message.
	if (!fromUserId) return;

	// message_type=BOT is NOT hard-dropped: the SDK never skips by message_type, and a
	// legitimate inbound could in principle carry it. Log it and keep processing; the
	// owner allowlist below still gates who may drive the agent.
	if (full.message_type === MessageType.BOT) {
		console.warn(`[wechat] message_type=BOT inbound (possible echo), processing: from=${fromUserId}`);
	}

	if (!isAllowed(fromUserId, opts.ownerUserId)) {
		console.log(`[wechat] ignoring sender outside allowlist: ${fromUserId}`);
		return;
	}

	// Remember the sender's context_token + address (for push delivery and renewal reminders).
	// Gated on the allowlist above, so unallowed senders are never recorded.
	if (contextToken) {
		updateContext(fromUserId, contextToken);
		recordContact("wechat", fromUserId);
	}

	// Decode inbound images (best-effort: one failure is warn-logged, the rest still go through).
	// Each holds the decoded Buffer so the runner can both show it to the model and save it.
	// Runs AFTER the allowlist check so unauthorized senders never trigger a CDN download/decrypt.
	const inboundImages: InboundImage[] = [];
	for (const item of full.item_list ?? []) {
		if (item.type !== MessageItemType.IMAGE) continue;
		try {
			inboundImages.push(await downloadInboundImage(item));
		} catch (e) {
			console.warn(`[wechat] image decode failed: ${String(e)}`);
		}
	}

	// v1 text-only skip becomes "no text AND no image".
	if (!text && inboundImages.length === 0) return;

	// Batch window: a selected photo is delivered immediately while the user may still be typing
	// the caption, so hold image-bearing messages briefly. A following text from the same sender
	// joins the batch and flushes now; otherwise the image(s) alone are dispatched when the
	// window expires. WECHAT_IMAGE_BATCH_SECONDS=0 disables batching (dispatch immediately).
	const action = batchCoordinator.onMessage(fromUserId, { text, images: inboundImages, contextToken });

	if (action.kind === "buffer") {
		armBatchTimer(fromUserId, opts);
		console.log(
			`[wechat] buffered ${inboundImages.length} image(s) from ${fromUserId} awaiting caption (${CONFIG.wechatImageBatchSeconds}s, batch has ${batchCoordinator.pendingImageCount(fromUserId)} image(s))`,
		);
		return;
	}

	// Dispatch now: standalone text, a caption that joined a pending image batch, or an image
	// dispatch when batching is disabled. A caption join cancels the flush timer.
	cancelBatchTimer(fromUserId);
	const messageId = full.message_id != null || full.client_id != null ? String(full.message_id ?? full.client_id) : undefined;
	await dispatchAndReply({
		text: action.dispatch.text,
		inboundImages: action.dispatch.images,
		contextToken: action.dispatch.contextToken,
		fromUserId,
		opts,
		id: messageId ?? action.dispatch.id,
	});
}

/** Dispatch a single task and send the reply back to the sender (text + any outbound images). */
async function dispatchAndReply(params: {
	text: string;
	inboundImages: InboundImage[];
	contextToken?: string;
	fromUserId: string;
	opts: { baseUrl: string; token?: string };
	/** Dedup id (from the coordinator, or the message's message_id/client_id when present). */
	id: string;
}): Promise<void> {
	const { text, inboundImages, contextToken, fromUserId, opts, id } = params;

	// Protocol: replying requires echoing this message's context_token. Without it we
	// cannot reply at all — log loudly instead of silently dropping the ack.
	if (!contextToken) {
		console.warn(`[wechat] missing context_token, cannot reply to this message (protocol requires echoing it): from=${fromUserId}`);
	}

	try {
		const reply = await dispatch(text, {
			id,
			source: "wechat",
			recipient: { source: "wechat", userId: fromUserId },
			inboundImages,
		});
		// Dedup hit returns a sentinel; don't echo it back to the user
		if (reply.text === DUPLICATE_SENTINEL) return;
		const plain = markdownToPlainText(reply.text).trim();
		if (plain) {
			await sendMessageWeixin({
				to: fromUserId,
				text: plain,
				baseUrl: opts.baseUrl,
				token: opts.token,
				contextToken,
			});
		}
		// Images the agent produced inside a data subproject (already path-checked by the runner),
		// or remote https URLs it referenced: upload to the WeChat CDN, then send one message each.
		for (const img of reply.images) {
			const localPath = /^https:\/\//i.test(img) ? await downloadRemoteImageToTemp(img) : img;
			const uploaded = await uploadImageToWeixin({
				filePath: localPath,
				toUserId: fromUserId,
				opts: { baseUrl: opts.baseUrl, token: opts.token },
			});
			await sendImageMessageWeixin({
				to: fromUserId,
				uploaded,
				baseUrl: opts.baseUrl,
				token: opts.token,
				contextToken,
			});
		}
	} catch (e) {
		console.error("[wechat] processing failed:", e instanceof Error ? e.message : String(e));
		if (contextToken) {
			await sendMessageWeixin({
				to: fromUserId,
				text: "⚠️ Something went wrong, please try again later.",
				baseUrl: opts.baseUrl,
				token: opts.token,
				contextToken,
			}).catch(() => {});
		}
	}
}

function sleep(ms: number): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Long-poll loop: getUpdates → process messages → dispatch → send replies.
 * Runs forever; terminated by process exit (matches telegram/feishu which don't
 * handle SIGINT either).
 */
export async function runWechatMonitor(): Promise<void> {
	const { baseUrl, token, userId: ownerUserId, configured } = resolveAccount();
	if (!configured) {
		throw new Error("WeChat account not logged in; please run: npm start -- wechat-login");
	}

	let getUpdatesBuf = loadSyncBuf() ?? "";
	let nextTimeoutMs = DEFAULT_LONG_POLL_TIMEOUT_MS;
	let consecutiveFailures = 0;

	console.log(`[wechat] monitor started (${baseUrl})`);

	while (true) {
		try {
			const resp = await getUpdates({
				baseUrl,
				token,
				get_updates_buf: getUpdatesBuf,
				timeoutMs: nextTimeoutMs,
			});

			if (resp.longpolling_timeout_ms != null && resp.longpolling_timeout_ms > 0) {
				nextTimeoutMs = resp.longpolling_timeout_ms;
			}

			const isApiError =
				(resp.ret !== undefined && resp.ret !== 0) ||
				(resp.errcode !== undefined && resp.errcode !== 0);

			if (isApiError) {
				const isSessionExpired =
					resp.errcode === SESSION_EXPIRED_ERRCODE || resp.ret === SESSION_EXPIRED_ERRCODE;
				if (isSessionExpired) {
					console.error(
						`[wechat] session expired (errcode ${SESSION_EXPIRED_ERRCODE}), please re-run: npm start -- wechat-login`,
					);
					setChannelDown("wechat", "session expired");
					consecutiveFailures = 0;
					await sleep(60_000); // re-log once a minute, don't hammer the API
					continue;
				}
				consecutiveFailures += 1;
				console.error(
					`[wechat] getUpdates failed: ret=${resp.ret} errcode=${resp.errcode} errmsg=${resp.errmsg ?? ""} (${consecutiveFailures}/${MAX_CONSECUTIVE_FAILURES})`,
				);
				if (consecutiveFailures >= MAX_CONSECUTIVE_FAILURES) {
					consecutiveFailures = 0;
					await sleep(BACKOFF_DELAY_MS);
				} else {
					await sleep(RETRY_DELAY_MS);
				}
				continue;
			}

			consecutiveFailures = 0;
			// A clean getUpdates round means the long poll is healthy again — recover from any prior down.
			setChannelUp("wechat");

			if (resp.get_updates_buf != null && resp.get_updates_buf !== "") {
				saveSyncBuf(resp.get_updates_buf);
				getUpdatesBuf = resp.get_updates_buf;
			}

			for (const full of resp.msgs ?? []) {
				await processOneMessage(full, { baseUrl, token, ownerUserId });
			}
		} catch (err) {
			consecutiveFailures += 1;
			console.error(
				`[wechat] getUpdates error (${consecutiveFailures}/${MAX_CONSECUTIVE_FAILURES}): ${String(err)}`,
			);
			if (consecutiveFailures >= MAX_CONSECUTIVE_FAILURES) {
				consecutiveFailures = 0;
				await sleep(BACKOFF_DELAY_MS);
			} else {
				await sleep(RETRY_DELAY_MS);
			}
		}
	}
}
