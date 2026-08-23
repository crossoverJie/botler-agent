import { createCipheriv, createDecipheriv } from "node:crypto";

/**
 * AES-128-ECB helpers for CDN media.
 * Encryption is used for outbound uploads, decryption for inbound image downloads.
 */

/** Encrypt a buffer with AES-128-ECB (PKCS7 padding is the Node default). */
export function encryptAesEcb(plaintext: Buffer, key: Buffer): Buffer {
	const cipher = createCipheriv("aes-128-ecb", key, null);
	return Buffer.concat([cipher.update(plaintext), cipher.final()]);
}

/** Decrypt a buffer with AES-128-ECB (PKCS7 padding is the Node default). */
export function decryptAesEcb(ciphertext: Buffer, key: Buffer): Buffer {
	const decipher = createDecipheriv("aes-128-ecb", key, null);
	return Buffer.concat([decipher.update(ciphertext), decipher.final()]);
}

/**
 * Resolve an inbound AES key to its 16 raw bytes.
 * Images carry the key as hex (`image_item.aeskey`, preferred) OR base64
 * (`image_item.media.aes_key`). Two base64 encodings are seen in the wild:
 * the raw 16 bytes, or base64(hex-string-of-16-bytes). The parser tolerates
 * either encoding in either field.
 */
export function parseInboundAesKey(hexKey?: string, base64Key?: string): Buffer {
	const key = hexKey || base64Key;
	if (!key) throw new Error("image has no aes key");
	return keyFromString(key, hexKey ? "image_item.aeskey" : "image_item.media.aes_key");
}

/** Resolve a raw 16-byte key from a hex or base64 encoding; throws when neither fits. */
function keyFromString(key: string, field: string): Buffer {
	if (/^[0-9a-fA-F]{32}$/.test(key)) return Buffer.from(key, "hex");
	const decoded = Buffer.from(key, "base64");
	if (decoded.length === 16) return decoded;
	if (decoded.length === 32 && /^[0-9a-fA-F]{32}$/.test(decoded.toString("ascii"))) {
		return Buffer.from(decoded.toString("ascii"), "hex");
	}
	throw new Error(`${field} must be a 32-char hex key or base64 (16 raw bytes / 32-char hex)`);
}

/** Ciphertext size of AES-128-ECB with PKCS7 padding (always pads, so +1 before rounding up). */
export function aesEcbPaddedSize(plaintextSize: number): number {
	return Math.ceil((plaintextSize + 1) / 16) * 16;
}
