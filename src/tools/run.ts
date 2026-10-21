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

/** Result of a {@link runScript} call. Never throws, so failures can flow into retry/push/stats uniformly. */
export interface RunResult {
	/** Whether the script ran and exited zero. */
	ok: boolean;
	/** stdout (trimmed) on success, or a human-readable failure description. */
	text: string;
	/** Subproject root the script ran in (only on success / runtime failure); absent on pre-condition rejection. */
	cwd?: string;
	/** Failure class: `precondition` (bad path / missing file / wrong type — never started) or `runtime` (started but non-zero exit). */
	errorKind?: "precondition" | "runtime";
}

/**
 * Execute an existing script inside a data subproject (only python3 .py / node .js|.mjs) **without
 * throwing**. No shell (execFileSync passes args as an array), working directory locked to the
 * script's subproject root, 60s timeout. Reused by both the `run` AgentTool and the scheduler's
 * direct-script path, so there is a single execution core and a single security boundary.
 * Pre-condition problems (out-of-bounds path, missing file, wrong type) are returned as a
 * `precondition` failure rather than thrown, so the scheduler can treat them like any other failure.
 */
export function runScript(script: string, args: string[] = []): RunResult {
	try {
		const abs = safePath(script, { projects: getTaskProjects() });
		if (!existsSync(abs) || !statSync(abs).isFile())
			return { ok: false, text: `Script does not exist or is not a file: ${script}`, errorKind: "precondition" };
		const project = projectOf(abs);
		if (!project)
			return { ok: false, text: `Script is not inside any data subproject: ${script}`, errorKind: "precondition" };
		const interpreter = interpreterOf(script);
		if (!interpreter)
			return { ok: false, text: `Unsupported script type (only .py / .js / .mjs): ${script}`, errorKind: "precondition" };
		const cwd = resolve(CONFIG.dataRoot, project);
		try {
			const out = execFileSync(interpreter, [abs, ...args], {
				cwd,
				encoding: "utf8",
				timeout: 60_000,
				maxBuffer: 1024 * 1024,
			});
			return { ok: true, text: out.trim() ? out : "(script ran successfully, no output)", cwd };
		} catch (e) {
			const err = e as { stdout?: string; stderr?: string; message?: string };
			const detail = `${err.stdout ?? ""}${err.stderr ?? ""}`.trim();
			const msg = detail
				? `Script failed (${err.message ?? ""}):\n${detail}`
				: `Script failed: ${err.message ?? String(e)}`;
			return { ok: false, text: msg, cwd, errorKind: "runtime" };
		}
	} catch (e) {
		return {
			ok: false,
			text: `Script path error: ${e instanceof Error ? e.message : String(e)}`,
			errorKind: "precondition",
		};
	}
}

/**
 * AgentTool wrapper over {@link runScript}. Preserves the current LLM behaviour: a pre-condition
 * error still throws (so the agent sees an error and can self-heal), while a runtime failure returns
 * its output as text for the model to decide whether to fix and retry.
 */
export const runTool: AgentTool<typeof schema> = {
	name: "run",
	label: "Run in-project script",
	description:
		"Run an existing script inside a data subproject (only python3 .py or node .js/.mjs), used to run build/refresh scripts defined by the project's conventions (e.g. build.py). No shell is used; args are passed directly; the working directory is locked to the script's subproject root with a 60-second timeout. Only run it when the project's conventions (AGENTS.md) require it; skip if the project has no such script.",
	parameters: schema,
	async execute(_toolCallId, { script, args = [] }) {
		const res = runScript(script, args);
		if (!res.ok && res.errorKind === "precondition") throw new Error(res.text);
		return {
			content: [{ type: "text", text: res.text }],
			details: { script, cwd: res.cwd, failed: !res.ok },
		};
	},
};
