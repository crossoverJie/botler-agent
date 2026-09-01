import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

let clearTool: typeof import("./clear-conversation.ts");
let taskContext: typeof import("./task-context.ts");
let store: typeof import("../conversation/store.ts");

before(async () => {
	const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-clear-tool-")));
	process.env.BOTLER_CONFIG_DIR = tmp;
	process.env.CONVERSATION_CONTEXT_TURNS = "5";
	clearTool = await import("./clear-conversation.ts");
	taskContext = await import("./task-context.ts");
	store = await import("../conversation/store.ts");
});

test("clear_conversation_context rejects when the current task is not an IM user task", async () => {
	taskContext.setTaskContext(null);
	await assert.rejects(
		clearTool.clearConversationTool.execute("tool-call-id", {}),
		/Conversation context can only be cleared for IM user tasks/,
	);
});

test("clear_conversation_context clears the shared IM session when allowed", async () => {
	taskContext.setTaskContext({ clearConversationAllowed: true });
	store.clearSession(store.IM_SESSION_KEY);
	store.appendTurn(
		store.IM_SESSION_KEY,
		{ ts: 1, project: "cook", user: "old", assistant: "old reply" },
		5,
	);

	await clearTool.clearConversationTool.execute("tool-call-id", {});

	assert.deepEqual(store.loadRecentTurns(store.IM_SESSION_KEY, 5), []);
	taskContext.setTaskContext(null);
});
