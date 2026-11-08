# AGENTS.md

This file is for AI coding assistants (and any contributors) working in this repo. It explains the v2 layout, commands, invariants, and the security boundaries that must not be broken.

## Scope: v2 is the Rust rewrite

The `v2` branch is a from-scratch rewrite in Rust (`crates/botler`) of the v1 TypeScript framework. The design and rationale live in [`docs/design/rust-rewrite-v2.md`](docs/design/rust-rewrite-v2.md); read it before changing session, queue, rotation, or memory behavior.

- **Rust is the product** on this branch. Put new behavior in `crates/botler`.
- **v1 TypeScript sources remain in the tree** (`src/`, `bin/`, `scripts/install.sh`, `Makefile`) as the parity reference and for the `botler-agent-app` submodule tooling. Do not extend them unless the change is explicitly v1 parity work.
- **Not yet ported**: Docker image, npm packaging, install/one-liner scripts, pi embedding (spikes S-1/S-5). The binary runs the deterministic core against `StubRuntime`; the network channels are behind the `channels` feature. The WebUI is ported behind the `webui` feature, but its token/tool/conversation views are degraded while S-1/S-5 are open and WeChat pairing is blocked by S-4 — see "Channels".
- **Spikes are unresolved**: S-1 (pi provider registration), S-2 (tool IO parity), S-4 (weixin-agent transport), S-5 (multi-session embedding), S-6 (license). Do not claim any of these work until the spike records a pass.

## Project overview

`botler-agent` is a general-purpose, lightweight personal agent framework. It receives messages from Telegram / Feishu / WeChat (iLink), completes short tasks inside data subprojects under the data root (`DATA_ROOT`), then sends the result back. An in-process scheduler can fire recurring tasks and push the result to the user.

Non-negotiable design principles (understand before changing):

1. **The framework only defines boundaries, not business logic.** Operable directories are the first-level, non-hidden subdirs of `DATA_ROOT`; the tools are `read / write / edit / run / schedule` plus `retain / recall` memory tools, and the IM-only `clear_conversation_context`. Business schemas and rules are self-described by each subproject's root `AGENTS.md`, selected at routing time and concatenated into the system prompt.
2. **One persistent session per `(source, conversation_id, project)`.** A message resumes that session instead of creating a fresh agent. The session file name is opaque (`s-<sha256-32>.jsonl`), derived from the canonical key hash; never derive it from a raw conversation id.
3. **Every inbound message is enqueued at receive time.** `Dispatcher::enqueue` returns a handle immediately; routing, session acquisition, tool execution, validation, and commit all happen on one process-wide worker. There is no per-project or per-channel concurrency: this is the ordering contract.
4. **A live session is never rotated mid-turn.** Rotation is a 5-step durable WAL (`rotation.rs`): write intent → rename + fsync → update index → append audit journal → delete intent. Recovery runs before any resume; ambiguous states are quarantined, never guessed.
5. **Freshness beats reuse.** A session is only served when its fingerprint matches: project, prompt fingerprint, provider, model, tool-policy version. The fingerprint is persisted in the session index, so a **cold resume** after restart or LRU eviction is checked too; a missing fingerprint counts as stale. Any mismatch archives the old session and opens a fresh one. A session file that the index does not vouch for is never resumed: it is quarantined (`.unindexed-<ts>.jsonl`) and a fresh session is opened. Never serve a stale prompt, provider, or tool policy.
6. **Memory is tiered and write-minimized.** Tier 1 personal is the default; tier 2 project-shared and tier 3 user-global require an explicit `tier` argument. Group conversations never read or write tier 3. Only a bounded digest and recall results enter the prompt, never the whole store.
7. **`PathGuard` (`paths.rs`) is the security boundary.** All file tools resolve through it. Do not relax it: no `realpath` of a new file (it would ENOENT), resolve the deepest existing ancestor and re-join the rest, prefix-match with a separator to block sibling-prefix escapes, and reject symlink escapes. A dangling symlink is rejected too: `symlink_metadata` must not be skipped just because `exists()` is false.
8. **Validation is syntax-only, with one self-heal retry.** The framework checks that every data JSON file parses; it never validates business semantics. The retry instruction must be self-contained (file, location, fix), because the agent has no memory of the failed turn.
9. **Config is externalized.** The real `.env`, `providers.json`, `system-prompt.md`, `schedules.json`, `contacts.json`, sessions, and memory live under `~/.botler-agent` (override with `BOTLER_CONFIG_DIR`). The repo `.env` is only a dev fallback. Never commit secrets or point `DATA_ROOT` at this repo.

