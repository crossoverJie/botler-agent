# Design: Scheduler directly executes a script (Plan A)

> Revision 2 — adds execution serialization, unified failure handling, the WebUI edit flow,
> and the schedule-tool update/switch rules. Supersedes revision 1.

## 1. Problem & goal

Today a schedule entry only carries a `message` (plain text). When it fires,
`engine.ts` sends that text through `dispatch()` → the full routing + LLM
execution pipeline (`src/scheduler/engine.ts:95`). Even for a fully deterministic
task (e.g. "run `notes/scripts/build.py` every night at 2am"), the work still:

- pays an LLM call + routing cost, and
- is **non-deterministic** — the model may pick a different execution path.

**Goal:** let a schedule entry run an existing in-project script *directly*,
bypassing the LLM, while (a) reusing the `run` tool's security constraints and
(b) not reintroducing data races or silent failures.

## 2. Security boundaries (must not change)

The design reuses the existing `run` tool's execution core so none of the
framework's red lines are relaxed:

- Path must pass `safePath()` (first-level `DATA_ROOT` allowlist).
- Only `.py` / `.js` / `.mjs`, with a fixed interpreter (`python3` / `node`).
- `execFileSync` with args as an array — **no shell**, no `; / && / |` injection.
- `cwd` locked to the script's subproject root, 60s timeout, 1MB maxBuffer.

## 3. Schema change — `src/scheduler/types.ts`

Add an alternative "action" to `ScheduleEntry`: instead of (or in addition to)
`message`, an entry may specify `script`. **Exactly one** of `message` / `script`
is required (enforced in `normalizeEntry`).

```ts
export interface ScheduleEntry {
	id: string;
	enabled: boolean;
	cron?: string;
	interval?: string;
	at?: string;
	once?: string;
	timezone: string;
	/** Action: exactly one of `message` (→ LLM pipeline) or `script` (→ direct run). */
	message?: string;          // now OPTIONAL (was required)
	script?: string;           // NEW: path relative to DATA_ROOT, e.g. "notes/scripts/build.py"
	scriptArgs?: string[];     // NEW: args passed to the script
	commit?: boolean;          // NEW: auto git-commit data changes the script made (default true)
	project?: string;
	retry?: ScheduleRetry;
	silentHours?: SilentHours;
	holidayMode?: "workday";
	recipient?: Recipient;
}
```

## 4. Extract a *non-throwing* executor — `src/tools/run.ts`

`runTool.execute` currently **throws** on pre-conditions (path out of bounds,
missing file, wrong type — `src/tools/run.ts:40-46`) and returns a text result
only on *runtime* failure. For the scheduler we need a function that **never
throws** so failures can flow into retry/push/status uniformly.

Extract `runScript()` returning a `RunResult`, and have the tool wrap it:

```ts
export interface RunResult {
	ok: boolean;
	text: string;
	cwd?: string;                 // subproject root (only on success / runtime failure)
	errorKind?: "precondition" | "runtime";
}

export function runScript(script: string, args: string[] = []): RunResult {
	try {
		const abs = safePath(script, { projects: getTaskProjects() });
		if (!existsSync(abs) || !statSync(abs).isFile())
			return { ok: false, text: `Script does not exist or is not a file: ${script}`, errorKind: "precondition" };
		const project = projectOf(abs);
		if (!project)
			return { ok: false, text: `Script is not inside any data subproject: ${script}`, errorKind: "precondition" };
		const interpreter = interpreterOf(script);
		if (!interpreter)
			return { ok: false, text: `Unsupported script type (only .py/.js/.mjs): ${script}`, errorKind: "precondition" };
		const cwd = resolve(CONFIG.dataRoot, project);
		try {
			const out = execFileSync(interpreter, [abs, ...args], { cwd, encoding: "utf8", timeout: 60_000, maxBuffer: 1024 * 1024 });
			return { ok: true, text: out.trim() ? out : "(script ran successfully, no output)", cwd };
		} catch (e) {
			const err = e as { stdout?: string; stderr?: string; message?: string };
			const detail = `${err.stdout ?? ""}${err.stderr ?? ""}`.trim();
			const msg = detail ? `Script failed (${err.message ?? ""}):\n${detail}` : `Script failed: ${err.message ?? String(e)}`;
			return { ok: false, text: msg, cwd, errorKind: "runtime" };
		}
	} catch (e) {
		return { ok: false, text: `Script path error: ${e instanceof Error ? e.message : String(e)}`, errorKind: "precondition" };
	}
}

// AgentTool wrapper preserves current LLM behaviour: pre-condition errors still throw
// (so the agent sees an error and can self-heal), runtime failure returns text.
export const runTool: AgentTool<typeof schema> = {
	/* ... */
	async execute(_toolCallId, { script, args = [] }) {
		const res = runScript(script, args);
		if (!res.ok && res.errorKind === "precondition") throw new Error(res.text);
		return { content: [{ type: "text", text: res.text }], details: { script, cwd: res.cwd, failed: !res.ok } };
	},
};
```

