import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { homedir } from "node:os";

/** App root directory (botler-agent/); the source-dir .env lives here (dev-phase fallback config). */
export const APP_ROOT = dirname(dirname(fileURLToPath(import.meta.url)));

/**
 * User-level config directory: the real config (.env, system-prompt.md) lives here, shared across clones / machines.
 * Overridable via BOTLER_CONFIG_DIR (for testing / multiple configs). Can only come from a real process env var,
 * not from the .env file itself (otherwise the config-dir location would be chicken-and-egg).
 */
export const USER_CONFIG_DIR = process.env.BOTLER_CONFIG_DIR ?? join(homedir(), ".botler-agent");

/**
 * Minimal .env loader: parses key=value lines, only filling in when process.env is unset.
 * Load order (later ones have lower priority and do not override existing values):
 *   1. ~/.botler-agent/.env (user-level, higher priority)
 *   2. source-dir .env (dev fallback)
 * Process env vars have the highest priority (neither of the above overrides them).
 */
function parseDotEnv(path: string): void {
	if (!existsSync(path)) return;
	const raw = readFileSync(path, "utf8");
	for (const line of raw.split("\n")) {
		const trimmed = line.trim();
		if (!trimmed || trimmed.startsWith("#")) continue;
		const eq = trimmed.indexOf("=");
		if (eq === -1) continue;
		const key = trimmed.slice(0, eq).trim();
		let value = trimmed.slice(eq + 1).trim();
		// Strip paired quotes
		if (
			(value.startsWith('"') && value.endsWith('"')) ||
			(value.startsWith("'") && value.endsWith("'"))
		) {
			value = value.slice(1, -1);
		}
		if (key && process.env[key] === undefined) {
			process.env[key] = value;
		}
	}
}

function loadDotEnv(): void {
	parseDotEnv(join(USER_CONFIG_DIR, ".env")); // user-level takes priority
	parseDotEnv(join(APP_ROOT, ".env")); // source-dir fallback (does not override user-level)
}

loadDotEnv();

/** Custom OpenAI-completions compatible provider (e.g. a self-hosted gateway). */
export interface ModelMeta {
	id: string;
	name: string;
	reasoning: boolean;
	contextWindow: number;
	maxTokens: number;
	/**
	 * Whether the model accepts image input (vision). Defaults to true, matching pi-ai's
	 * built-in default of text+image; set false for text-only models. When false, pi-ai
	 * replaces image content blocks with a placeholder instead of sending them.
	 */
	vision?: boolean;
}

/** Wire protocol for a custom provider (default: OpenAI Chat Completions). */
export type CustomProviderApi = "openai-completions" | "anthropic-messages";

export interface CustomProviderConfig {
	id: string;
	/** Which wire protocol to speak against baseUrl. */
	api: CustomProviderApi;
	baseUrl: string;
	apiKey: string;
	models: ModelMeta[];
}

export interface Config {
	/** The data root directory operated on by the agent (where data subprojects live). Allowlist = its first-level subdirs. */
	dataRoot: string;
	/** pi-ai provider id (anthropic or a custom provider id). */
	provider: string;
	/** Model id, e.g. claude-sonnet-4-5 or your-model-flash. */
	model: string;
	/** Custom OpenAI-completions providers (from ~/.botler-agent/providers.json, with a legacy CUSTOM_* env fallback). */
	customProviders: CustomProviderConfig[];
	telegramToken?: string;
	feishuAppId?: string;
	feishuAppSecret?: string;
	feishuVerificationToken?: string;
	feishuEncryptKey?: string;
	feishuPort: number;
	/** When WECHAT_ENABLED=1, start the WeChat iLink long-poll channel (requires a completed wechat-login). */
	wechatEnabled: boolean;
	/** Optional comma-separated extra ilink_user_id allowlist; the account owner (QR scanner) is always allowed. */
	wechatAllowFrom?: string;
	/**
	 * WeChat session renewal reminder threshold in hours: 0 = disabled, 1-24 = remind the owner
	 * when they've been quiet this long (clamped to <=24 so the "hours remaining" copy is never negative).
	 */
	wechatReminderHours: number;
	/**
	 * WeChat image batching window in seconds: a selected photo is delivered immediately, while the
	 * user may still be typing the caption. Image-bearing messages are buffered this long for a
	 * follow-up text from the same sender; 0 disables batching (dispatch immediately).
	 */
	wechatImageBatchSeconds: number;
	/** When GIT_PUSH=1, additionally git push after a write task commits (failure is only a warning; default off). */
	gitPush: boolean;
	/** When WEBUI_ENABLED=1, start the local task-log UI (binds 127.0.0.1 only). */
	webuiEnabled: boolean;
	/** Port for the task-log WebUI. */
	webuiPort: number;
	/** When SCHEDULER_ENABLED=1, run the in-process scheduler that fires schedules.json entries into dispatch. */
	schedulerEnabled: boolean;
	/** Path to the scheduler config file. Default: ~/.botler-agent/schedules.json. */
	schedulesFile: string;
	/** Path to the cached China legal-holiday calendar (for holidayMode:"workday" schedules). Default: ~/.botler-agent/holidays.json. */
	holidaysFile: string;
	/** HTTP source for the holiday calendar; the `{year}` placeholder is substituted. Default: NateScarlet/holiday-cn. */
	holidayApiUrl: string;
	/** Directory for task-log JSONL files (outside DATA_ROOT, does not break app/data separation). */
	logDir: string;
	/** When MONITOR_ENABLED !== "0", start the local health/metrics server (binds 127.0.0.1 only). Always on by default. */
	monitorEnabled: boolean;
	/** Port for the health/metrics server (default 8899; avoids 3000=feishu / 8900=webui). */
	monitorPort: number;
	/** Max tool-call turns for a single execution Agent (one assistant message with >=1 toolCall counts as 1 turn). Default 20. */
	maxToolTurns: number;
}

