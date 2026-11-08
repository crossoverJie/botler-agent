import { after, before, describe, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// Own module cycle (GIT_PUSH=1 at import time) — node:test runs each file in its own process,
// so git.test.ts's GIT_PUSH=0 import does not leak into this file.
let git: typeof import("./git.ts");

let tmp = "";
let data = "";
let realGit = "";
let shimLog = "";
const logs: string[] = [];

const origLog = console.log;
const origError = console.error;

function dir(name: string): string {
	return path.join(data, name);
}

function run(cwd: string, args: string[]): string {
	return execFileSync(realGit, args, { cwd, encoding: "utf8" }).trim();
}

function tryRun(cwd: string, args: string[]): string {
	try {
		return run(cwd, args);
	} catch {
		return "";
	}
}

function initRepo(name: string, opts: { remote?: string; commit?: boolean } = {}): void {
	const d = dir(name);
	fs.mkdirSync(d);
	run(d, ["init", "-q", "-b", "main"]);
	run(d, ["config", "user.email", "test@example.com"]);
	run(d, ["config", "user.name", "Test"]);
	if (opts.commit !== false) {
		fs.writeFileSync(path.join(d, "init.txt"), "init\n");
		run(d, ["add", "-A"]);
		run(d, ["commit", "-q", "-m", "init"]);
	}
	if (opts.remote) run(d, ["remote", "add", "origin", opts.remote]);
}

function bareRemote(name: string): string {
	const bare = path.join(tmp, `remote-${name}.git`);
	run(tmp, ["init", "-q", "--bare", bare]);
	return bare;
}

/** Invoke commitIfChanged while recording console output and every git invocation (via the shim). */
function invoke(msg: string, projects?: readonly string[]): { out: string; calls: string[] } {
	logs.length = 0;
	fs.writeFileSync(shimLog, "");
	git.commitIfChanged(msg, projects);
	const calls = fs.readFileSync(shimLog, "utf8").split("\n").filter(Boolean);
	return { out: logs.join("\n"), calls };
}

function pushes(calls: string[]): string[] {
	return calls.filter((c) => c.split(/\s+/)[0] === "push");
}

function ahead(name: string): number {
	return Number(tryRun(dir(name), ["rev-list", "--count", "@{u}..HEAD"]) || "-1");
}

before(async () => {
	tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-git-push-")));
	data = path.join(tmp, "data");
	fs.mkdirSync(data);

	realGit = execFileSync("sh", ["-c", "command -v git"], { encoding: "utf8" }).trim();

	// Isolate from the developer's global/system git config (e.g. a machine-wide hooksPath).
	const cfg = path.join(tmp, "gitconfig");
	fs.writeFileSync(cfg, "");
	process.env.GIT_CONFIG_GLOBAL = cfg;
	process.env.GIT_CONFIG_NOSYSTEM = "1";

	// shim `git` on PATH: records argv, then delegates to the real binary. Lets us assert that a
	// no-op round performs no network push at all.
	const bin = path.join(tmp, "bin");
	fs.mkdirSync(bin);
	shimLog = path.join(tmp, "git-calls.log");
	const shim = path.join(bin, "git");
	fs.writeFileSync(shim, `#!/bin/sh\nprintf '%s\\n' "$*" >> "${shimLog}"\nexec "${realGit}" "$@"\n`);
	fs.chmodSync(shim, 0o755);
	process.env.PATH = `${bin}:${process.env.PATH}`;

	initRepo("a", { remote: bareRemote("a") });
	initRepo("b");
	initRepo("c", { remote: bareRemote("c") });
	initRepo("d", { remote: bareRemote("d") });

	process.env.DATA_ROOT = data;
	process.env.GIT_PUSH = "1";

	console.log = (...args: unknown[]) => logs.push(args.map(String).join(" "));
	console.error = (...args: unknown[]) => logs.push(args.map(String).join(" "));

	git = await import("./git.ts");
});

after(() => {
	console.log = origLog;
	console.error = origError;
});

// Ordered on purpose: the cases build on each other's repo state (A1 → F), so run them serially.
describe("commit/push loop", { concurrency: 1 }, () => {
	test("A1 first round without upstream establishes tracking with a single -u push", () => {
		const { out } = invoke("test: first push", ["a"]);

		assert.match(out, /\[git\] pushed a/);
		assert.equal(run(dir("a"), ["config", "--get", "branch.main.merge"]), "refs/heads/main");
		assert.equal(ahead("a"), 0);
	});

	test("A2/A3 subsequent rounds without changes perform no push at all", () => {
		for (const round of [2, 3]) {
			const { out, calls } = invoke(`test: round ${round}`, ["a"]);
			assert.equal(out, "", `round ${round} should log nothing`);
			assert.deepEqual(pushes(calls), [], `round ${round} should not push`);
			// add -A + status --porcelain + rev-list --count
			assert.equal(calls.length, 3, `round ${round} calls: ${calls.join(" | ")}`);
		}
	});

	test("B a new change is committed and pushed in one round", () => {
		fs.writeFileSync(path.join(dir("a"), "note.json"), '{"v":1}');

		const { out } = invoke("test: commit and push", ["a"]);

		assert.match(out, /\[git\] committed a/);
		assert.match(out, /\[git\] pushed a/);
		assert.equal(ahead("a"), 0);
	});

	test("C a leftover unpushed commit is pushed even with no new changes", () => {
		invoke("test: establish tracking", ["c"]);
		fs.writeFileSync(path.join(dir("c"), "leftover.json"), '{"v":1}');
		run(dir("c"), ["add", "-A"]);
		run(dir("c"), ["commit", "-q", "-m", "leftover"]);
		assert.equal(ahead("c"), 1);

		const { out, calls } = invoke("test: self-heal", ["c"]);

		assert.match(out, /\[git\] pushed c/);
		assert.equal(pushes(calls).length, 1);
		assert.equal(ahead("c"), 0);
	});

	test("D the projects filter still scopes the whole round", () => {
		fs.writeFileSync(path.join(dir("b"), "untracked.json"), '{"v":1}');

		invoke("test: scoped", ["a"]);

		assert.match(run(dir("b"), ["status", "--porcelain"]), /untracked\.json/);
		assert.equal(run(dir("b"), ["rev-list", "--count", "HEAD"]), "1"); // only the init commit
	});

	test("E a detached HEAD repo is skipped so no branch named HEAD appears upstream", () => {
		fs.writeFileSync(path.join(dir("d"), "x.json"), '{"v":1}');
		run(dir("d"), ["add", "-A"]);
		run(dir("d"), ["commit", "-q", "-m", "init 2"]);
		run(dir("d"), ["checkout", "-q", "--detach"]);

		const { out } = invoke("test: detached", ["d"]);

		assert.match(out, /\[git\] skip push for d: detached HEAD/);
		assert.doesNotMatch(tryRun(dir("d"), ["ls-remote", "origin"]), /refs\/heads\/HEAD/);
	});

	test("F a rejected commit still lets a leftover unpushed commit go out", () => {
		fs.writeFileSync(path.join(dir("c"), "pending.json"), '{"v":2}');
		run(dir("c"), ["add", "-A"]);
		run(dir("c"), ["commit", "-q", "-m", "leftover 2"]);
		assert.equal(ahead("c"), 1);

		const hook = path.join(dir("c"), ".git", "hooks", "pre-commit");
		fs.writeFileSync(hook, '#!/bin/sh\necho "hook says no" >&2\nexit 1\n');
		fs.chmodSync(hook, 0o755);
		fs.writeFileSync(path.join(dir("c"), "rejected.json"), '{"v":3}');

		const { out } = invoke("test: rejected", ["c"]);

		assert.match(out, /\[git\] commit failed for c: hook says no/);
		assert.match(out, /\[git\] pushed c/);
		assert.equal(ahead("c"), 0);
		assert.doesNotMatch(run(dir("c"), ["log", "--all", "--pretty=%s"]), /rejected/);
	});

	test("G a configured but unresolvable upstream is never rewritten by -u", () => {
		const d = dir("e");
		initRepo("e", { remote: bareRemote("e") });
		// Upstream configured, but its tracking ref does not exist (remote branch renamed/pruned).
		run(d, ["config", "branch.main.remote", "origin"]);
		run(d, ["config", "branch.main.merge", "refs/heads/master"]);

		const { out, calls } = invoke("test: stale upstream", ["e"]);

		assert.match(out, /\[git\] skip push for e: upstream refs\/heads\/master is configured but unresolvable/);
		assert.deepEqual(pushes(calls), []);
		assert.equal(run(d, ["config", "--get", "branch.main.merge"]), "refs/heads/master");
		assert.equal(tryRun(d, ["ls-remote", "origin"]), "");
	});

	test("H a repo without an origin remote is skipped and warns only once", () => {
		const first = invoke("test: no origin", ["b"]);
		assert.match(first.out, /\[git\] skip push for b: no origin remote/);
		assert.deepEqual(pushes(first.calls), []);

		const second = invoke("test: no origin again", ["b"]);
		assert.equal(second.out, "", "the same skip must not be reported again");
		assert.deepEqual(pushes(second.calls), []);
	});

	test("I an unborn branch is skipped instead of failing a -u push every round", () => {
		initRepo("f", { remote: bareRemote("f"), commit: false });

		const { out, calls } = invoke("test: unborn", ["f"]);

		assert.match(out, /\[git\] skip push for f: branch has no commits yet/);
		assert.deepEqual(pushes(calls), []);
		assert.equal(tryRun(dir("f"), ["ls-remote", "origin"]), "");
	});

	test("J an undefined projects list still commits every changed repo", () => {
		const before = run(dir("b"), ["rev-list", "--count", "HEAD"]);
		fs.writeFileSync(path.join(dir("b"), "unscoped.json"), '{"v":1}');

		const { out } = invoke("test: unscoped", undefined);

		assert.match(out, /\[git\] committed b/);
		assert.equal(run(dir("b"), ["rev-list", "--count", "HEAD"]), String(Number(before) + 1));
	});
});
