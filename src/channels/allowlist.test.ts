import { test } from "node:test";
import assert from "node:assert/strict";
import { isAllowedSender } from "./allowlist.ts";

test("empty allowlist allows everyone", () => {
	assert.equal(isAllowedSender(undefined, ["123", "alice"]), true);
	assert.equal(isAllowedSender("", ["123"]), true);
	assert.equal(isAllowedSender(" , ", ["123"]), true);
});

test("non-empty allowlist requires an exact identity match", () => {
	assert.equal(isAllowedSender("123, alice", ["alice"]), true);
	assert.equal(isAllowedSender("123, alice", ["bob"]), false);
	assert.equal(isAllowedSender("123", ["123", "alice"]), true);
	assert.equal(isAllowedSender("123", [undefined, ""]), false);
});
