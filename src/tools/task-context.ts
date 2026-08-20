/**
 * Per-task context injected by the runner before an Agent run.
 *
 * `AgentOptions.beforeToolCall` cannot mutate the tool args, so the recipient for tools
 * that need it (the schedule tool) is injected here at the module level instead. The
 * dispatcher's sequential queue guarantees tasks run strictly one at a time, so a single
 * module-level slot is race-free.
 */

import type { Recipient } from "../push/types.ts";

let current: { recipient?: Recipient } | null = null;

export function setTaskContext(c: { recipient?: Recipient } | null): void {
	current = c;
}

export function getTaskContext(): { recipient?: Recipient } | null {
	return current;
}
