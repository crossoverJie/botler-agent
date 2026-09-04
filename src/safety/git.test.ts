import { before, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

let git: typeof import("./git.ts");

let root = "";

function log(project: string): string {
	try {
		return execFileSync("git", ["log", "--oneline"], { cwd: path.join(root, project) }).toString().trim();
	} catch {
		return "";
	}
}

before(async () => {
	root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-git-")));
	for (const name of ["cook", "vocab"]) {
		const dir = path.join(root, name);
		fs.mkdirSync(dir);
		execFileSync("git", ["init", "-q"], { cwd: dir });
		execFileSync("git", ["config", "user.email", "test@example.com"], { cwd: dir });
		execFileSync("git", ["config", "user.name", "Test"], { cwd: dir });
	}
	process.env.DATA_ROOT = root;
	process.env.GIT_PUSH = "0";
	git = await import("./git.ts");
});

test("commitIfChanged scoped to a selected repo only commits that repo", () => {
	for (const name of ["cook", "vocab"]) {
		fs.writeFileSync(path.join(root, name, "data.json"), '{"v":1}');
	}

	git.commitIfChanged("test: scoped commit", ["cook"]);

	assert.match(log("cook"), /test: scoped commit/);
	assert.equal(log("vocab"), "");
});

test("commitIfChanged with no projects commits every changed repo", () => {
	fs.writeFileSync(path.join(root, "vocab", "second.json"), '{"v":2}');

	git.commitIfChanged("test: all commit");

	assert.match(log("vocab"), /test: all commit/);
});

test("commitIfChanged with an empty set commits nothing", () => {
	fs.writeFileSync(path.join(root, "cook", "third.json"), '{"v":3}');

	const beforeCook = log("cook");
	git.commitIfChanged("test: empty", []);
	assert.equal(log("cook"), beforeCook);
});
