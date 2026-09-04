import { test } from "node:test";
import assert from "node:assert/strict";
import {
	emptyRoute,
	parseRoute,
	shouldClearConversationForRoute,
	shouldRecordConversationTurn,
} from "./decisions.ts";

const COOK_VOCAB = ["cook", "vocab"];

test("parseRoute accepts reset-context sentinel with tolerant casing and surrounding tokens", () => {
	for (const output of [
		"RESET_CONTEXT",
		"reset_context",
		"**RESET_CONTEXT**",
		"RESET_CONTEXT。",
		"RESET_CONTEXT（用户明确要求忽略上文）",
	]) {
		assert.deepEqual(parseRoute(output, COOK_VOCAB), {
			projects: [],
			attachmentProject: null,
			resetContext: true,
		});
	}
});

test("parseRoute does not treat reset-context prefixes as a reset decision", () => {
	assert.deepEqual(parseRoute("RESET_CONTEXTUAL", COOK_VOCAB), emptyRoute());
});

test("parseRoute parses a structured single-project JSON route", () => {
	assert.deepEqual(parseRoute('{"projects":["cook"],"attachmentProject":null}', COOK_VOCAB), {
		projects: ["cook"],
		attachmentProject: null,
		resetContext: false,
	});
});

test("parseRoute parses a multi-project JSON route preserving order", () => {
	assert.deepEqual(
		parseRoute('{"projects":["cook","vocab"],"attachmentProject":"cook"}', COOK_VOCAB),
		{ projects: ["cook", "vocab"], attachmentProject: "cook", resetContext: false },
	);
});

test("parseRoute deduplicates repeated project names in order", () => {
	assert.deepEqual(parseRoute('{"projects":["cook","vocab","cook"],"attachmentProject":null}', COOK_VOCAB), {
		projects: ["cook", "vocab"],
		attachmentProject: null,
		resetContext: false,
	});
});

test("parseRoute strips code fences and leading prose before JSON", () => {
	assert.deepEqual(
		parseRoute('好的：```json\n{"projects":["cook"],"attachmentProject":null}\n```', COOK_VOCAB),
		{ projects: ["cook"], attachmentProject: null, resetContext: false },
	);
});

test("parseRoute rejects UNKNOWN and natural-language markers", () => {
	for (const output of ["UNKNOWN", "无法确定", "不确定", "无法判断", "不明确"]) {
		assert.deepEqual(parseRoute(output, COOK_VOCAB), emptyRoute());
	}
});

test("parseRoute rejects an empty projects array", () => {
	assert.deepEqual(parseRoute('{"projects":[],"attachmentProject":null}', COOK_VOCAB), emptyRoute());
});

test("parseRoute rejects non-object / non-array-projects JSON", () => {
	assert.deepEqual(parseRoute('{"projects":"cook"}', COOK_VOCAB), emptyRoute());
	assert.deepEqual(parseRoute('{"projects":[1]}', COOK_VOCAB), emptyRoute());
	assert.deepEqual(parseRoute('"cook"', COOK_VOCAB), emptyRoute());
});

test("parseRoute rejects a project name not in the candidate list", () => {
	assert.deepEqual(parseRoute('{"projects":["other"],"attachmentProject":null}', COOK_VOCAB), emptyRoute());
});

test("parseRoute rejects __scheduler__ mixed with data projects", () => {
	assert.deepEqual(
		parseRoute('{"projects":["cook","__scheduler__"],"attachmentProject":null}', [...COOK_VOCAB, "__scheduler__"]),
		emptyRoute(),
	);
	assert.deepEqual(
		parseRoute('{"projects":["__scheduler__","cook"],"attachmentProject":null}', [...COOK_VOCAB, "__scheduler__"]),
		emptyRoute(),
	);
});

test("parseRoute accepts __scheduler__ only as the sole entry", () => {
	assert.deepEqual(
		parseRoute('{"projects":["__scheduler__"],"attachmentProject":null}', [...COOK_VOCAB, "__scheduler__"]),
		{ projects: ["__scheduler__"], attachmentProject: null, resetContext: false },
	);
});

test("parseRoute rejects a scheduler attachmentProject", () => {
	assert.deepEqual(
		parseRoute('{"projects":["__scheduler__"],"attachmentProject":"__scheduler__"}', [...COOK_VOCAB, "__scheduler__"]),
		emptyRoute(),
	);
});

test("parseRoute rejects an attachmentProject outside projects", () => {
	assert.deepEqual(
		parseRoute('{"projects":["cook"],"attachmentProject":"vocab"}', COOK_VOCAB),
		emptyRoute(),
	);
});

test("parseRoute treats a string \"null\" attachmentProject as no attachment", () => {
	assert.deepEqual(parseRoute('{"projects":["cook"],"attachmentProject":"null"}', COOK_VOCAB), {
		projects: ["cook"],
		attachmentProject: null,
		resetContext: false,
	});
});

test("parseRoute falls back to a single exact bare project name (non-JSON output)", () => {
	assert.deepEqual(parseRoute("cook", COOK_VOCAB), {
		projects: ["cook"],
		attachmentProject: null,
		resetContext: false,
	});
	assert.deepEqual(parseRoute("好的，是 cook", COOK_VOCAB), {
		projects: ["cook"],
		attachmentProject: null,
		resetContext: false,
	});
});

test("parseRoute does not misroute a project that is a substring of another", () => {
	// "cook" is a substring of "cooking"; an exact match must not route "cooking" to "cook".
	assert.deepEqual(parseRoute("cooking", ["cook", "cooking"]), {
		projects: ["cooking"],
		attachmentProject: null,
		resetContext: false,
	});
	assert.deepEqual(parseRoute("cook", ["cook", "cooking"]), {
		projects: ["cook"],
		attachmentProject: null,
		resetContext: false,
	});
});

test("parseRoute returns unknown when more than one bare name appears in non-JSON output", () => {
	assert.deepEqual(parseRoute("cook and vocab", COOK_VOCAB), emptyRoute());
	assert.deepEqual(parseRoute("cook/vocab", COOK_VOCAB), emptyRoute());
});

test("parseRoute routes scheduler aliases only when the virtual project is a candidate", () => {
	assert.deepEqual(parseRoute("定时", ["cook", "__scheduler__"]), {
		projects: ["__scheduler__"],
		attachmentProject: null,
		resetContext: false,
	});
	assert.deepEqual(parseRoute("定时", ["cook"]), emptyRoute());
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
