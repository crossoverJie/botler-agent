/**
 * Scheduler configuration types.
 *
 * A schedule is one of four mutually-exclusive trigger forms (cron / interval / at / once),
 * plus an optional timezone, a message to fire, and optional project / retry / silentHours fields.
 * The config lives in `~/.botler-agent/schedules.json` (externalized, like providers.json).
 */

import type { Recipient } from "../push/types.ts";

export interface ScheduleRetry {
	/** Max number of retries after a failure (0 = no retry). */
	max: number;
	/** Fixed backoff between retries, in ms. */
	backoffMs: number;
}

export interface SilentHours {
	/** "HH:MM" start of the do-not-disturb window (local to the schedule timezone). */
	from: string;
	/** "HH:MM" end of the do-not-disturb window (local to the schedule timezone). */
	to: string;
}

export interface ScheduleEntry {
	/** Unique, stable id. Used as the taskId source, dedup prefix, and WebUI row key. */
	id: string;
	/** Whether this entry is active. */
	enabled: boolean;
	/** 5-field cron expression: "min hour day-of-month month day-of-week". */
	cron?: string;
	/** Simple interval: "5m" / "2h" / "1d" (minute granularity, wall-clock aligned). */
	interval?: string;
	/** Daily fixed time: "HH:MM" (local to timezone). */
	at?: string;
	/** One-shot absolute time, ISO 8601. Fires exactly once, then becomes inert. */
	once?: string;
	/** IANA timezone; defaults to "Asia/Shanghai". */
	timezone: string;
	/** Message sent to the Agent when this fires (goes through normal routing / execution). */
	message: string;
	/** Optional routing hint; a valid subproject name skips the routing LLM call. */
	project?: string;
	/** Optional failure retry. */
	retry?: ScheduleRetry;
	/** Optional do-not-disturb window; fires landing inside it are deferred to the window end. */
	silentHours?: SilentHours;
	/**
	 * China legal-workday gating for cron/interval/at triggers: when "workday", the entry fires
	 * only on legal workdays — skips statutory holidays (法定假日) and includes 调休 makeup workdays
	 * (补班). The cron's date fields are ignored; only the hour:minute(s) matter. Cannot be combined
	 * with `once`.
	 */
	holidayMode?: "workday";
	/** Optional push recipient; when set, the fire result is pushed back to this address (with channel fallback). */
	recipient?: Recipient;
}
