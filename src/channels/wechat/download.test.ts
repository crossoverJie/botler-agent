import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// download.ts imports safePath, which computes its allowlist from DATA_ROOT at module load and
// throws when the directory is missing — so create a real temp DATA_ROOT (with one subproject)
// and only then import the module dynamically.
let download: typeof import("./download.ts");
let tmpRoot: string;

before(async () => {
	// realpath the temp dir: on macOS os.tmpdir() lives under /var which symlinks to
	// /private/var, and safePath's symlink-escape guard compares against the un-realpath'd
	// allowlist — so DATA_ROOT must be a symlink-free real path for persistence tests.
	tmpRoot = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-download-test-")));
	fs.mkdirSync(path.join(tmpRoot, "cook"));
	process.env.DATA_ROOT = tmpRoot;
	download = await import("./download.ts");
});

test("sniffMime identifies PNG from magic bytes", () => {
	const buf = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
	assert.equal(download.sniffMime(buf).mimeType, "image/png");
	assert.equal(download.sniffMime(buf).ext, "png");
});

test("sniffMime identifies JPEG from magic bytes", () => {
	const buf = Buffer.from([0xff, 0xd8, 0xff, 0xe0]);
	assert.equal(download.sniffMime(buf).mimeType, "image/jpeg");
	assert.equal(download.sniffMime(buf).ext, "jpg");
});

test("sniffMime identifies GIF from magic bytes", () => {
	const buf = Buffer.from([0x47, 0x49, 0x46, 0x38, 0x39, 0x61]);
	assert.equal(download.sniffMime(buf).mimeType, "image/gif");
	assert.equal(download.sniffMime(buf).ext, "gif");
});

test("sniffMime identifies WebP from magic bytes", () => {
	const buf = Buffer.concat([
		Buffer.from("RIFF", "ascii"),
		Buffer.from([0, 0, 0, 0]),
		Buffer.from("WEBP", "ascii"),
	]);
	assert.equal(download.sniffMime(buf).mimeType, "image/webp");
	assert.equal(download.sniffMime(buf).ext, "webp");
});

test("sniffMime defaults unknown bytes to JPEG", () => {
	const buf = Buffer.from([0x00, 0x01, 0x02, 0x03]);
	assert.equal(download.sniffMime(buf).mimeType, "image/jpeg");
	assert.equal(download.sniffMime(buf).ext, "jpg");
});

test("persistInboundImage writes under DATA_ROOT/<project>/photos and returns the relative path", async () => {
	const img = { buffer: Buffer.from("fake-jpeg-bytes", "utf8"), mimeType: "image/jpeg", ext: "jpg" };
	const rel = await download.persistInboundImage(img, "cook");
	assert.match(rel, /^cook\/photos\/\d{4}-\d{2}-\d{2}-[0-9a-f]{8}\.jpg$/);
	const abs = path.join(tmpRoot, rel);
	assert.ok(fs.existsSync(abs), "persisted file should exist");
	assert.deepEqual(fs.readFileSync(abs), img.buffer);
});

test("persistInboundImage rejects a project outside the allowlist", async () => {
	const img = { buffer: Buffer.from("x", "utf8"), mimeType: "image/jpeg", ext: "jpg" };
	await assert.rejects(() => download.persistInboundImage(img, "../evil"));
});
