/**
 * Per-channel known contact addresses (for push fallback delivery).
 *
 * Stores the addresses that have contacted the bot on each channel, so a push whose primary
 * channel fails can fall back to another configured channel that the user has actually used.
 * Lives in USER_CONFIG_DIR/contacts.json (mode 0600, same pattern as wechat/account.json).
 */

import { existsSync, mkdirSync, readFileSync, writeFileSync, chmodSync } from "node:fs";
import { join } from "node:path";
import { USER_CONFIG_DIR } from "../config.ts";
import type { ChannelSource } from "./types.ts";

type ContactsMap = Partial<Record<ChannelSource, string[]>>;

function contactsPath(): string {
	return join(USER_CONFIG_DIR, "contacts.json");
}

function loadContacts(): ContactsMap {
	try {
		if (existsSync(contactsPath())) {
			const raw = JSON.parse(readFileSync(contactsPath(), "utf8")) as ContactsMap;
			if (raw && typeof raw === "object") return raw;
		}
	} catch {
		// ignore → empty map
	}
	return {};
}

function saveContacts(map: ContactsMap): void {
	mkdirSync(USER_CONFIG_DIR, { recursive: true });
	writeFileSync(contactsPath(), JSON.stringify(map, null, 2), "utf8");
	try {
		chmodSync(contactsPath(), 0o600);
	} catch {
		// best-effort
	}
}

/** Record that `userId` contacted the bot on `source` (deduped). */
export function recordContact(source: ChannelSource, userId: string): void {
	if (!userId) return;
	const map = loadContacts();
	const list = map[source] ?? [];
	if (!list.includes(userId)) {
		map[source] = [...list, userId];
		saveContacts(map);
	}
}

/** Known addresses for a channel (the push fallback order uses these). */
export function getContacts(source: ChannelSource): string[] {
	return loadContacts()[source] ?? [];
}

/** Forget all known addresses of a channel (e.g. on WeChat re-login). */
export function removeContacts(source: ChannelSource): void {
	const map = loadContacts();
	if (map[source]) {
		delete map[source];
		saveContacts(map);
	}
}
