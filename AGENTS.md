# AGENTS.md

This file is for AI coding assistants (and any contributors) working in this repo. It explains the project structure, commands, conventions, and the security boundaries that must not be broken.

## Project overview

`botler-agent` is a **general-purpose, lightweight personal agent framework**: it receives messages from Telegram / Feishu / WeChat and uses `@earendil-works/pi-agent-core` + `@earendil-works/pi-ai` (the pi SDK) to autonomously complete short tasks within each data subproject under the data root (`DATA_ROOT`), then sends the results back.

Key design principles (understand before changing, do not break):

1. **The framework only defines boundaries, it does not hardcode business logic**: the operable directories (first-level subdirs of `DATA_ROOT`), the toolset (read/write/edit/run + schedule, where run is limited to existing in-project scripts and schedule only writes the fixed externalized `schedules.json`, neither is an arbitrary shell), the post-write JSON-validity fallback, and automatic git commit. Business schemas and rules are self-described by each data subproject's root `AGENTS.md`, concatenated into the system prompt at runtime.
2. **Each message = a brand-new `Agent` instance**, with no cross-task memory. Therefore any "retry/fix" must be a **self-contained instruction** (pointing to the file + location + how to change it), not relying on the previous conversation.
3. **App / data separation**: this repo (including `.env`) and the `DATA_ROOT` data directory are two separate locations. The data directory contains only the projects being operated on — no source code or secrets.
4. **Config externalized**: the system prompt, the real `.env`, and `providers.json` (custom model providers) live in `~/.botler-agent/` (overridable via `BOTLER_CONFIG_DIR`), reused across clones / machines. The source-dir `.env` is only a dev fallback.
5. **Path allowlist is a security boundary**: the agent's tools may only read/write first-level subdirs of `DATA_ROOT`. Do not relax `safePath`, do not add `bash`-class tools to the agent unless explicitly requested and the consequences are understood.

## Commands

```bash
npm install            # install dependencies
npm run init           # initialize ~/.botler-agent/ (.env template + providers.json template + system-prompt.md template)
npm start              # run: no args starts the persistent channel; a message arg enters CLI mode
npm start -- "message" # CLI mode: process a single message directly (local debugging)
npm run typecheck      # tsc --noEmit type check (must pass before committing)
npm test               # node:test suite (scheduler cron + store)
```

> Environment variables are in `.env.example`; the real config goes in `~/.botler-agent/.env`. `.env` is gitignored — do not commit real credentials.

## Architecture and data flow

```
channel (telegram.ts / feishu.ts)
   → dispatcher.ts  dispatch(text, {id, source})
       ├─ dedup (5-minute window, by id)
       ├─ sequential queue (Promise chain, serializes writes)
       └─ runner.ts  runTask(text)
             ├─ resolveModel() (provider/model resolution + cache)
             ├─ loadSystemPrompt() (externalized first, fallback to built-in default; injects __DATA_ROOT__ / __PROJECTS__)
             ├─ new Agent({ systemPrompt, model, tools: fileTools })
             ├─ agent.prompt(text)
             ├─ take the body from the last assistant message in state.messages
             └─ split out markdown image refs (`![](…)`), each validated by safePath
   → write task: validateState() (all data JSON valid) → commitIfChanged() commits
   → return `{ text, images }` to the channel (only the WeChat channel sends images; others use text only)

scheduler (scheduler/engine.ts) → dispatch(schedule.message, {id, source:"scheduler", projectHint})
   → if entry has a recipient: deliver({text, images}, recipient) pushes the result back
       → primary channel first, then fallback telegram → feishu → wechat (configured + recorded contacts only)
```

### Module responsibilities

