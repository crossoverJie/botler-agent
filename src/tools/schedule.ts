/**
 * Schedule tool: create / list / update / delete / enable / disable scheduled tasks.
 *
 * This is a narrow, deliberate exception to the "path allowlist = DATA_ROOT first-level
 * subdirs" red line: it lets the Agent persist scheduling state to the fixed externalized
 * file `schedules.json` (outside DATA_ROOT). The blast radius is contained by:
 *  - no file-path parameter at all (the tool always writes the one fixed file)
 *  - `normalizeSchedules` full validation + 10KB message cap + atomic write w/ backup
 *  - the push recipient is injected by the framework (task-context), never guessed by the model
 *
 * safePath itself is untouched; the run-tool boundary is unchanged.
 */

import type { AgentTool } from "@earendil-works/pi-agent-core";
import { Type } from "@earendil-works/pi-ai";
import { loadSchedules, saveSchedules, RETRY_MAX_LIMIT, RETRY_BACKOFF_MS_LIMIT } from "../scheduler/store.ts";
import { nextFireEpoch, parseOnceEpoch, isValidTimezone } from "../scheduler/cron.ts";
import type { ScheduleEntry } from "../scheduler/types.ts";
import { getTaskContext } from "./task-context.ts";

const schema = Type.Object({
	action: Type.Union(
		[
			Type.Literal("list"),
			Type.Literal("create"),
			Type.Literal("update"),
			Type.Literal("delete"),
			Type.Literal("enable"),
			Type.Literal("disable"),
		],
		{ description: "list=view all scheduled tasks; create=create; update=modify; delete=delete; enable/disable=enable/disable" },
	),
		id: Type.Optional(
		Type.String({ description: "Scheduled task id (short, e.g. drink-water). Required for update/delete/enable/disable; auto-generated if omitted on create" }),
	),
	message: Type.Optional(Type.String({ description: "Message content sent to the Agent when the task fires (exactly one of message/script)" })),
	script: Type.Optional(
		Type.String({ description: "Run an existing in-project script directly when the task fires, bypassing the LLM (exactly one of message/script). Path relative to DATA_ROOT, e.g. 'notes/scripts/build.py' (.py/.js/.mjs only)." }),
	),
	scriptArgs: Type.Optional(
		Type.Array(Type.String(), { description: "Arguments passed to the script (optional), e.g. ['--all']" }),
	),
	commit: Type.Optional(
		Type.Boolean({ description: "Auto git-commit the script's data changes on success (default true; set false for read-only scripts)" }),
	),
	retryMax: Type.Optional(
		Type.Integer({ maximum: RETRY_MAX_LIMIT, description: `Failure retry count (0 = no retry, the default; max ${RETRY_MAX_LIMIT}). For idempotent scripts set >0 so transient failures self-recover; for non-idempotent scripts (sends messages / hits external APIs) keep 0 to avoid repeating side effects.` }),
	),
	retryBackoffMs: Type.Optional(
		Type.Integer({ maximum: RETRY_BACKOFF_MS_LIMIT, description: `Fixed backoff between retries, in ms (used only when retryMax > 0; max ${RETRY_BACKOFF_MS_LIMIT}).` }),
	),
	cron: Type.Optional(Type.String({ description: "5-field cron expression, e.g. '0 8 * * *' (recurring; one of four for create)" })),
	interval: Type.Optional(Type.String({ description: "Simple interval, e.g. '5m' / '2h' / '1d' (recurring; one of four for create)" })),
	at: Type.Optional(Type.String({ description: "Fixed DAILY time 'HH:MM' — repeats every day (one of four for create). Do NOT use for one-off reminders like 'tonight 10pm'; use once instead." })),
	once: Type.Optional(Type.String({ description: "ONE-SHOT absolute date-time in ISO 8601, e.g. '2026-08-20T22:00:00+08:00' — fires exactly once then becomes inert (one of four for create). Use for 'tonight 10pm' / 'tomorrow 3pm' / 'Aug 20 at 10am'. MUST include the full date; compute it from today's date. Do NOT use 'at' (that repeats daily)." })),
	timezone: Type.Optional(Type.String({ description: "IANA timezone, default Asia/Shanghai" })),
	project: Type.Optional(
		Type.String({ description: "Routing hint (data subproject name); when set, the routing LLM is skipped on fire and the project runs directly" }),
	),
	holidayMode: Type.Optional(
		Type.Union([Type.Literal("workday"), Type.Literal("off")], {
			description:
				"China legal-workday gating for cron/interval/at triggers. 'workday' = fire only on China legal workdays (skip 法定假日, include 调休补班; cron date fields are ignored, only hour:minute(s) matter). 'off' / absent = normal cron behavior. Cannot be combined with 'once'.",
		}),
	),
});

