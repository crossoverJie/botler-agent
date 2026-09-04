import { execFileSync } from "node:child_process";
import { existsSync, statSync } from "node:fs";
import { resolve } from "node:path";
import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";
import { safePath, projectOf } from "./paths.ts";
import { getTaskProjects } from "./task-context.ts";
import { CONFIG } from "../config.ts";

/** Fixed interpreter per extension; the script must already exist inside an allowlisted project, eliminating arbitrary execution like `python3 -c`. */
function interpreterOf(script: string): string | null {
	if (script.endsWith(".py")) return "python3";
	if (script.endsWith(".js") || script.endsWith(".mjs")) return "node";
	return null;
}

const schema = Type.Object({
	script: Type.String({
		description: "Script path relative to DATA_ROOT, e.g. my-project/scripts/build.py",
	}),
	args: Type.Optional(
		Type.Array(Type.String(), { description: "Arguments passed to the script (optional), e.g. ['--dry-run']" }),
	),
});

/**
 * Execute an existing script inside a data subproject (only python3 .py / node .js|.mjs).
 * No shell (execFileSync passes args as an array), working directory locked to the script's subproject root, 60s timeout.
 * Used to run build/refresh scripts from a project's conventions (AGENTS.md, e.g. build.py); on script failure,
 * stdout/stderr is returned to the Agent as text (rather than throwing and interrupting), so the LLM can decide whether to fix and retry.
 */
export const runTool: AgentTool<typeof schema> = {
	name: "run",
	label: "Run in-project script",
	description:
		"Run an existing script inside a data subproject (only python3 .py or node .js/.mjs), used to run build/refresh scripts defined by the project's conventions (e.g. build.py). No shell is used; args are passed directly; the working directory is locked to the script's subproject root with a 60-second timeout. Only run it when the project's conventions (AGENTS.md) require it; skip if the project has no such script.",
	parameters: schema,
	async execute(_toolCallId, { script, args = [] }) {
		const abs = safePath(script, { projects: getTaskProjects() });
		if (!existsSync(abs) || !statSync(abs).isFile()) {
			throw new Error(`Script does not exist or is not a file: ${script}`);
		}
		const project = projectOf(abs);
		if (!project) throw new Error(`Script is not inside any data subproject: ${script}`);
		const interpreter = interpreterOf(script);
		if (!interpreter) throw new Error(`Unsupported script type (only .py / .js / .mjs): ${script}`);
		const cwd = resolve(CONFIG.dataRoot, project);
		try {
			const out = execFileSync(interpreter, [abs, ...args], {
				cwd,
				encoding: "utf8",
				timeout: 60_000,
				maxBuffer: 1024 * 1024,
			});
			return {
				content: [{ type: "text", text: out.trim() ? out : "(script ran successfully, no output)" }],
				details: { script, cwd },
			};
		} catch (e) {
			const err = e as { stdout?: string; stderr?: string; message?: string };
			const detail = `${err.stdout ?? ""}${err.stderr ?? ""}`.trim();
			const msg = detail
				? `Script failed (${err.message ?? ""}):\n${detail}`
				: `Script failed: ${err.message ?? String(e)}`;
			return {
				content: [{ type: "text", text: msg }],
				details: { script, cwd, failed: true },
			};
		}
	},
};