## 5. Validation — `src/scheduler/store.ts` (`normalizeEntry`)

Replace the hard `message`-required check (`src/scheduler/store.ts:54-58`) with an
**exactly-one-action** rule, and validate the new fields:

```ts
const hasMessage = typeof o.message === "string" && o.message.trim().length > 0;
const hasScript  = typeof o.script  === "string" && o.script.trim().length > 0;
if (!hasMessage && !hasScript) throw new Error(`schedule "${id}" needs a message or a script`);
if (hasMessage && hasScript) throw new Error(`schedule "${id}" must specify exactly one of message/script`);
if (hasMessage && o.message.length > MESSAGE_MAX_CHARS) throw new Error(`schedule "${id}" message exceeds ${MESSAGE_MAX_CHARS} chars`);

if (hasScript) {
	const s = o.script as string;
	if (!/^(?!\/|~|\.\.)[\w./-]+\.(py|js|mjs)$/.test(s))
		throw new Error(`schedule "${id}" invalid script path "${s}" (relative path, .py/.js/.mjs only)`);
	if (o.scriptArgs !== undefined) {
		if (!Array.isArray(o.scriptArgs) || o.scriptArgs.some((a) => typeof a !== "string" || a.length > 256))
			throw new Error(`schedule "${id}" invalid scriptArgs`);
		if (o.scriptArgs.length > 64) throw new Error(`schedule "${id}" too many scriptArgs`);
	}
	if (o.commit !== undefined && typeof o.commit !== "boolean")
		throw new Error(`schedule "${id}" invalid commit`);
}
```

- File existence is **not** checked at validate time (script may be added after
  the schedule is saved); `runScript()` enforces it at fire time and turns a
  missing file into a `precondition` failure result.
- The single-trigger rule (one of cron/interval/at/once) is unchanged.

## 6. Execution serialization — route through the dispatcher queue (fixes Concern 1)

