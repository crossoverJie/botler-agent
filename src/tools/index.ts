import type { AgentTool } from "@earendil-works/pi-agent-core";
import { readTool } from "./read.ts";
import { writeTool } from "./write.ts";
import { editTool } from "./edit.ts";
import { runTool } from "./run.ts";
import { scheduleTool } from "./schedule.ts";
import { clearConversationTool } from "./clear-conversation.ts";

/**
 * Custom file tools registered with the Agent, limited to the allowlisted data directories.
 * run is not bash: it only executes existing python3/node scripts inside projects (no shell, args passed directly).
 * schedule and clear_conversation_context are narrow framework-level exceptions to the
 * DATA_ROOT allowlist: they write/clear fixed externalized state outside DATA_ROOT and take no
 * file-path parameter. safePath itself is untouched.
 */
/** Data tools available to every execution; no framework-level conversation control. */
export const dataTools: AgentTool[] = [readTool, writeTool, editTool, runTool, scheduleTool];

/** Execution tools for IM user tasks; includes the narrow clear-conversation control tool. */
export const fileTools: AgentTool[] = [...dataTools, clearConversationTool];
