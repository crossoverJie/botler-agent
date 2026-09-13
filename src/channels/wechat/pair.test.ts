import { test } from "node:test";
import assert from "node:assert/strict";
import {
	createPairing,
	computeInheritedRefreshCount,
	renderPairQr,
	PAIR_TTL_MS,
	type PairSession,
} from "./pair.ts";
import type { StatusResponse } from "./login.ts";

/** Minimal QR response; the machine only reads qrcode / qrcode_img_content. */
const FETCH = async () => ({ qrcode: "q1", qrcode_img_content: "content1" });

/** A poll mock that replays a fixed sequence, holding the last status forever. */
function makePoll(seq: StatusResponse[]): (baseUrl: string, qrcode: string) => Promise<StatusResponse> {
	let i = 0;
	return async () => seq[Math.min(i++, seq.length - 1)] ?? { status: "wait" };
}

test("pending → scanned → confirmed applies the account", async () => {
	let applied: { token?: string; baseUrl?: string; userId?: string } | null = null;
	const machine = createPairing({ auto: false,
		fetchQR: FETCH,
		poll: makePoll([
			{ status: "scaned" },
			{ status: "confirmed", bot_token: "tok", ilink_bot_id: "bot1", baseurl: "https://x", ilink_user_id: "u1" },
		]),
		onConfirmed: (a) => {
			applied = a;
		},
	});

	const r = await machine.start();
	assert.equal(r.state, "pending");
	assert.equal(machine.status()?.state, "pending");

	await machine.step(); // scaned
	assert.equal(machine.status()?.state, "scanned");
	assert.ok(machine.status()?.scannedAt != null);

	await machine.step(); // confirmed
	assert.equal(machine.status()?.state, "confirmed");
	assert.deepEqual(applied, { token: "tok", baseUrl: "https://x", userId: "u1" });
});

test("wait keeps the session pending", async () => {
	const machine = createPairing({ auto: false, fetchQR: FETCH, poll: async () => ({ status: "wait" }) });
	await machine.start();
	const done = await machine.step();
	assert.equal(done, false);
	assert.equal(machine.status()?.state, "pending");
});

test("scaned_but_redirect switches baseUrl and keeps polling", async () => {
	const machine = createPairing({ auto: false,
		fetchQR: FETCH,
		poll: async () => ({ status: "scaned_but_redirect", redirect_host: "redirect.example.com" }),
	});
	await machine.start();
	const done = await machine.step();
	assert.equal(done, false);
	assert.equal(machine.status()?.baseUrl, "https://redirect.example.com");
	assert.equal(machine.status()?.state, "pending");
});

test("confirmed without token / bot id marks the session error (no side effect)", async () => {
	let applied = false;
	const machine = createPairing({ auto: false,
		fetchQR: FETCH,
		poll: async () => ({ status: "confirmed" }),
		onConfirmed: () => {
			applied = true;
		},
	});
	await machine.start();
	const done = await machine.step();
	assert.equal(done, true);
	assert.equal(machine.status()?.state, "error");
	assert.equal(applied, false);
});

test("a throwing onConfirmed marks the session error instead of crashing", async () => {
	const machine = createPairing({ auto: false,
		fetchQR: FETCH,
		poll: async () => ({ status: "confirmed", bot_token: "tok", ilink_bot_id: "bot1" }),
		onConfirmed: () => {
			throw new Error("disk full");
		},
	});
	await machine.start();
	const done = await machine.step();
	assert.equal(done, true);
	assert.equal(machine.status()?.state, "error");
	assert.equal(machine.status()?.error, "disk full");
});

test("expired increments refreshCount; start inherits it and caps at MAX_QR_REFRESH_COUNT", async () => {
	const machine = createPairing({ auto: false, fetchQR: FETCH, poll: async () => ({ status: "expired" }) });

	await machine.start(); // fresh: refreshCount 0
	await machine.step(); // expired → 1
	assert.equal(machine.status()?.state, "expired");
	assert.equal(machine.status()?.refreshCount, 1);

	await machine.start(); // inherit 1, issue QR #2
	assert.equal(machine.status()?.refreshCount, 1);
	await machine.step(); // expired → 2

	await machine.start(); // inherit 2, issue QR #3
	assert.equal(machine.status()?.refreshCount, 2);
	await machine.step(); // expired → 3

	await machine.start(); // inherit 3, issue QR #4 (last allowed refresh)
	assert.equal(machine.status()?.refreshCount, 3);
	await machine.step(); // expired → 4

	const r = await machine.start(); // inherit 4 > MAX → error, no new QR
	assert.equal(r.state, "error");
	assert.equal(machine.status()?.state, "error");
	assert.ok(machine.status()?.error);
});

test("TTL timeout marks the session expired and increments refreshCount", async () => {
	let t = 0;
	const machine = createPairing({ auto: false,
		fetchQR: FETCH,
		poll: async () => ({ status: "wait" }),
		now: () => t,
	});
	await machine.start();
	assert.equal(machine.status()?.state, "pending");
	t = PAIR_TTL_MS + 1;
	const done = await machine.step();
	assert.equal(done, true);
	assert.equal(machine.status()?.state, "expired");
	assert.equal(machine.status()?.refreshCount, 1);
});

test("cancel resets the session to idle", async () => {
	const machine = createPairing({ auto: false, fetchQR: FETCH, poll: async () => ({ status: "wait" }) });
	await machine.start();
	assert.equal(machine.status()?.state, "pending");
	machine.cancel();
	assert.equal(machine.status(), null);
});

test("computeInheritedRefreshCount inherits only from an expired session", () => {
	const base: PairSession = {
		state: "pending",
		qrcode: "q",
		content: "c",
		baseUrl: "https://x",
		expiresAt: 0,
		refreshCount: 5,
	};
	assert.equal(computeInheritedRefreshCount(null), 0);
	assert.equal(computeInheritedRefreshCount({ ...base, state: "pending" }), 0);
	assert.equal(computeInheritedRefreshCount({ ...base, state: "scanned" }), 0);
	assert.equal(computeInheritedRefreshCount({ ...base, state: "confirmed" }), 0);
	assert.equal(computeInheritedRefreshCount({ ...base, state: "error" }), 0);
	assert.equal(computeInheritedRefreshCount({ ...base, state: "expired" }), 5);
});

test("renderPairQr returns a PNG data URL", async () => {
	const url = await renderPairQr("https://example.com/some-content");
	assert.match(url, /^data:image\/png;base64,/);
});
