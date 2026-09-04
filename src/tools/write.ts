import { writeFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";
import { safePath } from "./paths.ts";
import { getTaskProjects } from "./task-context.ts";

const schema = Type.Object({
	path: Type.String({
		description: "Target file path relative to DATA_ROOT (inside a data subproject under DATA_ROOT)",
	}),
	content: Type.Any({
		description:
			"JSON value to write (object or array). Serialized with 2-space indentation. Pass the data structure itself, not a JSON text string.",
	}),
});

/**
 * Write structured data (object / array) to a file, automatically serialized into valid 2-space-indented JSON.
 *
 * Key point: producing "valid JSON" is guaranteed by this tool (the model only needs to give the correct structure,
 * not hand-write JSON syntax), but field types / semantics still need your confirmation — the validator is the safety net.
 * Parent directories are created on demand before writing.
 */
export const writeTool: AgentTool<typeof schema> = {
	name: "write",
	label: "Write JSON file",
	description:
		"Write data (object or array) to a file, automatically serialized into formatted valid JSON. Used to create or modify JSON files inside a data subproject under DATA_ROOT. Note: pass the data structure itself (not JSON text) — this tool handles serialization. Before writing, read the existing content with read, modify it in memory, then write the whole thing back.",
	parameters: schema,
	async execute(_toolCallId, { path, content }) {
		// Auto-recovery: the model occasionally passes content as a JSON text string (which would otherwise be
		// double-encoded into a string wrapper). If it's a string, parse it once; the parse result must be an
		// object/array, otherwise return an actionable error.
		let value: unknown = content;
		if (typeof content === "string") {
			try {
				value = JSON.parse(content);
			} catch {
				throw new Error(
					"The write tool received a string for content, and it is not valid JSON text. Pass the data structure itself (object or array) — this tool handles serialization; do not pass a JSON text string.",
				);
			}
		}
		if (typeof value !== "object" || value === null) {
			throw new Error(
				`The write tool's content must be an object or array; received ${value === null ? "null" : typeof value
				}. Pass the data structure itself — this tool handles serialization.`,
			);
		}
		const abs = safePath(path, { projects: getTaskProjects() });
		mkdirSync(dirname(abs), { recursive: true });
		const text = JSON.stringify(value, null, 2);
		writeFileSync(abs, text, "utf8");
		return {
			content: [{ type: "text", text: `Wrote ${path} (${text.length} bytes, valid JSON)` }],
			details: { path, bytes: text.length, recovered: typeof content === "string" },
		};
	},
};
