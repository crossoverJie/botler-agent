# botler-agent

> 🌐 [中文文档](README-ZH.md)

<p align="center">
  <img src="docs/images/IMG_0592.PNG" alt="botler-agent running screenshot 1" width="32%">
  <img src="docs/images/IMG_0593.PNG" alt="botler-agent running screenshot 2" width="32%">
  <img src="docs/images/IMG_0594.PNG" alt="botler-agent running screenshot 3" width="32%">
</p>

A general-purpose, lightweight personal agent framework: it receives messages from **Telegram / Feishu / WeChat (iLink)**, autonomously completes short tasks within each data subproject under the **data root (`DATA_ROOT`)**, and sends the results back to the user. Optionally runs scheduled tasks (via an in-process scheduler) and exposes a local WebUI plus a health/metrics monitor.

- **The framework only defines boundaries**: an allowlist of operable directories, six tools (`read / write / edit / run / schedule`, plus IM-only `clear_conversation_context`; `run` only executes existing in-project scripts and `schedule` only writes the fixed externalized `schedules.json`; neither is an arbitrary shell), post-write JSON validity checks, and automatic git commit on changes.
- **Business rules are self-described by data projects**: each data subproject's root `AGENTS.md` describes its file structure and operating rules; the agent reads it before acting. The framework hardcodes no specific business logic.
- **Prompts and config are externalized**: stored in `~/.botler-agent/`, reusable across clones / machines; the user can customize them or have AI generate them.

## Why botler-agent?

A deliberately **lightweight** alternative to heavyweight agent frameworks:

- **Lightweight** — only six tools, no heavy runtime or daemon; a single `tsx` process, installed in seconds.
- **Token-thrifty** — every task is a fresh, short-lived Agent; only a bounded recent-visible-conversation window is reused across IM turns. Routing uses a tiny project-name + summary prompt, and only the selected subproject's conventions are loaded. No giant system prompts, no chasing long contexts — most tasks cost a fraction of a general-assistant call.
- **Focused on simple vertical tasks** — not a general-purpose chatbot. It shines at small, repetitive, well-scoped jobs (meal logging, vocabulary lookups, reminders), each described by its own `AGENTS.md`.

## How botler-agent compares

| | **botler-agent** | **General-purpose agents** (OpenClaw / WorkBuddy) | **Coding agents** (Claude Code / Codex) | **Cloud chatbots** (Doubao / Yuanbao / ChatGPT) |
|---|---|---|---|---|
| **Positioning** | Lightweight personal data assistant | Broad task automation | Software engineering in a codebase | Conversational Q&A service |
| **Install footprint** | A single `tsx` process, installed in seconds — no heavy runtime | Large bundles carrying many features you may never use | Heavy; expects a full dev environment | Web / app — nothing to install |
| **Built-in tools** | Only 6 controlled tools (`read / write / edit / run / schedule` + IM-only `clear_conversation_context`) | Many built-in, often complex tools | Full shell, filesystem, and command access | Chat only — no file tools |
| **File operations** | Path allowlist — only first-level subdirs of `DATA_ROOT` | Broad file access | Reads/writes across the whole workspace | None — cloud only |
| **Permissions on your machine** | Extremely restrained — no arbitrary shell | More open | Highly open (run commands, modify code) | N/A (remote cloud service) |
| **Interaction** | Mobile-first: chat from WeChat / Telegram / Feishu | Multi-surface | Desktop / terminal-centric | Web / app chat |
| **Best at** | Lightweight daily logging (meals, vocab, reminders) | General automation | Writing, refactoring, and debugging code | Conversational Q&A, drafting, brainstorming |
| **Data storage** | Local-first: lives in your own `DATA_ROOT`, structured on demand | Depends on the platform | In the codebase / repo | Cloud-only, usually unstructured markdown-like chat history |

- **vs. general-purpose agents**: botler stays lean. It has no bloated installer piling on redundant capabilities, no kitchen-sink toolset, and safer, scope-limited file access.
- **vs. coding agents**: botler deliberately does *less*. It is built for quick, lightweight record-keeping on your phone — not for operating on your computer — so it asks for almost no privileges over your machine and needs no complex tooling.
- **vs. cloud chatbots**: chatbots keep your data in their cloud as loose, unstructured chat history that is hard to maintain or reuse over the long run. botler stores everything **locally** under your control — you can shape it into structured data whenever you want, so the output stays stable and is far better suited to long-term, dependable maintenance.

