import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

let validate: typeof import("./validate.ts");

before(async () => {
	const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-validate-")));
	fs.mkdirSync(path.join(root, "cook", "data"), { recursive: true });
	fs.mkdirSync(path.join(root, "vocab", "data"), { recursive: true });
	fs.writeFileSync(path.join(root, "cook", "data", "a.json"), '{"ok":true}');
	fs.writeFileSync(path.join(root, "vocab", "data", "broken.json"), '{"oops":');
	process.env.DATA_ROOT = root;
	validate = await import("./validate.ts");
});

test("validateState with no projects scans everything and reports the broken project", () => {
	const r = validate.validateState();
	assert.equal(r.ok, false);
	assert.match(r.fix ?? "", /broken\.json/);
});

test("validateState scoped to a valid selected project passes even when a sibling is broken", () => {
	const r = validate.validateState(["cook"]);
	assert.equal(r.ok, true);
});

test("validateState scoped to the broken project fails", () => {
	const r = validate.validateState(["vocab"]);
	assert.equal(r.ok, false);
	assert.match(r.fix ?? "", /broken\.json/);
});

test("validateState with an empty set validates nothing", () => {
	assert.equal(validate.validateState([]).ok, true);
});

test("validateState ignores an unknown project name and does not traverse outside DATA_ROOT", () => {
	// ".." and a nonexistent name must be dropped, not resolved into the filesystem.
	assert.equal(validate.validateState([".."]).ok, true);
	assert.equal(validate.validateState(["nonexistent"]).ok, true);
});
