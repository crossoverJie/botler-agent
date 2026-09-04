import { readFileSync, existsSync, readdirSync, type Dirent } from "node:fs";
import { resolve, join, extname, relative } from "node:path";
import { CONFIG } from "../config.ts";
import { listProjectDirs } from "../prompts/system-prompt.ts";

export interface ValidationResult {
	ok: boolean;
	/** Self-contained fix instruction on validation failure: point to the file + exact location + how to change it. */
	fix?: string;
}

/** Directory names skipped during traversal (build artifacts / dependencies, not data dirs). */
const SKIP_DIRS = new Set(["node_modules", ".git", "dist", "public", ".pi", ".codebuddy"]);

function* walkJson(dir: string): Generator<string> {
	let entries: Dirent[];
	try {
		entries = readdirSync(dir, { withFileTypes: true });
	} catch {
		return;
	}
	for (const e of entries) {
		if (e.name.startsWith(".")) continue; // Hidden dirs/files (.git, etc.)
		const full = join(dir, e.name);
		if (e.isDirectory()) {
			if (SKIP_DIRS.has(e.name)) continue;
			yield* walkJson(full);
		} else if (e.isFile() && extname(e.name).toLowerCase() === ".json") {
			yield full;
		}
	}
}

/** Collect JSON data files within the selected project dirs, returning paths relative to DATA_ROOT. */
function collectJsonFiles(projects?: readonly string[]): string[] {
	if (!CONFIG.dataRoot) return [];
	const root = resolve(CONFIG.dataRoot);
	if (!existsSync(root)) return [];
	const rel: string[] = [];
	for (const d of readdirSync(root, { withFileTypes: true })) {
		if (!d.isDirectory() || d.name.startsWith(".")) continue;
		if (projects && !projects.includes(d.name)) continue;
		for (const f of walkJson(join(root, d.name))) {
			rel.push(relative(root, f));
		}
	}
	return rel;
}

/**
 * Post-write validation: all data JSON must be valid JSON.
 * The write tool guarantees valid serialization; edit is text replacement and may break JSON syntax — this is the safety net.
 * Business-semantic correctness (field values, aggregation consistency, etc.) is guaranteed by each subproject's AGENTS.md conventions and its own scripts; the framework does not hardcode it.
 *
 * `projects` scopes validation to the selected set (an unrelated dirty/broken sibling must not
 * block a multi-project task); `undefined` validates every project. Names are verified against
 * `listProjectDirs()` so a ".." or unknown name can never escape the DATA_ROOT boundary.
 */
export function validateState(projects?: readonly string[]): ValidationResult {
	const scoped = projects ? listProjectDirs().filter((n) => projects.includes(n)) : undefined;
	for (const rel of collectJsonFiles(scoped)) {
		let raw: string;
		try {
			raw = readFileSync(resolve(CONFIG.dataRoot, rel), "utf8");
		} catch {
			continue;
		}
		try {
			JSON.parse(raw);
		} catch (e) {
			const msg = e instanceof Error ? e.message : String(e);
			return {
				ok: false,
				fix: `${rel} is not valid JSON (parse failed: ${msg}). Read the file, fix the syntax error, and write it back. Do not create a new file.`,
			};
		}
	}
	return { ok: true };
}
