import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, type Dirent } from "node:fs";
import { join, resolve } from "node:path";
import { CONFIG } from "../config.ts";

/**
 * Commit changed data subprojects. Each first-level subdir under DATA_ROOT, if it is its own git repo
 * and has non-empty `git status --porcelain`, gets add + commit. The glue layer does the commit; the agent never touches bash.
 * When GIT_PUSH=1, additionally push (failure is only a warning, not a blocker).
 */
export function commitIfChanged(msg: string, projects?: readonly string[]): void {
	if (!CONFIG.dataRoot) return;
	const root = resolve(CONFIG.dataRoot);
	if (!existsSync(root)) return;

	let entries: Dirent[];
	try {
		entries = readdirSync(root, { withFileTypes: true });
	} catch {
		return;
	}

	for (const d of entries) {
		if (!d.isDirectory() || d.name.startsWith(".")) continue;
		if (projects && !projects.includes(d.name)) continue;
		const dir = join(root, d.name);
		if (!existsSync(join(dir, ".git"))) continue;
		try {
			execFileSync("git", ["add", "-A"], { cwd: dir, stdio: "ignore" });
			const status = execFileSync("git", ["status", "--porcelain"], { cwd: dir }).toString();
			if (status.trim()) {
				execFileSync("git", ["commit", "-m", msg], { cwd: dir, stdio: "ignore" });
				console.log(`[git] committed ${d.name}: ${msg}`);
				if (CONFIG.gitPush) {
					try {
						execFileSync("git", ["push"], { cwd: dir, stdio: "ignore" });
						console.log(`[git] pushed ${d.name}`);
					} catch (e) {
						console.error(`[git] push failed for ${d.name}：${e instanceof Error ? e.message : e}`);
					}
				}
			}
		} catch (e) {
			const msgText = e instanceof Error ? e.message : String(e);
			console.error(`[git] commit failed for ${d.name}: ${msgText}`);
		}
	}
}
