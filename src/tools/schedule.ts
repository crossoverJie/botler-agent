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
import { loadSchedules, saveSchedules } from "../scheduler/store.ts";
import { nextFireEpoch } from "../scheduler/cron.ts";
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
	message: Type.Optional(Type.String({ description: "Message content sent to the Agent when the task fires (required for create)" })),
	cron: Type.Optional(Type.String({ description: "5-field cron expression, e.g. '0 8 * * *' (one of three for create)" })),
	interval: Type.Optional(Type.String({ description: "Simple interval, e.g. '5m' / '2h' / '1d' (one of three for create)" })),
	at: Type.Optional(Type.String({ description: "Fixed daily time 'HH:MM' (one of three for create)" })),
	timezone: Type.Optional(Type.String({ description: "IANA timezone, default Asia/Shanghai" })),
	project: Type.Optional(
		Type.String({ description: "Routing hint (data subproject name); when set, the routing LLM is skipped on fire and the project runs directly" }),
	),
});

type ScheduleArgs = {
	action: "list" | "create" | "update" | "delete" | "enable" | "disable";
	id?: string;
	message?: string;
	cron?: string;
	interval?: string;
	at?: string;
	timezone?: string;
	project?: string;
};

function triggerOf(e: ScheduleEntry): string {
	if (e.cron) return `cron ${e.cron}`;
	if (e.interval) return `interval ${e.interval}`;
	if (e.at) return `at ${e.at}`;
	return "(no trigger)";
}

function fmtFire(epoch: number, tz: string): string {
	return new Date(epoch).toLocaleString("en-US", { timeZone: tz, hour12: false });
}

function nextFireOf(e: ScheduleEntry): string | null {
	try {
		return fmtFire(nextFireEpoch(e, Date.now()), e.timezone);
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
		"Create / view / modify / delete / enable / disable scheduled tasks (writes the fixed schedule config, takes effect immediately). Use when the user says things like 'remind me every morning at 8am…', 'summarize every 2 hours…', 'remind me in X minutes…' that contain scheduling / reminder / calendar intent. create must specify exactly one of cron / interval / at, plus a non-empty message (the instruction sent back to the Agent on fire). After creating or modifying, confirm the next fire time to the user in your reply.",
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
			const fire = !e.enabled ? " disabled" : next ? ` next-fire=${next}` : "";
			return `- ${e.id} [${e.enabled ? "enabled" : "disabled"}] ${triggerOf(e)} tz=${e.timezone}${proj}${rec}${fire}`;
		});
		return textResult(`Scheduled tasks (${entries.length}):\n${lines.join("\n")}`, { count: entries.length });
		}

		// create
		if (action === "create") {
			const id = args.id?.trim() || `sched-${Date.now().toString(36)}`;
			const message = args.message?.trim();
			if (!message) throw new Error("create requires a non-empty message (the instruction sent to the Agent on fire)");
			const triggers: Array<[keyof Omit<ScheduleArgs, "action" | "id" | "message" | "timezone" | "project">, string]> = [];
			if (args.cron !== undefined && args.cron !== "") triggers.push(["cron", args.cron]);
			if (args.interval !== undefined && args.interval !== "") triggers.push(["interval", args.interval]);
			if (args.at !== undefined && args.at !== "") triggers.push(["at", args.at]);
			if (triggers.length !== 1) {
				throw new Error("create must specify exactly one of cron / interval / at");
			}
			if (loadSchedules().some((e) => e.id === id)) {
				throw new Error(`Scheduled task id already exists: ${id}. Use update to modify it.`);
			}
			const ctx = getTaskContext();
			const raw: Record<string, unknown> = {
				id,
				enabled: true,
				timezone: args.timezone ?? "Asia/Shanghai",
				message,
				[triggers[0][0]]: triggers[0][1],
			};
			if (args.project?.trim()) raw.project = args.project.trim();
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
		if (args.message !== undefined) {
			const m = args.message.trim();
			if (!m) throw new Error("update message must not be empty");
			patch.message = m;
		}
		if (args.timezone !== undefined) patch.timezone = args.timezone;
		if (args.project !== undefined) patch.project = args.project.trim() || undefined;
		// Swapping the trigger must drop the previous trigger field(s), or the entry would
		// become invalid (normalizeSchedules would reject the leftover second trigger).
		// Empty strings count as "not provided" (same rule as create), so a model passing
		// cron:"" can't blank out an existing trigger and trip validation.
		if (args.cron !== undefined && args.cron !== "") {
			patch.cron = args.cron;
			delete patch.interval;
			delete patch.at;
		} else if (args.interval !== undefined && args.interval !== "") {
			patch.interval = args.interval;
			delete patch.cron;
			delete patch.at;
		} else if (args.at !== undefined && args.at !== "") {
			patch.at = args.at;
			delete patch.cron;
			delete patch.interval;
		}
		current[idx] = patch as unknown as ScheduleEntry;
		saveSchedules(current);
		const entry = loadSchedules().find((e) => e.id === id);
		const next = entry ? nextFireOf(entry) : null;
		return textResult(
			`Updated scheduled task "${id}": ${entry ? triggerOf(entry) : ""}${next ? `, next fire: ${next}` : ""}`,
			{ id },
		);
	},
};
