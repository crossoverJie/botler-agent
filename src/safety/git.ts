import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, type Dirent } from "node:fs";
import { join, resolve } from "node:path";
import { CONFIG } from "../config.ts";

type GitResult = { ok: boolean; stdout: string; stderr: string };

/** git's useful error signal is on the first line (Permission denied (publickey) / Host key verification failed / no upstream). */
function firstLine(s: string): string {
	return s.split("\n").map((l) => l.trim()).find((l) => l.length > 0) ?? "";
}

/** Run git with captured stdout/stderr; never throws. */
function git(cwd: string, args: string[]): GitResult {
	try {
		const stdout = execFileSync("git", args, { cwd, stdio: ["ignore", "pipe", "pipe"] }).toString();
		return { ok: true, stdout, stderr: "" };
	} catch (e) {
		const err = e as { stdout?: Buffer | string; stderr?: Buffer | string; message?: string };
		const stderr = firstLine((err.stderr ?? "").toString());
		return { ok: false, stdout: (err.stdout ?? "").toString(), stderr: stderr || firstLine(err.message ?? "") };
	}
}

/**
 * Skips are reported once per (repo, reason) per process: a repo with no origin remote or an unborn
 * branch must not warn on every single write task.
 */
const skipWarned = new Set<string>();

function warnSkip(reason: string, name: string, message: string): void {
	const key = `${reason}:${name}`;
	if (skipWarned.has(key)) return;
	skipWarned.add(key);
	console.error(message);
}

function reportPush(name: string, out: GitResult): void {
	if (out.ok) console.log(`[git] pushed ${name}`);
	else console.error(`[git] push failed for ${name}: ${out.stderr}`);
}

/**
 * Push only when there is something to push. `rev-list --count @{u}..HEAD` walks local objects only
 * (no network round trip) and fails when `@{u}` cannot be resolved.
 */
function pushIfPending(dir: string, name: string): void {
	const ahead = git(dir, ["rev-list", "--count", "@{u}..HEAD"]);
	if (ahead.ok) {
		if (Number(ahead.stdout.trim() || "0") > 0) reportPush(name, git(dir, ["push"]));
		return;
	}

	// `rev-list` failed, and the two causes must not be conflated:
	//  - no upstream configured yet → one `push -u origin HEAD` is correct;
	//  - upstream configured but its tracking ref is gone (remote branch renamed/deleted, `git remote
	//    prune`) → `push -u origin HEAD` would silently rewrite branch.<name>.merge and create a new
	//    branch upstream. Only warn.
	const branch = git(dir, ["symbolic-ref", "--short", "HEAD"]);
	if (!branch.ok) {
		// `push origin HEAD` on a detached HEAD would create a branch literally named HEAD upstream.
		warnSkip("detached", name, `[git] skip push for ${name}: detached HEAD`);
		return;
	}

	if (!git(dir, ["remote", "get-url", "origin"]).ok) {
		warnSkip("no-origin", name, `[git] skip push for ${name}: no origin remote`);
		return;
	}

	const mergeRef = git(dir, ["config", "--get", `branch.${branch.stdout.trim()}.merge`]);
	if (mergeRef.ok && mergeRef.stdout.trim()) {
		warnSkip(
			"stale-upstream",
			name,
			`[git] skip push for ${name}: upstream ${mergeRef.stdout.trim()} is configured but unresolvable (${ahead.stderr})`,
		);
		return;
	}

	// Unborn branch: `push -u` would fail with "src refspec HEAD does not match any" on every round.
	if (!git(dir, ["rev-parse", "--verify", "--quiet", "HEAD"]).ok) {
		warnSkip("unborn", name, `[git] skip push for ${name}: branch has no commits yet`);
		return;
	}

	// The first push must be `-u`: a bare `push origin HEAD` only creates the remote-tracking ref and
	// leaves branch.<name>.merge unset, so @{u} would never resolve and every dispatch would push once
	// for nothing. `-u` here sets up tracking; it never rewrites an existing one (checked above).
	reportPush(name, git(dir, ["push", "-u", "origin", "HEAD"]));
}

/**
 * Commit changed data subprojects. Each first-level subdir under DATA_ROOT, if it is its own git repo
 * and has non-empty `git status --porcelain`, gets add + commit. The glue layer does the commit; the agent never touches bash.
 * When GIT_PUSH=1, every repo with pending commits is pushed afterwards — decoupled from this round's
 * commit result, so a commit rejected by a hook still lets a previous round's failed push self-heal.
 * Push failures are logged (first stderr line) and only warn, never fail the task.
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

		const added = git(dir, ["add", "-A"]);
		if (!added.ok) {
			console.error(`[git] add failed for ${d.name}: ${added.stderr}`);
			continue;
		}

		const status = git(dir, ["status", "--porcelain"]);
		if (!status.ok) {
			console.error(`[git] status failed for ${d.name}: ${status.stderr}`);
			continue;
		}

		if (status.stdout.trim()) {
			const commit = git(dir, ["commit", "-m", msg]);
			if (commit.ok) console.log(`[git] committed ${d.name}: ${msg}`);
			else console.error(`[git] commit failed for ${d.name}: ${commit.stderr}`);
		}

		// Independent of this round's commit result: any leftover unpushed commit gets one push attempt.
		if (CONFIG.gitPush) pushIfPending(dir, d.name);
	}
}
