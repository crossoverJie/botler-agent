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
	process.env.CONVERSATION_CONTEXT_MAX_CHARS = "80";
	store = await import("./store.ts");
	systemPrompt = await import("../prompts/system-prompt.ts");
});

function turn(n: number, project: string | null = "cook"): ConversationTurn {
	return {
		ts: 1_000 + n,
		project,
		user: `user-${n}`,
		assistant: `assistant-${n}`,
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

test("appendTurn preserves a null project for unknown-project turns", () => {
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(store.IM_SESSION_KEY, turn(1, null), 5);
	assert.equal(store.loadRecentTurns(store.IM_SESSION_KEY, 5)[0].project, null);
});

test("appendTurn round-trips a multi-project turn with a null legacy project", () => {
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(
		store.IM_SESSION_KEY,
		{ ts: 1, project: null, projects: ["cook", "vocab"], user: "u", assistant: "a" },
		5,
	);
	const [t] = store.loadRecentTurns(store.IM_SESSION_KEY, 5);
	assert.deepEqual(t.projects, ["cook", "vocab"]);
	assert.equal(t.project, null);
});

test("normalizeTurn falls back to [project] for old single-project data", () => {
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(store.IM_SESSION_KEY, turn(1, "cook"), 5);
	const [t] = store.loadRecentTurns(store.IM_SESSION_KEY, 5);
	assert.deepEqual(t.projects, ["cook"]);
	assert.equal(t.project, "cook");
});

test("formatRecentTurns renders joined project labels for multi-project turns", () => {
	const turns = [{ ts: 1, project: null, projects: ["cook", "vocab"], user: "u", assistant: "a" }];
	assert.equal(
		store.formatRecentTurns(turns, { includeProject: true }),
		"（项目：cook, vocab）用户：u\nBot：a",
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
		},
		5,
	);
	const [stored] = store.loadRecentTurns(store.IM_SESSION_KEY, 5);
	assert.equal(stored.user.length, 20);
	assert.equal(stored.assistant.length, 20);
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
	assert.equal(store.shouldRecordTurn("execute", "unknown-project", store.IM_SESSION_KEY, false), false);
});

test("formatRecentTurns includes project labels only when requested", () => {
	const turns = [turn(1, "cook")];
	assert.equal(store.formatRecentTurns(turns), "用户：user-1\nBot：assistant-1");
	assert.equal(
		store.formatRecentTurns(turns, { includeProject: true }),
		"（项目：cook）用户：user-1\nBot：assistant-1",
	);
});

test("formatRecentTurns separates turns with blank lines and a horizontal rule", () => {
	const turns = [turn(1, "cook"), turn(2, "cook")];
	assert.equal(
		store.formatRecentTurns(turns, { maxChars: 1000 }),
		"用户：user-1\nBot：assistant-1\n\n---\n\n用户：user-2\nBot：assistant-2",
	);
});

test("formatRecentTurns drops oldest turns first when the whole-window cap is exceeded", () => {
	const turns = [
		{ ts: 1, project: "cook", user: "old-user", assistant: "old-assistant" },
		{ ts: 2, project: "cook", user: "new-user", assistant: "new-assistant" },
	];
	const formatted = store.formatRecentTurns(turns, { maxChars: 29 });
	assert.equal(formatted, "用户：new-user\nBot：new-assistant");
	assert.doesNotMatch(formatted, /old-user/);
});

test("shouldLoadRecentTurns only loads execute-phase IM sessions", () => {
	const base = { enabled: true, source: "telegram", sessionKey: store.IM_SESSION_KEY };
	assert.equal(store.shouldLoadRecentTurns({ ...base, phase: "execute" }), true);
	assert.equal(store.shouldLoadRecentTurns({ ...base, phase: "self-heal" }), false);
	assert.equal(store.shouldLoadRecentTurns({ ...base, source: "scheduler", phase: "execute" }), false);
	assert.equal(store.shouldLoadRecentTurns({ ...base, source: "cli", phase: "execute" }), false);
	assert.equal(store.shouldLoadRecentTurns({ ...base, enabled: false, phase: "execute" }), false);
	assert.equal(store.shouldLoadRecentTurns({ enabled: true, source: "telegram", phase: "execute" }), false);
});

test("buildRoutePrompt includes the recent user-visible conversation", () => {
	const turns = [turn(1, "cook")];
	const prompt = systemPrompt.buildRoutePrompt("确认", true, false, turns);
	assert.match(prompt, /最近对话/);
	assert.match(prompt, /用户：user-1/);
	assert.match(prompt, /Bot：assistant-1/);
	assert.match(prompt, /确认/);
});

test("buildRoutePrompt includes reset-context routing only for IM sessions", () => {
	const withReset = systemPrompt.buildRoutePrompt("新任务", true, false, [], true);
	assert.match(withReset, /RESET_CONTEXT/);
	assert.match(withReset, /清空 \/ 忽略 \/ 重置之前的上下文/);

	const withoutReset = systemPrompt.buildRoutePrompt("新任务", true, false, [], false);
	assert.doesNotMatch(withoutReset, /RESET_CONTEXT/);
});

test("clearSession removes the stored conversation file", () => {
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(store.IM_SESSION_KEY, turn(1), 5);
	store.clearSession(store.IM_SESSION_KEY);
	assert.deepEqual(store.loadRecentTurns(store.IM_SESSION_KEY, 5), []);
});
