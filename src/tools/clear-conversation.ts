/**
 * Conversation-control tool for IM user tasks.
 *
 * This is a narrow framework-level tool, like `schedule`: it never touches DATA_ROOT and has
 * no path parameter. The runner only exposes it to execute-phase IM tasks, and the task context
 * flag prevents scheduler / CLI / self-heal runs from clearing the shared conversation.
 */

import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";
import { clearSession } from "../conversation/store.ts";
import { getTaskContext } from "./task-context.ts";

const schema = Type.Object({});

export const clearConversationTool: AgentTool<typeof schema> = {
	name: "clear_conversation_context",
	label: "Clear conversation context",
	description:
		"Clear the shared recent IM conversation history. Call this only when the user explicitly asks to start a new task and ignore/reset/clear the previous context. After clearing, continue with the current request if one was provided.",
	parameters: schema,
	async execute(_toolCallId) {
		const ctx = getTaskContext();
		const sessionKey = ctx?.conversationSessionKey;
		if (!sessionKey) {
			throw new Error("Conversation context can only be cleared for IM user tasks.");
		}
		clearSession(sessionKey);
		return {
			content: [{ type: "text", text: "Conversation context cleared." }],
			details: {},
		};
	},
};