The dispatcher serializes all data writes on a module-level `chain` promise
(`src/dispatcher.ts:58`); `dispatch()` appends every run to `chain.then(...)`.
Directly calling `runScript()` from the scheduler would bypass that lock, so a
script could run concurrently with a chat task, and its `commitIfChanged()` (scoped
to the script's project) could race with in-flight chat writes.

**Fix:** add a `dispatchScriptedTask()` to `src/dispatcher.ts` that runs the script
**on the same `chain`**, reusing the exact queue-acquisition + status-accounting
pattern of `dispatch()`. The scheduler's `fire()` calls it instead of `runScript()`.

```ts
export interface ScriptTaskOptions {
	id: string;
	script: string;
	scriptArgs?: string[];
	commit?: boolean;
	source?: string;
	recipient?: Recipient;          // echoed back for stats only; delivery stays in engine
}

/** Build a history-compatible TaskLog for a direct script run (mirrors minimalLog in dispatch, but with the script's project). */
function scriptTaskLog(o: {
	taskId: string;
	source: string;
	startedAt: number;
	status: TaskStatus;
	script: string;
	replyText: string;
	project: string | null;
}): TaskLog {
	return {
		id: randomUUID(),
		taskId: o.taskId,
		phase: "execute",
		source: o.source,
		provider: CONFIG.provider,
		model: CONFIG.model,
		project: o.project,
		projects: o.project ? [o.project] : [],
		status: o.status,
		startedAt: o.startedAt,
		endedAt: Date.now(),
		durationMs: Date.now() - o.startedAt,
		userMessage: `run ${o.script}`,
		replyText: o.replyText,
		images: [],
		// For scripts, `mutated` means "the script executed, so it MAY have changed data" —
		// not merely "it succeeded". A script that errors or writes invalid JSON may still
		// have written partial data, so `ran` (not `status === "success"`) drives this flag.
		mutated: o.ran,
		tools: [],
		conversation: [],
		usage: ZERO_USAGE,
		calls: [],
		modelCache: { queries: 0, hits: 0, hitRate: 0 },
	};
}

export async function dispatchScriptedTask(opts: ScriptTaskOptions): Promise<DispatchResult> {
	const startedAt = Date.now();
	const source = opts.source ?? "scheduler";
	markEnqueued();
	markDispatched();
	const out = (text: string, status: TaskStatus): DispatchResult => ({ text, images: [], status });
	const run = chain.then(async (): Promise<DispatchResult> => {
		markStarted();
		let finalStatus: TaskStatus = "success";
		let text = "";
		let project: string | null = null;
		let ran = false;
		try {
			const res = runScript(opts.script, opts.scriptArgs ?? []);
			text = res.text;
			// Resolve the project from the script's cwd on BOTH success and failure, so the
			// task log (and thus history filtering by project) always has attribution (Fix 3).
			project = res.cwd ? projectOf(res.cwd) : null;
			// `ran` = the script actually executed (success OR runtime failure), as opposed to a
			// precondition rejection (bad path / missing file / wrong type) that never started it.
			ran = res.ok || res.errorKind === "runtime";
			// Precondition errors: the script never ran, so no data was touched — no validation needed.
			if (!res.ok && res.errorKind === "precondition") {
				finalStatus = "error";
				return out(text, "error");
			}
			// EVERY script that actually started (success OR non-zero exit) may have written data,
			// so ALWAYS validate its project (Fix 1) — even when the script also failed. This keeps
			// the JSON safety boundary regardless of the script's exit status.
			const v = validateState(project ? [project] : []);
			if (!v.ok) {
				finalStatus = "validation-failed";
				// If the script also exited non-zero, combine its output with the data-safety warning
				// so the pushed / recorded message shows BOTH. The data-safety status wins for history
				// and stats (it is the more actionable signal), but the script error is not lost.
				const scriptErr = !res.ok ? `Script failed: ${text}\n` : "";
				text = `⚠️ ${scriptErr}Script produced invalid data (left as-is, not committed): ${v.fix ?? ""}`;
				return out(text, "validation-failed");
			}
			if (!res.ok) {
				// Ran, wrote valid data, but exited non-zero.
				finalStatus = "error";
				return out(text, "error");
			}
			// Ran successfully with valid data.
			if (opts.commit ?? true) {
				commitIfChanged(`agent: scheduled script ${opts.id}`, project ? [project] : undefined);
			}
			return out(text, "success");
		} catch (e) {
			finalStatus = "error";
			text = `⚠️ Script task error: ${e instanceof Error ? e.message : String(e)}`;
			return out(text, "error");
		} finally {
			// Mirrors dispatch()'s finally (src/dispatcher.ts:270) AND appends a history-compatible
			// TaskLog so script runs appear in the WebUI schedule history / recent runs / stats (see §6.1).
			appendTaskLog(
				scriptTaskLog({ taskId: opts.id, source, startedAt, status: finalStatus, script: opts.script, replyText: text, project, ran }),
			);
			markFinished();
			recordStatus(finalStatus);
			stats.lastDispatchDurationMs = Date.now() - startedAt;
			if (finalStatus === "error" || finalStatus === "validation-failed") markFailed();
		}
	});
	// Keep the queue moving even on error (src/dispatcher.ts:281).
	chain = run.then(() => undefined, () => undefined);
	return run;
}
```

`engine.fire()` then branches only on *which* dispatcher entry point to call; the
retry / push / stats logic below is fully shared:

```ts
const result = e.script
	? await dispatchScriptedTask({ id: `schedule:${e.id}:${Date.now()}`, script: e.script, scriptArgs: e.scriptArgs, commit: e.commit, source: "scheduler" })
	: await dispatch(e.message!, { id: `schedule:${e.id}:${Date.now()}`, source: "scheduler", projectHint: e.project });
```

Both paths return a `DispatchResult`, so `deliver()` to `recipient` and the
`stats.lastFireAt/lastFireId` recording apply unchanged. One **deliberate exception:**
retry. `engine.fire()` retries on `error` *and* `validation-failed` for message tasks
(the LLM self-heals), but for **script** tasks it must retry **only on `error`** —
re-running a script on `validation-failed` would repeat its side effects (e.g. write
the bad file again). Change the retry guard in `src/scheduler/engine.ts:101`:

```ts
const failed = result.status === "error" || (result.status === "validation-failed" && !e.script);
```

A `validation-failed` script run is reported, logged, and pushed (if it has a
recipient) but **not** retried; the user fixes the script/data and the next fire
attempts again. (This is why `dispatchScriptedTask` returns `validation-failed`
rather than swallowing the error — so the failure is visible and recorded.)

**Why this resolves the race:** `dispatchScriptedTask` waits for its turn on `chain`,
so it never overlaps a chat task's writes. Its `commitIfChanged` is further scoped
to `[project]` (the script's own subproject), so it cannot commit another project's
half-written files even by accident.

### 6.1 Task history recording (fixes the "no history" gap)

The WebUI schedule history / recent runs / per-schedule stats are queried from the
task logs, matched by `taskId` prefix `schedule:<id>:` via `scheduleIdFromTaskId`
(`src/scheduler/history.ts:5,78,137`). `dispatch()` already writes a `TaskLog` for
every run; the script path must do the same or script tasks would be invisible there.

`dispatchScriptedTask` therefore appends a `scriptTaskLog(...)` in its `finally`
(mirrored from `minimalLog` in `dispatch`, but with the script's `project` filled
in). Two requirements make it show up correctly:

- The `opts.id` passed by `engine.fire()` is `schedule:${e.id}:${Date.now()}` — the
  **same** format `dispatch()` uses — so `scheduleIdFromTaskId` resolves it to the
  right schedule.
- The log's `status` (`success` / `error`) and `replyText` (script stdout, or the
  error text) feed the history table and the success/failed counters unchanged.

> If a future variant intentionally *skips* logging (e.g. a very high-frequency
> script), that must be an explicit, documented opt-out — not the default. This
> design logs every run.

### 6.2 Auto-commit scope & risk (explicit behavioural note)

On success, `dispatchScriptedTask` calls `commitIfChanged(msg, [project])`, which
runs `git add -A` over the script's project directory (`src/safety/git.ts:11,29`).
This is the **same** auto-commit behaviour the LLM path uses, so it inherits the
same property:

- It commits **all** uncommitted changes in that project — including edits the user
  made by hand (or another process made) that are unrelated to the scheduled script.
- Because a schedule fires unattended, this is more likely to surprise the user than
  an interactive chat commit.

**Default policy:** `commit` defaults to `true` (script runs are treated like any
agent write — the data project stays self-managed and committed). The risk is
accepted on the same basis as the existing auto-commit (the data dir holds no
secrets and is user-owned). Mitigations the user can apply:

- Set `commit: false` for read-only / report-only scripts that must not touch git
  history.
- Keep the project's working tree clean before relying on scheduled commits, or use
  a dedicated subproject for scheduled scripts so unrelated WIP never cohabits.

This trade-off is recorded here deliberately rather than hidden; it is **not** a
reason to drop auto-commit, because matching the existing `dispatch` behaviour keeps
one commit path and one mental model.

### 6.3 Execution semantics: at-least-once & idempotency (Fix 2)

A script fire is **at-least-once** with respect to side effects:

- On `error`, `engine.fire()` retries up to `retry.max` times (the existing
  `retry` field). If the script had already performed an external side effect
  (sent a notification, written a file, called an API) *before* failing, that
  effect is **repeated** on each retry.
- `validation-failed` is never retried (covered in §6), so it is exactly-once on
  that axis.

Because the framework cannot tell whether a script is safe to re-run, idempotency is
a **convention the user asserts via the schedule config**, not something enforced in
code:

- **Idempotent script** (re-running with the same args yields the same end-state —
  e.g. recompute-and-overwrite a report, upsert, `git add -A`): it is safe to set
  `retry: { max: N, backoffMs: M }` so transient failures self-recover.
- **Non-idempotent script** (sends messages, appends, hits external APIs with
  effects): it MUST use `retry: { max: 0 }` (the default when `retry` is omitted —
  `e.retry?.max ?? 0`), so a failure fires once and is reported rather than repeated.

The default (no `retry` field) is already safe: no retry, no repeated side effects.
This design does **not** add a separate "idempotent" flag; the existing `retry` field
is the switch, and the rule above is documented in the scheduling-duties section of
the system prompt (see §11) so the agent steers users correctly when they create
script schedules.

## 7. Unified failure handling (fixes Concern 2)

Because `runScript()` now returns a `RunResult` instead of throwing, *every* script
failure becomes a status inside `dispatchScriptedTask` and flows through the same
three mechanisms as the message path:

- **retry** — `error` only. `engine.fire()` retries a script task **only** when
  `status === "error"` (precondition / runtime failure). `validation-failed` is
  **recorded and pushed but NOT retried** — re-running the script would repeat its
  side effects (e.g. write the bad file again). This is the opposite of the message
  path, which retries `validation-failed` to let the LLM self-heal. (See §6 for the
  retry-guard change at `src/scheduler/engine.ts:101`.)
- **push**: a failed run (`error` *or* `validation-failed`) is still `deliver()`-ed
  to `recipient` (if any) so the user is told "task failed" rather than hearing
  nothing (`engine.ts:113-118`).
- **status recording**: `markFailed()` / `recordStatus()` fire in `dispatchScriptedTask`'s
  `finally`, and `stats.lastFireAt/lastFireId` are recorded in `engine.fire()`.

No script error can silently slip past these three anymore. (One nuance: a
`validation-failed` script run leaves the invalid data on disk — it is reported and
logged, but not auto-corrected; the user must fix the script/data themselves.)

## 8. `schedule` tool — create *and* update/switch (fixes the "update" gap)

`src/tools/schedule.ts` must learn the new fields and enforce exactly-one action on
**both** create and update, clearing the *other* action field when one is set (the
same pattern already used for the trigger fields, `src/tools/schedule.ts:215-239`).

Add to the schema (`ScheduleArgs` + `schema`): `script`, `scriptArgs`, `commit`.

**create** (`src/tools/schedule.ts:125-128`): replace
`if (!message) throw "create requires a non-empty message"` with:

```ts
const message = args.message?.trim();
const script = args.script?.trim();
if (!message && !script) throw new Error("create requires a non-empty message OR script");
if (message && script) throw new Error("create must specify exactly one of message/script");
```

and build `raw` with `script`/`scriptArgs`/`commit` when `script` is set, else `message`.

**update** (`src/tools/schedule.ts:198-255`): replace the existing
`if (args.message !== undefined) { if (!m) throw ... }` block (`:201-205`) with
action switching. **Single rule — empty strings never clear the current action:**

- A **non-empty** `script` switches to the script action: set `patch.script`, drop
  `patch.message` / `patch.scriptArgs` / `patch.commit`.
- A **non-empty** `message` switches back to the message action: set `patch.message`,
  drop `patch.script` / `patch.scriptArgs` / `patch.commit`.
- An **empty** value for either field is treated as "not provided" (a no-op) — it
  never clears the current action. This removes the old "empty message throws" rule,
  which was only correct when `message` was the only possible action.

```ts
// Action switching — reject ambiguous input, then switch only when exactly one is set.
// Empty strings are ignored (no-op), matching the trigger rule at :217.
const newMsg = args.message?.trim() ?? "";
const newScript = args.script?.trim() ?? "";
if (newMsg && newScript) {
	// Both non-empty is ambiguous — refuse rather than silently let one win (Fix 2).
	throw new Error("update must not set both message and script; pick exactly one action");
}
if (newScript) {
	patch.script = newScript;
	delete patch.message;
	delete patch.scriptArgs;
	delete patch.commit;
} else if (newMsg) {
	patch.message = newMsg;
	delete patch.script;
	delete patch.scriptArgs;
	delete patch.commit;
}
// scriptArgs / commit only apply alongside a script.
if (patch.script) {
	if (args.scriptArgs !== undefined) patch.scriptArgs = args.scriptArgs;
	if (args.commit !== undefined) patch.commit = args.commit;
} else {
	delete patch.scriptArgs;
	delete patch.commit;
}
```

`saveSchedules(current)` → `normalizeSchedules` → `normalizeEntry` remains the single
source of truth: an update that ends with **zero** or **two** action fields is
rejected with a readable error, so "exactly one action" holds for create *and*
update. (Because empty strings are no-ops, a well-formed existing entry can never be
accidentally emptied by an unrelated field edit.) Add a defensive guard before save:
`if (!patch.message && !patch.script) throw new Error("schedule must have a message or script")`.

List output (`src/tools/schedule.ts:106-119`) should show `script` when present
(e.g. `▶ notes/scripts/build.py`) instead of/in addition to `message`.

## 9. WebUI edit flow (fixes Concern 3)

The WebUI is **not** config-only — it has its own field whitelist and a forced
`message`. Two real spots must change:

**(a) Save whitelist — `src/webui/index.html:986-998` (`scheduleEntryForSave`).**
Currently it hardcodes `message` and drops everything else. Make `message` optional
and forward the script fields:

```js
function scheduleEntryForSave(e){
  const entry = { id:e.id, enabled:e.enabled!==false, timezone:e.timezone||"Asia/Shanghai" };
  if (e.message) entry.message = e.message;
  if (e.script) { entry.script = e.script; if (e.scriptArgs) entry.scriptArgs = e.scriptArgs; if (typeof e.commit==="boolean") entry.commit = e.commit; }
  if(e.cron) entry.cron = e.cron; else if(e.interval) entry.interval = e.interval;
  else if(e.at) entry.at = e.at; else if(e.once) entry.once = e.once;
  if(e.project) entry.project = e.project;
  if(e.retry) entry.retry = e.retry;
  if(e.silentHours) entry.silentHours = e.silentHours;
  if(e.holidayMode) entry.holidayMode = e.holidayMode;
  if(e.recipient) entry.recipient = e.recipient;
  return entry;
}
```

**(b) Modal form + save — `src/webui/index.html:1095`, `:1100-1146`, `:1170-1220`.**
- Default object (`:1095`) gains `script:"", scriptArgs:"", commit:true, action: e.script?"script":"message"`.
- Add an **Action** radio (Message / Run script). When "Run script" is selected,
  reveal `#m-script` + `#m-script-args` + `#m-commit` and make Message optional; when
  "Message" is selected, Message is required and the script fields are cleared on save.
- Replace the forced guard `if(!msg){...Message cannot be empty}` (`:1186`) with an
  action-aware check: script action requires non-empty script; message action
  requires non-empty message.
- Build `entry` accordingly, clearing the other action field so an edit never
  persists both (mirrors the trigger-clearing already done at `:1202-1207`).

**(c) List rendering — `src/webui/index.html:911`, `:921`.** When `e.script` is
present, show the script label (e.g. `▶ notes/scripts/build.py`) instead of
`e.message` in the summary card and the message column, so script tasks are
recognizable at a glance.

The read-only `GET /api/config/schedules` and `POST .../preview` endpoints need no
change — they pass entries through; once the whitelist keeps `script`, it survives
the round trip. (Keep `scheduleEntryForSave` in sync with `normalizeEntry`: a field
added there but not here is silently dropped on save — the comment at `:985`
already warns about exactly this.)

## 10. Decisions summary

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| 1 | Reuse execution core | extract non-throwing `runScript()` | one security boundary, no drift |
| 2 | Action selector | exactly one of `message`/`script` | unambiguous; avoids double execution |
| 3 | Serialization | `dispatchScriptedTask()` on dispatcher `chain` | never races chat writes; shared accounting |
| 4 | Commit scope | `commitIfChanged(msg, [scriptProject])` | can't commit another project's WIP |
| 5 | Pre-condition errors | `runScript` returns `precondition` result, not throw | failures reach retry/push/stats |
| 6 | Failure path | shared retry + `deliver()` + `markFailed` | no silent failures |
| 7 | File existence | enforced at fire time, not validate time | script may be added later |
| 8 | WebUI | whitelist + modal + save guard + list all updated | script tasks editable & visible |
| 9 | Tool update | switch action via non-empty value; empty string = no-op; `normalizeEntry` is SOT | exactly-one-action holds for updates |
| 10 | `commit` default | `true`; opt-out for read-only scripts | mirrors agent write behaviour |
| 11 | History logging | `dispatchScriptedTask` writes a `TaskLog` (same `schedule:<id>:` taskId) | script runs show in WebUI history/stats |
| 12 | Update action rule | non-empty `script` → script action; non-empty `message` → message; empty = no-op | no contradictory empty-clearing |
| 13 | Auto-commit risk | `git add -A` commits all project WIP, incl. unrelated; documented + `commit:false` opt-out | transparent, not hidden |
| 14 | Post-write validation | `validateState(project)` runs for EVERY script that started (success OR non-zero exit), even with `commit:false`; invalid JSON → `validation-failed`, data left as-is | keeps the write-safety boundary regardless of exit status |
| 15 | Retry on failure | script retries on `error` only, NOT `validation-failed` | avoids repeating script side effects |
| 16 | Ambiguous update | both `message`+`script` non-empty → reject | no silent winner |
| 17 | Failure log attribution | resolve `project` from `res.cwd` on failure too | history filtering stays correct |
| 18 | Concurrent error + bad data | `validation-failed` wins; message combines script stderr + JSON fix | data safety surfaced, script error not lost |
| 19 | `mutated` field | driven by `ran` (script executed), not `status==="success"` | log reflects possible partial writes |
| 20 | Execution semantics | at-least-once; `error` retry only if script is idempotent (`retry.max>0`); non-idempotent must use `retry.max:0` | repeated side effects are a user-asserted choice |

## 11. Files touched

1. `src/scheduler/types.ts` — `ScheduleEntry` (`message` optional; add `script` / `scriptArgs` / `commit`).
2. `src/tools/run.ts` — extract non-throwing `runScript()`; `runTool.execute` wraps it (pre-condition still throws for the LLM path).
3. `src/scheduler/store.ts` — `normalizeEntry`: exactly-one-action; validate `script`/`scriptArgs`/`commit`.
4. `src/dispatcher.ts` — add `dispatchScriptedTask()` (same `chain` + status accounting as `dispatch`); import `runScript` + `projectOf`.
5. `src/scheduler/engine.ts` — `fire()` branches to `dispatchScriptedTask()` vs `dispatch()`; retry/push/stats unchanged.
6. `src/tools/schedule.ts` — schema + `ScheduleArgs` add `script`/`scriptArgs`/`commit`; create + update enforce/switch action; list shows script.
7. `src/webui/index.html` — `scheduleEntryForSave` (whitelist), `openSchedModal` (Action radio + script fields), save guard, list rendering.
8. `src/prompts/system-prompt.ts` (scheduling-duties section) — document that the agent can offer "run a script on a schedule" and the exactly-one-action rule.

## 12. Example `schedules.json` entry

```json
{
  "id": "nightly-notes-build",
  "enabled": true,
  "cron": "0 2 * * *",
  "timezone": "Asia/Shanghai",
  "script": "notes/scripts/build.py",
  "scriptArgs": ["--all"],
  "commit": true,
  "recipient": { "source": "telegram", "userId": "123456" }
}
```