| File | Responsibility |
|------|----------------|
| `src/index.ts` | Entry: positional args → CLI; otherwise start Telegram / Feishu per `.env` |
| `src/init.ts` | Initialize `~/.botler-agent/` (.env + providers.json + system-prompt.md templates); existing files not overwritten |
| `src/config.ts` | Two-level `.env` loading (user-level > source-level) + providers.json loading + `CONFIG` construction + `USER_CONFIG_DIR` |
| `src/dispatcher.ts` | Dedup, sequential queue, validation retry, commit orchestration (**never rejects**) |
| `src/runner.ts` | Build Agent, run task, extract final reply, decide whether it mutated; greeting short-circuit (`greeting.ts`) + image-aware routing |
| `src/providers.ts` | Build a custom provider config into a pi-ai `Provider` (openai-completions) |
| `src/prompts/system-prompt.ts` | Built-in generic default prompt + `loadSystemPrompt()` (externalized first + placeholder injection) |
| `src/tools/paths.ts` | **Security boundary**: `safePath()` allowlist check + `projectOf()` |
| `src/tools/{read,write,edit,run,schedule}.ts` | Five custom `AgentTool`s; read/write/edit/run checked by `safePath` (run limited to in-project python3/node scripts); `schedule` manages `schedules.json` (fixed file, no path param — the one narrow allowlist exception) |
| `src/tools/task-context.ts` | Module-level per-task context; injects the message sender as the push recipient for the `schedule` tool |
| `src/scheduler/{types,store,engine,cron}.ts` | Schedule config schema + validation/atomic store (`schedules.json`), in-process firing loop (watermark + `nextFireEpoch`), saved-listener so a schedule write wakes the loop immediately |
| `src/push/{types,contacts,deliver}.ts` | Push types; per-channel known-address store (`contacts.json`); `deliver()` primary-channel send + `telegram → feishu → wechat` fallback |
| `src/safety/validate.ts` | Post-write check that all data JSON is valid (catches edit breaking syntax), returns a self-contained `fix` instruction |
| `src/safety/git.ts` | Iterate first-level subdirs of `DATA_ROOT`, commit each independent git repo only if changed (optional push) |
| `src/channels/{telegram,feishu}.ts` | Channel adapters: grammy long polling / Feishu webhook |
| `src/channels/wechat/*` | WeChat iLink channel: QR login (`login.ts`), long-poll monitor (`monitor.ts`), media send/upload, context_token persistence (`context.ts`), owner renewal reminder loop (`reminder.ts`), inbound image download + persist (`download.ts`) and per-sender image batching (`image-batch.ts`) |

## Key implementation details (read before changing)

### 1. How to extract the final reply (`runner.ts`)

`agent.prompt()` returns `Promise<void>`; the result is not in the return value. The correct way to get it:

```ts
const state = agent.state;
const last = state.messages[state.messages.length - 1];
// Take only the text block of the last assistant message; do not concatenate all text_delta.
// (The thinking before a tool call is also a text_delta; concatenating it would mix into the reply.)
```

Do not change it to concatenate all deltas. When the LLM fails, the last message is `error`/`aborted` and `content` may be empty — return an error message instead of an empty string.

### 2. Path allowlist (`tools/paths.ts`, security boundary)

The allowlist = first-level (non-hidden) subdirs of `DATA_ROOT`, computed dynamically by `computeAllowed()` at module load — **not hardcoded** to specific project names (e.g. notes/vocab). Rules (several historical bugs were fixed here; understand before changing):

- Relative paths are `resolve`d against `DATA_ROOT`; absolute paths are kept as-is (but still throw if out of bounds).
- Use `root + sep` prefix matching to avoid `/agent2`, `/agent-bak` slipping in.
- **Do not** `realpathSync` the target file directly (a new file would cause ENOENT). Instead, `realpath` the "deepest existing ancestor" then concatenate the remaining path back, preventing symlink escapes.
- Adding a data subproject = just create a directory under `DATA_ROOT`; no change to this file needed.

### 3. Validation and self-healing (`safety/validate.ts` + `dispatcher.ts`)

The framework **does not validate business semantics** (field values, aggregation consistency, etc.) — it only checks "all data JSON is valid JSON". This is the last line of defense against `edit` text replacement breaking JSON syntax (the `write` tool itself guarantees validity). Business rules are guaranteed by each subproject's `AGENTS.md` conventions + the project's own scripts.

Each `runTask` is a brand-new Agent with no memory. On validation failure, the `fix` must be a **self-contained** correction instruction ("read file X, fix the JSON syntax, do not create a new file"). The dispatcher retries at most once; if still failing, it returns an error and does not commit.

### 4. Commit strategy (`safety/git.ts`)

