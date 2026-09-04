import { readFileSync, writeFileSync } from "node:fs";
import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";
import { safePath } from "./paths.ts";
import { getTaskProjects } from "./task-context.ts";

const schema = Type.Object({
	path: Type.String({ description: "File path relative to DATA_ROOT" }),
	oldText: Type.String({ description: "Original text to be replaced; must match the file content exactly" }),
	newText: Type.String({ description: "Replacement text" }),
	all: Type.Optional(Type.Boolean({ description: "Replace all matches; default false (first occurrence only)" })),
});

/**
 * In-file string replacement. Used for local modifications inside JSON.
 * oldText must match the file's existing content exactly (including whitespace and newlines), otherwise it errors.
 * Prefer write for a full rewrite; edit suits cases where only one or two spots change in a very large file.
 */
export const editTool: AgentTool<typeof schema> = {
	name: "edit",
	label: "Text replacement",
	description:
		"Replace oldText with newText in a file. oldText must match the file's existing content character-for-character (including spaces and newlines), otherwise it fails. all=true replaces all matches, otherwise only the first. Used for local fixes inside JSON files.",
	parameters: schema,
	async execute(_toolCallId, { path, oldText, newText, all }) {
		const abs = safePath(path, { projects: getTaskProjects() });
		const current = readFileSync(abs, "utf8");
		if (!current.includes(oldText)) {
			throw new Error(`oldText not found in the original file (first 60 chars): ${oldText.slice(0, 60)}`);
		}
		const next = all ? current.split(oldText).join(newText) : current.replace(oldText, newText);
		writeFileSync(abs, next, "utf8");
		return {
			content: [{ type: "text", text: `Updated ${path}` }],
			details: { path },
		};
	},
};