## Repository layout

```text
crates/botler/
  src/
    main.rs              binary: admin CLI, channel startup, scheduler, retention, WebUI/monitor
    lib.rs               module tree + tracing init
    config.rs            two-level .env loader, providers.json, all settings
    dispatcher.rs        enqueue + serial queue, dedup, validate/commit, task-log writes
    runner.rs            route -> acquire session -> run turn -> extract reply
    runtime.rs           AgentRuntime / AgentSession seam + StubRuntime
    identity.rs          SessionKey, PrincipalKey, MemoryContext/Namespace, opaque names
    session_registry.rs  hot pool, fingerprint check, acquire/rotate
    rotation.rs          WAL rotation + crash recovery + audit journal
    session_index.rs     live/archived index (atomic)
    retention.rs         session TTL + size-cap sweep, discovery, CLI helpers
    memory.rs            three-tier store, recall, digest, retain/recall tools
    prompt.rs            system prompt rendering and routing prompt
    greeting.rs          greeting short-circuit (zero-cost)
    paths.rs             PathGuard allowlist (security boundary)
    fsutil.rs            atomic write, fsync, dirs, time helpers
    task_log/            v1-shaped TaskLog schema + per-day JSONL store (always compiled)
    monitor/             health snapshot, Prometheus metrics, 5s sampler (cfg webui)
    webui/               axum routers, read-only API, config backup/restore, SPA (cfg webui)
    tools/               read, write, edit, run, schedule, clear, memory, task_context
    safety/              JSON validation + per-subproject git commit
    scheduler/           types, store, engine, cron/once, China holidays
    push/                types, contacts store, deliver with channel fallback
    channels/            allowlist, image batching, telegram, feishu, wechat/*
  tests/
    acceptance.rs        end-to-end scenarios over the real dispatcher
    parity.rs            table-driven v1 parity fixtures
    fixtures/            greeting.json, image_batch.json, cron.json, log_query.json
```

## Commands

The deterministic core builds on stable. The pinned `nightly-2026-08-31` is only for the future pi embedding (`--features pi-sdk`), so use `cargo +stable` explicitly while iterating.

```bash
cargo +stable build                          # debug build
cargo +stable build --release --features channels
cargo +stable build --features webui,channels            # WebUI + health endpoints
cargo +stable test -p botler --lib           # unit tests
cargo +stable test -p botler --lib --features webui     # monitor / WebUI unit + HTTP tests
cargo +stable test -p botler --test acceptance
cargo +stable test -p botler --test parity
cargo +stable test -p botler --features channels   # includes channel adapters
cargo +stable test -p botler --features webui,channels
cargo +stable fmt --all -- --check
cargo +stable clippy --all-targets --features channels -- -D warnings
cargo +stable clippy --all-targets --features webui,channels -- -D warnings
cargo +stable run -- "had a steamed bun for breakfast"   # one CLI task
cargo +stable run                                        # server mode (channels + scheduler)
```

Admin commands (recognized before any task runs, so they never reach the model):

```bash
botler sessions list
botler sessions inspect <file>
botler sessions purge [--older-than-days N] [--max-bytes N] [--all]
botler memory list [project]
botler memory delete <id> [project]
botler memory clear <personal|project|global> [project]
```

Process control for a local deployment is in `scripts/botlerctl.sh` (`restart|start|build|stop|status|logs`); it only touches the repo directory and `target/`, never `DATA_ROOT` or `~/.botler-agent`. `restart` defaults to the Rust branch (`BOTLER_BRANCH`, default `v2`), not `main` (v1 TypeScript); `restart stable` follows the latest release and falls back to the branch when no tag exists.

## Architecture and data flow

