import fs from "node:fs";
import path from "node:path";
import { USER_CONFIG_DIR } from "../../config.ts";
import { removeContacts } from "../../push/contacts.ts";
import { clearContexts } from "./context.ts";

export const DEFAULT_BASE_URL = "https://ilinkai.weixin.qq.com";

function wechatDir(): string {
	return path.join(USER_CONFIG_DIR, "wechat");
}

function accountPath(): string {
	return path.join(wechatDir(), "account.json");
}

function syncPath(): string {
	return path.join(wechatDir(), "sync.json");
}

export type WechatAccountData = {
	token?: string;
	baseUrl?: string;
	/** The ilink_user_id of the person who scanned the QR (the account owner). */
	userId?: string;
	savedAt?: string;
};

export function loadAccount(): WechatAccountData | null {
	try {
		if (fs.existsSync(accountPath())) {
			return JSON.parse(fs.readFileSync(accountPath(), "utf-8")) as WechatAccountData;
		}
	} catch {
		// ignore
	}
	return null;
}

/** Persist account data after QR login (merges into existing file). */
export function saveAccount(update: { token?: string; baseUrl?: string; userId?: string }): void {
	fs.mkdirSync(wechatDir(), { recursive: true });
	const existing = loadAccount() ?? {};
	const data: WechatAccountData = {
		...(update.token?.trim() ? { token: update.token.trim(), savedAt: new Date().toISOString() } : { token: existing.token }),
		...(update.baseUrl?.trim() ? { baseUrl: update.baseUrl.trim() } : {}),
		...(update.userId?.trim() ? { userId: update.userId.trim() } : {}),
	};
	fs.writeFileSync(accountPath(), JSON.stringify(data, null, 2), "utf-8");
	try {
		fs.chmodSync(accountPath(), 0o600);
	} catch {
		// best-effort
	}
}

export function clearAccount(): void {
	try {
		fs.unlinkSync(accountPath());
	} catch {
		// ignore
	}
	try {
		fs.unlinkSync(syncPath());
	} catch {
		// ignore
	}
	// Re-login must not keep the old user's context tokens / contact addresses.
	clearContexts();
	removeContacts("wechat");
}

/** Resolve baseUrl + token + owner userId from stored account. */
export function resolveAccount(): {
	baseUrl: string;
	token?: string;
	userId?: string;
	configured: boolean;
} {
	const data = loadAccount();
	return {
		baseUrl: data?.baseUrl?.trim() || DEFAULT_BASE_URL,
		token: data?.token?.trim() || undefined,
		userId: data?.userId?.trim() || undefined,
		configured: Boolean(data?.token?.trim()),
	};
}

/** Load persisted get_updates_buf cursor. */
export function loadSyncBuf(): string | undefined {
	try {
		if (fs.existsSync(syncPath())) {
			const data = JSON.parse(fs.readFileSync(syncPath(), "utf-8")) as { get_updates_buf?: string };
			if (typeof data.get_updates_buf === "string") return data.get_updates_buf;
		}
	} catch {
		// ignore
	}
	return undefined;
}

export function saveSyncBuf(buf: string): void {
	fs.mkdirSync(wechatDir(), { recursive: true });
	fs.writeFileSync(syncPath(), JSON.stringify({ get_updates_buf: buf }), "utf-8");
	try {
		fs.chmodSync(syncPath(), 0o600);
	} catch {
		// best-effort (the buf is an opaque cursor, not a secret)
	}
}
