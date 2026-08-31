import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ConversationTurn } from "./store.ts";

let store: typeof import("./store.ts");
let systemPrompt: typeof import("../prompts/system-prompt.ts");

before(async () => {
	const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-conversation-")));
	process.env.BOTLER_CONFIG_DIR = tmp;
	process.env.CONVERSATION_CONTEXT_TURNS = "5";
	process.env.CONVERSATION_TURN_MAX_CHARS = "20";
	store = await import("./store.ts");
	systemPrompt = await import("../prompts/system-prompt.ts");
});

function turn(n: number, project: string | null = "cook"): ConversationTurn {
	return {
		ts: 1_000 + n,
		project,
		user: `user-${n}`,
		assistant: `assistant-${n}`,
		imageRefs: [],
	};
}

test("loadRecentTurns returns the newest N turns oldest-to-newest", () => {
	store.clearSession(store.IM_SESSION_KEY);
	for (let i = 1; i <= 3; i++) store.appendTurn(store.IM_SESSION_KEY, turn(i), 5);
	const turns = store.loadRecentTurns(store.IM_SESSION_KEY, 5);
	assert.deepEqual(
		turns.map((t) => t.user),
		["user-1", "user-2", "user-3"],
	);
});

test("appendTurn drops turns older than the sliding window", () => {
	store.clearSession(store.IM_SESSION_KEY);
	for (let i = 1; i <= 7; i++) store.appendTurn(store.IM_SESSION_KEY, turn(i), 5);
	const turns = store.loadRecentTurns(store.IM_SESSION_KEY, 5);
	assert.deepEqual(
		turns.map((t) => t.user),
		["user-3", "user-4", "user-5", "user-6", "user-7"],
	);
});

test("appendTurn truncates user and assistant messages to the configured max chars", () => {
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(
		store.IM_SESSION_KEY,
		{
			ts: 1,
			project: "cook",
			user: "u".repeat(40),
			assistant: "a".repeat(40),
			imageRefs: ["cook/photos/example.jpg"],
		},
		5,
	);
	const [stored] = store.loadRecentTurns(store.IM_SESSION_KEY, 5);
	assert.equal(stored.user.length, 20);
	assert.equal(stored.assistant.length, 20);
	assert.deepEqual(stored.imageRefs, ["cook/photos/example.jpg"]);
});

test("shouldRecordTurn only records execute-phase success/auto-fixed/unknown-project with a session", () => {
	assert.equal(store.shouldRecordTurn("execute", "success", store.IM_SESSION_KEY), true);
	assert.equal(store.shouldRecordTurn("execute", "auto-fixed", store.IM_SESSION_KEY), true);
	assert.equal(store.shouldRecordTurn("execute", "unknown-project", store.IM_SESSION_KEY), true);
	assert.equal(store.shouldRecordTurn("execute", "validation-failed", store.IM_SESSION_KEY), false);
	assert.equal(store.shouldRecordTurn("execute", "error", store.IM_SESSION_KEY), false);
	assert.equal(store.shouldRecordTurn("execute", "duplicate", store.IM_SESSION_KEY), false);
	assert.equal(store.shouldRecordTurn("self-heal", "success", store.IM_SESSION_KEY), false);
	assert.equal(store.shouldRecordTurn("execute", "success"), false);
});

test("formatRecentTurns includes project labels only when requested", () => {
	const turns = [turn(1, "cook")];
	assert.equal(store.formatRecentTurns(turns), "用户：user-1\nBot：assistant-1");
	assert.equal(
		store.formatRecentTurns(turns, { includeProject: true }),
		"（项目：cook）用户：user-1\nBot：assistant-1",
	);
});

test("buildRoutePrompt includes the recent user-visible conversation", () => {
	const turns = [turn(1, "cook")];
	const prompt = systemPrompt.buildRoutePrompt("确认", true, false, turns);
	assert.match(prompt, /最近对话/);
	assert.match(prompt, /用户：user-1/);
	assert.match(prompt, /Bot：assistant-1/);
	assert.match(prompt, /确认/);
});

test("clearSession removes the stored conversation file", () => {
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(store.IM_SESSION_KEY, turn(1), 5);
	store.clearSession(store.IM_SESSION_KEY);
	assert.deepEqual(store.loadRecentTurns(store.IM_SESSION_KEY, 5), []);
});