```text
channel adapter (telegram / feishu / wechat)
   |  message
   v
Dispatcher::enqueue  (immediate handle; dedup by message id)
   |
   v
single worker  ->  Runner::run
   |  route: pick a project (or __scheduler__); UNKNOWN asks the user to clarify
   |  acquire: session registry keyed by (source, conversation_id, project)
   |           fingerprint mismatch -> archive + fresh session (WAL)
   |  execute: session turn with the project's tools
   |  reply:   last assistant text only; images parsed and path-checked
   v
Validator (all data JSON parses) -> on failure, one self-contained fix retry
   v
Git commit (per independent subproject repo, only if changed)
   v
reply to the originating channel (WeChat also sends images)

Scheduler -> dispatcher (source = scheduler, id = schedule:<id>:<epoch>)
   -> optional push via deliver(): primary channel, then telegram/feishu/wechat
```

## Key implementation details

### Sessions and the queue

- `identity::SessionKey` hashes length-prefixed parts; two distinct keys can never share a file name. `Dispatcher` owns a single `mpsc` channel and one worker task. Never await a tool or model call outside the worker and then write data, or the ordering contract breaks.
- `SessionRegistry::acquire` returns a hot handle, a resumed handle, or a fresh one. `rotated` and `fresh` are surfaced so the caller can explain the change. A corrupt/incompatible file is quarantined to `archived/*.corrupt-<ms>.jsonl` and reported once; the process must not crash-loop.
- `SessionIndex` is the only source of truth for live vs archived locations. It is written atomically. The rotation journal is append-only and idempotent by `rot_id`.
- `retention.rs` enforces `BOTLER_SESSION_RETENTION_DAYS` and `BOTLER_SESSION_RETENTION_MAX_BYTES` at startup and every 24h. TTL is applied first, then the oldest survivors are removed until the cap holds. `0` disables that dimension. It never touches the index, intents, or journal.

### Memory

- Tiers: 1 personal `(source, principal|conversation, project)`, 2 project `(project)`, 3 user-global `(source, principal)`.
- `retain` defaults to tier 1; tier 2/3 require the explicit argument. Group contexts are refused tier 3 on both read and write.
- Prompt injection uses a bounded digest (`BOTLER_MEMORY_DIGEST_MAX_CHARS`); `recall` returns a token-budgeted slice. Never dump the store into the prompt.
- The admin CLI constructs a synthetic local-owner context; that is the confirmation for a project-wide `clear`.

### Tools and safety

- `run` is not a shell. It executes existing `.py` / `.js` / `.mjs` files inside an allowlisted project, with a fixed interpreter and an argv array (no shell), `cwd` locked to the project, 60s timeout. Both pipes are drained concurrently and only the first 1MB of each is kept, so a noisy script cannot block on a full pipe or exhaust memory. Non-zero exit is returned as text, not an error. `run` reports `mutated` even on failure: a script can write before exiting non-zero, so validation and commit must still run.
- `schedule` has no path argument; it only writes the configured `schedules.json`, normalizes and caps messages, writes atomically, and keeps a backup. The push `recipient` and the creator `origin` are both injected by task context, never guessed by the model. `origin` (source + principal + conversation kind) is what selects memory identity; the recipient is only the delivery address.
- `clear_conversation_context` is IM-only and only clears the active session; it takes no path.
- `write` guarantees valid JSON for `.json` files. `edit` does not, which is why the post-turn validator exists.
- Commit is per independent git repo under `DATA_ROOT`, only when `git status --porcelain` is non-empty, message `agent: <first 60 chars of the message>`. `GIT_PUSH=1` pushes as a best-effort extra step; push failure is a warning, not a task failure.

### Routing and prompt

- Routing is a separate lightweight call over project name + short summary. A single project skips it. `__scheduler__` is a virtual project for scheduling messages.
- `prompt.rs` renders the externalized `system-prompt.md` if present, otherwise the built-in default. Supported placeholders: `__DATA_ROOT__`, `__PROJECTS__`, `__PROJECT_CONTEXT__`, `__TODAY__`. The rendered system prompt must stay date-free (the date belongs in the per-turn envelope): persistent sessions are reused across days, so a concrete date baked into the prompt would be stale. `__TODAY__` therefore renders an instruction to read the turn envelope, not the date itself.
- A bare greeting with no images and no project hint short-circuits before routing: deterministic Chinese welcome, zero model cost.
- The user-facing chat replies stay Chinese; code, comments, logs, and repo docs stay English.

