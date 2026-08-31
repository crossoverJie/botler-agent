/**
 * Local task-log WebUI.
 *
 * Zero dependencies: Node built-in `http` only, bound to 127.0.0.1. Serves the single-file
 * `index.html` and a small read-only JSON API plus one write endpoint (`POST /api/cleanup`)
 * that only deletes day-files inside the log directory. Never reads DATA_ROOT, config, or source.
 */

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { CONFIG } from "../config.ts";
import { buildSnapshot, buildMetrics, getHistory, ensureMonitorSampling } from "../monitor/health.ts";
import {
	queryLogs,
	getLog,
	summary,
	diskUsage,
	cleanupLogs,
	tokenTimeSeries,
	type CleanupOptions,
	type TokenTimeQuery,
} from "../logging/store.ts";
import {
	readConfigFile,
	writeConfigFile,
	listBackups,
	restoreBackup,
	type ConfigFileName,
} from "./config-store.ts";
import { loadSchedules, saveSchedules } from "../scheduler/store.ts";
import { reloadSchedules } from "../scheduler/engine.ts";
import { nextFireEpoch } from "../scheduler/cron.ts";
import { scheduleOverview, scheduleRuns, scheduleRunStats } from "../scheduler/history.ts";
import { loadAccount, resolveAccount } from "../channels/wechat/account.ts";
import { getContext } from "../channels/wechat/context.ts";
import { reminderStatus } from "../channels/wechat/reminder.ts";

const INDEX_HTML = join(dirname(fileURLToPath(import.meta.url)), "index.html");

function json(res: ServerResponse, data: unknown, status = 200): void {
	const body = JSON.stringify(data);
	res.writeHead(status, { "Content-Type": "application/json; charset=utf-8" });
	res.end(body);
}

function serveIndex(res: ServerResponse): void {
	try {
		const html = readFileSync(INDEX_HTML, "utf8");
		res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
		res.end(html);
	} catch (e) {
		res.statusCode = 500;
		res.end(`Cannot load index.html: ${e instanceof Error ? e.message : e}`);
	}
}

function readJsonBody(req: IncomingMessage): Promise<unknown> {
	return new Promise((resolve, reject) => {
		let data = "";
		req.on("data", (chunk) => {
			data += chunk;
			if (data.length > 1_000_000) reject(new Error("body too large"));
		});
		req.on("end", () => {
			if (!data) return resolve({});
			try {
				resolve(JSON.parse(data));
			} catch {
				reject(new Error("invalid JSON body"));
			}
		});
		req.on("error", reject);
	});
}

function parseQuery(url: URL) {
	const sp = url.searchParams;
	const num = (k: string): number | undefined => {
		const v = sp.get(k);
		// `Number("") === 0` would silently turn an empty param into epoch 0.
		return v === null || v === "" ? undefined : Number(v);
	};
	return {
		from: num("from"),
		to: num("to"),
		project: sp.get("project") ?? undefined,
		source: sp.get("source") ?? undefined,
		q: sp.get("q") ?? undefined,
		limit: num("limit"),
		offset: num("offset"),
	};
}

/**
 * CSRF mitigation (zero-dependency, zero-config): all write endpoints require a custom header
 * `X-Botler-UI: 1`. A cross-origin `fetch` carrying a custom header triggers a CORS preflight;
 * this server sends no CORS headers, so the browser blocks it. Same-origin UI requests carry the
 * header normally. Returns false (and writes 403) when the header is missing.
 */
function requireUiHeader(req: IncomingMessage, res: ServerResponse): boolean {
	if (req.headers["x-botler-ui"] === "1") return true;
	res.statusCode = 403;
	res.end("Missing X-Botler-UI header");
	return false;
}

/**
 * Read-only WeChat status for the WebUI: login (account.json savedAt), the built-in 24h-window
 * renewal-reminder loop (process-local state from reminder.ts), and the owner's context
 * (lastMsgAt / lastRemindedAt from contexts.json). Never writes anything.
 */
