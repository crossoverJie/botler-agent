import { test } from "node:test";
import assert from "node:assert/strict";
import { pollDelay, MAX_SLEEP_MS, IDLE_POLL_MS, MIN_SLEEP_MS } from "./engine.ts";

const MAX_INT32 = 2 ** 31 - 1;

test("pollDelay caps a far-future target below the 32-bit setTimeout limit", () => {
	const now = 1_790_128_171_897; // 2026-09-23
	const soonest = 1_797_782_400_000; // 2026-12-21 (~88 days ahead)
	const raw = soonest - now;
	assert.ok(raw > MAX_INT32, "precondition: raw delay must overflow a 32-bit signed int");
	assert.equal(pollDelay(soonest, now), MAX_SLEEP_MS);
	assert.ok(pollDelay(soonest, now) <= MAX_INT32);
});

test("pollDelay returns the raw wait for a near-future target", () => {
	const now = 1_790_128_171_897;
	const soonest = now + 5 * 60_000; // 5 minutes ahead
	assert.equal(pollDelay(soonest, now), 5 * 60_000);
});

test("pollDelay floors a past/zero target", () => {
	const now = 1_790_128_171_897;
	assert.equal(pollDelay(now, now), MIN_SLEEP_MS);
	assert.equal(pollDelay(now - 10_000, now), MIN_SLEEP_MS);
});

test("pollDelay uses the idle poll when nothing is scheduled", () => {
	assert.equal(pollDelay(Infinity, 1_790_128_171_897), IDLE_POLL_MS);
});
