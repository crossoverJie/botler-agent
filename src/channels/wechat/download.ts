import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { decryptAesEcb, parseInboundAesKey } from "./aes-ecb.ts";
import { CDN_BASE_URL } from "./upload.ts";
import { MessageItemType, type MessageItem } from "./types.ts";
import { safePath } from "../../tools/paths.ts"; // security boundary for the persisted copy
import { today } from "../../prompts/system-prompt.ts"; // local-tz date, matches the agent's __TODAY__

/**
 * Inbound image pipeline for the WeChat channel.
 *
 * Outbound images are uploaded via upload.ts; this is the mirror half: an IMAGE message item
 * carries a CDN download token (`encrypt_query_param` / `full_url`) + an AES-128 key; we fetch
 * the ciphertext, decrypt it with AES-128-ECB, and hand the decoded bytes to the caller so it
 * can both feed them inline to the model for vision recognition and persist the original under
 * the target data subproject.
 */

/** Decoded inbound image, held in memory until the target subproject is known. */
export interface InboundImage {
	buffer: Buffer;
	mimeType: string;
	ext: string; // "jpg" | "png" | "gif" | "webp"
}

const CDN_DOWNLOAD_TIMEOUT_MS = 15_000;
const CDN_DOWNLOAD_MAX_RETRIES = 2; // initial attempt + 1 retry

/**
 * GET the ciphertext with an AbortSignal timeout; retry once on network/timeout errors.
 * Without a timeout a hung CDN connection would block the long-poll loop (monitor.ts).
 */
async function downloadWithRetry(url: string): Promise<Buffer> {
	let lastErr: unknown;
	for (let attempt = 1; attempt <= CDN_DOWNLOAD_MAX_RETRIES; attempt++) {
		try {
			const res = await fetch(url, { signal: AbortSignal.timeout(CDN_DOWNLOAD_TIMEOUT_MS) });
			if (!res.ok) throw new Error(`CDN download client error ${res.status}`);
			return Buffer.from(await res.arrayBuffer());
		} catch (err) {
			lastErr = err;
			// 4xx is not transient; abort immediately (mirrors postToCdn's client-error handling).
			if (err instanceof Error && /client error/.test(err.message)) throw err;
			console.warn(`[wechat] CDN download attempt ${attempt}/${CDN_DOWNLOAD_MAX_RETRIES} failed: ${String(err)}`);
		}
	}
	throw lastErr instanceof Error ? lastErr : new Error("CDN download failed");
}

/** Sniff the image format from magic bytes; defaults to JPEG when unrecognized. */
export function sniffMime(buf: Buffer): { mimeType: string; ext: string } {
	if (buf.length >= 8 && buf.subarray(0, 8).equals(Buffer.from("89504e470d0a1a0a", "hex"))) {
		return { mimeType: "image/png", ext: "png" };
	}
	if (buf.length >= 3 && buf.subarray(0, 3).equals(Buffer.from("ffd8ff", "hex"))) {
		return { mimeType: "image/jpeg", ext: "jpg" };
	}
	if (buf.length >= 4 && buf.subarray(0, 4).equals(Buffer.from("47494638", "hex"))) {
		return { mimeType: "image/gif", ext: "gif" };
	}
	if (
		buf.length >= 12 &&
		buf.subarray(0, 4).toString("ascii") === "RIFF" &&
		buf.subarray(8, 12).toString("ascii") === "WEBP"
	) {
		return { mimeType: "image/webp", ext: "webp" };
	}
	return { mimeType: "image/jpeg", ext: "jpg" };
}

/** Download + decrypt one inbound IMAGE item; throws when the item is not an image or cannot be decoded. */
export async function downloadInboundImage(item: MessageItem): Promise<InboundImage> {
	if (item.type !== MessageItemType.IMAGE) throw new Error("not an image item");
	const img = item.image_item;
	const media = img?.media;
	if (!media?.encrypt_query_param && !media?.full_url) {
		// Include the available media/image keys so a decode failure on a real inbound message
		// is debuggable from the monitor log (the protocol shape is not always exactly as expected).
		const mediaKeys = Object.keys(media ?? {}).join(",") || "(none)";
		const imgKeys = Object.keys(img ?? {}).join(",") || "(none)";
		throw new Error(
			`image item has no encrypt_query_param / full_url (image_item keys: ${imgKeys}; media keys: ${mediaKeys})`,
		);
	}
	const key = parseInboundAesKey(img?.aeskey, media?.aes_key);
	const url = media.full_url
		? media.full_url
		: `${CDN_BASE_URL}/download?encrypted_query_param=${encodeURIComponent(media.encrypt_query_param!)}`;

	const cipher = await downloadWithRetry(url);
	const plain = decryptAesEcb(cipher, key);
	const { mimeType, ext } = sniffMime(plain);
	return { buffer: plain, mimeType, ext };
}

const PHOTOS_SUBDIR = "photos";
const ALLOWED_EXT = new Set(["jpg", "png", "gif", "webp"]);

/**
 * Persist a decoded image under DATA_ROOT/<project>/photos/<date>-<rand>.<ext>.
 * Validated through safePath (first-level subdir allowlist) + an image-extension whitelist,
 * so the file can never escape DATA_ROOT or be written with a non-image extension.
 * Returns the RELATIVE path (e.g. cook/photos/2026-08-23-ab12cd.jpg) for the agent to cite.
 */
export async function persistInboundImage(img: InboundImage, project: string): Promise<string> {
	const ext = ALLOWED_EXT.has(img.ext) ? img.ext : "jpg";
	// Local-timezone date (same as the agent's __TODAY__), so the file name matches the
	// date the agent writes into its records even for UTC+8 users sending near midnight.
	const name = `${today()}-${crypto.randomBytes(4).toString("hex")}.${ext}`;
	const rel = path.join(project, PHOTOS_SUBDIR, name);
	const abs = safePath(rel); // throws if outside the allowlisted first-level subdir
	await fs.mkdir(path.dirname(abs), { recursive: true });
	await fs.writeFile(abs, img.buffer);
	return rel;
}
