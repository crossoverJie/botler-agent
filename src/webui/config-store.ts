/**
 * Config-file store: read / atomic write / backup / restore for the three USER_CONFIG_DIR files
 * the WebUI may edit (.env, system-prompt.md, schedules.json). All writes go through
 * `writeConfigFile`, which snapshots the previous version into `.backups/` (mode 0700) and then
 * performs an atomic tmp-file + rename. Restore first snapshots the current file so rollback is itself
 * reversible.
 *
 * Security: backup file names are strictly validated by a regex; restore rejects path traversal via
 * a realpath parent check. No user-supplied path is ever used as-is.
 */

import {
	existsSync,
	mkdirSync,
	readFileSync,
	writeFileSync,
	renameSync,
	copyFileSync,
	statSync,
	readdirSync,
	unlinkSync,
	realpathSync,
} from "node:fs";
import { join, dirname } from "node:path";
import { CONFIG, USER_CONFIG_DIR } from "../config.ts";

export const CONFIG_FILE_NAMES = [".env", "system-prompt.md", "schedules.json"] as const;
export type ConfigFileName = (typeof CONFIG_FILE_NAMES)[number];

const BACKUP_DIR = join(USER_CONFIG_DIR, ".backups");
const MAX_BACKUPS = 10;
const BACKUP_RE = /^(\.env|system-prompt\.md|schedules\.json)\.\d{8}T\d{6}$/;

function pad(n: number): string {
	return String(n).padStart(2, "0");
}

function timestamp(): string {
	const d = new Date();
	return (
		`${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}` +
		`T${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`
	);
}

/**
 * Live location of a config file. `.env` / `system-prompt.md` always live in USER_CONFIG_DIR;
 * schedules.json honors the BOTLER_SCHEDULES_FILE override so a WebUI save lands in the same file
 * the scheduler actually reads (otherwise the two would silently diverge).
 */
function configFilePath(name: ConfigFileName): string {
	if (name === "schedules.json") return CONFIG.schedulesFile;
	return join(USER_CONFIG_DIR, name);
}

/** Read a config file's raw text. Missing file returns "" (callers handle defaults). */
export function readConfigFile(name: ConfigFileName): string {
	const p = configFilePath(name);
	if (!existsSync(p)) return "";
	try {
		return readFileSync(p, "utf8");
	} catch {
		return "";
	}
}

/** Snapshot the current file into .backups/ (creating + chmod 0700 the dir). Returns the backup path or null if nothing to back up. */
export function backupConfigFile(name: ConfigFileName): string | null {
	const src = configFilePath(name);
	if (!existsSync(src)) return null;
	mkdirSync(BACKUP_DIR, { recursive: true, mode: 0o700 });
	const file = `${name}.${timestamp()}`;
	const dest = join(BACKUP_DIR, file);
	copyFileSync(src, dest);
	pruneBackups(name);
	return dest;
}

function pruneBackups(name: ConfigFileName): void {
	if (!existsSync(BACKUP_DIR)) return;
	const matched = readdirSync(BACKUP_DIR)
		.filter((f) => BACKUP_RE.test(f) && f.startsWith(`${name}.`))
		.sort(); // ascending → oldest first
	while (matched.length > MAX_BACKUPS) {
		const old = matched.shift();
		if (!old) break;
		try {
			unlinkSync(join(BACKUP_DIR, old));
		} catch {
			// best-effort
		}
	}
}

/** Write content atomically: backup → write tmp → rename. */
export function writeConfigFile(name: ConfigFileName, content: string): void {
	backupConfigFile(name);
	const target = configFilePath(name);
	mkdirSync(dirname(target), { recursive: true });
	const tmp = `${target}.tmp.${process.pid}.${Date.now()}`;
	writeFileSync(tmp, content, "utf8");
	renameSync(tmp, target);
}

export interface BackupEntry {
	name: ConfigFileName;
	file: string;
	ts: number;
	bytes: number;
}

/** List available backups, newest first. */
export function listBackups(): BackupEntry[] {
	if (!existsSync(BACKUP_DIR)) return [];
	const out: BackupEntry[] = [];
	for (const f of readdirSync(BACKUP_DIR)) {
		if (!BACKUP_RE.test(f)) continue;
		const name = (f.startsWith("system-prompt.md")
			? "system-prompt.md"
			: f.startsWith("schedules.json")
				? "schedules.json"
				: ".env") as ConfigFileName;
		const tsPart = f.slice(f.lastIndexOf(".") + 1);
		const iso = `${tsPart.slice(0, 4)}-${tsPart.slice(4, 6)}-${tsPart.slice(6, 8)}T${tsPart.slice(9, 11)}:${tsPart.slice(11, 13)}:${tsPart.slice(13, 15)}`;
		const ts = Date.parse(iso);
		let bytes = 0;
		try {
			bytes = statSync(join(BACKUP_DIR, f)).size;
		} catch {
			// ignore
		}
		out.push({ name, file: f, ts: Number.isNaN(ts) ? 0 : ts, bytes });
	}
	out.sort((a, b) => b.ts - a.ts);
	return out;
}

/**
 * Restore a backup over the live config file. `name` must be a known config file and `file` must
 * match the strict backup-name regex; the file's realpath parent must be BACKUP_DIR (no traversal).
 * The current live file is snapshotted first, so the restore itself is reversible.
 */
export function restoreBackup(name: ConfigFileName, file: string): void {
	if (!CONFIG_FILE_NAMES.includes(name)) throw new Error(`invalid config file name: ${String(name)}`);
	if (!BACKUP_RE.test(file)) throw new Error(`invalid backup file name: ${file}`);
	const src = join(BACKUP_DIR, file);
	const resolved = realpathSync(src);
	// Resolve BACKUP_DIR too: on macOS /tmp is a symlink to /private/tmp, so a literal comparison
	// would spuriously reject legitimate backups. Compare canonical dirs.
	let backupDirResolved = BACKUP_DIR;
	try {
		backupDirResolved = realpathSync(BACKUP_DIR);
	} catch {
		// .backups may not exist yet; the regex + name check below still gate the path.
	}
	if (dirname(resolved) !== backupDirResolved) throw new Error("backup path traversal rejected");
	if (!existsSync(resolved)) throw new Error("backup not found");
	// Snapshot the live file before overwriting (rollback is reversible).
	backupConfigFile(name);
	copyFileSync(resolved, configFilePath(name));
}
