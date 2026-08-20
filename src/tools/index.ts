import type { AgentTool } from "@earendil-works/pi-agent-core";
import { readTool } from "./read.ts";
import { writeTool } from "./write.ts";
import { editTool } from "./edit.ts";
import { runTool } from "./run.ts";
import { scheduleTool } from "./schedule.ts";

/**
 * Custom file tools registered with the Agent, limited to the allowlisted data directories.
 * run is not bash: it only executes existing python3/node scripts inside projects (no shell, args passed directly).
 * schedule is the one exception to the DATA_ROOT allowlist: it writes the fixed externalized
 * schedules.json (no file-path parameter, full validation, atomic write) so users can create
 * scheduled tasks from chat. safePath itself is untouched.
 */
export const fileTools: AgentTool[] = [readTool, writeTool, editTool, runTool, scheduleTool];
