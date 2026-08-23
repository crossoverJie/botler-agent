import crypto from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { getUploadUrl, type WeixinApiOptions } from "./api.ts";
import { aesEcbPaddedSize, encryptAesEcb } from "./aes-ecb.ts";
import { UploadMediaType } from "./types.ts";

/**
 * Upload outbound images to the WeChat media CDN.
 *
 * Pipeline: read file → plaintext md5/size → random filekey + AES-128 key → getUploadUrl
 * → AES-128-ECB encrypt → POST ciphertext to the CDN → read the download param from the
 * `x-encrypted-param` response header. That param + the AES key identify the image when
 * it is referenced in an outbound IMAGE message item.
 */

/**
 * WeChat media CDN root, shared by the outbound upload path and the inbound download path.
 * Not configurable: there is no known reason to point elsewhere.
 */
export const CDN_BASE_URL = "https://novac2c.cdn.weixin.qq.com/c2c";

/** Temp dir for remote images downloaded before upload. */
const MEDIA_TEMP_DIR = path.join(os.tmpdir(), "botler-agent", "wechat");

const UPLOAD_MAX_RETRIES = 3;

export type UploadedFileInfo = {
	filekey: string;
	/** CDN download param; goes into ImageItem.media.encrypt_query_param */
	downloadEncryptedQueryParam: string;
	/** AES-128 key, hex-encoded; see sendImageMessageWeixin for how it is encoded on the wire */
	aeskey: string;
	/** Plaintext size in bytes */
	fileSize: number;
	/** Ciphertext size in bytes (AES-128-ECB + PKCS7); goes into ImageItem.mid_size */
	fileSizeCiphertext: number;
};

/**
 * Encrypt and POST one buffer to the CDN; returns the download param.
 * Retries on server/network errors; 4xx aborts immediately (the upload URL is not going to fix itself).
 */
async function postToCdn(params: {
	buf: Buffer;
	uploadFullUrl?: string;
	uploadParam?: string;
	filekey: string;
	aeskey: Buffer;
}): Promise<string> {
	const { buf, uploadFullUrl, uploadParam, filekey, aeskey } = params;
	const ciphertext = encryptAesEcb(buf, aeskey);

	let cdnUrl: string;
	if (uploadFullUrl?.trim()) {
		cdnUrl = uploadFullUrl.trim();
	} else if (uploadParam) {
		cdnUrl = `${CDN_BASE_URL}/upload?encrypted_query_param=${encodeURIComponent(uploadParam)}&filekey=${encodeURIComponent(filekey)}`;
	} else {
		throw new Error("CDN upload URL missing (need upload_full_url or upload_param)");
	}

	let lastError: unknown;
	for (let attempt = 1; attempt <= UPLOAD_MAX_RETRIES; attempt++) {
		try {
			const res = await fetch(cdnUrl, {
				method: "POST",
				headers: { "Content-Type": "application/octet-stream" },
				body: new Uint8Array(ciphertext),
			});
			if (res.status >= 400 && res.status < 500) {
				throw new Error(
					`CDN upload client error ${res.status}: ${res.headers.get("x-error-message") ?? ""}`,
				);
			}
			if (res.status !== 200) {
				throw new Error(
					`CDN upload server error ${res.status}: ${res.headers.get("x-error-message") ?? ""}`,
				);
			}
			const downloadParam = res.headers.get("x-encrypted-param");
			if (!downloadParam) {
				throw new Error("CDN upload response missing x-encrypted-param header");
			}
			return downloadParam;
		} catch (err) {
			lastError = err;
			if (err instanceof Error && err.message.includes("client error")) throw err;
			console.error(`[wechat] CDN upload attempt ${attempt}/${UPLOAD_MAX_RETRIES} failed: ${String(err)}`);
		}
	}
	throw lastError instanceof Error
		? lastError
		: new Error(`CDN upload failed after ${UPLOAD_MAX_RETRIES} attempts`);
}

/** Upload a local image file to the WeChat CDN. */
export async function uploadImageToWeixin(params: {
	filePath: string;
	toUserId: string;
	opts: WeixinApiOptions;
}): Promise<UploadedFileInfo> {
	const { filePath, toUserId, opts } = params;

	const plaintext = await fs.readFile(filePath);
	const rawsize = plaintext.length;
	const rawfilemd5 = crypto.createHash("md5").update(plaintext).digest("hex");
	const filesize = aesEcbPaddedSize(rawsize);
	const filekey = crypto.randomBytes(16).toString("hex");
	const aeskey = crypto.randomBytes(16);

	const resp = await getUploadUrl({
		...opts,
		filekey,
		media_type: UploadMediaType.IMAGE,
		to_user_id: toUserId,
		rawsize,
		rawfilemd5,
		filesize,
		no_need_thumb: true,
		aeskey: aeskey.toString("hex"),
	});

	const downloadEncryptedQueryParam = await postToCdn({
		buf: plaintext,
		uploadFullUrl: resp.upload_full_url || undefined,
		uploadParam: resp.upload_param ?? undefined,
		filekey,
		aeskey,
	});

	console.log(`[wechat] image uploaded: ${filePath} (${rawsize} bytes, filekey=${filekey})`);

	return {
		filekey,
		downloadEncryptedQueryParam,
		aeskey: aeskey.toString("hex"),
		fileSize: rawsize,
		fileSizeCiphertext: filesize,
	};
}

/** Download a remote image URL to a local temp file, so it can go through the same upload pipeline. */
export async function downloadRemoteImageToTemp(url: string): Promise<string> {
	const res = await fetch(url);
	if (!res.ok) {
		throw new Error(`remote image download failed: ${res.status} ${res.statusText}`);
	}
	const buf = Buffer.from(await res.arrayBuffer());
	await fs.mkdir(MEDIA_TEMP_DIR, { recursive: true });
	// The extension is cosmetic: the upload protocol carries bytes + md5, never a file name.
	const ext = path.extname(new URL(url).pathname).slice(0, 5);
	const name = `${Date.now()}-${crypto.randomBytes(4).toString("hex")}${ext}`;
	const filePath = path.join(MEDIA_TEMP_DIR, name);
	await fs.writeFile(filePath, buf);
	return filePath;
}
