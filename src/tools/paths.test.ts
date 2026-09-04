import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

let paths: typeof import("./paths.ts");

before(async () => {
	const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-paths-")));
	fs.mkdirSync(path.join(root, "cook"));
	fs.mkdirSync(path.join(root, "vocab"));
	fs.writeFileSync(path.join(root, "cook", "a.json"), "{}");
	fs.writeFileSync(path.join(root, "vocab", "b.json"), "{}");
	process.env.DATA_ROOT = root;
	paths = await import("./paths.ts");
});

test("safePath with no projects stays unrestricted across all projects", () => {
	assert.ok(paths.safePath("cook/a.json").endsWith(path.join("cook", "a.json")));
	assert.ok(paths.safePath("vocab/b.json").endsWith(path.join("vocab", "b.json")));
});

test("safePath authorizes a path inside the selected project", () => {
	assert.ok(paths.safePath("cook/a.json", { projects: ["cook"] }).endsWith(path.join("cook", "a.json")));
});

test("safePath rejects a path inside an unselected project", () => {
	assert.throws(() => paths.safePath("vocab/b.json", { projects: ["cook"] }), /not selected/);
});

test("safePath rejects a path out of bounds regardless of the selected set", () => {
	assert.throws(() => paths.safePath("../outside.json", { projects: ["cook"] }), /out of bounds/);
	assert.throws(() => paths.safePath("cook/../../../etc/passwd", { projects: ["cook"] }), /out of bounds/);
});

test("safePath rejects a sibling prefix that is not a real project", () => {
	// "cook2" is not an allowlisted project; it must never match the "cook" root.
	assert.throws(() => paths.safePath("cook2/x.json", { projects: ["cook"] }), /out of bounds/);
});

test("safePath rejects a traversal from a selected project into an unselected one", () => {
	assert.throws(() => paths.safePath("cook/../vocab/b.json", { projects: ["cook"] }), /not selected/);
});

test("safePath authorizes a multi-project set", () => {
	assert.ok(paths.safePath("cook/a.json", { projects: ["cook", "vocab"] }).endsWith(path.join("cook", "a.json")));
	assert.ok(paths.safePath("vocab/b.json", { projects: ["cook", "vocab"] }).endsWith(path.join("vocab", "b.json")));
});

test("safePath rejects a symlink escaping into an unselected project", () => {
	fs.symlinkSync(path.join(process.env.DATA_ROOT!, "vocab"), path.join(process.env.DATA_ROOT!, "cook", "link"));
	assert.throws(() => paths.safePath("cook/link/b.json", { projects: ["cook"] }), /not selected/);
});

test("projectOf returns the owning project name", () => {
	const abs = paths.safePath("cook/a.json");
	assert.equal(paths.projectOf(abs), "cook");
});