### Channels

- Telegram long polling and the Feishu webhook are behind the `channels` feature (`axum`). Sender allowlists are shared helpers; empty means allow all, except WeChat where the account owner is always allowed.
- The WebUI and health/metrics endpoints are behind a separate `webui` feature (also `axum`); `channels` is not required. `WEBUI_ENABLED=1` serves the SPA on `127.0.0.1:8900` by default and `MONITOR_ENABLED` (default on) serves `/healthz`, `/metrics`, `/healthz/history` on `127.0.0.1:8899`. WebUI enablement is deliberately **not** part of `Config::has_channels()`. Every state-changing write requires `X-Botler-UI: 1`; no CORS headers are emitted. Task-log records are appended only inside the single dispatcher worker, and WebUI handlers hold only read-only handles (`Arc<DispatchStats>` + a store path), never `Arc<Dispatcher>` or `PathGuard`.
- WebUI parity is **surface-complete but data-degraded**: under `StubRuntime` there is no LLM, so token usage, tool/conversation timelines, system prompt, routing, and model-cache fields are zero/empty (records carry `degraded: true`); the Stats chart and log detail drawer are empty. V8 heap and event-loop-lag metrics are zero; RSS is real only on Linux. WeChat pairing endpoints return an explicit unavailable payload because the `weixin-agent` transport (S-4) is not linked. Never claim these views carry real data.
- Feishu is fail-closed. `FEISHU_VERIFICATION_TOKEN` is mandatory whenever the app id/secret are set: config load fails fast, and startup refuses to serve a webhook without it. Every event (including `url_verification`) must carry a matching token. When `FEISHU_ENCRYPT_KEY` is configured, plaintext events are rejected and encrypted requests must carry a valid `X-Lark-Signature` (`hex(sha256(timestamp + nonce + encrypt_key + body))`); an invalid or missing signature is `401`.
- Feishu encryption is AES-128-CBC with `key = md5(encrypt_key)`, IV = first 16 bytes of the ciphertext, PKCS#7 padding. That matches the Feishu spec, not v1's commented AES-256 path; keep the vector test.
- WeChat app layer (gate, context tokens, batching, reminder, image download/persist) is complete, but the `weixin-agent` transport is spike S-4 and not linked. The channel logs that it cannot receive messages in this build.
- Inbound WeChat images are held for `WECHAT_IMAGE_BATCH_SECONDS` (default 60, `0` disables) so a late caption joins one task. Persisted files go through `PathGuard` plus an image-extension whitelist. Each buffered image re-arms the flush timer (generation counter), so a burst flushes only after the last image's window expires; an older timer that loses the claim is a no-op.

### Scheduler and push

- One of `cron` / `interval` / `at` / `once`; `holidayMode: "workday"` gates cron/interval/at to China legal workdays (skips holidays, includes makeup Saturdays), ignoring cron date fields and honoring only `hour:minute`. `once` is an absolute ISO 8601 instant and bypasses `silentHours`.
- Saves wake the firing loop immediately. Firing goes through the same global queue with an id that bypasses dedup.
- A scheduled entry stores two independent identities: `recipient` (where the result is pushed) and `origin` (who created it: source, principal, conversation kind). Memory context is built from `origin`, never from the delivery chat id. A schedule created in a group stays a group context, so tier 3 remains blocked; legacy entries without `origin` keep the old recipient-based behavior.
- Delivery tries the entry's primary channel first, then `telegram -> feishu -> wechat`, only over configured channels with a recorded contact. WeChat is the only channel that sends images.

## Testing expectations