type ScheduleArgs = {
	action: "list" | "create" | "update" | "delete" | "enable" | "disable";
	id?: string;
	message?: string;
	script?: string;
	scriptArgs?: string[];
	commit?: boolean;
	retryMax?: number;
	retryBackoffMs?: number;
	cron?: string;
	interval?: string;
	at?: string;
	once?: string;
	timezone?: string;
	project?: string;
	holidayMode?: "workday" | "off";
};

function triggerOf(e: ScheduleEntry): string {
	if (e.cron) return `cron ${e.cron}`;
	if (e.interval) return `interval ${e.interval}`;
	if (e.at) return `at ${e.at} (daily)`;
	if (e.once) return `once ${e.once}`;
	return "(no trigger)";
}

function fmtFire(epoch: number, tz: string): string {
	return new Date(epoch).toLocaleString("en-US", { timeZone: tz, hour12: false });
}

function nextFireOf(e: ScheduleEntry): string | null {
	try {
		const next = nextFireEpoch(e, Date.now());
		if (next === Infinity) return null; // expired one-shot
		return fmtFire(next, e.timezone);
	} catch {
		return null;
	}
}

function textResult(text: string, details?: Record<string, unknown>) {
	return { content: [{ type: "text" as const, text }], details: details ?? {} };
}

