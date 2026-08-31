import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { createHash, createDecipheriv } from "node:crypto";
import { CONFIG } from "../config.ts";
import { dispatch } from "../dispatcher.ts";
import { IM_SESSION_KEY } from "../conversation/store.ts";
import { setChannelUp, setChannelDown } from "../monitor/stats.ts";

const FEISHU_API = "https://open.feishu.cn/open-apis";

/** Feishu API timeout: a hung request must never block the scheduler loop (which awaits deliver()). */
const API_TIMEOUT_MS = 15_000;

async function fetchWithTimeout(url: string, init: RequestInit, timeoutMs = API_TIMEOUT_MS): Promise<Response> {
	const controller = new AbortController();
	const t = setTimeout(() => controller.abort(), timeoutMs);
	try {
		return await fetch(url, { ...init, signal: controller.signal });
	} finally {
		clearTimeout(t);
	}
}

/** Feishu tenant_access_token cache. */
let cachedToken: { token: string; expireAt: number } | null = null;

async function getTenantToken(): Promise<string> {
	if (cachedToken && cachedToken.expireAt > Date.now() + 60_000) {
		return cachedToken.token;
	}
	if (!CONFIG.feishuAppId || !CONFIG.feishuAppSecret) {
		throw new Error("Missing FEISHU_APP_ID / FEISHU_APP_SECRET; cannot obtain tenant_access_token");
	}
	const resp = await fetchWithTimeout(`${FEISHU_API}/auth/v3/tenant_access_token/internal`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ app_id: CONFIG.feishuAppId, app_secret: CONFIG.feishuAppSecret }),
	});
	const data = (await resp.json()) as { code: number; tenant_access_token?: string; expire?: number; msg?: string };
	if (data.code !== 0 || !data.tenant_access_token) {
		throw new Error(`Failed to obtain tenant_access_token: ${data.msg ?? data.code}`);
	}
	cachedToken = {
		token: data.tenant_access_token,
		expireAt: Date.now() + (data.expire ?? 7200) * 1000,
	};
	return cachedToken.token;
}

/**
 * Reply with text to a given chat via the Feishu API.
 * Throws on HTTP error or non-zero business code — the push fallback (deliver.ts) depends on
 * this throw semantics, matching the telegram/wechat channels.
 */
export async function replyFeishu(chatId: string, text: string): Promise<void> {
	const token = await getTenantToken();
	const resp = await fetchWithTimeout(`${FEISHU_API}/im/v1/messages?receive_id_type=chat_id`, {
		method: "POST",
		headers: {
			Authorization: `Bearer ${token}`,
			"Content-Type": "application/json",
		},
		body: JSON.stringify({
			receive_id: chatId,
			msg_type: "text",
			content: JSON.stringify({ text }),
		}),
	});
	// Check HTTP status BEFORE parsing: on non-2xx the body may not be JSON at all, and
	// json() would throw a parse error that masks the real HTTP status.
	if (!resp.ok) {
		throw new Error(`Feishu send failed: HTTP ${resp.status}`);
	}
	const data = (await resp.json()) as { code: number; msg?: string };
	if (data.code !== 0) {
		throw new Error(`Feishu send failed: ${data.msg ?? data.code}`);
	}
}

/**
 * Decrypt a Feishu encrypted event body (AES-256-CBC).
 * key = md5(encryptKey) (16 bytes), iv = first 16 bytes of the ciphertext, with PKCS7 padding removed.
 */
function decryptFeishu(encryptKey: string, encryptB64: string): string {
	const buf = Buffer.from(encryptB64, "base64");
	const key = createHash("md5").update(encryptKey).digest();
	const iv = buf.subarray(0, 16);
	const decipher = createDecipheriv("aes-256-cbc", key, iv);
	decipher.setAutoPadding(false);
	const decrypted = Buffer.concat([decipher.update(buf.subarray(16)), decipher.final()]);
	// Strip PKCS7 padding
	const pad = decrypted[decrypted.length - 1];
	return decrypted.subarray(0, decrypted.length - pad).toString("utf8");
}

function sendJson(res: ServerResponse, code: number, body: unknown): void {
	const text = JSON.stringify(body);
	res.writeHead(code, { "Content-Type": "application/json" });
	res.end(text);
}

function readBody(req: IncomingMessage): Promise<string> {
	return new Promise((resolve, reject) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => resolve(body));
		req.on("error", reject);
	});
}

/**
 * Feishu channel: event subscription webhook.
 * - Handle URL verification (url_verification)
 * - Decrypt (if FEISHU_ENCRYPT_KEY is configured) or process plaintext events
 * - On a text message → dispatch → reply via API
 */
export function startFeishu(): void {
	const port = CONFIG.feishuPort;
	const server = createServer(async (req, res) => {
		try {
			const body = await readBody(req);

			// Empty body (Feishu verification sometimes sends a GET with no body) — ignore
			if (!body) {
				sendJson(res, 200, {});
				return;
			}

			let event: any;
			try {
				event = JSON.parse(body);
			} catch {
				sendJson(res, 400, { code: 400, msg: "invalid json" });
				return;
			}

			// 1) URL verification
			if (event.type === "url_verification") {
				console.log("[feishu] URL verification");
				sendJson(res, 200, { challenge: event.challenge });
				return;
			}

			// 2) Encrypted event body
			if (event.encrypt) {
				if (!CONFIG.feishuEncryptKey) {
					console.warn("[feishu] Received encrypted event but FEISHU_ENCRYPT_KEY not configured; ignored");
					sendJson(res, 200, {});
					return;
				}
				const plain = decryptFeishu(CONFIG.feishuEncryptKey, event.encrypt);
				event = JSON.parse(plain);
			}

			// 3) Message event
			const header = event.header;
			if (header?.event_type === "im.message.receive_v1" && event.event?.message) {
				const message = event.event.message;
				const chatId = message.chat_id;
				const messageId = message.message_id;
				const messageType = message.message_type;
				let text = "";
				if (messageType === "text") {
					try {
						text = JSON.parse(message.content).text ?? "";
					} catch {
						text = "";
					}
				}
				if (text) {
					const reply = await dispatch(text, { id: messageId, source: "feishu", sessionKey: IM_SESSION_KEY });
					// Text-only channel: an images-only reply still needs a non-empty body
					const body = reply.text || "(image generated, but the Feishu channel does not support sending images yet)";
					await replyFeishu(chatId, body).catch((e) =>
						console.error("[feishu] Failed to reply:", e instanceof Error ? e.message : e),
					);
				}
				sendJson(res, 200, {});
				return;
			}

			// Other events (not handled here)
			sendJson(res, 200, {});
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			console.error("[feishu] Error processing:", msg);
			sendJson(res, 200, {});
		}
	});

	server.on("error", (e) => {
		// Bind failure (e.g. port in use) means the webhook channel is dead.
		setChannelDown("feishu", e);
	});
	server.listen(port, () => {
		console.log(`[feishu] webhook listening on http://0.0.0.0:${port}/`);
		setChannelUp("feishu");
	});
}