/**
 * Model metadata for the legacy CUSTOM_* env fallback. Mirrors the models configured for
 * the gateway, used only when no providers.json is present.
 */
const CUSTOM_MODELS: ModelMeta[] = [
	{ id: "your-model-pro", name: "Your Model Pro", reasoning: true, contextWindow: 1048576, maxTokens: 393216 },
	{ id: "your-model-flash", name: "Your Model Flash", reasoning: false, contextWindow: 1048576, maxTokens: 393216 },
];

/** Normalize the per-provider `api` value; null = unsupported (skip that provider). */
function parseProviderApi(raw: unknown): CustomProviderApi | null {
	if (raw === undefined || raw === null || raw === "") return "openai-completions";
	if (raw === "anthropic" || raw === "anthropic-messages") return "anthropic-messages";
	if (raw === "openai" || raw === "openai-completions") return "openai-completions";
	return null;
}

/**
 * Load custom providers from the externalized providers.json (primary source of truth).
 *
 * Location: `USER_CONFIG_DIR/providers.json` (i.e. ~/.botler-agent/providers.json, overridable
 * via BOTLER_CONFIG_DIR). Format:
 *   { "providers": { "<id>": { "api"?, "baseUrl", "apiKey", "models": [{ "id", "name", "reasoning", "contextWindow", "maxTokens" }] } } }
 * `api` selects the wire protocol: "openai-completions" (default) or "anthropic-messages".
 * Adding a provider/model is just editing this file — no framework change needed.
 * Malformed entries are skipped; a missing / unusable file returns [] so the caller can fall back.
 */
function loadProvidersFile(): CustomProviderConfig[] {
	const file = join(USER_CONFIG_DIR, "providers.json");
	if (!existsSync(file)) return [];
	try {
		const raw = JSON.parse(readFileSync(file, "utf8")) as {
			providers?: Record<string, { api?: unknown; baseUrl?: unknown; apiKey?: unknown; models?: unknown }>;
		};
		const out: CustomProviderConfig[] = [];
		for (const [id, p] of Object.entries(raw.providers ?? {})) {
			if (typeof p.baseUrl !== "string" || !p.baseUrl) continue;
			if (typeof p.apiKey !== "string" || !p.apiKey) continue;
			if (!Array.isArray(p.models)) continue;
			const api = parseProviderApi(p.api);
			if (!api) {
				console.warn(`[config] skipping provider "${id}": unsupported api "${String(p.api)}"`);
				continue;
			}
			const models: ModelMeta[] = [];
			for (const m of p.models) {
				const mm = m as Partial<ModelMeta>;
				if (typeof mm.id !== "string" || !mm.id) continue;
				models.push({
					id: mm.id,
					name: typeof mm.name === "string" && mm.name ? mm.name : mm.id,
					reasoning: mm.reasoning === true,
					contextWindow: Number(mm.contextWindow) || 0,
					maxTokens: Number(mm.maxTokens) || 0,
					vision: mm.vision,
				});
			}
			if (models.length === 0) continue;
			out.push({ id, api, baseUrl: p.baseUrl, apiKey: p.apiKey, models });
		}
		return out;
	} catch (e) {
		console.warn(`[config] failed to parse ${file}, falling back to CUSTOM_* env config:`, e);
		return [];
	}
}

