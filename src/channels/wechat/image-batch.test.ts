import { test } from "node:test";
import assert from "node:assert/strict";
import { ImageBatchCoordinator } from "./image-batch.ts";

// Structurally matches InboundImage ({ buffer, mimeType, ext }) without importing download.ts,
// which would pull in the DATA_ROOT-dependent safePath at module load.
function img(seed: string, ext = "jpg"): { buffer: Buffer; mimeType: string; ext: string } {
	return { buffer: Buffer.from(`fake-image-${seed}`, "utf8"), mimeType: "image/jpeg", ext };
}

const WINDOW = 60_000; // seconds → ms used by the coordinator

test("image message is buffered, not dispatched", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	const action = c.onMessage("alice", { text: "", images: [img("a")] });
	assert.equal(action.kind, "buffer");
	assert.ok(c.hasPending("alice"));
	assert.equal(c.pendingCount(), 1);
});

test("a second image extends the same batch", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	c.onMessage("alice", { text: "", images: [img("a")] });
	c.onMessage("alice", { text: "", images: [img("b")] });
	assert.ok(c.hasPending("alice"));
	assert.equal(c.pendingImageCount("alice"), 2);
	const flushed = c.flush("alice")!;
	assert.equal(flushed.images.length, 2);
});

test("text joins a pending image batch and dispatches as one task", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	c.onMessage("alice", { text: "", images: [img("a")] });
	const action = c.onMessage("alice", { text: "记一下这顿", images: [], contextToken: "tok2" });
	assert.equal(action.kind, "dispatch");
	const d = action.kind === "dispatch" ? action.dispatch : undefined!;
	assert.equal(d.text, "记一下这顿");
	assert.equal(d.images.length, 1);
	assert.equal(d.contextToken, "tok2");
	// Batch consumed by the join.
	assert.ok(!c.hasPending("alice"));
	assert.equal(c.pendingCount(), 0);
});

test("flush without caption dispatches the [图片] placeholder with the image", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	c.onMessage("alice", { text: "", images: [img("a")], contextToken: "tok1" });
	const d = c.flush("alice")!;
	assert.equal(d.text, "[图片]");
	assert.equal(d.images.length, 1);
	assert.equal(d.contextToken, "tok1");
	assert.ok(!c.hasPending("alice"));
});

test("standalone text dispatches immediately without buffering", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	const action = c.onMessage("alice", { text: "查一下英语", images: [] });
	assert.equal(action.kind, "dispatch");
	const d = action.kind === "dispatch" ? action.dispatch : undefined!;
	assert.equal(d.text, "查一下英语");
	assert.equal(d.images.length, 0);
	assert.ok(!c.hasPending("alice"));
});

test("flush of a sender with no pending batch returns undefined", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	assert.equal(c.flush("nobody"), undefined);
});

test("window of 0 disables batching: images dispatch immediately", () => {
	const c = new ImageBatchCoordinator(0);
	const action = c.onMessage("alice", { text: "", images: [img("a")] });
	assert.equal(action.kind, "dispatch");
	const d = action.kind === "dispatch" ? action.dispatch : undefined!;
	assert.equal(d.text, "[图片]");
	assert.equal(d.images.length, 1);
	assert.ok(!c.hasPending("alice"));
});

test("batches for different senders are independent", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	c.onMessage("alice", { text: "", images: [img("a")] });
	c.onMessage("bob", { text: "", images: [img("b")] });
	assert.equal(c.pendingCount(), 2);
	// Text for bob flushes only bob's batch.
	const action = c.onMessage("bob", { text: "caption", images: [] });
	const d = action.kind === "dispatch" ? action.dispatch : undefined!;
	assert.equal(d.images.length, 1);
	assert.ok(c.hasPending("alice"));
	assert.ok(!c.hasPending("bob"));
});

test("batch id is deterministic and content-based", () => {
	const c = new ImageBatchCoordinator(WINDOW);
	c.onMessage("alice", { text: "", images: [img("a")] });
	const d1 = c.flush("alice")!;
	const c2 = new ImageBatchCoordinator(WINDOW);
	c2.onMessage("alice", { text: "", images: [img("a")] });
	const d2 = c2.flush("alice")!;
	assert.equal(d1.id, d2.id);
	assert.match(d1.id, /^alice:batch:/);
});

test("rejects a negative window", () => {
	assert.throws(() => new ImageBatchCoordinator(-1));
});
