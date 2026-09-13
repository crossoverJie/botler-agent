/**
 * WeChat QR pairing session (WebUI-driven, non-blocking).
 *
 * The CLI path (`wechatLogin`) is a blocking loop that prints to the terminal; the WebUI needs
 * the same flow but as a queryable singleton it can poll over HTTP. This module owns a single
 * "pairing session" state machine: `startPairing()` fetches a QR and kicks off a background poll
 * loop, `getPairSession()` reports progress, and `cancelPairing()` aborts it. On `confirmed` the
 * loop applies the new account and starts the monitor live (no restart).
 *
 * The state machine is built as a factory (`createPairing`) with injectable `fetchQR` / `poll` /
 * `onConfirmed` / `now` so the transitions are unit-testable without the network; the module-level
 * singleton below wires in the real primitives for the server/CLI.
 */

import QRCode from "qrcode";
import {
	DEFAULT_ILINK_BOT_TYPE,
	FIXED_BASE_URL,
	MAX_QR_REFRESH_COUNT,
	fetchQRCode,
	pollQRStatus,
	type QRCodeResponse,
	type StatusResponse,
} from "./login.ts";
import { applyNewAccount } from "./account.ts";

export type PairState = "idle" | "pending" | "scanned" | "confirmed" | "expired" | "error";

export interface PairSession {
	state: PairState;
	/** Poll id passed to get_qrcode_status. */
	qrcode: string;
	/** qrcode_img_content — the URL/content string to encode into a QR image. */
	content: string;
	baseUrl: string;
	scannedAt?: number;
	/** 8-minute TTL, matching the CLI deadline. */
	expiresAt: number;
	/** Number of QR refreshes so far in THIS continuous pairing flow. */
	refreshCount: number;
	error?: string;
}

/** Overall pairing deadline (matches the CLI's 8-minute timeout). */
export const PAIR_TTL_MS = 480_000;
const PAIR_POLL_INTERVAL_MS = 1_000;

export type FetchQRFn = (apiBaseUrl: string, botType: string) => Promise<QRCodeResponse>;
export type PollFn = (apiBaseUrl: string, qrcode: string) => Promise<StatusResponse>;
export type OnConfirmedFn = (account: { token?: string; baseUrl?: string; userId?: string }) => void;

export interface PairingDeps {
	fetchQR?: FetchQRFn;
	poll?: PollFn;
	onConfirmed?: OnConfirmedFn;
	now?: () => number;
	/** Spawn the background poll loop on start() (default true; tests set false to drive step() manually). */
	auto?: boolean;
}

export interface PairingApi {
	/** Fetch a QR and start polling. Returns the content to encode and the resulting state. */
	start(): Promise<{ content: string; state: PairState }>;
	/** Abort any in-flight pairing and reset to idle. */
	cancel(): void;
	/** Snapshot of the current session (idle = null). */
	status(): PairSession | null;
	/** Run one poll + transition (exposed for tests; the auto-loop uses this too). */
	step(): Promise<boolean>;
}

/**
 * A refresh (not a fresh start) only when the previous session expired: its refresh count is
 * inherited so the total stays capped at MAX_QR_REFRESH_COUNT across the whole pairing flow.
 */
