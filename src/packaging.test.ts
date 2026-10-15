import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

// Guards the container entrypoint contract: bin/botler.mjs imports tsx/esm/api at
// runtime, and the Dockerfile installs with `npm ci --omit=dev`. If tsx is ever moved
// back into devDependencies the built image fails to start with
// ERR_MODULE_NOT_FOUND: Cannot find package 'tsx'. Keep tsx a runtime dependency.
const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8")) as {
	dependencies?: Record<string, string>;
	devDependencies?: Record<string, string>;
};

test("tsx is a runtime dependency (required by the Docker entrypoint)", () => {
	assert.ok(pkg.dependencies?.tsx, "tsx must be in `dependencies`");
	assert.ok(!pkg.devDependencies?.tsx, "tsx must NOT be in `devDependencies`");
});
