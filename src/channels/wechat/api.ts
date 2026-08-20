import crypto from "node:crypto";
import type {
	BaseInfo,
	GetUpdatesReq,
	GetUpdatesResp,
	GetUploadUrlReq,
	GetUploadUrlResp,
	SendMessageReq,
} from "./types.ts";

export type WeixinApiOptions = {
	baseUrl: string;
	token?: string;
	timeoutMs?: number;
};

const CHANNEL_VERSION = "0.1.0";

/** Build the `base_info` payload included in every API request. */
export function buildBaseInfo(): BaseInfo {
	return { channel_version: CHANNEL_VERSION };
}

const DEFAULT_LONG_POLL_TIMEOUT_MS = 35_000;
const DEFAULT_API_TIMEOUT_MS = 15_000;

function ensureTrailingSlash(url: string): string {
	return url.endsWith("/") ? url : `${url}/`;
}

/** X-WECHAT-UIN header: random uint32 -> decimal string -> base64. */
function randomWechatUin(): string {
	const uint32 = crypto.randomBytes(4).readUInt32BE(0);
	return Buffer.from(String(uint32), "utf-8").toString("base64");
}

function buildHeaders(opts: { token?: string; body: string }): Record<string, string> {
	const headers: Record<string, string> = {
		"Content-Type": "application/json",
		AuthorizationType: "ilink_bot_token",
		"Content-Length": String(Buffer.byteLength(opts.body, "utf-8")),
		"X-WECHAT-UIN": randomWechatUin(),
	};
	if (opts.token?.trim()) {
		headers.Authorization = `Bearer ${opts.token.trim()}`;
	}
	return headers;
}

/** GET fetch wrapper (QR login endpoints use GET). Returns raw text; throws on HTTP error/timeout. */
export async function apiGetFetch(params: {
	baseUrl: string;
	endpoint: string;
	timeoutMs: number;
	label: string;
}): Promise<string> {
	const base = ensureTrailingSlash(params.baseUrl);
	const url = new URL(params.endpoint, base);
	const controller = new AbortController();
	const t = setTimeout(() => controller.abort(), params.timeoutMs);
	try {
		const res = await fetch(url.toString(), { method: "GET", signal: controller.signal });
		clearTimeout(t);
		const rawText = await res.text();
		if (!res.ok) {
			throw new Error(`${params.label} ${res.status}: ${rawText}`);
		}
		return rawText;
	} catch (err) {
		clearTimeout(t);
		throw err;
	}
}

/** POST JSON fetch wrapper. Returns raw text; throws on HTTP error/timeout. */
async function apiPostFetch(params: {
	baseUrl: string;
	endpoint: string;
	body: string;
	token?: string;
	timeoutMs: number;
	label: string;
	abortSignal?: AbortSignal;
}): Promise<string> {
	const base = ensureTrailingSlash(params.baseUrl);
	const url = new URL(params.endpoint, base);
	const hdrs = buildHeaders({ token: params.token, body: params.body });
	const controller = new AbortController();
	const t = setTimeout(() => controller.abort(), params.timeoutMs);
	const onAbort = () => controller.abort();
	params.abortSignal?.addEventListener("abort", onAbort, { once: true });
	try {
		const res = await fetch(url.toString(), {
			method: "POST",
			headers: hdrs,
			body: params.body,
			signal: controller.signal,
		});
		clearTimeout(t);
		const rawText = await res.text();
		if (!res.ok) {
			throw new Error(`${params.label} ${res.status}: ${rawText}`);
		}
		return rawText;
	} catch (err) {
		clearTimeout(t);
		throw err;
	} finally {
		params.abortSignal?.removeEventListener("abort", onAbort);
	}
}

/**
 * Long-poll getUpdates. Server holds the request up to timeoutMs; on client-side
 * timeout returns an empty response so the caller can simply retry.
 */
export async function getUpdates(
	params: GetUpdatesReq & {
		baseUrl: string;
		token?: string;
		timeoutMs?: number;
		abortSignal?: AbortSignal;
	},
): Promise<GetUpdatesResp> {
	const timeout = params.timeoutMs ?? DEFAULT_LONG_POLL_TIMEOUT_MS;
	try {
		const rawText = await apiPostFetch({
			baseUrl: params.baseUrl,
			endpoint: "ilink/bot/getupdates",
			body: JSON.stringify({
				get_updates_buf: params.get_updates_buf ?? "",
				base_info: buildBaseInfo(),
			}),
			token: params.token,
			timeoutMs: timeout,
			label: "getUpdates",
			abortSignal: params.abortSignal,
		});
		return JSON.parse(rawText) as GetUpdatesResp;
	} catch (err) {
		if (err instanceof Error && err.name === "AbortError") {
			return { ret: 0, msgs: [], get_updates_buf: params.get_updates_buf };
		}
		throw err;
	}
}

/**
 * Request a CDN upload URL for a media file.
 * Sizes/MD5 describe the plaintext; `filesize` is the AES-128-ECB ciphertext size.
 */
export async function getUploadUrl(
	params: GetUploadUrlReq & WeixinApiOptions,
): Promise<GetUploadUrlResp> {
	const rawText = await apiPostFetch({
		baseUrl: params.baseUrl,
		endpoint: "ilink/bot/getuploadurl",
		body: JSON.stringify({
			filekey: params.filekey,
			media_type: params.media_type,
			to_user_id: params.to_user_id,
			rawsize: params.rawsize,
			rawfilemd5: params.rawfilemd5,
			filesize: params.filesize,
			no_need_thumb: params.no_need_thumb,
			aeskey: params.aeskey,
			base_info: buildBaseInfo(),
		}),
		token: params.token,
		timeoutMs: params.timeoutMs ?? DEFAULT_API_TIMEOUT_MS,
		label: "getUploadUrl",
	});
	return JSON.parse(rawText) as GetUploadUrlResp;
}

/** Send a single message downstream. */
export async function sendMessage(params: WeixinApiOptions & { body: SendMessageReq }): Promise<void> {
	await apiPostFetch({
		baseUrl: params.baseUrl,
		endpoint: "ilink/bot/sendmessage",
		body: JSON.stringify({ ...params.body, base_info: buildBaseInfo() }),
		token: params.token,
		timeoutMs: params.timeoutMs ?? DEFAULT_API_TIMEOUT_MS,
		label: "sendMessage",
	});
}