export function computeInheritedRefreshCount(prev: PairSession | null): number {
	if (prev && prev.state === "expired") return prev.refreshCount;
	return 0;
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/**
 * Default side effect on a confirmed login: apply the account and bring the monitor online.
 *
 * `index.ts` is imported lazily (not statically) so that pair.ts never pulls in the monitor →
 * download → safePath chain at module load: `computeAllowed()` in tools/paths.ts throws when
 * DATA_ROOT is unset, which would break `npm run webui` standalone (no DATA_ROOT, no channels).
 */
function defaultOnConfirmed(account: { token?: string; baseUrl?: string; userId?: string }): void {
	applyNewAccount(account);
	void import("./index.ts")
		.then((m) => m.ensureWechatMonitor())
		.catch((e) => {
			console.error("[wechat] failed to start monitor after pair:", e instanceof Error ? e.message : String(e));
		});
}

export function createPairing(deps: PairingDeps = {}): PairingApi {
	const fetchQR = deps.fetchQR ?? fetchQRCode;
	const poll = deps.poll ?? pollQRStatus;
	const onConfirmed = deps.onConfirmed ?? defaultOnConfirmed;
	const now = deps.now ?? Date.now;
	const auto = deps.auto ?? true;

	let session: PairSession | null = null;
	let pollEpoch = 0;

	/** Apply one poll response to the session, mutating state in place and returning the new state. */
	function applyTransition(cur: PairSession, resp: StatusResponse): PairState {
		switch (resp.status) {
			case "wait":
				cur.state = "pending";
				break;
			case "scaned":
				if (cur.state !== "scanned") {
					cur.state = "scanned";
					cur.scannedAt = now();
				}
				break;
			case "scaned_but_redirect":
				if (resp.redirect_host) {
					cur.baseUrl = `https://${resp.redirect_host}`;
				}
				break;
			case "expired":
				cur.state = "expired";
				cur.refreshCount += 1;
				cur.error = "QR code expired";
				break;
			case "confirmed": {
				if (!resp.bot_token || !resp.ilink_bot_id) {
					cur.state = "error";
					cur.error = "Login confirmed but server did not return token / bot id";
					break;
				}
				// A confirmed login applies the account (disk writes: mkdir/write/chmod/unlink).
				// A failure here (disk full, permissions) must not crash the process via the
				// unhandled `void autoLoop(...)` promise — surface it as an error state instead.
				try {
					onConfirmed({
						token: resp.bot_token,
						baseUrl: resp.baseurl,
						userId: resp.ilink_user_id,
					});
					cur.state = "confirmed";
				} catch (e) {
					cur.state = "error";
					cur.error = e instanceof Error ? e.message : String(e);
				}
				break;
			}
		}
		return cur.state;
	}

	/** One poll + transition. Returns true when the loop should stop (terminal state). */
	async function step(): Promise<boolean> {
		const cur = session;
		if (!cur) return true;
		if (cur.state !== "pending" && cur.state !== "scanned") return true;

		if (now() > cur.expiresAt) {
			cur.state = "expired";
			cur.refreshCount += 1;
			cur.error = "QR code expired (no scan within 8 minutes)";
			return true;
		}

		const resp = await poll(cur.baseUrl, cur.qrcode);
		if (session !== cur) return true; // superseded while the poll was in flight
		const next = applyTransition(cur, resp);
		return next === "confirmed" || next === "expired" || next === "error";
	}

	/** Background poll loop; stops itself when superseded by a newer `start`. */
	async function autoLoop(epoch: number): Promise<void> {
		for (;;) {
			if (epoch !== pollEpoch) return;
			const done = await step();
			if (epoch !== pollEpoch) return;
			if (done) return;
			await sleep(PAIR_POLL_INTERVAL_MS);
		}
	}

	async function start(): Promise<{ content: string; state: PairState }> {
		pollEpoch += 1; // a new start supersedes any in-flight loop
		const epoch = pollEpoch;

		const refreshCount = computeInheritedRefreshCount(session);
		if (refreshCount > MAX_QR_REFRESH_COUNT) {
			session = {
				state: "error",
				qrcode: "",
				content: "",
				baseUrl: FIXED_BASE_URL,
				expiresAt: now() + PAIR_TTL_MS,
				refreshCount,
				error: `QR code expired ${MAX_QR_REFRESH_COUNT} times; start a new pairing to retry`,
			};
			return { content: "", state: "error" };
		}

		const initial = await fetchQR(FIXED_BASE_URL, DEFAULT_ILINK_BOT_TYPE);
		session = {
			state: "pending",
			qrcode: initial.qrcode,
			content: initial.qrcode_img_content,
			baseUrl: FIXED_BASE_URL,
			expiresAt: now() + PAIR_TTL_MS,
			refreshCount,
		};
		if (auto) void autoLoop(epoch);
		return { content: initial.qrcode_img_content, state: "pending" };
	}

	function cancel(): void {
		pollEpoch += 1; // abort any in-flight loop
		session = null;
	}

	function status(): PairSession | null {
		return session ? { ...session } : null;
	}

	return { start, cancel, status, step };
}

// The process-wide singleton used by the WebUI.
const pairing = createPairing();

/** Start (or replace) a pairing session and return the QR content to encode. */
export function startPairing(): Promise<{ content: string; state: PairState }> {
	return pairing.start();
}

/** Abort the current pairing session (back to idle). */
export function cancelPairing(): void {
	pairing.cancel();
}

/** Snapshot of the current pairing session, or null when idle. */
export function getPairSession(): PairSession | null {
	return pairing.status();
}

/** Encode the QR content string into a PNG data URL for the browser `<img src>`. */
export async function renderPairQr(content: string): Promise<string> {
	return QRCode.toDataURL(content, { margin: 1 });
}
