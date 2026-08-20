import { test } from "node:test";
import assert from "node:assert/strict";
import { normalizeEntry, normalizeSchedules } from "./store.ts";

const MESSAGE_MAX_CHARS = 10 * 1024;

const base = { id: "t1", enabled: true, timezone: "Asia/Shanghai", message: "m", at: "08:00" };

// ---------------------------------------------------------------------------
// Basic normalization
// ---------------------------------------------------------------------------

test("normalizeEntry accepts a valid at entry and preserves fields", () => {
	const e = normalizeEntry(base);
	assert.equal(e.id, "t1");
	assert.equal(e.message, "m");
	assert.equal(e.at, "08:00");
	assert.equal(e.timezone, "Asia/Shanghai");
	assert.equal(e.enabled, true);
});

test("normalizeEntry defaults enabled=true and timezone", () => {
	const e = normalizeEntry({ id: "x", message: "m", cron: "0 8 * * *" });
	assert.equal(e.enabled, true);
	assert.equal(e.timezone, "Asia/Shanghai");
});

test("normalizeEntry accepts cron and interval triggers", () => {
	const cron = normalizeEntry({ id: "a", message: "m", cron: "0 9 * * *" });
	assert.equal(cron.cron, "0 9 * * *");
	const iv = normalizeEntry({ id: "b", message: "m", interval: "2h" });
	assert.equal(iv.interval, "2h");
});

test("normalizeEntry rejects an entry with no trigger", () => {
	assert.throws(() => normalizeEntry({ id: "x", message: "m" }), /needs one of cron\/interval\/at/);
});

test("normalizeEntry rejects invalid timezone", () => {
	assert.throws(
		() => normalizeEntry({ id: "x", message: "m", at: "08:00", timezone: "Mars/Olympus" }),
		/invalid timezone/,
	);
});

test("normalizeEntry rejects an out-of-range interval", () => {
	assert.throws(() => normalizeEntry({ id: "x", message: "m", interval: "90m" }), /invalid interval/);
});

// ---------------------------------------------------------------------------
// message presence + 10KB cap
// ---------------------------------------------------------------------------

test("normalizeEntry rejects a missing message", () => {
	assert.throws(() => normalizeEntry({ id: "x", at: "08:00" }), /missing message/);
});

test("normalizeEntry rejects a blank message", () => {
	assert.throws(() => normalizeEntry({ id: "x", message: "   ", at: "08:00" }), /missing message/);
});

test("normalizeEntry rejects a message over 10KB", () => {
	assert.throws(
		() => normalizeEntry({ id: "x", message: "x".repeat(MESSAGE_MAX_CHARS + 1), at: "08:00" }),
		/message exceeds/,
	);
});

test("normalizeEntry accepts a message of exactly 10KB", () => {
	const e = normalizeEntry({ id: "x", message: "x".repeat(MESSAGE_MAX_CHARS), at: "08:00" });
	assert.equal(e.message.length, MESSAGE_MAX_CHARS);
});

// ---------------------------------------------------------------------------
// recipient validation
// ---------------------------------------------------------------------------

test("normalizeEntry preserves a valid recipient", () => {
	const e = normalizeEntry({ ...base, recipient: { source: "wechat", userId: "owner-1" } });
	assert.deepEqual(e.recipient, { source: "wechat", userId: "owner-1" });
});

test("normalizeEntry accepts telegram and feishu recipients", () => {
	for (const source of ["telegram", "feishu"]) {
		const e = normalizeEntry({ ...base, recipient: { source, userId: "123" } });
		assert.equal(e.recipient?.source, source);
		assert.equal(e.recipient?.userId, "123");
	}
});

test("normalizeEntry rejects an unknown recipient.source", () => {
	assert.throws(
		() => normalizeEntry({ ...base, recipient: { source: "sms", userId: "1" } }),
		/invalid recipient\.source/,
	);
});

test("normalizeEntry rejects an empty recipient.userId", () => {
	assert.throws(
		() => normalizeEntry({ ...base, recipient: { source: "wechat", userId: "" } }),
		/invalid recipient\.userId/,
	);
});

test("normalizeEntry rejects a missing recipient.userId", () => {
	assert.throws(
		() => normalizeEntry({ ...base, recipient: { source: "wechat" } }),
		/invalid recipient\.userId/,
	);
});

// ---------------------------------------------------------------------------
// normalizeSchedules (all-or-nothing list validation)
// ---------------------------------------------------------------------------

test("normalizeSchedules passes a valid list", () => {
	const out = normalizeSchedules([base, { ...base, id: "t2", at: "09:00" }]);
	assert.equal(out.length, 2);
});

test("normalizeSchedules rejects duplicate ids", () => {
	assert.throws(() => normalizeSchedules([base, { ...base }]), /duplicate schedule id "t1"/);
});

test("normalizeSchedules rejects a list containing a bad entry", () => {
	assert.throws(
		() => normalizeSchedules([base, { id: "t2", message: "m" }]), // t2 has no trigger
		/needs one of cron\/interval\/at/,
	);
});
