/**
 * Per-task context injected by the runner before an Agent run.
 *
 * `AgentOptions.beforeToolCall` cannot mutate the tool args, so per-task data used by tools
 * (the schedule recipient and the clear-conversation guard) is injected here at the module
 * level instead. The dispatcher's sequential queue guarantees tasks run strictly one at a
 * time, so a single module-level slot is race-free.
 */

import type { Recipient } from "../push/types.ts";

let current: {
	recipient?: Recipient;
	conversationSessionKey?: string;
	projects?: readonly string[];
} | null = null;

export function setTaskContext(c: {
	recipient?: Recipient;
	conversationSessionKey?: string;
	projects?: readonly string[];
} | null): void {
	current = c;
}

export function getTaskContext(): {
	recipient?: Recipient;
	conversationSessionKey?: string;
	projects?: readonly string[];
} | null {
	return current;
}

/** Task-local selected projects for path-tool enforcement; undefined when no subset is active. */
export function getTaskProjects(): readonly string[] | undefined {
	return current?.projects;
}
