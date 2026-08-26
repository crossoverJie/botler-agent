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
	assert.throws(() => normalizeEntry({ id: "x", message: "m" }), /needs one of cron\/interval\/at\/once/);
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
		/needs one of cron\/interval\/at\/once/,
	);
});

// ---------------------------------------------------------------------------
// once (one-shot) trigger
// ---------------------------------------------------------------------------

test("normalizeEntry accepts a valid once entry and preserves it", () => {
	const e = normalizeEntry({ id: "o1", message: "m", once: "2026-08-20T22:00:00+08:00" });
	assert.equal(e.once, "2026-08-20T22:00:00+08:00");
	assert.equal(e.at, undefined);
});

test("normalizeEntry accepts a naive once and applies the entry timezone", () => {
	const e = normalizeEntry({
		id: "o2", message: "m", timezone: "Asia/Shanghai", once: "2026-08-20T22:00",
	});
	assert.equal(e.once, "2026-08-20T22:00");
});

test("normalizeEntry rejects a malformed once value", () => {
	assert.throws(
		() => normalizeEntry({ id: "o3", message: "m", once: "not-a-date" }),
		/invalid once/,
	);
	assert.throws(
		() => normalizeEntry({ id: "o4", message: "m", once: "22:00" }), // bare time is an `at`, not once
		/invalid once/,
	);
});

test("normalizeEntry rejects multiple triggers (mutual exclusion)", () => {
	assert.throws(
		() => normalizeEntry({ id: "o5", message: "m", cron: "0 8 * * *", once: "2026-08-20T22:00:00+08:00" }),
		/exactly one/,
	);
	assert.throws(
		() => normalizeEntry({ id: "o6", message: "m", at: "22:00", interval: "1h" }),
		/exactly one/,
	);
});

// CRITICAL boundary: the store layer must NEVER enforce "once must be in the future".
// saveSchedules writes the whole list all-or-nothing, and an expired once is left in config after
// it fires (engine does not delete it). A future-time check here would make EVERY subsequent write
// (creating another reminder, editing a different task, even disabling the expired once) fail,
// locking the whole schedule config read-only. Lock this in with a test.
test("normalizeEntry does NOT reject an expired once (no future-time check on the store path)", () => {
	const expired = "2000-01-01T00:00:00Z";
	const e = normalizeEntry({ id: "old-once", message: "m", once: expired });
	assert.equal(e.once, expired);
});

test("normalizeSchedules accepts a list containing an expired once alongside other entries (no write poisoning)", () => {
	const out = normalizeSchedules([
		{ id: "old-once", message: "fired", once: "2000-01-01T00:00:00Z" },
		{ id: "daily", message: "m", at: "08:00" },
	]);
	assert.equal(out.length, 2);
	assert.equal(out[0].once, "2000-01-01T00:00:00Z");
});

// ---------------------------------------------------------------------------
// holidayMode ("workday" gating)
// ---------------------------------------------------------------------------

test("normalizeEntry with holidayMode:workday succeeds and ignores impossible date fields", () => {
	// "0 18 31 2 *" would be rejected by the normal compiler (Feb 31 never occurs), but in workday
	// mode the date fields are neutralized, so it is accepted and holidayMode is preserved.
	const e = normalizeEntry({ id: "wd1", message: "m", cron: "0 18 31 2 *", holidayMode: "workday" });
	assert.equal(e.holidayMode, "workday");
	assert.equal(e.cron, "0 18 31 2 *");
});

test("normalizeEntry rejects holidayMode:workday combined with once", () => {
	assert.throws(
		() => normalizeEntry({ id: "wd2", message: "m", once: "2026-08-20T22:00:00+08:00", holidayMode: "workday" }),
		/cannot be combined with once/,
	);
});

test("normalizeEntry with holidayMode:off omits the field entirely", () => {
	const e = normalizeEntry({ id: "wd3", message: "m", at: "08:00", holidayMode: "off" });
	assert.equal(e.holidayMode, undefined);
	assert.equal(e.at, "08:00");
});