`commitIfChanged(msg)` iterates each first-level subdir of `DATA_ROOT` and only does `add + commit` for those that are **independent git repos** (have `.git`) and have non-empty `git status --porcelain`. Commit message format: `agent: <user message truncated to first 60 chars>`. When `GIT_PUSH=1`, additionally push (failure is only a warning, not a blocker).

### 5. Config loading (`config.ts`)

Priority: process env vars > `~/.botler-agent/.env` > source `.env` > built-in defaults. In implementation, `parseDotEnv` only fills in when `process.env[key] === undefined`, loading user-level first then source-level, so user-level is never overridden by source-level. `USER_CONFIG_DIR` can only come from the real process env var `BOTLER_CONFIG_DIR` (not from `.env` itself, to avoid a chicken-and-egg situation).

Custom providers are defined in `~/.botler-agent/providers.json` — the externalized, primary source of truth — with each provider carrying its own `api` (wire protocol: `"openai-completions"` default, or `"anthropic-messages"`), `baseUrl` / `apiKey` / `models`. `loadProvidersFile()` parses it (malformed entries are skipped with a warning); if it is missing or unusable, `buildCustomProviders()` falls back to the legacy `CUSTOM_BASE_URL` / `CUSTOM_API_KEY` env vars + the built-in `CUSTOM_MODELS` list. `runner.ts` registers every resolved custom provider, and `PI_PROVIDER` / `PI_MODEL` select which one is used.

### 6. System prompt and two-phase routing (`prompts/system-prompt.ts` + `runner.ts`)

`runTask` is split into two phases to avoid the system prompt growing linearly with the number of subprojects:

1. **Routing** (`buildRoutePrompt`): a lightweight LLM call that only gets "project name + first 300 chars of AGENTS.md summary" to decide which subproject the message belongs to; a single project skips routing; undetermined returns `UNKNOWN`, and the dispatcher asks the user to clarify and does not execute.
2. **Execution** (`loadSystemPrompt(projectName)`): concatenates only the selected subproject's convention doc (`AGENTS.md` > `CLAUDE.md` > `CODEBUDDY.md`, first one found) in full, replacing the `__DATA_ROOT__`/`__PROJECTS__`/`__PROJECT_CONTEXT__`/`__TODAY__` placeholders.

Externalized prompt takes priority (`~/.botler-agent/system-prompt.md`), falling back to the built-in default; re-read on every call, so changes need no restart.

### 7. The run tool (controlled script execution, `tools/run.ts`)

Not an arbitrary shell. Constraints:
- Can only execute **existing** scripts inside allowlisted projects, with a fixed interpreter per extension (`.py`→python3, `.js/.mjs`→node), eliminating arbitrary execution like `python3 -c`.
- `execFileSync` passes args as an array, **not through a shell** (no `;`/`&&`/pipe injection).
- `cwd` is locked to the subproject root of the script, timeout 60s, maxBuffer 1MB.
- Script failure (non-zero exit) does not throw and interrupt; stdout/stderr are returned as text so the LLM can decide whether to fix and retry.