**Custom structured data**: botler-agent enforces no fixed schema. Each subproject under `DATA_ROOT` brings its own `AGENTS.md` (format + conventions) and data files, so you define the structure that fits your life. For concrete, ready-to-use examples of structured-data layouts — nutrition/meal tracking, daily logs, personal accounting, travel events — see the open-source template set [`botler-agent-app`](https://github.com/crossoverJie/botler-agent-app).

## Installation

**Option A — clone from GitHub (recommended)**

```bash
git clone https://github.com/crossoverJie/botler-agent.git
cd botler-agent
npm install
npm run init
```

**Option B — curl one-liner**

```bash
curl -fsSL https://raw.githubusercontent.com/crossoverJie/botler-agent/main/install.sh | bash
```

Clones to `~/.local/share/botler-agent` (override with `BOTLER_INSTALL_DIR`), runs `npm install` + `npm run init`, and is idempotent — re-running does a `git pull`.

**Option C — npm global CLI**

```bash
npm i -g botler-agent
botler init     # scaffold ~/.botler-agent/ (.env + providers.json + system-prompt.md templates)
botler -- "your first message"   # or just `botler` to start channels per .env
```

All three methods then need the same setup: edit `~/.botler-agent/.env` (DATA_ROOT / model selection / channel credentials), optionally configure custom gateways in `~/.botler-agent/providers.json`, and customize `~/.botler-agent/system-prompt.md` if desired.

**Option D — Docker (NAS / container deployment)**

A multi-arch (`linux/amd64` + `linux/arm64`) image is published to Docker Hub (`crossoverjie/botler-agent`, tags `latest` / `edge` / `vX.Y.Z`). The container runs the persistent channel directly via `tsx` (no build step inside the container) and persists everything through two mounted volumes:

| Host concern | In-container path | Recommended mount |
| --- | --- | --- |
| Config dir (`BOTLER_CONFIG_DIR`) | `/config` | `./config:/config` |
| Data root (`DATA_ROOT`) | `/data` | `./data:/data` |

Quick start:

```bash
# 1. Generate the config templates in-place (copies .env / providers.json / system-prompt.md / schedules.json)
docker run --rm -v "$PWD/config:/config" crossoverjie/botler-agent:latest init

# 2. Edit ./config/.env: set PI_PROVIDER / PI_MODEL and your channel credentials

# 3. Start the container (WebUI / scheduler / monitor are ON by default in the image)
docker run -d --name botler --restart unless-stopped \
  -e TZ=Asia/Shanghai \
  -p 8900:8900 \
  -v "$PWD/config:/config" \
  -v "$PWD/data:/data" \
  crossoverjie/botler-agent:latest

# Open the WebUI at http://<host-ip>:8900
```

Image defaults (`WEBUI_HOST=0.0.0.0`, `WEBUI_ENABLED=1`, `SCHEDULER_ENABLED=1`, `MONITOR_ENABLED=1`) take precedence over the mounted `/config/.env` — override them at container creation (e.g. `-e WEBUI_ENABLED=0`). A `/healthz` healthcheck on port 8899 is built in. For WeChat QR login, the Feishu port, git commit identity, and upgrading steps, see [Docker deployment (NAS)](docs/docker.md).

## Quick Start

```bash
npm install

# 1. Initialize the user-level config directory (~/.botler-agent/)
npm run init

# 2. Edit ~/.botler-agent/.env, fill in DATA_ROOT / model selection (PI_PROVIDER / PI_MODEL) / channel credentials
# 3. Edit ~/.botler-agent/providers.json with your model providers (baseUrl / apiKey / models)
# 4. (Optional) Customize ~/.botler-agent/system-prompt.md (or have AI generate it from project context)

# 5a. CLI mode (local debugging: pass a single message)
npm start -- "had a steamed bun for breakfast"

# 5b. Persistent channel mode (start Telegram / Feishu / WeChat per .env)
npm start

# 6. (Optional) WeChat iLink channel — log in first via QR:
npm start -- wechat-login

# 7. (Optional) Enable the scheduler / WebUI / monitor via the .env switches (SCHEDULER_ENABLED / WEBUI_ENABLED / MONITOR_ENABLED)
```

## Example data templates

This repo bundles an open-source **data template set** as a git submodule at the repo root: [`botler-agent-app`](https://github.com/crossoverJie/botler-agent-app). It contains the *format and conventions* (not real data) of four ready-to-use subprojects — `cook` (nutrition/meal tracking), `daily-log` (daily-life logs), `ledger` (personal accounting), and `travel` (travel events). Each ships an `AGENTS.md` describing its schema plus synthetic `*.sample.json` files.

> The `botler-agent-app` submodule is **discovery-only**: botler-agent reads `DATA_ROOT`, a separate location from this framework repo, so it never operates on the in-repo submodule. Clone `botler-agent-app` into your own `DATA_ROOT` to actually use it.

Clone with the submodule:

```bash
git clone --recurse-submodules <this-repo>
# or, after a plain clone:
git submodule update --init --recursive
```

Use it as your data root (botler-agent loads each subproject's `AGENTS.md` at runtime):

```bash
export DATA_ROOT=/path/to/botler-agent-app   # or copy selected subproject folders here
npm start
```

See [`botler-agent-app/README.md`](botler-agent-app/README.md) for the per-subproject schema and customization notes.

## Architecture

```
Telegram / Feishu / WeChat (iLink)
      │  message
      ▼
 channel adapter ──► Dispatcher (dedup + sequential queue, serializes writes)
                        │
                        ▼
                  Runner (two phases: route → execute)
                        ├─ ① route: decide which subproject the message belongs to (or __scheduler__); if ambiguous, ask the user to clarify
                        ├─ ② execute: concatenate that subproject's AGENTS.md into the system prompt as needed
                        ├─ model (anthropic or a custom OpenAI-completions / anthropic-messages gateway)
                        └─ tools read / write / edit / run / schedule (allowlisted directories; run limited to in-project scripts; schedule only writes schedules.json; IM executes add clear_conversation_context)
                        │
                        ▼
                  read/write each subproject under DATA_ROOT (each subproject ships its own AGENTS.md with the conventions)
                        │
                        ▼
                  Validator (verify all data JSON is valid JSON; on failure, self-heal and retry)
                        │
                        ▼
                  Git commit (iterate each independent git repo under DATA_ROOT; commit only if changed)
                        │
                        └──► final text reply (WeChat also sends images)

Scheduler ──► dispatch(schedule.message) ──► (same pipeline)
   └─ if entry has a recipient: deliver({text,images}, recipient) → primary channel → fallback telegram → feishu → wechat
```


## Config directory `~/.botler-agent/`

| File | Purpose |
|------|---------|
| `.env` | User-level config (`DATA_ROOT`, model, channel credentials, `GIT_PUSH`, `SCHEDULER_ENABLED`, `WEBUI_ENABLED`, `MONITOR_ENABLED`, etc.), permission 600 |
| `providers.json` | Custom model providers (OpenAI-completions / Anthropic-messages gateways): `api` / `baseUrl` / `apiKey` / `models` per provider |
| `system-prompt.md` | Externalized system prompt, supports `__DATA_ROOT__` / `__PROJECTS__` / `__PROJECT_CONTEXT__` / `__TODAY__` placeholders |
| `schedules.json` | Scheduled-task config (externalized, like providers.json); created via chat, WebUI, or hand-editing |
| `contacts.json` | Per-channel known-address store for push delivery (WeChat `context_token`s + recorded addresses) |
| `task-logs/` | Per-day JSONL task logs (consumed by the WebUI) |
| `wechat/account.json` | WeChat iLink login credentials (never commit) |

Config load priority (high to low):

```
process env vars  >  ~/.botler-agent/.env  >  source .env (dev fallback)  >  built-in defaults
```

`BOTLER_CONFIG_DIR` can override the `~/.botler-agent` location (for testing / multiple configs).

### Custom model providers (`providers.json`)

Custom OpenAI-completions / Anthropic-messages compatible gateways (self-hosted / internal endpoints, e.g. a self-hosted OpenAI-completions gateway or Volcengine Ark coding endpoint) are defined in `~/.botler-agent/providers.json` (generated by `npm run init`). It is the primary source of truth for provider metadata; when the file is missing, the legacy `CUSTOM_BASE_URL` / `CUSTOM_API_KEY` env vars still work as a fallback.

`api` selects the wire protocol per provider: `"openai-completions"` (default) or `"anthropic-messages"`.

```json
{
  "providers": {
    "custom": {
      "api": "openai-completions",
      "baseUrl": "https://your-gateway.example.com/v1",
      "apiKey": "sk-…",
      "models": [
        { "id": "your-model-pro", "name": "Your Model Pro", "reasoning": true, "contextWindow": 1048576, "maxTokens": 393216, "vision": true },
        { "id": "your-model-flash", "name": "Your Model Flash", "reasoning": false, "contextWindow": 1048576, "maxTokens": 393216, "vision": true }
      ]
    },
    "ark": {
      "api": "anthropic-messages",
      "baseUrl": "https://ark.cn-beijing.volces.com/api/coding",
      "apiKey": "sk-…",
      "models": [
        { "id": "ark-code-latest", "name": "…", "reasoning": false, "contextWindow": 524288, "maxTokens": 32768, "vision": false }
      ]
    }
  }
}
```

- **Select**: set `PI_PROVIDER` to a provider id and `PI_MODEL` to one of its model ids in `.env`, then restart — the model is cached per process.
- **Add a provider / model**: just edit this file, no framework change needed. Malformed entries are skipped with a warning.
- **`api`**: `"openai-completions"` (OpenAI Chat Completions, default) or `"anthropic-messages"` (Anthropic Messages API, whose SDK appends `/v1/messages` to `baseUrl`).
- **Model fields**: `id` (as sent to the gateway), `name`, `reasoning` (supports thinking), `contextWindow` (context tokens), `maxTokens` (max output tokens), `vision` (whether the model accepts image input; **defaults to `true` — image input is enabled**. Only set `vision: false` for text-only models. If you omit the field entirely, vision stays ON. Do NOT add `vision: false` unless the model genuinely cannot read images, or WeChat inbound images and any image-based tasks will silently fail).
- **Built-in anthropic**: set `PI_PROVIDER=anthropic` and authenticate via `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` (no `providers.json` entry needed).

### Customizing the system prompt

Once the system prompt is externalized, you (or the AI assistant) can customize it for your project background:

1. `npm run init` generates `~/.botler-agent/system-prompt.md` (a general default template).
2. Replace the template with the conventions of your own data subprojects (file structure, operating rules, etc.), keeping the `__DATA_ROOT__` / `__PROJECTS__` / `__PROJECT_CONTEXT__` / `__TODAY__` placeholders.
3. You can also have the AI assistant generate it directly, e.g.:

   > My botler-agent data directory has the following subprojects: `notes/` (daily notes), `vocab/` (word book).
   > Please write a `~/.botler-agent/system-prompt.md`: describe each subproject's data file structure and operating rules,
   > following "read before write, keep JSON valid, keep field types consistent, report in Chinese after writing", and keep the `__DATA_ROOT__` / `__PROJECTS__` / `__PROJECT_CONTEXT__` / `__TODAY__` placeholders.

> **The security boundary does not change with the prompt**: allowlisted directories, no bash, etc. are enforced by code; the prompt content cannot break the tool boundary.

## Channels

- **Telegram** (`grammy`, long polling): simplest, preferred. Requires `TELEGRAM_BOT_TOKEN`; on the mainland China network a `TG_PROXY` is required to connect to the API. Set `TELEGRAM_ALLOW_FROM` (comma-separated Telegram user ids or usernames) to restrict senders; leave empty to allow all.
- **Feishu** (event-subscription webhook with decryption): requires `FEISHU_APP_ID` / `FEISHU_APP_SECRET`; optional `FEISHU_VERIFICATION_TOKEN` / `FEISHU_ENCRYPT_KEY`, listens on `FEISHU_PORT` (default 3000). Set `FEISHU_ALLOW_FROM` (comma-separated sender `open_id`/`user_id`/`union_id` or chat ids) to restrict senders; leave empty to allow all.
- **WeChat (iLink / ClawBot official Bot API)**: DM channel. Enable with `WECHAT_ENABLED=1` after logging in via `npm start -- wechat-login` (prints a QR code in the terminal; scan it with WeChat — credentials saved to `~/.botler-agent/wechat/account.json`). The account owner (the QR scanner) is always allowed; `WECHAT_ALLOW_FROM` can allowlist extra `ilink_user_id`s. A renewal reminder (`WECHAT_REMINDER_HOURS`) nudges the owner to refresh the 24h `context_token` window.
  - **Inbound images as vision input**: WeChat can receive images you send. They are decoded and fed to the model as vision input, and the originals are persisted under `DATA_ROOT/<project>/photos/`. Because WeChat delivers a selected photo immediately while you may still be typing the caption ("选图即发，文字后到"), inbound image messages are held for a short window (`WECHAT_IMAGE_BATCH_SECONDS`, default 60s; `0` disables) so a following text joins the batch as one task; multiple photos in the window also merge. If the configured model cannot read images, the agent replies in Chinese asking you to add a short text hint.
  - **Greeting short-circuit**: a bare greeting (e.g. 你好 / hi) skips the routing LLM call entirely and gets a deterministic Chinese welcome that lists the available subprojects — zero model cost.
  - **Context reset**: when the user explicitly asks for a new task or to ignore/reset previous context, the routing or execution model clears the shared recent-conversation window. A pure reset command is not stored.

Only the WeChat channel sends images; other channels deliver text only (image markdown is stripped from the reply).

## Scheduler (`schedules.json`)

Set `SCHEDULER_ENABLED=1` to run the in-process scheduler. Each entry fires into `dispatch` (bypassing dedup) and the result can be pushed back to the user if a `recipient` is set. Entries are created from chat (the `schedule` tool), the WebUI, or by hand-editing `schedules.json` — all the same store, and a save immediately wakes the firing loop.

Schema — exactly one of `cron` / `interval` / `at` / `once`:

```json
{
  "schedules": [
    {
      "id": "morning-water",
      "enabled": true,
      "cron": "0 8 * * *",
      "timezone": "Asia/Shanghai",
      "message": "提醒我该喝水了",
      "project": "my-project",
      "recipient": { "source": "wechat", "userId": "wxid_xxx" },
      "retry": { "max": 1, "backoffMs": 60000 },
      "silentHours": { "from": "23:00", "to": "07:00" }
    },
    {
      "id": "workday-standup",
      "enabled": true,
      "cron": "0 9,18 * * *",
      "timezone": "Asia/Shanghai",
      "message": "提醒我复盘今天的工作",
      "holidayMode": "workday",
      "recipient": { "source": "wechat", "userId": "wxid_xxx" }
    }
  ]
}
```

- `cron`: 5-field expression (`min hour day-of-month month day-of-week`).
- `interval`: simple `"5m"` / `"2h"` / `"1d"` (minute granularity, wall-clock aligned).
- `at`: daily fixed `"HH:MM"` (local to `timezone`).
- `once`: one-shot absolute ISO 8601 datetime (e.g. `"2026-08-20T22:00:00+08:00"` or `"2026-08-20T14:00:00Z"`). Fires **exactly once** at that instant, then becomes inert. `silentHours` is intentionally **not** applied — a one-shot reminder is an explicit, exact instant.
- `timezone`: IANA timezone; default `Asia/Shanghai`.
- `message`: instruction fired back to the Agent (goes through normal routing/execution).
- `project`: optional routing hint; a valid subproject name skips the routing LLM call.
- `recipient`: optional; when set, the fire result is pushed via `deliver()` — primary channel first, then `telegram → feishu → wechat` fallback over channels that are configured and have a recorded contact. WeChat pushes strip markdown and are the only ones that also send images.
- `retry` / `silentHours`: optional failure retry and do-not-disturb window (fires landing inside it are deferred to the window end).
- `holidayMode: "workday"`: **China legal-workday gating** for `cron` / `interval` / `at` triggers. The entry fires **only on China legal workdays** — it skips statutory holidays (法定假日) and fires on 调休 makeup workdays (补班). The cron's date fields are ignored; only its `hour:minute`(s) matter, so `0 9,18 * * *` fires at 09:00 and 18:00 on every workday. Cannot be combined with `once`. The calendar is fetched (at startup + every 24h) from `BOTLER_HOLIDAY_API_URL` into `BOTLER_HOLIDAYS_FILE`; any outage keeps the cached data and degrades to plain Mon–Fri.

**Routing**: messages about creating/managing schedules (or containing the Chinese keywords 定时 / 提醒 / 日程) route to the virtual `__scheduler__` project; the `schedule` tool is in `dataTools`, so it works in every execution context. Non-scheduler IM execute runs additionally get `clear_conversation_context`.

## WebUI

Set `WEBUI_ENABLED=1` to start a local task-log UI (binds `127.0.0.1` only, port `WEBUI_PORT`, default 8900). It reads the per-day JSONL logs under `task-logs/`, including a Monitor view (same data as the `/metrics` endpoint). The only write action is deleting task logs.

<p align="center">
  <img src="docs/images/webui-tasklog.png" alt="botler-agent WebUI task log view" width="85%">
</p>

## Monitor

By default a local health/metrics server runs on `127.0.0.1:MONITOR_PORT` (default 8899; avoid 3000=feishu / 8900=webui), exposing `/healthz` (JSON), `/metrics` (Prometheus text), and `/healthz/history` (trend sampling). Set `MONITOR_ENABLED=0` to disable only the standalone scrape endpoint; the WebUI Monitor view is sampled by the WebUI process itself and is unaffected.

## Security boundaries

- **App / data separation**: the framework (this repo, including `.env`) and the data directory (`DATA_ROOT`) are two separate locations. The data directory contains only the projects being operated on — no source code or secrets.
- **Path allowlist**: `safePath()` only permits first-level subdirectories under `DATA_ROOT`; it uses `root + sep` prefix matching to avoid `/agent2` slipping in, and does a `realpath` on the deepest existing ancestor to prevent symlink escapes.
- **No arbitrary shell**: only read/write/edit + a controlled run (limited to existing python3/node scripts inside allowlisted projects, no shell, args passed directly, 60s timeout) + a controlled schedule (only writes the fixed `schedules.json`, no file-path parameter). IM execute runs additionally expose `clear_conversation_context` only for that session and outside `DATA_ROOT`. Git commits are done by the glue layer via `execFileSync`.



## Environment variables

| Variable | Description |
|----------|-------------|
| `DATA_ROOT` | The data root directory operated on by the agent (allowlist = its first-level subdirs) |
| `BOTLER_CONFIG_DIR` | Override the user-level config directory (default `~/.botler-agent`) |
| `PI_PROVIDER` | pi-ai provider id (`anthropic` or a provider id defined in `providers.json`) |
| `PI_MODEL` | Model id (e.g. `claude-sonnet-4-5` or `your-model-flash`) |
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | Built-in anthropic auth (when `PI_PROVIDER=anthropic`) |
| `~/.botler-agent/providers.json` | Custom OpenAI-completions / Anthropic-messages gateways (baseUrl / apiKey / models per provider); legacy fallback: `CUSTOM_BASE_URL` / `CUSTOM_API_KEY` env vars |
| `TELEGRAM_BOT_TOKEN` | Telegram BotFather token; left empty to disable Telegram |
| `TG_PROXY` | Telegram API needs a proxy on the mainland China network (e.g. `http://127.0.0.1:7890`) |
| `TELEGRAM_ALLOW_FROM` | Optional comma-separated Telegram sender allowlist (user ids or usernames); empty = allow all |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | Feishu app credentials; left empty to disable Feishu |
| `FEISHU_VERIFICATION_TOKEN` | Feishu event subscription URL verification token (optional) |
| `FEISHU_ENCRYPT_KEY` | Feishu message encryption key; if empty, only plaintext events are processed |
| `FEISHU_PORT` | Feishu webhook listening port, default `3000` |
| `FEISHU_ALLOW_FROM` | Optional comma-separated Feishu sender allowlist (`open_id`/`user_id`/`union_id` or chat id); empty = allow all |
| `WECHAT_ENABLED` | `=1` to start the WeChat iLink channel (requires a completed `wechat-login`) |
| `WECHAT_ALLOW_FROM` | Optional comma-separated extra `ilink_user_id` allowlist (owner always allowed) |
| `WECHAT_REMINDER_HOURS` | WeChat renewal reminder threshold: `0`=off, `1`-`24`=remind owner after N hours (default 23) |
| `WECHAT_IMAGE_BATCH_SECONDS` | WeChat inbound-image batching window in seconds: `0`=disable batching (dispatch immediately), positive integer holds images briefly so a follow-up caption merges into one task (default 60) |
| `GIT_PUSH` | `=1` additionally git push after commit (default off; push failure is only a warning) |
| `WEBUI_ENABLED` | `=1` to start the local task-log WebUI (binds `127.0.0.1`) |
| `WEBUI_PORT` | WebUI listening port, default `8900` |
| `BOTLER_LOG_DIR` | Task-log JSONL dir (default `~/.botler-agent/task-logs/`) |
| `MONITOR_ENABLED` | `!=0` enables the local health/metrics server (default on; `=0` disables only the standalone port) |
| `MONITOR_PORT` | Health/metrics server port, default `8899` (avoid 3000 / 8900) |
| `SCHEDULER_ENABLED` | `=1` to run the in-process scheduler that fires `schedules.json` entries |
| `BOTLER_SCHEDULES_FILE` | Schedule config file (default `~/.botler-agent/schedules.json`) |
| `BOTLER_HOLIDAY_API_URL` | China legal-holiday calendar source for `holidayMode:"workday"`; the `{year}` placeholder is substituted (default `https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/{year}.json`) |
| `BOTLER_HOLIDAYS_FILE` | Cached holiday calendar file (default `~/.botler-agent/holidays.json`) |
| `MAX_TOOL_TURNS` | Max tool-call turns per task (default 20; the model is hinted to wrap up as the limit approaches, then hard-stopped) |
| `CONVERSATION_CONTEXT_ENABLED` | `=0` disables recent-conversation injection (default enabled) |
| `CONVERSATION_CONTEXT_TURNS` | Number of recent visible turns kept per IM session (default 5; `0` disables injection) |
| `CONVERSATION_TURN_MAX_CHARS` | Max characters kept per stored user/assistant message (default 4000) |
| `CONVERSATION_CONTEXT_MAX_CHARS` | Whole-window character cap for injected context; oldest turns are dropped first (default 12000) |

## How it works

### Lifecycle of a single task

1. Channel receives a message → `dispatch(text, { id, source, recipient })`.
2. **Dedup**: the same `id` (e.g. `chatId:messageId`) repeated within a 5-minute window is ignored.
3. **Sequential queue**: all tasks run serially to avoid concurrent writes corrupting the same file.
4. `runTask` has two phases: first route to decide which subproject the message belongs to (or `__scheduler__`; if undetermined, reply asking the user to clarify), then create a fresh independent Agent, concatenate that subproject's `AGENTS.md` conventions as needed, and execute the task. A `MAX_TOOL_TURNS` cap bounds the run: as the limit approaches the model is hinted to wrap up, and at the limit it is hard-stopped with a diagnostic message.
5. Read-only tasks return directly; write tasks first `validateState()` (whether all data JSON is valid):
   - Pass → `commitIfChanged()` iterates and commits the changed subprojects, appends a task log, returns the result.
   - Fail → run a fresh Agent again with a **self-contained fix instruction** to repair in place (no new record added); if still failing, return an error message and do not commit.
6. Each task appends a JSONL task log (`task-logs/`) for the WebUI/monitor, regardless of outcome.

### Scheduler & push

- `SCHEDULER_ENABLED=1` starts the firing loop; due entries are dispatched with `source: "scheduler"` and an id like `schedule:<id>:<epoch>` to bypass dedup.
- If an entry has a `recipient`, the result is pushed via `deliver()`: primary channel first, then `telegram → feishu → wechat` fallback over configured channels with a recorded contact address.

### Data file conventions

The framework hardcodes no data schema. Each data subproject's root `AGENTS.md` (or `CLAUDE.md`, or `CODEBUDDY.md` — first one found) is the authoritative convention, concatenated into the system prompt on demand once routing determines the subproject. The `write` tool guarantees serializing valid JSON; `validate.ts` only catches cases where `edit` breaks JSON syntax.

## Scripts

| Command | Description |
|---------|-------------|
| `npm start` | Run (CLI or persistent channel, depending on args and .env) |
| `npm run dev` | Same as `npm start` |
| `npm run init` | Initialize `~/.botler-agent/` |
| `npm run typecheck` | `tsc --noEmit` type check |
| `npm run cli` | Same as `npm start` |
| `npm test` | `node:test` suite (scheduler cron + store) |

## License

[AGPL-3.0](LICENSE) (GNU Affero General Public License v3). Third-party dependency licenses are reproduced in [`THIRD-PARTY-LICENSES`](THIRD-PARTY-LICENSES).


## Related docs

- Docker deployment (NAS / container): [docs/docker.md](docs/docker.md)
- Repo guide for AI coding assistants: [`AGENTS.md`](AGENTS.md)
- Environment variable template: [`.env.example`](.env.example)