- **Unit tests live with the code** (`#[cfg(test)] mod tests`). Deterministic pieces need tests; time and randomness must be injected or derived from explicit timestamps.
- **Acceptance** (`tests/acceptance.rs`) exercises the real dispatcher: ordering across projects, channel non-collision, scheduler firing, reset rotation, docs-edit rotation, corrupt resume, hot-session reuse, v1-shaped JSONL task logs. Keep it green.
- **Parity** (`tests/parity.rs`) runs table-driven fixtures for greeting, image batching, cron/once/workday/silent-hours, and task-log query primitives (`log_query.json`). Add a fixture case rather than a bespoke test when the behavior is pure.
- **WebUI HTTP tests** live inline in `webui/server.rs` (`#[cfg(feature = "webui")]`): bind `build_router` to `127.0.0.1:0` with `tokio::spawn(axum::serve(...))` and drive it with the existing `reqwest` dependency. No `tower` dev-dep. Cover status codes, `X-Botler-UI` gating, body limits, and v1 response shapes.
- **Property tests** cover the path boundary and hostile identifiers (`proptest`), including traversal, sibling prefixes, and symlink escapes.
- `npm run typecheck` and the v1 `node:test` suites do not cover v2. Run the cargo commands above before committing.

## Conventions

- **Language**: code, comments, identifiers, logs, and repo docs are English. User-facing chat text (system prompt and the fixed greeting/routing-failure replies) is Chinese, matching the user. Do not introduce Chinese into code or logs beyond those reply constants.
- **Rust**: edition 2021, `rust-version = 1.90`, stable-compatible core. Keep the `AgentRuntime` seam small; pi types must not leak into the deterministic core.
- **Dependencies**: minimal. Adding a crate needs a reason; prefer std/`tokio`/`serde`/existing crates. `proptest` and `tempfile` are dev-only.
- **License**: the workspace is `AGPL-3.0-only`, matching the repository `LICENSE` and v1 `package.json`. Do not declare a different license in `Cargo.toml` or a crate manifest.
- **Errors**: channel entry points, the dispatcher, and the scheduler loop must convert failures into readable text and keep running. A single bad message must never kill the process.
- **Time**: sessions, retention, scheduler, and tests use epoch milliseconds; do not call `SystemTime::now()` deep in pure logic without an injectable seam.
- **No comments that restate the code.** Comment only hidden constraints and invariants (why the deepest-ancestor check exists, why a fingerprint excludes `__TODAY__`, why a rotation step is ordered).

## Common change scenarios

- **Add a data subproject**: create a directory under `DATA_ROOT` with an `AGENTS.md`. Allowlist, routing, validation, and commits pick it up automatically; no source change.
- **Add a model/provider**: edit `~/.botler-agent/providers.json` and select with `PI_PROVIDER` / `PI_MODEL`. The core does not need a change; the pi bridge does (spike S-1).
- **Add a tool**: implement `tools::Tool`, resolve every path through `PathGuard`, set `mutated` honestly, and register it in `runner.rs`. If it is IM-only, gate it there.
- **Change rotation**: update the plan, the WAL steps, and the kill-point tests together. Any new step must be recoverable from the intent file alone.
- **Add a channel**: implement the adapter against `Dispatcher` + `PushService`, add an allowlist, and gate it behind a feature or an env switch.
- **Add a WebUI endpoint**: keep the handler read-only against `AppState` (`MonitorState`), require `X-Botler-UI: 1` on writes, preserve the v1 JSON shape so the SPA needs no edit, and add an HTTP test. Never enqueue work or write task logs from a handler.

## Red lines

- Do not relax `PathGuard` or add a general shell/`bash` tool. `run`, `schedule`, and `clear_conversation_context` are the only deliberate exceptions, each with no arbitrary command or path surface.
- Do not rotate, archive, or replace a session while a turn holds it. Use the WAL path after the turn.
- Do not enqueue work outside the single dispatcher worker in a way that permits concurrent writes to the same project.
- Do not make memory tier 2/3 the default, and do not expose tier 3 to group conversations.
- Do not validate business semantics in the framework; that belongs to each project's `AGENTS.md` and scripts.
- Do not concatenate all text deltas as the reply; take the last assistant message's text blocks only.
- Do not commit secrets, real `.env` files, or source code into `DATA_ROOT`; app and data stay separate.
- Do not claim the pi runtime, WeChat transport, or Docker image works while S-1/S-4/S-5 are open. The WebUI is ported, but do not claim its token/tool/conversation views, WeChat pairing, or monitor lag/heap metrics carry real data while those spikes are open.
