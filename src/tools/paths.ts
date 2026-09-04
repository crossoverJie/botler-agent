import { resolve, sep, basename, dirname, join } from "node:path";
import { existsSync, realpathSync, readdirSync } from "node:fs";
import { CONFIG } from "../config.ts";

/**
 * Data directory allowlist: the first-level (non-hidden) subdirs of DATA_ROOT are the operable projects.
 * The app directory (with .env / source code) is entirely outside DATA_ROOT, and the data directory holds no secrets,
 * so the allowlist is part of defense in depth.
 */
function computeAllowed(): string[] {
	if (!CONFIG.dataRoot) {
		throw new Error(
			"DATA_ROOT is not set. Configure DATA_ROOT (the data root operated by the Agent) in ~/.botler-agent/.env or the source .env.",
		);
	}
	const root = resolve(CONFIG.dataRoot);
	if (!existsSync(root)) {
		throw new Error(`DATA_ROOT does not exist: ${root}`);
	}
	const names = readdirSync(root, { withFileTypes: true })
		.filter((d) => d.isDirectory() && !d.name.startsWith("."))
		.map((d) => resolve(root, d.name));
	if (names.length === 0) {
		throw new Error(`DATA_ROOT has no subproject directories: ${root}`);
	}
	return names;
}

const ALLOWED = computeAllowed();
// If the root dir exists, realpath it once; the target file may not yet exist (new-file scenario), so we cannot realpath it
const ROOTS = ALLOWED.map((d) => (existsSync(d) ? resolve(d) : d));

export interface SafePathOptions {
	/** Whether to realpath the deepest existing ancestor, to prevent symlink escapes outside the allowlist. Default on. */
	followSymlinks?: boolean;
	/** Optional task-local subset: when set, the resolved path must fall inside one of these projects (on top of the global allowlist). */
	projects?: readonly string[];
}

/** Enforce the task-local subset (additive on top of the global allowlist); no-op for framework calls without a subset. */
function assertSelectedProject(abs: string, projects: readonly string[] | undefined): void {
	if (!projects) return;
	const selected = new Set(projects);
	const matchedRoot = ROOTS.find((root) => abs === root || abs.startsWith(root + sep));
	if (!matchedRoot || !selected.has(basename(matchedRoot))) {
		throw new Error(`Path not selected for this task: ${abs}`);
	}
}

/**
 * Resolve a path relative to DATA_ROOT into an absolute path and verify it falls within the allowlist.
 * Relative paths are resolved against DATA_ROOT; absolute paths are kept as-is (but still throw if out of bounds).
 * Uses `root + sep` prefix matching to avoid `/agent2`, `/agent-bak` slipping in.
 */
export function safePath(p: string, opts: SafePathOptions = {}): string {
	const abs = resolve(CONFIG.dataRoot, p);
	const ok = ROOTS.some((root) => abs === root || abs.startsWith(root + sep));
	if (!ok) {
		throw new Error(`Path out of bounds (not within the allowed directory): ${p}`);
	}
	assertSelectedProject(abs, opts.projects);

	if (opts.followSymlinks ?? true) {
		// realpath the "deepest existing ancestor", then concatenate the remaining path back, and verify once more,
		// to prevent a symlink inside a data subproject from pointing outside (e.g. /etc/passwd). The data dir holds no trusted external content, so the cost is minimal.
		let anc = abs;
		const tail: string[] = [];
		while (!existsSync(anc)) {
			tail.unshift(basename(anc));
			anc = dirname(anc);
		}
		const real = join(realpathSync(anc), ...tail);
		if (!ROOTS.some((root) => real === root || real.startsWith(root + sep))) {
			throw new Error(`Path out of bounds (symlink escape): ${p}`);
		}
		assertSelectedProject(real, opts.projects);
		return real;
	}

	return abs;
}

/** Determine which data subproject a (resolved) absolute path belongs to, or null if none. */
export function projectOf(absPath: string): string | null {
	for (const root of ROOTS) {
		if (absPath === root || absPath.startsWith(root + sep)) return basename(root);
	}
	return null;
}