This is the only relaxation of the "no bash for the agent" red line: the agent can only run scripts the user has placed in the data project (e.g. my-project's build.py), not arbitrary commands. Residual risk: in-project scripts are Turing-complete, so in theory the agent could `write` a new script and then `run` it; the data directory contains no secrets and is self-managed by the user, so this risk is currently accepted.

### 8. Scheduler, the schedule tool, and push delivery (`scheduler/*` + `push/*` + `tools/schedule.ts`)

Scheduled tasks live in `~/.botler-agent/schedules.json` (`schedulesFile`): each entry is one of cron / interval / at / once, plus timezone / message / optional project / retry / silentHours / `holidayMode` / recipient. They are created from chat via the `schedule` tool, from the WebUI, or by hand-editing the file — all the same store, and a save immediately wakes the scheduler loop.

- **`holidayMode: "workday"`** (China legal-workday gating): when set on a cron / interval / at entry, it fires **only on China legal workdays** — skips statutory holidays (法定假日) and **includes 调休 makeup workdays** (补班). The cron's date fields are ignored; only its `hour:minute`(s) matter. `holidayMode: "off"` / absent = normal cron behavior. Cannot be combined with `once`. The calendar is fetched (startup + every 24h) from `BOTLER_HOLIDAY_API_URL` into `BOTLER_HOLIDAYS_FILE`; any outage keeps cached data and degrades to plain Mon–Fri.

- The `schedule` tool is a **narrow, deliberate exception** to the DATA_ROOT allowlist: it takes no file-path parameter, always writes the fixed `schedules.json` through `saveSchedules` (full normalization + 10KB message cap + atomic write + backup), and the push recipient is injected by the framework (task-context), never guessed by the model. `safePath` itself is untouched.
- `saveSchedules` fires a saved listener (`setSchedulesSavedListener`) that the engine registers with `reloadSchedules` — keeping the dependency store → engine one-way avoids the engine → dispatcher → runner → tools → schedule → engine cycle.
- The engine fires due entries into `dispatch` (source `scheduler`, id `schedule:<id>:<epoch>` to bypass dedup). If the entry has a `recipient`, the result is pushed via `deliver()`: primary channel first, then fallback `telegram → feishu → wechat`, only over channels that are configured and have a recorded contact address. WeChat pushes strip markdown (same as the reply path) and are the only ones that also send images.
- **`once` trigger (one-shot)**: a schedule entry may carry `once` — an absolute ISO 8601 datetime (`"2026-08-20T22:00:00+08:00"` or with a `Z`/offset) — instead of `cron`/`interval`/`at`. It fires exactly once at that instant and then becomes inert (`nextFireEpoch` returns `Infinity` after the watermark passes). `silentHours` is intentionally **not** applied to `once` — an explicit, exact instant. `cron.ts` resolves `once` before `compileSchedule()` (which has no `once` case and would otherwise reject the entry).
- **Routing**: messages about creating/managing schedules route to the virtual project `__scheduler__` (aliases: `scheduler`, or containing the Chinese keywords 定时 / 提醒 / 日程 — matched only AFTER real-project matches so data messages are never stolen). `loadSystemPrompt("__scheduler__")` returns the base prompt plus a scheduling-duties section instead of a data subproject's conventions. The `schedule` tool is in `fileTools`, so it works in any execution context (a single-project setup routes schedule messages to the data project but the tool still works).
- **WeChat renewal reminders**: the monitor records each allowed sender's `context_token` (+ `contacts.json` address). `wechat/reminder.ts` checks the owner's quiet time against `WECHAT_REMINDER_HOURS` (0 = off, 1-24, default 23) and nudges them via `deliver()` before the 24h window expires; `lastRemindedAt` prevents repeat nudges per quiet stretch. Re-login (`clearAccount`) drops old tokens and contacts.

### 9. WeChat inbound images (vision input)

The WeChat channel can receive images the user sends and feed them to the model as vision input.

- **Download + persist** (`download.ts`): `downloadInboundImage()` fetches the image bytes and sniffs the MIME to pick an extension; `persistInboundImage(img, project)` writes it under `DATA_ROOT/<project>/photos/<date>-<rand>.<ext>` **through `safePath`** (first-level allowlist) plus an image-extension whitelist, so the file can never escape `DATA_ROOT` or be written with a non-image extension. It returns the relative path so the agent can cite/operate on it. The date uses the agent's local `__TODAY__` timezone so filenames match the records.
- **Batching** (`image-batch.ts` + `monitor.ts`): WeChat delivers a selected photo immediately while the user may still be typing the caption ("选图即发，文字后到"). `ImageBatchCoordinator` holds image-bearing messages for a window (`WECHAT_IMAGE_BATCH_SECONDS` × 1000 ms; `0` disables) so a following text from the same sender joins as ONE task (caption + vision), and multiple photos in the window merge. The coordinator is pure/unit-tested; the monitor owns the flush timer and dispatch. Setting `WECHAT_IMAGE_BATCH_SECONDS=0` dispatches each image immediately.
- **Image-aware routing** (`runner.ts` + `greeting.ts`): `buildRoutePrompt` gets a `hasImages` flag; a message carrying images is **never** treated as a bare greeting (so the image is not swallowed). If routing cannot resolve a project AND the message had images, `fallbackUnknownReply(hasImages=true)` returns a Chinese hint telling the user to add a short text description (since the model may not be able to read the image). Providers must declare image-input support (see `providers.ts` fix) or images never reach the model.

### 10. Greeting short-circuit + Chinese routing fallback (`greeting.ts`)

- `isGreeting(msg)` strips whitespace/punctuation/symbols/combining marks (incl. full-width spaces and trailing emoji like 👋/❤️) and matches a small Chinese/English greeting set (你好/您好/喂/哈喽/嗨/hi/hello/hey/在吗…). A bare greeting has no subproject to route to.
- `runner.ts` short-circuits: when there is no `projectHint`, the source is not `scheduler`, there are no inbound images, and `isGreeting` is true, it returns `greetingReply()` **without** calling the routing LLM — a deterministic, zero-cost Chinese welcome that lists available subprojects (`projectCapabilities()`). This keeps greetings free and avoids the confusing English `UNKNOWN` template.
- `fallbackUnknownReply()` is the Chinese fallback when a (non-greeting) message cannot be routed to any subproject; it lists subprojects and, when `hasImages`, adds the image-specific guidance above. Both functions emit user-facing Chinese chat text (the intentional exception to the English-only rule for code/comments/logs).

## Conventions

- **Language**: Code (comments, UI text, identifiers), repo docs, config files (`.env`, `.env.example`, `package.json`, etc.), and runtime log/console strings are all in English. The agent-facing system prompt (`src/prompts/system-prompt.ts`) is the one deliberate exception and stays in Chinese so the agent keeps operating in Chinese with the user. Any Chinese that appears in code, config, or log/console strings must be translated into English; do not introduce new Chinese there. Identifiers are in English. **Exception for user-facing chat replies**: the framework's fixed conversational replies to the user (e.g. the greeting / routing-failure templates in `src/greeting.ts`) may be written in the user's language (Chinese) to match the agent's Chinese interaction; only code, comments, and log/console strings stay English.
- **TypeScript**: `strict: true`, `verbatimModuleSyntax: false`, `moduleResolution: Bundler`, `allowImportingTsExtensions` (imports use the `.ts` suffix, e.g. `import { CONFIG } from "./config.ts"`).
- **Modules**: `"type": "module"` (ESM), runs directly via `tsx` with no build artifacts (`noEmit`).
- **Error handling**: channel entry points and `dispatch` always catch exceptions and convert them to readable text — never let a single message crash the whole process.
- **Dependency discipline**: keep dependencies minimal. Core deps are only `pi-agent-core`, `pi-ai`, `grammy`, `https-proxy-agent`; do not introduce new deps for trivial things (follow YAGNI).
- **Before committing**: `npm run typecheck` must pass.

## Common change scenarios

- **Add a data subproject**: create a directory under `DATA_ROOT` and put an `AGENTS.md` describing its data file structure and operating rules (the agent reads it first). Allowlist, validation, and commits are all covered automatically — **no framework source change needed**.
- **Add a model/provider**: add or edit a provider block in `~/.botler-agent/providers.json` (`api` / `baseUrl` / `apiKey` / `models`), then pick it via `PI_PROVIDER` / `PI_MODEL` in `.env` and restart — **no framework source change needed**. `providers.ts` already generically supports both openai-completions and anthropic-messages gateways.
- **Add a channel**: add an adapter under `channels/`, reuse `dispatch(text, { id, source })`, and start it from `index.ts` via the `.env` switch.
- **Add a scheduled task**: just tell the agent "remind me every morning at 8am…" in any channel — it uses the `schedule` tool to write `schedules.json` and reports the next fire time. To have the result pushed back, the entry carries a `recipient` (auto-filled from the sender); you can edit `schedules.json` or the WebUI directly. **No framework source change needed.**

## Things not to do (red lines)

- Do not relax the `safePath` allowlist or add arbitrary `bash`/shell command tools to the agent (the `run` and `schedule` tools are the only exceptions: `run` only executes existing python3/node scripts inside allowlisted projects, no shell, args passed directly, 60s timeout; `schedule` only writes the fixed `schedules.json`, no file-path parameter).
- Do not hardcode secrets, paths, or model credentials into source code (always go through `.env`).
- Do not break "app / data separation": do not put app source code / `.env` into `DATA_ROOT`.
- Do not change it to concatenate all `text_delta` as the reply.
- Do not hardcode any specific subproject's business schema in the framework (that belongs in the data project's `AGENTS.md`).