/**
 * Build all custom OpenAI-completions providers.
 * Priority: providers.json (externalized, primary) → legacy CUSTOM_BASE_URL / CUSTOM_API_KEY env fallback.
 */
function buildCustomProviders(): CustomProviderConfig[] {
	const fromFile = loadProvidersFile();
	if (fromFile.length > 0) return fromFile;

	// Legacy dev-phase fallback: CUSTOM_* env vars + the built-in model list
	if (process.env.PI_PROVIDER === "anthropic") return [];
	const baseUrl = process.env.CUSTOM_BASE_URL;
	const apiKey = process.env.CUSTOM_API_KEY;
	if (!baseUrl || !apiKey) return [];
	return [{ id: process.env.PI_PROVIDER ?? "custom", api: "openai-completions", baseUrl, apiKey, models: CUSTOM_MODELS }];
}

/** Parse MAX_TOOL_TURNS; fall back to default 20 for invalid values (NaN / non-integer / < 1). */
function parseMaxToolTurns(): number {
	const n = Number(process.env.MAX_TOOL_TURNS ?? "20");
	return Number.isInteger(n) && n >= 1 ? n : 20;
}

/**
 * Parse WECHAT_REMINDER_HOURS. Single parse branch (avoids `Number(env)||23` letting 0<x<1 /
 * negative values through):
 * - "0": explicitly disabled (0 = reminders off)
 * - 1-24: active (>24 clamped to 24, so the `${24-hours}` copy never goes negative)
 * - anything else (invalid / NaN / <1): fall back to default 23
 */
function parseWechatReminderHours(): number {
	const raw = process.env.WECHAT_REMINDER_HOURS;
	if (raw === "0") return 0;
	const n = Number(raw ?? "23");
	return Number.isFinite(n) && n >= 1 ? Math.min(n, 24) : 23;
}

/**
 * Parse WECHAT_IMAGE_BATCH_SECONDS. "0" explicitly disables batching; a positive integer sets the
 * window in seconds; anything invalid falls back to the default 60.
 */
function parseWechatImageBatchSeconds(): number {
	const raw = process.env.WECHAT_IMAGE_BATCH_SECONDS;
	if (raw === "0") return 0;
	const n = Number(raw ?? "60");
	return Number.isFinite(n) && n >= 0 ? Math.floor(n) : 60;
}

export const CONFIG: Config = {
	dataRoot: process.env.DATA_ROOT ?? "",
	provider: process.env.PI_PROVIDER ?? "anthropic",
	model: process.env.PI_MODEL ?? "claude-sonnet-4-5",
	customProviders: buildCustomProviders(),
	telegramToken: process.env.TELEGRAM_BOT_TOKEN || undefined,
	feishuAppId: process.env.FEISHU_APP_ID || undefined,
	feishuAppSecret: process.env.FEISHU_APP_SECRET || undefined,
	feishuVerificationToken: process.env.FEISHU_VERIFICATION_TOKEN || undefined,
	feishuEncryptKey: process.env.FEISHU_ENCRYPT_KEY || undefined,
	feishuPort: Number(process.env.FEISHU_PORT ?? "3000"),
	wechatEnabled: process.env.WECHAT_ENABLED === "1",
	wechatAllowFrom: process.env.WECHAT_ALLOW_FROM || undefined,
	wechatReminderHours: parseWechatReminderHours(),
	wechatImageBatchSeconds: parseWechatImageBatchSeconds(),
	gitPush: process.env.GIT_PUSH === "1",
	webuiEnabled: process.env.WEBUI_ENABLED === "1",
	webuiPort: Number(process.env.WEBUI_PORT ?? "8900"),
	schedulerEnabled: process.env.SCHEDULER_ENABLED === "1",
	schedulesFile: process.env.BOTLER_SCHEDULES_FILE ?? join(USER_CONFIG_DIR, "schedules.json"),
	holidaysFile: process.env.BOTLER_HOLIDAYS_FILE ?? join(USER_CONFIG_DIR, "holidays.json"),
	holidayApiUrl:
		process.env.BOTLER_HOLIDAY_API_URL ??
		"https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/{year}.json",
	logDir: process.env.BOTLER_LOG_DIR ?? join(USER_CONFIG_DIR, "task-logs"),
	maxToolTurns: parseMaxToolTurns(),
	monitorEnabled: process.env.MONITOR_ENABLED !== "0",
	monitorPort: (() => {
		const n = Number(process.env.MONITOR_PORT ?? "8899");
		return Number.isInteger(n) && n > 0 ? n : 8899;
	})(),
};
