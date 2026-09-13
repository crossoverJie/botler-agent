import { before, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// account.ts (via config.ts) computes USER_CONFIG_DIR from BOTLER_CONFIG_DIR at module load, so
// point it at a fresh temp dir before importing anything that reads the real ~/.botler-agent.
let account: typeof import("./account.ts");
let context: typeof import("./context.ts");
let contacts: typeof import("../../push/contacts.ts");

before(async () => {
	const tmpDir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "botler-account-test-")));
	process.env.BOTLER_CONFIG_DIR = tmpDir;
	account = await import("./account.ts");
	context = await import("./context.ts");
	contacts = await import("../../push/contacts.ts");
});

test("applyNewAccount saves the account and clears contexts/contacts/sync without deleting the new account", () => {
	// Pre-populate the OLD owner's persisted state.
	account.saveAccount({ token: "old-token", baseUrl: "https://old", userId: "old-user" });
	account.saveSyncBuf("cursor-123");
	context.updateContext("old-user", "old-context-token");
	contacts.recordContact("wechat", "old-user");

	assert.equal(account.resolveAccount().token, "old-token");
	assert.equal(account.loadSyncBuf(), "cursor-123");
	assert.ok(context.getContext("old-user"));
	assert.deepEqual(contacts.getContacts("wechat"), ["old-user"]);

	account.applyNewAccount({ token: "new-token", baseUrl: "https://new", userId: "new-user" });

	// New account persisted (and stamped), not deleted.
	const saved = account.loadAccount();
	assert.equal(saved?.token, "new-token");
	assert.equal(saved?.userId, "new-user");
	assert.equal(saved?.baseUrl, "https://new");
	assert.ok(saved?.savedAt);
	assert.equal(account.resolveAccount().token, "new-token");

	// Old owner's stale state is dropped.
	assert.equal(context.getContext("old-user"), undefined);
	assert.deepEqual(contacts.getContacts("wechat"), []);
	assert.equal(account.loadSyncBuf(), undefined);
});
