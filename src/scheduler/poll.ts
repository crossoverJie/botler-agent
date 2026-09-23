/**
 * Scheduler poll-delay math.
 *
 * Kept in its own module (no imports) so it is unit-testable without pulling in the engine's heavy
 * dependency graph — importing engine.ts transitively loads tools/paths.ts, which throws at module
 * load when DATA_ROOT is unset (e.g. on CI).
 */

export const IDLE_POLL_MS = 60_000;
export const MIN_SLEEP_MS = 1_000;

/**
 * setTimeout clamps any delay above the 32-bit signed integer max (2^31-1 ms, ~24.8 days) to 1 ms
 * and emits a TimeoutOverflowWarning. Cap just below that so a far-future `once` entry (whose
 * absolute epoch minus `now` easily exceeds the limit) cannot spin the loop. Sleeping in chunks is
 * correctness-neutral: each round re-reads schedules.json and recomputes the soonest fire.
 */
export const MAX_SLEEP_MS = 2_000_000_000; // ~23.1 days

/** Delay before the next scheduler round: idle when nothing is pending, else the gap to `soonest`. */
export function pollDelay(soonest: number, now: number): number {
	const raw = soonest === Infinity ? IDLE_POLL_MS : soonest - now;
	return Math.min(MAX_SLEEP_MS, Math.max(MIN_SLEEP_MS, raw));
}
