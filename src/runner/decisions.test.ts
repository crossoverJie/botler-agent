import { test } from "node:test";
import assert from "node:assert/strict";
import {
	parseRoute,
	shouldClearConversationForRoute,
	shouldRecordConversationTurn,
} from "./decisions.ts";

test("parseRoute accepts reset-context sentinel with tolerant casing and surrounding tokens", () => {
	for (const output of [
		"RESET_CONTEXT",
		"reset_context",
		"**RESET_CONTEXT**",
		"RESET_CONTEXT。",
		"RESET_CONTEXT（用户明确要求忽略上文）",
	]) {
		assert.deepEqual(parseRoute(output, ["cook", "vocab"]), {
			project: null,
			resetContext: true,
		});
	}
});

test("parseRoute does not treat reset-context prefixes as a reset decision", () => {
	assert.deepEqual(parseRoute("RESET_CONTEXTUAL", ["cook", "vocab"]), {
		project: null,
		resetContext: false,
	});
});

test("parseRoute still routes ordinary project names and unknown output", () => {
	assert.deepEqual(parseRoute("cook", ["cook", "vocab"]), {
		project: "cook",
		resetContext: false,
	});
	assert.deepEqual(parseRoute("无法确定", ["cook", "vocab"]), {
		project: null,
		resetContext: false,
	});
});

test("parseRoute routes scheduler aliases only when the virtual project is a candidate", () => {
	assert.deepEqual(parseRoute("定时", ["cook", "__scheduler__"]), {
		project: "__scheduler__",
		resetContext: false,
	});
	assert.deepEqual(parseRoute("定时", ["cook"]), {
		project: null,
		resetContext: false,
	});
});

test("shouldRecordConversationTurn suppresses only clear-only executions", () => {
	assert.equal(shouldRecordConversationTurn([]), true);
	assert.equal(shouldRecordConversationTurn(["read"]), true);
	assert.equal(shouldRecordConversationTurn(["clear_conversation_context"]), false);
	assert.equal(shouldRecordConversationTurn(["clear_conversation_context", "read"]), true);
	assert.equal(shouldRecordConversationTurn(["clear_conversation_context", "write"]), true);
	assert.equal(shouldRecordConversationTurn(["clear_conversation_context"], 1), true);
});

test("shouldClearConversationForRoute requires reset decision, IM session, and a key", () => {
	assert.equal(shouldClearConversationForRoute(true, true, "im"), true);
	assert.equal(shouldClearConversationForRoute(true, true), false);
	assert.equal(shouldClearConversationForRoute(true, false, "im"), false);
	assert.equal(shouldClearConversationForRoute(false, true, "im"), false);
});
