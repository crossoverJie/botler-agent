import { readFileSync } from "node:fs";
import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";
import { safePath } from "./paths.ts";
import { getTaskProjects } from "./task-context.ts";

const schema = Type.Object({
	path: Type.String({
		description: "File path relative to DATA_ROOT (inside a data subproject under DATA_ROOT), e.g. project-name/data/xxx.json",
	}),
});

/** Read the content (text / JSON) of a file inside the data directory. */
export const readTool: AgentTool<typeof schema> = {
	name: "read",
	label: "Read file",
	description:
		"Read the content of a file inside the data directory. The path is relative to DATA_ROOT and limited to data subprojects under DATA_ROOT. Returns the raw file content (JSON files as JSON text) so you can understand the current data before deciding how to modify it.",
	parameters: schema,
	async execute(_toolCallId, { path }) {
		const abs = safePath(path, { projects: getTaskProjects() });
		const content = readFileSync(abs, "utf8");
		return {
			content: [{ type: "text", text: content }],
			details: { path, bytes: content.length },
		};
	},
};