function wechatStatus() {
	const now = Date.now();
	const resolved = resolveAccount();
	const account = loadAccount();
	const owner = resolved.userId;
	const ctx = owner ? getContext(owner) : undefined;
	const threshold = CONFIG.wechatReminderHours;
	const loginAt = account?.savedAt ? Date.parse(account.savedAt) : NaN;
	const reminder = reminderStatus();
	return {
		configured: resolved.configured,
		owner: owner ?? null,
		loginAt: Number.isFinite(loginAt) ? loginAt : null,
		loginAgeMs: Number.isFinite(loginAt) ? now - loginAt : null,
		thresholdHours: threshold,
		reminderEnabled: threshold >= 1,
		reminderLoopStarted: reminder.started,
		reminderTickMs: reminder.tickMs,
		lastTickAt: reminder.lastTickAt,
		nextTickAt: reminder.nextTickAt,
		ownerHasContext: Boolean(ctx),
		lastMsgAt: ctx?.lastMsgAt ?? null,
		quietMs: ctx ? now - ctx.lastMsgAt : null,
		lastRemindedAt: ctx?.lastRemindedAt ?? null,
		remindedThisStretch: Boolean(ctx && ctx.lastRemindedAt != null && ctx.lastRemindedAt >= ctx.lastMsgAt),
		nextReminderAt: ctx ? ctx.lastMsgAt + threshold * 3600e3 : null,
	};
}

/** The full config view returned to GET /api/config. */
function configView() {
	return {
		systemPrompt: readConfigFile("system-prompt.md"),
		schedules: loadSchedules(),
		backups: listBackups(),
		wechat: wechatStatus(),
	};
}