export const scheduleTool: AgentTool<typeof schema> = {
	name: "schedule",
	label: "Manage scheduled tasks",
	description:
		"Create / view / modify / delete / enable / disable scheduled tasks (writes the fixed schedule config, takes effect immediately). Use when the user says things like 'remind me every morning at 8am…', 'summarize every 2 hours…', 'remind me in X minutes…' that contain scheduling / reminder / calendar intent. create must specify exactly one trigger plus exactly one ACTION: a non-empty `message` (the instruction sent back to the Agent on fire) OR a `script` (an existing in-project script path relative to DATA_ROOT, e.g. 'notes/scripts/build.py', run directly on fire — bypasses the LLM; optionally `scriptArgs` and `commit`). Optional `retryMax` / `retryBackoffMs` control failure retries: set retryMax>0 only for idempotent scripts (re-running yields the same result); for non-idempotent scripts (sends messages / hits external APIs) keep retryMax=0 so a failure fires once and is reported, never repeated. Four trigger types: (1) cron — 5-field cron for complex recurring schedules; (2) interval — simple '5m'/'2h'/'1d' recurring; (3) at — a DAILY fixed time 'HH:MM' that REPEATS EVERY DAY; (4) once — a ONE-SHOT absolute date-time (ISO 8601, MUST include the full date) that fires EXACTLY ONCE. CRITICAL DISTINCTION: for one-off reminders like 'tonight 10pm', 'tomorrow 3pm', 'next Monday 8am', or any specific date, you MUST use 'once' (compute the full YYYY-MM-DD from today's date) — do NOT use 'at', because 'at' repeats every day. Use 'at' only when the user explicitly says 'every day'. After creating or modifying, confirm the next fire time to the user in your reply.",
	parameters: schema,
	async execute(_toolCallId, args: ScheduleArgs) {
		const { action } = args;

		if (action === "list") {
			const entries = loadSchedules();
			if (entries.length === 0) {
				return textResult("There are no scheduled tasks right now.");
			}
		const lines = entries.map((e) => {
			// Disabled entries don't fire, so don't show a (misleading) next-fire time.
			const next = e.enabled ? nextFireOf(e) : null;
			const rec = e.recipient ? ` push=${e.recipient.source}:${e.recipient.userId}` : "";
			const proj = e.project ? ` project=${e.project}` : "";
			const hm = e.holidayMode ? ` holidayMode=${e.holidayMode}` : "";
			const action = e.script ? `▶ ${e.script}` : `msg=${e.message}`;
			const rt = e.retry ? ` retry=${e.retry.max}` : "";
			const fire = !e.enabled
				? " disabled"
				: next
					? ` next-fire=${next}`
					: e.once
						? " (expired)"
						: "";
			return `- ${e.id} [${e.enabled ? "enabled" : "disabled"}] ${triggerOf(e)} ${action}${rt} tz=${e.timezone}${proj}${rec}${hm}${fire}`;
		});
		return textResult(`Scheduled tasks (${entries.length}):\n${lines.join("\n")}`, { count: entries.length });
		}

		// create
		if (action === "create") {
			const id = args.id?.trim() || `sched-${Date.now().toString(36)}`;
			const message = args.message?.trim();
			const script = args.script?.trim();
			if (!message && !script) throw new Error("create requires a non-empty message OR script");
			if (message && script) throw new Error("create must specify exactly one of message/script");
		const tz = args.timezone || "Asia/Shanghai";
		if (!isValidTimezone(tz)) throw new Error(`invalid timezone "${tz}"`);
		const triggers: Array<[keyof ScheduleArgs, string]> = [];
			if (args.cron !== undefined && args.cron !== "") triggers.push(["cron", args.cron]);
			if (args.interval !== undefined && args.interval !== "") triggers.push(["interval", args.interval]);
			if (args.at !== undefined && args.at !== "") triggers.push(["at", args.at]);
			if (args.once !== undefined && args.once !== "") triggers.push(["once", args.once]);
			if (triggers.length !== 1) {
				throw new Error("create must specify exactly one of cron / interval / at / once");
			}
			// Entry-level future-time check only for one-shot triggers (store layer stays permissive so
			// an expired once left in config after firing never blocks subsequent writes).
			if (triggers[0][0] === "once") {
				const t = parseOnceEpoch(triggers[0][1], tz);
				if (t === null) throw new Error(`invalid once value "${triggers[0][1]}"; use ISO 8601 date-time, e.g. 2026-08-20T22:00:00+08:00`);
				if (t <= Date.now()) throw new Error(`once time must be in the future (got ${triggers[0][1]}); use a full date-time, e.g. 2026-08-20T22:00:00+08:00`);
			}
			if (loadSchedules().some((e) => e.id === id)) {
				throw new Error(`Scheduled task id already exists: ${id}. Use update to modify it.`);
			}
			const ctx = getTaskContext();
			const raw: Record<string, unknown> = {
				id,
				enabled: true,
				timezone: tz,
				[triggers[0][0]]: triggers[0][1],
			};
			if (script) {
				raw.script = script;
				if (args.scriptArgs !== undefined) raw.scriptArgs = args.scriptArgs;
				if (args.commit !== undefined) raw.commit = args.commit;
			} else {
				raw.message = message;
			}
			const rmax = args.retryMax ?? 0;
			const rms = args.retryBackoffMs ?? 0;
			if (rmax > 0) raw.retry = { max: rmax, backoffMs: rms };
			if (args.project?.trim()) raw.project = args.project.trim();
			// holidayMode only ever carries "workday" from the agent; "off" is the default and is omitted.
			if (args.holidayMode === "workday") raw.holidayMode = "workday";
			// recipient is injected by the framework from the current message sender (CLI has none).
			if (ctx?.recipient) raw.recipient = ctx.recipient;

			saveSchedules([...loadSchedules(), raw]);
			const entry = loadSchedules().find((e) => e.id === id);
			if (!entry) throw new Error(`Failed to create: saved schedule ${id} not found`);
			const next = nextFireOf(entry);
			return textResult(
				`Created scheduled task "${entry.id}": ${triggerOf(entry)} (timezone ${entry.timezone})${next ? `, next fire: ${next}` : ""}`,
				{ id: entry.id },
			);
		}

		// update / delete / enable / disable
		if (!args.id?.trim()) {
			throw new Error(`${action} requires an id`);
		}
		const id = args.id.trim();
		const current = loadSchedules();
		const idx = current.findIndex((e) => e.id === id);
		if (idx === -1) throw new Error(`Scheduled task ${id} not found`);

		if (action === "delete") {
			const [removed] = current.splice(idx, 1);
			saveSchedules(current);
			return textResult(`Deleted scheduled task "${removed.id}".`, { id });
		}

		if (action === "enable" || action === "disable") {
			const next = { ...current[idx], enabled: action === "enable" };
			current[idx] = next;
			saveSchedules(current);
			return textResult(
				`${action === "enable" ? "Enabled" : "Disabled"} scheduled task "${id}".${action === "enable" ? (nextFireOf(next) ? ` next fire: ${nextFireOf(next)}` : "") : ""}`,
				{ id },
			);
		}

		// update
		const patch: Record<string, unknown> = { ...current[idx] };
		let newOnceForCheck: { val: string; tz: string } | null = null;
		// Action switching — reject ambiguous input, then switch only when exactly one is set.
		// Empty strings are ignored (no-op), matching the trigger rule at :219, so an unrelated
		// field edit can never accidentally empty the entry's only action.
		const newMsg = args.message?.trim() ?? "";
		const newScript = args.script?.trim() ?? "";
		if (newMsg && newScript) {
			throw new Error("update must not set both message and script; pick exactly one action");
		}
		// An actual action-type switch (message <-> script) discards the old retry, which was tuned for
		// the previous action; carrying it onto, say, a non-idempotent script could repeat side effects.
		// Editing within the same action type (e.g. a message's text, or a script's path) keeps it.
		// The caller may explicitly re-supply retry below; otherwise it stays cleared on a switch.
		const wasScript = !!current[idx].script;
		if (newScript) {
			patch.script = newScript;
			delete patch.message;
			delete patch.scriptArgs;
			delete patch.commit;
			if (!wasScript) delete patch.retry;
		} else if (newMsg) {
			patch.message = newMsg;
			delete patch.script;
			delete patch.scriptArgs;
			delete patch.commit;
			if (wasScript) delete patch.retry;
		}
		// scriptArgs / commit only apply alongside a script.
		if (patch.script) {
			if (args.scriptArgs !== undefined) patch.scriptArgs = args.scriptArgs;
			if (args.commit !== undefined) patch.commit = args.commit;
		} else {
			delete patch.scriptArgs;
			delete patch.commit;
		}
		// retry: preserve the fields that were NOT provided by merging with the existing retry,
		// so updating only retryMax does not reset an existing backoffMs (and vice versa). normalizeEntry
		// re-validates (non-negative integers, upper bounds), so a bad value is rejected with a readable error.
		if (args.retryMax !== undefined || args.retryBackoffMs !== undefined) {
			const existing = patch.retry as { max?: number; backoffMs?: number } | undefined;
			const rmax = args.retryMax ?? existing?.max ?? 0;
			const rms = args.retryBackoffMs ?? existing?.backoffMs ?? 0;
			if (rmax > 0) patch.retry = { max: rmax, backoffMs: rms };
			else delete patch.retry;
		}
		if (args.timezone !== undefined) {
			if (!isValidTimezone(args.timezone)) throw new Error(`invalid timezone "${args.timezone}"`);
			patch.timezone = args.timezone;
		}
		if (args.project !== undefined) patch.project = args.project.trim() || undefined;
		// holidayMode: "workday" enables gating; "off" (or unset) clears it. Never carries other values.
		if (args.holidayMode !== undefined) {
			patch.holidayMode = args.holidayMode === "workday" ? "workday" : undefined;
		}
		// Swapping the trigger must drop the previous trigger field(s), or the entry would
		// become invalid (normalizeSchedules would reject the leftover second trigger).
		// Empty strings count as "not provided" (same rule as create), so a model passing
		// cron:"" can't blank out an existing trigger and trip validation.
		if (args.cron !== undefined && args.cron !== "") {
			patch.cron = args.cron;
			delete patch.interval;
			delete patch.at;
			delete patch.once;
		} else if (args.interval !== undefined && args.interval !== "") {
			patch.interval = args.interval;
			delete patch.cron;
			delete patch.at;
			delete patch.once;
		} else if (args.at !== undefined && args.at !== "") {
			patch.at = args.at;
			delete patch.cron;
			delete patch.interval;
			delete patch.once;
		} else if (args.once !== undefined && args.once !== "") {
			patch.once = args.once;
			delete patch.cron;
			delete patch.interval;
			delete patch.at;
			newOnceForCheck = { val: args.once, tz: (patch.timezone as string) || "Asia/Shanghai" };
		}
		// Future-time check only when this update sets/changes the once value; edits that leave an
		// existing expired once untouched (e.g. disabling the entry) must not be blocked.
		if (newOnceForCheck) {
			const t = parseOnceEpoch(newOnceForCheck.val, newOnceForCheck.tz);
			if (t === null) throw new Error(`invalid once value "${newOnceForCheck.val}"; use ISO 8601 date-time, e.g. 2026-08-20T22:00:00+08:00`);
			if (t <= Date.now()) throw new Error(`once time must be in the future (got ${newOnceForCheck.val})`);
		}
		current[idx] = patch as unknown as ScheduleEntry;
		// Defensive guard: an update must always leave exactly one action (normalizeEntry is the
		// real SOT, but this blocks a careless edit from persisting a zero/dual-action entry).
		if (!patch.message && !patch.script) {
			throw new Error("schedule must have a message or script");
		}
		saveSchedules(current);
		const entry = loadSchedules().find((e) => e.id === id);
		const next = entry ? nextFireOf(entry) : null;
		return textResult(
			`Updated scheduled task "${id}": ${entry ? triggerOf(entry) : ""}${next ? `, next fire: ${next}` : ""}`,
			{ id },
		);
	},
};
