/**
 * Shared push types: who to push to (recipient) and what to push (payload).
 * A recipient's `source` is the channel and `userId` is the address that channel
 * understands (Telegram chatId, Feishu chat_id, WeChat ilink_user_id).
 */

export type ChannelSource = "wechat" | "telegram" | "feishu";

export interface Recipient {
	source: ChannelSource;
	userId: string;
}

export interface PushPayload {
	text: string;
	images: string[];
}
