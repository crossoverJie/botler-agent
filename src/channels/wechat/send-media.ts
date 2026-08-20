import crypto from "node:crypto";
import { sendMessage } from "./api.ts";
import {
	MessageItemType,
	MessageState,
	MessageType,
	type MessageItem,
	type SendMessageReq,
} from "./types.ts";
import type { UploadedFileInfo } from "./upload.ts";

/** Options shared by every outbound send: contextToken must echo the inbound message's. */
export type SendOptions = {
	baseUrl: string;
	token?: string;
	contextToken: string | undefined;
};

function generateClientId(): string {
	return `botler-wechat:${Date.now()}-${crypto.randomBytes(4).toString("hex")}`;
}

/** Convert a markdown-formatted model reply to plain text for WeChat delivery. */
export function markdownToPlainText(text: string): string {
	let result = text;
	// Code blocks: strip fences, keep code content
	result = result.replace(/```[^\n]*\n?([\s\S]*?)```/g, (_, code: string) => code.trim());
	// Images: remove entirely
	result = result.replace(/!\[[^\]]*\]\([^)]*\)/g, "");
	// Links: keep display text only
	result = result.replace(/\[([^\]]+)\]\([^)]*\)/g, "$1");
	// Tables: remove separator rows, then strip pipes
	result = result.replace(/^\|[\s:|-]+\|$/gm, "");
	result = result.replace(/^\|(.+)\|$/gm, (_, inner: string) =>
		inner.split("|").map((cell: string) => cell.trim()).join("  "),
	);
	// Strip inline markdown formatting
	result = result
		.replace(/\*\*(.+?)\*\*/g, "$1")
		.replace(/\*(.+?)\*/g, "$1")
		.replace(/__(.+?)__/g, "$1")
		.replace(/_(.+?)_/g, "$1")
		.replace(/~~(.+?)~~/g, "$1")
		.replace(/`(.+?)`/g, "$1");
	return result;
}

/** Build a SendMessageReq carrying exactly one item. */
function buildMessageReq(to: string, item: MessageItem, contextToken?: string): SendMessageReq {
	return {
		msg: {
			from_user_id: "",
			to_user_id: to,
			client_id: generateClientId(),
			message_type: MessageType.BOT,
			message_state: MessageState.FINISH,
			item_list: [item],
			context_token: contextToken ?? undefined,
		},
	};
}

/**
 * Send a plain text message downstream.
 * contextToken is required — it must echo the inbound message's context_token,
 * otherwise WeChat cannot associate the reply with the conversation.
 */
export async function sendMessageWeixin(params: { to: string; text: string } & SendOptions): Promise<void> {
	const { to, text, baseUrl, token, contextToken } = params;
	if (!contextToken) {
		throw new Error("sendMessageWeixin: contextToken is required");
	}
	const item: MessageItem = { type: MessageItemType.TEXT, text_item: { text } };
	await sendMessage({ baseUrl, token, body: buildMessageReq(to, item, contextToken) });
}

/**
 * Send an image message downstream, referencing a previously uploaded CDN file.
 *
 * ⚠️ aes_key on the wire is base64 of the 32-char *hex string*, not base64 of the 16 raw
 * key bytes. That looks like a bug but it is what the protocol expects for image items
 * (files/voice/video use the same encoding; only inbound image decryption uses raw bytes).
 * Do not "fix" this to Buffer.from(hex, "hex") — the receiver would fail to decrypt.
 */
export async function sendImageMessageWeixin(
	params: { to: string; uploaded: UploadedFileInfo } & SendOptions,
): Promise<void> {
	const { to, uploaded, baseUrl, token, contextToken } = params;
	if (!contextToken) {
		throw new Error("sendImageMessageWeixin: contextToken is required");
	}
	const item: MessageItem = {
		type: MessageItemType.IMAGE,
		image_item: {
			media: {
				encrypt_query_param: uploaded.downloadEncryptedQueryParam,
				aes_key: Buffer.from(uploaded.aeskey).toString("base64"),
				encrypt_type: 1,
			},
			mid_size: uploaded.fileSizeCiphertext,
		},
	};
	await sendMessage({ baseUrl, token, body: buildMessageReq(to, item, contextToken) });
}
