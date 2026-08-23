import { test } from "node:test";
import assert from "node:assert/strict";
import { decryptAesEcb, encryptAesEcb, parseInboundAesKey } from "./aes-ecb.ts";

const KEY16 = Buffer.from("0123456789abcdef0123456789abcdef", "hex"); // 16 raw bytes
const HEX_STR = KEY16.toString("hex"); // 32-char hex string

test("decryptAesEcb round-trips encryptAesEcb", () => {
	const plain = Buffer.from("hello wechat image payload", "utf8");
	const cipher = encryptAesEcb(plain, KEY16);
	assert.deepEqual(decryptAesEcb(cipher, KEY16), plain);
});

test("parseInboundAesKey accepts a 32-char hex key (image_item.aeskey)", () => {
	assert.deepEqual(parseInboundAesKey(HEX_STR), KEY16);
});

test("parseInboundAesKey accepts base64 of the raw 16 bytes (media.aes_key)", () => {
	assert.deepEqual(parseInboundAesKey(undefined, KEY16.toString("base64")), KEY16);
});

test("parseInboundAesKey accepts base64 of the 32-char hex string", () => {
	const base64OfHex = Buffer.from(HEX_STR, "ascii").toString("base64");
	assert.deepEqual(parseInboundAesKey(undefined, base64OfHex), KEY16);
});

test("parseInboundAesKey prefers the hex key over base64 when both are present", () => {
	assert.deepEqual(parseInboundAesKey(HEX_STR, KEY16.toString("base64")), KEY16);
});

test("parseInboundAesKey rejects a non-hex aeskey", () => {
	assert.throws(() => parseInboundAesKey("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"));
});

test("parseInboundAesKey rejects a short aeskey", () => {
	assert.throws(() => parseInboundAesKey("abcd"));
});

test("parseInboundAesKey rejects a base64 key that decodes to the wrong length", () => {
	// base64 of "hello" decodes to 5 bytes, neither 16 raw nor a 32-char hex string.
	assert.throws(() => parseInboundAesKey(undefined, Buffer.from("hello", "ascii").toString("base64")));
});

test("parseInboundAesKey rejects a base64 key that decodes to 32 non-hex bytes", () => {
	const nonHex32 = Buffer.from("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "ascii").toString("base64");
	assert.throws(() => parseInboundAesKey(undefined, nonHex32));
});

test("parseInboundAesKey throws when no key is given", () => {
	assert.throws(() => parseInboundAesKey());
});
