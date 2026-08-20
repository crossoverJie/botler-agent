/** Common request metadata attached to every CGI request. */
export interface BaseInfo {
	channel_version?: string;
}

/** proto: MessageItemType */
export const MessageItemType = {
	NONE: 0,
	TEXT: 1,
	IMAGE: 2,
	VOICE: 3,
	FILE: 4,
	VIDEO: 5,
} as const;

/** proto: MessageType */
export const MessageType = {
	NONE: 0,
	USER: 1,
	BOT: 2,
} as const;

/** proto: MessageState */
export const MessageState = {
	NEW: 0,
	GENERATING: 1,
	FINISH: 2,
} as const;

export interface TextItem {
	text?: string;
}

/** Voice-to-text content (VOICE item carries an optional transcribed text). */
export interface VoiceItem {
	text?: string;
}

/** A media reference on the WeChat CDN (proto: CDNMedia). */
export interface CDNMedia {
	encrypt_query_param?: string;
	aes_key?: string;
	encrypt_type?: number;
	full_url?: string;
}

/** Outbound image payload (proto: ImageItem). Only media + mid_size are used (no thumbnail). */
export interface ImageItem {
	media?: CDNMedia;
	thumb_media?: CDNMedia;
	aeskey?: string;
	url?: string;
	mid_size?: number;
	thumb_size?: number;
	hd_size?: number;
}

export interface MessageItem {
	type?: number;
	msg_id?: string;
	text_item?: TextItem;
	voice_item?: VoiceItem;
	image_item?: ImageItem;
	[k: string]: unknown;
}

/** Unified message (proto: WeixinMessage). */
export interface WeixinMessage {
	seq?: number;
	message_id?: number;
	from_user_id?: string;
	to_user_id?: string;
	client_id?: string;
	create_time_ms?: number;
	update_time_ms?: number;
	session_id?: string;
	group_id?: string;
	message_type?: number;
	message_state?: number;
	item_list?: MessageItem[];
	context_token?: string;
	[k: string]: unknown;
}

export interface GetUpdatesReq {
	get_updates_buf?: string;
}

export interface GetUpdatesResp {
	ret?: number;
	errcode?: number;
	errmsg?: string;
	msgs?: WeixinMessage[];
	get_updates_buf?: string;
	longpolling_timeout_ms?: number;
}

export interface SendMessageReq {
	msg?: WeixinMessage;
}

/** proto: UploadMediaType */
export const UploadMediaType = {
	IMAGE: 1,
	VIDEO: 2,
	FILE: 3,
	VOICE: 4,
} as const;

export interface GetUploadUrlReq {
	filekey?: string;
	media_type?: number;
	to_user_id?: string;
	/** Plaintext file size in bytes */
	rawsize?: number;
	/** Plaintext file MD5, hex-encoded */
	rawfilemd5?: string;
	/** Ciphertext file size in bytes (AES-128-ECB with PKCS7 padding) */
	filesize?: number;
	no_need_thumb?: boolean;
	/** AES-128 key, hex-encoded */
	aeskey?: string;
}

export interface GetUploadUrlResp {
	upload_param?: string;
	upload_full_url?: string;
}
