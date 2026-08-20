import { createCipheriv } from "node:crypto";

/**
 * AES-128-ECB helpers for CDN upload.
 * Only encryption is needed: we upload outbound media, we never download inbound media.
 */

/** Encrypt a buffer with AES-128-ECB (PKCS7 padding is the Node default). */
export function encryptAesEcb(plaintext: Buffer, key: Buffer): Buffer {
	const cipher = createCipheriv("aes-128-ecb", key, null);
	return Buffer.concat([cipher.update(plaintext), cipher.final()]);
}

/** Ciphertext size of AES-128-ECB with PKCS7 padding (always pads, so +1 before rounding up). */
export function aesEcbPaddedSize(plaintextSize: number): number {
	return Math.ceil((plaintextSize + 1) / 16) * 16;
}