export function startWebui(): void {
	// Drive the event-loop-lag histogram + history sampler so the monitor view has real data
	// even when the standalone 8899 monitor port is disabled (MONITOR_ENABLED=0).
	ensureMonitorSampling();

	const server = createServer(async (req, res) => {
		const url = new URL(req.url ?? "/", "http://localhost");
		try {
			const path = url.pathname;
			if (path === "/" || path === "/index.html") return serveIndex(res);

			// Process health/metrics (served in-process; the same data is also exposed on the
			// standalone monitor port 8899 for external Prometheus scrapers). Read-only.
			if (path === "/healthz") return json(res, buildSnapshot());
			if (path === "/healthz/history") return json(res, getHistory());
			if (path === "/metrics") {
				res.writeHead(200, { "Content-Type": "text/plain; version=0.0.4; charset=utf-8" });
				return res.end(buildMetrics());
			}

			const idMatch = path.match(/^\/api\/logs\/([^/]+)$/);
			if (path === "/api/logs") return json(res, queryLogs(parseQuery(url)));
			if (idMatch) return json(res, getLog(decodeURIComponent(idMatch[1])) ?? null);
			if (path === "/api/summary") return json(res, summary());
			if (path === "/api/usage") return json(res, diskUsage());
			if (path === "/api/token-stats") {
				const sp = url.searchParams;
				const g = sp.get("granularity");
				const gb = sp.get("groupBy");
				return json(
					res,
					tokenTimeSeries({
						...parseQuery(url),
						granularity: g === "hour" || g === "week" || g === "day" ? g : undefined,
						groupBy: gb === "project" || gb === "source" ? gb : undefined,
					} satisfies TokenTimeQuery),
				);
			}

			if (path === "/api/cleanup") {
				if (req.method !== "POST") {
					res.statusCode = 405;
					return res.end("Method Not Allowed");
				}
				if (!requireUiHeader(req, res)) return;
				const body = (await readJsonBody(req)) as CleanupOptions;
				return json(res, cleanupLogs(body));
			}

			const schedRunsMatch = path.match(/^\/api\/schedules\/([^/]+)\/runs$/);
			const schedStatsMatch = path.match(/^\/api\/schedules\/([^/]+)\/stats$/);

			if (path === "/api/schedules/overview") return json(res, scheduleOverview());
			if (schedRunsMatch) {
				return json(res, scheduleRuns(decodeURIComponent(schedRunsMatch[1]), parseQuery(url)));
			}
			if (schedStatsMatch) {
				return json(res, scheduleRunStats(decodeURIComponent(schedStatsMatch[1])));
			}

			// ---- Config endpoints (WebUI) ----
			if (path === "/api/config") {
				if (req.method !== "GET") {
					res.statusCode = 405;
					return res.end("Method Not Allowed");
				}
				return json(res, configView());
			}

			if (path === "/api/config/system-prompt") {
				if (req.method === "GET") return json(res, { content: readConfigFile("system-prompt.md") });
				if (req.method === "PUT") {
					if (!requireUiHeader(req, res)) return;
					const b = (await readJsonBody(req)) as { content?: unknown };
					const content = String(b.content ?? "");
				if (!content.trim()) return json(res, { error: "system prompt must not be empty" }, 400);
				if (content.length > 100_000) return json(res, { error: "system prompt too long (limit 100KB)" }, 400);
					writeConfigFile("system-prompt.md", content);
					return json(res, { ok: true, live: true });
				}
				res.statusCode = 405;
				return res.end("Method Not Allowed");
			}

			if (path === "/api/config/schedules") {
				if (req.method === "GET") return json(res, loadSchedules());
				if (req.method === "PUT") {
					if (!requireUiHeader(req, res)) return;
					const b = (await readJsonBody(req)) as { schedules?: unknown };
					if (!Array.isArray(b.schedules)) return json(res, { error: "schedules must be an array" }, 400);
					try {
						saveSchedules(b.schedules);
					} catch (e) {
						return json(res, { error: e instanceof Error ? e.message : String(e) }, 400);
					}
					reloadSchedules();
					return json(res, { ok: true, live: true });
				}
				res.statusCode = 405;
				return res.end("Method Not Allowed");
			}

			// Read-only: compute the next fire time for a candidate schedule spec (no persistence).
			if (path === "/api/config/schedules/preview") {
				if (req.method === "POST") {
					const b = (await readJsonBody(req)) as {
						cron?: string;
						interval?: string;
						at?: string;
						once?: string;
						timezone?: string;
						holidayMode?: "workday";
					};
					try {
						const e = {
							id: "preview",
							enabled: true,
							timezone: b.timezone || "Asia/Shanghai",
							message: "",
							...(b.holidayMode ? { holidayMode: b.holidayMode } : {}),
							...(b.cron ? { cron: b.cron } : {}),
							...(b.interval ? { interval: b.interval } : {}),
							...(b.at ? { at: b.at } : {}),
							...(b.once ? { once: b.once } : {}),
						};
						const next = nextFireEpoch(e, Date.now());
						return json(res, { ok: true, next: next === Infinity ? null : next });
					} catch (e) {
						return json(res, { error: e instanceof Error ? e.message : String(e) }, 400);
					}
				}
				res.statusCode = 405;
				return res.end("Method Not Allowed");
			}

			if (path === "/api/config/backups") {
				if (req.method === "GET") return json(res, listBackups());
				res.statusCode = 405;
				return res.end("Method Not Allowed");
			}

			if (path === "/api/config/backups/restore") {
				if (req.method === "POST") {
					if (!requireUiHeader(req, res)) return;
					const b = (await readJsonBody(req)) as { name?: unknown; file?: unknown };
					const name = String(b.name ?? "") as ConfigFileName;
					const file = String(b.file ?? "");
					if (!["system-prompt.md", ".env", "schedules.json"].includes(name)) {
						return json(res, { error: "invalid file name" }, 400);
					}
					try {
						restoreBackup(name, file);
					} catch (e) {
						return json(res, { error: e instanceof Error ? e.message : String(e) }, 400);
					}
					if (name === "schedules.json") reloadSchedules();
					return json(res, { ok: true, live: name !== ".env" });
				}
				res.statusCode = 405;
				return res.end("Method Not Allowed");
			}

			res.statusCode = 404;
			res.end("not found");
		} catch (e) {
			res.statusCode = 400;
			res.end(String(e instanceof Error ? e.message : e));
		}
	});

	server.listen(CONFIG.webuiPort, "127.0.0.1", () => {
		console.log(`[webui] task log UI on http://127.0.0.1:${CONFIG.webuiPort}`);
	});
}

// Allow direct run (`npm run webui`): start the UI without channels.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
	startWebui();
}
