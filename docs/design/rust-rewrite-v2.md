# Botler v2 — Rust Rewrite on the Pi Rust SDK

Status: proposal, rev.9 — tier 3 keyed by `PrincipalKey = (source, authenticated_principal_id)` (no raw-id merges; cross-channel sharing only via post-v2.0 user-confirmed account linking), plus normative tier-3 write-minimization (explicit request or non-sensitive task-necessary facts; sensitive categories never auto-retained); S-3 and scenario 11 updated with identity-collision and minimization checks. (rev.8: three-tier memory model for cross-project association. rev.7: `rot_id` audit de-dup. rev.6: WAL index-before-intent-deletion ordering. rev.5: preamble honesty + fsync WAL. rev.4: memory namespaces, static fingerprint, §4.8, 10 scenarios. rev.3: channel-namespaced key, routing in-queue, conservative rotation, P5 retention. rev.2: normative §4.0 contract, S-1…S-6 P0 gate.)
Branch: `v2`
Dependencies under evaluation:

| Component | Source | Role |
|---|---|---|
| `pi_agent_rust` (crate `pi`, lib target `pi::sdk`) | `github.com/Dicklesworthstone/pi_agent_rust` v0.8.0 | Agent core: sessions, providers, streaming, compaction, memory, tools |
| `weixin-agent-sdk-rs` (crate `weixin-agent`) | `github.com/spensercai/weixin-agent-sdk-rs` | WeChat iLink protocol layer (replaces `src/channels/wechat/*` protocol code) |

---

## 1. Goals and non-goals

### Goals

1. Rewrite botler-agent in Rust as a **single native binary** (no Node runtime), preserving every v1 user-visible behavior: channels, tools, scheduler, push, validation, git commit, path allowlist.
2. **Persistent, compaction-managed conversation history across restarts** (replaces the `CONVERSATION_CONTEXT_TURNS` sliding window), following the openclaw model:
   - automatic context compaction when the window fills;
   - long-term memory with explicit retain + recall retrieval;
   - cross-restart continuity (sessions survive process restarts).
   Deliberate scope wording: this is **not "unlimited context."** History is still bounded by the model window, compaction summarization is lossy, and recall only returns what was explicitly retained or survives in session files. §4.2 states exactly what compaction does *not* guarantee.
3. Keep the v1 security red lines intact (AGENTS.md "Things not to do"), re-expressed on top of pi's tool framework: `safePath` allowlist, no arbitrary shell, `schedule` writing only `schedules.json`, app/data separation.
4. Config compatibility: reuse `~/.botler-agent/.env` semantics and the two-level loading order wherever possible, so switching between v1 and v2 during migration is low-friction.
5. Port the v1 WebUI and observability surface (task-log SPA, read-only JSON API, config editing with backup/restore, derived schedule views, `/healthz` + `/metrics` + the standalone 8899 monitor) behind a `webui` feature, with v1-shaped JSONL task logs so both branches can read the same log directory during a cutover. See §13.4.

### Non-goals

- No multi-agent orchestration, subagents, or TUI features from pi — we consume `pi::sdk`, not the pi CLI product surface.
- No new channels beyond the existing three.
- No change to the "framework defines boundaries, data projects self-describe rules" philosophy.
- Do not embed or vendor the pi binary; depend on the crate as a library.
- The WebUI port is **surface-complete, not data-complete**: while S-1/S-5 are open the deterministic core runs on `StubRuntime`, so task logs have no real token/tool/conversation data, and while S-4 is open WeChat pairing cannot work. Node-only metrics (V8 heap, event-loop lag) have no Rust equivalent. These are reported as honest zeros / explicit `unavailable` payloads, never as parity.

---

## 2. Why this stack

Verified against the pi v0.8.0 source:

- **`pi::sdk` is the officially stable embedder surface** (`src/lib.rs` "Public API policy"). `SessionOptions` exposes everything botler needs: `system_prompt` / `append_system_prompt`, `no_context_files` (host owns the whole prompt), `provider` / `model` / `api_key`, `enabled_tools: Some(vec![])` (disable *all* builtins), `tool_factory: Arc<dyn ToolFactory>` (inject custom tools), `no_session` / `session_path` / `session_dir`, `compaction_settings`, `retry` / `failover`, `runtime_handle` (shared executor for all embedded sessions), `on_event` / `on_tool_start` / `on_tool_end`.
- **Persistent sessions**: `AgentSessionHandle::prompt()` continues the same conversation across calls; `sqlite-sessions` is a default feature; `rewind_to_user_message`, `switch_to_entry`, checkpoints are public methods.
- **Auto-compaction**: `src/compaction.rs` — `AutoCompactionMode::{Summary, Aggressive, ShakeFirst}` triggered by the model context window, overridable per session via `SessionOptions::compaction_settings`. Includes semantic-compaction quality auditing (loss classes), which is beyond what openclaw publishes.
- **Long-term memory**: `src/memory.rs` — `MemoryStore::open(project_root)` (per-project isolation), `retain` / `supersede` / `recall` / `mental_model` / `list`, secret screening (`screen_secrets`), plus ready-made agent tools `RetainTool` / `RecallTool` / `MemoryEditTool`.
- **Cross-session search**: `src/session_index.rs` — `SessionIndex` over the sessions root.
- **WeChat**: `weixin-agent` covers the full protocol layer botler hand-rolled in TS — getUpdates long-poll loop with backoff and token-stale callbacks, `context_token` echo (reply requires it), QR login, CDN upload/download (AES-128-ECB), inbound media parsing (image > video > file > voice), outbound markdown filtering (`StreamingMarkdownFilter`), and a context-token store. It is pure protocol: allowlist, batching, contacts, and reminders stay ours (as in v1).

### Accepted costs (decided trade-offs, not open issues)

1. **Nightly toolchain**: pi pins `nightly-2026-08-31` (Rust ≥ 1.95) and uses nightly features; the whole workspace inherits this pin. `rust-toolchain.toml` in v2 must match, and embedders need `#![recursion_limit = "256"]` (pi's own SDK example says so).
2. **Heavy dependency graph**: asupersync, QuickJS, SQLite, etc. Mitigate by disabling pi's default features we do not use (`tui`, `ftui`, `clipboard`…) and keeping `sqlite-sessions` (required for persistence) + `bpe-tokens` (evaluate whether needed).
3. **`Tool` trait lives in a `#[doc(hidden)]` module** (`pi::tools`, trait at `src/tools.rs:337`). Strictly outside the declared-stable set. Mitigation: pin the exact version, and wrap all pi imports in one internal module (`crates/botler/src/pi_bridge.rs`) so an API break is contained to one file plus tests.

---

## 3. Architecture: v1 → v2 mapping

```
v1 (TypeScript)                                v2 (Rust)
---------------------------------------------  ----------------------------------------------
src/index.ts                entry              crates/botler/src/main.rs
src/config.ts               .env + providers   crates/botler/src/config.rs        (dotenvy + serde)
src/dispatcher.ts           dedup+queue+retry  crates/botler/src/dispatcher.rs
src/runner.ts + decisions   agent per message  crates/botler/src/runner.rs
                                              + session_registry.rs   ★ redesigned (§4)
src/conversation/store.ts   sliding window     ✂ deleted  (replaced by pi sessions)
src/greeting.ts             isGreeting         crates/botler/src/greeting.rs      (verbatim port)
src/prompts/system-prompt.ts                   crates/botler/src/prompt.rs        (verbatim port)
src/providers.ts            providers.json     crates/botler/src/providers.rs     (bridges into pi, see §7)
src/tools/paths.ts          safePath ★         crates/botler/src/paths.rs         (verbatim port + property tests)
src/tools/{read,write,edit}                    pi::sdk::create_{read,write,edit}_tool(cwd)
                                                wrapped by allowlist guard (§6)
src/tools/run.ts            fixed scripts      crates/botler/src/tools/run.rs     (std::process::Command, args array)
src/tools/schedule.ts                         crates/botler/src/tools/schedule.rs
src/tools/clear-conversation.ts               crates/botler/src/tools/clear_conv.rs ★ semantics change (§4.4)
src/scheduler/*             cron engine        crates/botler/src/scheduler/*      (tokio-cron-scheduler or port)
src/push/*                  deliver+contacts   crates/botler/src/push/*
src/safety/validate.ts                        crates/botler/src/safety/validate.rs
src/safety/git.ts                             crates/botler/src/safety/git.rs      (std::process::Command)
src/channels/telegram.ts    grammy             crates/botler/src/channels/telegram.rs (teloxide)
src/channels/feishu.ts      webhook            crates/botler/src/channels/feishu.rs   (axum + reqwest)
src/channels/wechat/*       hand-rolled iLink  ✂ protocol layer → weixin-agent crate;
                                                app layer ported: allowlist, image-batch,
                                                contacts, reminder (§5.3)
src/logging/{types,collect,store}.ts           crates/botler/src/task_log/*  (v1-shaped JSONL)
src/webui/{server,config-store}                crates/botler/src/webui/*     (axum, cfg webui)
src/webui/index.html                           crates/botler/src/webui/index.html (embedded verbatim)
src/monitor/{stats,health}.ts                  crates/botler/src/monitor/*     (cfg webui)
src/scheduler/history.ts    derived views      crates/botler/src/webui/schedule_history.rs
```

The last five rows were **added by the P6 WebUI port**: the original §1 goal list and §3
mapping omitted v1's WebUI / logging / monitor modules, which is why `AGENTS.md` and the
README still described WebUI as "not ported". See §13.4.

Single crate, module layout mirroring v1. Do not split into a cargo workspace of many crates until a real need appears (YAGNI; the only reason to split would be compile-time isolation of the `pi` dependency, which is acceptable at ~1 crate since `pi` is one dependency regardless).

### Core runtime shape

- One shared **asupersync runtime handle** created at startup and passed to every `SessionOptions.runtime_handle` (pi's documented embedder pattern; per-session fallback spawns 4 worker threads each).
- tokio powers channels/web/scheduler (teloxide/axum/reqwest are tokio-based); pi's runtime is private to pi sessions. Bridge at exactly one seam: dispatcher hands work to pi sessions on pi's runtime, receives results via channels. Keep this bridge in `pi_bridge.rs`.

---

## 4. The context model (the v2 headline feature)

Persistent sessions change v1's per-message semantics ("every message = a brand-new agent: re-route, re-load the prompt, global serial write"). §4.0 is the normative contract defining which of those semantics v2 **preserves** and which it deliberately changes; §4.1–§4.7 are the mechanisms realizing it. Implementation must satisfy every invariant below.

### 4.0 Session & concurrency contract (normative)

- **C1 — The session key is (source, conversation_id, project).** Channel ID namespaces are disjoint and can collide (a Telegram numeric chat id, a Feishu `oc_…` id, and a WeChat `ilink_user_id` are three different identity spaces), so the key carries the channel source explicitly: `("telegram"|"feishu"|"wechat", conversation_id, project)`. `conversation_id` is the DM peer id or the group id — never a bare `chatId` without its source. Two-phase routing still runs for **every** message; an agent session is selected or created only **after** routing resolves a project. `UNKNOWN` never creates or continues a session. **Group chats share one session per (group, project)** — group members already co-view the same conversation surface and every participant passes the channel allowlist, so per-`(group, sender)` history splitting is rejected (it would fragment a project's conversation into per-member shards with no shared continuity). Switching a conversation from `notes` to `vocab` resumes `vocab`'s own history (and back); one project's history, `AGENTS.md`, tool `project_root`, or memory is **never** carried into another project's execution. Cross-project **and cross-channel** context leakage are classified as security defects, not UX defects.
- **C2 — The global task queue stays, and its boundary starts at routing.** v1's serialization boundary is the whole task; v2 defines the critical section as `route → session selection (fingerprint check + handle get/create/resume, §4.6) → agent tool calls → validateState → self-heal retry → commitIfChanged`, process-wide. Messages are enqueued **at channel receive time, in global arrival order**. Routing must not run ahead of the queue asynchronously — otherwise a later-arriving message in the same conversation can enqueue before an earlier one and invert persistent-history order (v1's sliding window tolerated reordering; a persistent session cannot). Scheduler and CLI enter through the same `dispatch()` entry as IM. The per-handle `&mut` requirement is a reentry guard on the session object, **not** the concurrency contract — replacing the global queue with per-conversation queues is rejected (it would let different chats, the scheduler, and CLI mutate the same project concurrently and break validate/commit atomicity). The only pre-approved relaxation is a per-data-project lock with routing→…→commit inside the same critical section, and only after parity testing shows the global queue is the bottleneck.
- **C3 — Freshness beats stickiness.** A hot session must never serve a stale framework prompt, project conventions, provider/model selection, or tool policy. §4.6 defines the per-message fingerprint check enforcing this.
- **C4 — Tools do not mutate session lifecycle directly.** `clear_conversation_context` only flags the active task; rotation happens in the dispatcher after the turn's critical section completes (§4.4).
- **C5 — Crash windows are specified.** Every multi-step mutation (write→validate→commit, reset rotation, session-file archive) has a defined recovery behavior for process death mid-sequence (§4.7).

### 4.1 Session registry (keyed by conversation + project)

```rust
// session_registry.rs
struct SessionKey {
    source: ChannelSource,          // telegram | feishu | wechat | scheduler | cli
    conversation_id: String,        // DM peer id or group id, namespaced by source (C1)
    project: String,                // routed subproject; never part of a chat-only key
}

struct PooledSession {
    handle: AgentSessionHandle,
    meta: SessionMeta,   // {project, prompt_fingerprint, provider, model, tool_policy_version} — §4.6
}

struct SessionRegistry {
    sessions: Mutex<HashMap<SessionKey, PooledSession>>,
    sessions_dir: PathBuf,   // ~/.botler-agent/sessions/  (BOTLER_CONFIG_DIR-scoped)
}
```

- **Create-or-resume after routing**: inside the queued task (C2), once phase-1 routing resolves project `P`, look up `(source, conversation_id, P)`. Miss → create an `AgentSessionHandle` (workspace = `P`, `session_dir` set) or resume `P`'s saved session file for this conversation (pi owns the file format; we own the directory plus a key→file index; file names opaque per §4.8). Hit → `prompt()` on the live handle; the context is that project's true history for this conversation.
- **Restart continuity**: handles are not eagerly reopened at startup. The first message for a key resumes from disk — subject to the §4.6 freshness check and the §4.7 corruption fallback.
- **Serialization (C2)**: the dispatcher's global sequential queue is the ordering contract; scheduler and CLI share it. The registry lock guards the map only; handle `&mut` guards self-reentrancy. `RpcControlHandle::abort` is reserved for a future user-facing "stop" command.
- **Idle eviction**: LRU-cap hot handles (e.g. 32). Eviction drops the handle only — session files remain on disk, so the next message resumes (with the §4.6 check re-run).

### 4.2 Compaction policy

- `compaction_settings` per session: `AutoCompactionMode::Summary` default; window from the selected model's context size (pi derives this; we only pass provider overrides via §7's `context_window` field).
- Compaction is lossy by design — fidelity comes from memory (§4.3). This is the openclaw trade-off, accepted knowingly.
- **What compaction does NOT guarantee** (must be stated in user-facing help): verbatim recall of early turns, tool-call internals/thinking history, and exact numbers or file paths in compacted-away turns. Anything the user or agent may need later must be **retained to memory (§4.3) or committed into data files before it is compacted** — the v1 "self-contained instruction" discipline extends to memory retention as an explicit system-prompt rule.
- Surface a user command later (`/compact` on demand); out of scope for the first cut.

### 4.3 Memory: three namespace tiers — per-conversation, project-shared, user-global

Sessions are isolated by `(source, conversation_id, project)`, so memory must not be coarser than the history that produced it — but it must also not be **barlocked per project**: life facts reference across projects ("bought a banana for 2 yuan this morning, recorded while operating `ledger`" should inform "ate a banana this afternoon in `cook`"). v2 therefore defines three tiers. A session's **visible set** is the union of tiers it may read; `recall` and `mental_model()` search exactly that union.

- **Tier 1 — personal conversation tier**, keyed `(source, principal_or_conversation, project)`: DMs key by sender principal, group chats key by the group conversation (its participants share one memory domain, mirroring C1's shared group session). The **default** `retain` target. This is the wall P0 scenario 8 guards.
- **Tier 2 — project-shared tier**, keyed `(project)` only: cross-conversation, same-project knowledge. Readable by every conversation routed to `P`; **writing to it requires an explicit tool parameter** (never the default), so personal data cannot become project-wide by model accident.
- **Tier 3 — user-global tier**, keyed **`PrincipalKey = (source, authenticated_principal_id)` — never a bare id string**: principal ids from different channels are disjoint namespaces, and a raw-id collision must never merge two people's life memories (the identity-space twin of the §4.8 path-injection flaw). Cross-channel sharing for one human (the same person on Telegram + WeChat) may come **only** from an explicit, user-confirmed **account-linking** mapping to an internal `GlobalUserId`; automatic string-equality merging is forbidden. Account linking itself is **post-v2.0** — until it ships, each channel's identity carries its own tier 3 namespace, and the key structure is designed so linking can be added later without re-keying existing memories. Content: **cross-project facts about the person** — purchases, expenses, plans, commitments, preferences.
- **Tier-3 write minimization (normative)**: the explicit tool parameter stays required, and the framework default prompt restricts *when* the agent may choose tier 3: **(a) the user explicitly asked to remember it or to associate across projects, or (b) it is a non-sensitive, task-necessary fact** (e.g. a ledger transaction). **Sensitive categories (health, relationships, precise location, finances beyond aggregate transaction records, credentials) are never auto-retained to tier 3 — explicit user request only.** The `/memory` cleanup below is the correction path, not a substitute for write-time minimization.
- **Tier-3 visibility safety line (normative)**: a user-global namespace is readable **only when the acting principal's `PrincipalKey` is the namespace owner, alone** (or a `GlobalUserId` the linking map verifiably resolves to — same rule applies to every constituent key) — a 1:1 DM, the local CLI, or a scheduler entry created by that owner. **Group conversations never read any member's tier 3 and never write into it**: surfacing "I bought this" to group onlookers is the same leakage class scenario 8 prohibits. A group session's visible set = tier 1(group) + tier 2 only.
- **Worked example (acceptance-grade)**: morning, ledger task retains `banana purchase, 2 yuan` → tier 3(owner). Afternoon, cook session in the same DM: `recall("banana")` searches {tier1(DM,cook), tier2(cook), tier3(owner)} → hits the purchase → the agent can infer the eaten banana is the purchased one, price known. `mental_model()` injects the digest of the same visible union, within the length budget below.
- **Association channel, not file access**: agents do **not** read other projects' data files to make such links — that would break per-project `safePath`/routing confinement (§6). Memory tiers are the sanctioned cross-project association mechanism.
- **Storage contract**: DB(s) under `BOTLER_CONFIG_DIR`, physical path independent of `DATA_ROOT` layout, namespaced by **stable opaque IDs** per §4.8 (never raw identity strings in paths). Spike S-3's acceptance now requires proving pi's `MemoryStore` can express **all three tiers plus the tier-3 visibility rule**; if it cannot, the fallback memory adapter implements the tier model directly (one DB, `tier` + `namespace_hash` columns), and pi's memory tools are re-implemented against it. `screen_secrets` still runs on retain, but it is explicitly **not** the leakage boundary — the tier separation is.
- **Register `RetainTool` + `RecallTool` in our `ToolFactory` output**: `retain` takes an optional tier target (default tier 1; tier 2/3 explicit parameters, tier 3 additionally gated by the write-minimization prompt rule); `recall` searches the session's visible union computed from `(source, conversation-type, PrincipalKey, project)` at task-context injection time. `MemoryEditTool` — evaluate, probably expose (it lets the agent fix stale memories) — edit rights scoped to the session's writable tiers.
- **Content caps and budgets** (v1's discipline extended): per-retain content cap (proposal: 2KB; same cap-class as the scheduler's 10KB message cap), a recall result token budget, and a fixed length budget for the `mental_model()` digest injected into the system prompt (truncate with an explicit marker; injection must not grow unboundedly with memory count).
- **User-visible cleanup is tier-scoped**: retained text may quote data-project content, so keeping memory opaque violates the app/data philosophy. Expose `/memory list | delete <id> | clear <tier>` — a principal sees their own tier-1 and tier-3 namespaces plus each project's tier 2; purging tier 3 is the "forget the stuff about me" path. P0 scenarios 8 + 11 (§10.1) gate the isolation and the linkage.
- Scheduler and CLI executions attach to: the routed project's tier 2 + the **entry creator/owner's** tier 3 + a synthetic tier-1 keyed to the scheduler/CLI context (the sender captured via task-context at schedule creation; CLI uses the local owner identity). Sessions stay ephemeral (`no_session: true`) for scheduler/CLI, but memory + `append_system_prompt` still apply.

### 4.4 `clear_conversation_context` — flag now, rotate in the dispatcher (C4)

The tool must **not** rename/archive the session file while its handle is live: the turn keeps writing to the session (assistant message, checkpoints) after the tool returns, so in-tool rotation races with post-tool writes, can archive an incomplete file, can leave the registry pointing at a replaced path, and hits platform file-handle behavior on the renamed file.

Rotation sequence instead:

1. The tool sets `reset_requested` in the active task context (same mechanism that injects the sender for `schedule`) and returns an acknowledgment. Nothing else happens mid-turn.
2. After the turn's critical section completes (validate → retry → commit, per C2), the dispatcher rotates atomically: remove the `PooledSession` from the registry, flush/close the handle, journal the move (§4.7), rename the session file to `sessions_dir/archived/<name>-<epoch>.jsonl`, and record the rotation.
3. The next message for that `(source, conversation_id, project)` key starts a genuinely fresh session (no resume).
- A future user `/new` command and the §4.6 incompatible-change rebuild reuse **this same single rotation path** — one implementation, one crash contract.

### 4.5 What is deleted

`src/conversation/store.ts` and its `~/.botler-agent/conversations/im.json` have no v2 counterpart — pi sessions subsume them. No data migration (v1 window content is transient by design; anything worth keeping should have been retained as memory or committed data).

### 4.6 Session freshness / config staleness (C3)

v1 re-reads the externalized system prompt and the routed project's conventions on **every** task, so edits apply to the next message without restart (`AGENTS.md`: "re-read on every call"). A hot `AgentSessionHandle` would otherwise freeze them at creation. Every pooled session therefore carries `SessionMeta { project, prompt_fingerprint, provider, model, tool_policy_version }`, checked before each message:

- **`prompt_fingerprint` = static inputs only.** Hash over: framework `system-prompt.md` **template** (pre-rendering), routed project's `AGENTS.md`/`CLAUDE.md`/`CODEBUDDY.md`, the memory digest inputs (§4.3), provider/model, `tool_policy_version`, compaction settings. **Request-level dynamic context is excluded by definition** — `__TODAY__`, `__PROJECTS__` rendering, sender binding, and any other per-execution placeholder value must *not* enter the fingerprint, or a midnight date change alone would rotate every hot session daily and defeat persistence.
- **Dynamic preamble — honest persistence stance.** The runner delivers the fresh date/sender/project rendering as a small preamble envelope on **each user turn** (v1-style), outside the persisted system prompt. But pi sessions persist user turns, so older preambles **do remain in history** — v2.0 makes no "never stored" claim. Baseline guarantees instead: (a) every new turn carries a fresh preamble; (b) the static system prompt declares **only the current turn's preamble authoritative** and explicitly forbids reciting dates/sender identity from earlier turns as current facts; (c) scenario 9 (§10.1) verifies latest-preamble-wins behavior. If spike S-7 proves pi offers a per-prompt ephemeral (non-persisted) context channel, we adopt it as an optimization; the stance above is what ships if it does not.
- **Rotation rule (conservative, first cut)**: any fingerprint drift **other than a pure memory-digest change** rotates the session via the §4.4 path — prompt/convention file content changes, provider/model switches, `tool_policy_version` bumps, compaction-settings changes, all of them. Rationale: "compatible update" needs a decider for arbitrary doc diffs and a trustworthy `<summary>` generator that do not exist yet, and a misclassification would strand stale (possibly relaxed) security constraints inside a *persistent* history — worse than v1, where every task re-read the prompt anyway. **A tool-allowlist or security-policy change must never ride into a live session.**
- **Deferred optimization (not in v2.0)**: the cheaper "compatible → append bounded system-update" branch may replace the conservative rule only after all three are proven: (a) pi supports appending a message with true **system role and priority** to a persistent session (not a user/assistant text trick that the model may re-interpret as conversation content); (b) a defined, deterministic compatibility classifier for prompt/convention diffs; (c) behavior tests showing appended updates override compacted-away older constraints. Until then, §4.6 ships rotation-only.
- **Memory-digest refresh**: recomputed at the same per-message check, not only at session creation; its inputs are part of the fingerprint. A digest-only change is the sole non-rotating case, applied as a bounded system-update within the §4.3 length budget (still subject to proof (a) above; if pi cannot append at system priority, digest changes rotate too).
- **`.env` / `providers.json` changes**: the provider/model component of the meta detects them; incompatible → rotate. A restart-resume of a cold key always runs this check first, so a restart can never resurrect a stale policy.

### 4.7 Crash-window recovery (C5)

- **Crash between data writes and commit**: inherited v1 behavior verbatim (v1 has the identical window): the data dir is left dirty and uncommitted; the next task's `validateState` and the user's own git state surface it. Documented, accepted — not made worse by v2.
- **Crash between commit and reply-send**: same as v1 (commit exists, reply lost); per-project commit identity lets a follow-up turn restate the outcome.
- **Crash mid-rotation (§4.4)**: rotation follows the §4.8 WAL in order — fsync intent → rename + parent fsync → **durable index update to `dst`** → completion audit record → delete intent + dir fsync. Startup recovery scans live intent files and reconciles against the actual `src`/`dst`/index state to decide *complete* vs *finalize-only*; because intent deletion is the commit point, a vanished intent always means the index is already durable at `dst` — no orphaned-stale-index window exists. No split-brain: the registry only ever points at `src` or `dst`, never a third state, and ambiguous states quarantine the key rather than guess.
- **Session file corrupt / format-upgrade-incompatible on resume**: attempt pi's session recovery path; on failure, rename to `<name>.corrupt`, start a fresh session for the key, and return a user-visible notice that this project's conversation context was reset. Resume failure must never crash-loop the channel.

### 4.8 Identifier and persistence-file safety

Channel-supplied identifiers are attacker-influenced strings (a WeChat/Feishu/Telegram id surface is not ours to trust) and project names are user-authored; neither may ever be interpolated into filesystem paths or record syntax:

- **Opaque session names.** The key→file index maps `SessionKey` to filenames derived from a cryptographic digest of the canonical key tuple (e.g. `s-<hex(sha256(source|conversation_id|project))>.jsonl`) — never a direct concatenation of `source`/`conversation_id`/`project`. This kills path traversal (`../`, embedded separators), separator collisions (`a:b` vs `a` + `b`), over-long names, and control/Unicode-normalization tricks in one rule.
- **Index integrity.** The index stores the full key tuple *as data* beside the opaque name (for §4.3 namespace resolution and user-facing listing), and is validated on load; an entry whose recomputed digest ≠ its filename is quarantined, not followed.
- **WAL-style rotation protocol (fsync-complete).** `src` and `dst` are required to share one filesystem (rename atomicity precondition). One rotation is a fixed sequence of durable steps — **ordered so that the intent file is the last artifact removed**, i.e. index commitment strictly precedes intent deletion:
  1. write the per-rotation intent file `rotate-<key_hash>.intent` (versioned JSON `{v, rot_id, key_hash, src, dst, ts}`; `rot_id` is fresh per rotation) via temp-file + atomic rename, then **fsync the file and its parent directory** — per-intent files sidestep the torn-append problem entirely (and never use whitespace-separated records, which channel ids containing spaces would shred);
  2. rename `src` → `dst`; **fsync both parent directories**;
  3. update the key→file index to point at `dst` and make it **durable**;
  4. append the completion record to the JSONL audit journal (fsync), carrying the same `rot_id`;
  5. delete the intent file and **fsync that directory** — the commit point of the whole rotation.
  Invariant this ordering buys: *if the intent file is gone, the index is already durable at `dst`* — there is no crash window with a vanished intent and a stale `src` index.
- **Recovery is intent-driven, not journal-guesswork.** At startup, for each surviving intent file: `src` exists & `dst` absent → step 2 never landed, redo the rename and continue from step 3; `dst` exists & `src` absent → the rename landed: re-apply step 3 if the index still says `src`, else the crash was between 3 and 5 — **look up the audit journal for a completion record with this rotation's `rot_id`: present → skip step 4 (no duplicate audit record) and go straight to step 5; absent → write step 4, then step 5**; **both** exist or **neither** exists → anomalous (a same-fs rename cannot produce this): keep whatever files exist, quarantine the key (no auto-resume), emit a user-visible notice. **No intent file → no pending rotation exists → the index is authoritative as-is** (by the invariant above); no index re-scan for orphaned `src` entries is needed. The append-only completion journal is an audit trail, never the recovery authority; torn trailing journal lines are tolerated as informational only.
- Same opaque-name discipline applies to memory DB files (§4.3) and archived session names; the human-readable mapping lives in the index, not in filenames.

---

## 5. Channels

### 5.1 Telegram — `teloxide`

Long polling (grammy equivalent), sender allowlist port, `dispatch()`-shaped entry, proxy support via `teloxide`'s `reqwest` hookup (replaces `https-proxy-agent`). Images: v1 sends text only on Telegram — keep.

### 5.2 Feishu — webhook

`axum` router + `reqwest` client: URL-verification challenge, event decryption (AES + signing as in v1), sender allowlist. Mechanical port.

### 5.3 WeChat — `weixin-agent` + ported app layer

```rust
WeixinClient::builder(config)
    .on_message(BotlerHandler)   // our MessageHandler impl
    .build()?.start(None).await  // owns the long-poll loop
```

The SDK replaces v1's `api.ts`/`login.ts`/`monitor.ts` loop/`download.ts` transport/`send` upload. We keep and port:

| v1 piece | v2 placement |
|---|---|
| owner + `WECHAT_ALLOW_FROM` gate, dedup ids | inside `BotlerHandler::on_message` before dispatch |
| `ImageBatchCoordinator` (选图即发，文字后到) | verbatim port (pure logic, already unit-tested in spirit) |
| inbound image → `persistInboundImage` via `safePath` | SDK decrypts CDN bytes → our handler writes under `DATA_ROOT/<project>/photos/…` through the v2 path guard |
| `contacts.json` + `context_token` persistence | app-level store, seeded from `MessageContext.from` + `.context_token`; SDK's token store may back this — evaluate |
| owner renewal reminder loop | port (`deliver()` → SDK `reply_text`/send-text) |
| markdown strip on replies | use SDK `StreamingMarkdownFilter` (default on) — delete our bespoke stripper after parity check |

QR login flow: SDK `QrLoginApi` / `StandaloneQrLogin` replaces v1's `login.ts`; token-stale callback triggers the renewal reminder path.

Open check (spike S-4): SDK targets openclaw-weixin **2.4.6**; diff its behavior against whatever protocol revision v1's TS channel relies on (24h context_token window, media sizes, typing tickets). Protocol drift is the main functional risk of this choice.

---

## 6. Security boundary (red lines, re-implemented)

`SessionOptions` shape for every botler session:

```rust
SessionOptions {
    system_prompt: Some(botler_prompt),      // framework prompt + routed project AGENTS.md
    no_context_files: true,                  // host owns the prompt; no pi AGENTS.md discovery
    workspace_trusted: false,
    enabled_tools: Some(vec![]),             // ALL pi builtins off — including bash-class
    tool_factory: Some(Arc::new(BotlerToolFactory { project_root, data_root })),
    no_session: false, /* + session_dir */ …
    ..Default::default()
}
```

`BotlerToolFactory` registers exactly the v1 set:

1. **read/write/edit**: pi's cwd-scoped constructors wrapped so every path argument passes the v2 `paths.rs` port of `safePath` (first-level `DATA_ROOT` subdirs, `root+sep` prefix rule, deepest-existing-ancestor realpath for symlink-escape prevention). The wrapper, not pi's workspace confinement, remains **the** allowlist — same as v1, so the audited rules keep their meaning. Alternatively: hand-rolled thin tools over `std::fs` with the guard inline; decide in spike S-2 by diffing pi tool IO behavior against v1's tool outputs (the system prompt and model expectations are tuned to v1 output shapes).
2. **run**: own implementation — `Command::new(interpreter).args(user_args).cwd(project_root)`, extension→interpreter fixed (`.py`→python3, `.js`/`.mjs`→node), existing-file check, 60s timeout, output caps, stdout/stderr returned as text without failing the turn. No shell.
3. **schedule**: own implementation — writes only `schedules.json` in the config dir, via the scheduler store's normalize+atomic-write+backup path. No path parameter.
4. **retain / recall** (memory): scoped to the session's tier visibility union (§4.3); tier 2/3 writes require the explicit parameter; group sessions never see or touch tier 3; storage location per spike S-3.
5. **clear_conversation_context**: own implementation (§4.4); IM-registered only.

`validateState` (all data JSON valid) and `commitIfChanged` (per-subproject git add+commit, optional push) port verbatim; dispatcher retry-once-with-self-contained-fix instruction carries over unchanged, including the "fix must be self-contained" rule (still relevant: compaction summaries are not a memory of tool-call internals).

Greeting short-circuit and two-phase routing (route LLM → project) port verbatim — per C1 routing runs for **every** message regardless of session stickiness, and per C2 it runs **inside the queued task** (enqueued at channel receive, arrival order) so persistent history can never reorder same-conversation messages; the session key is chosen only after routing resolves a project. They are provider-level calls, expressible either as a tiny direct `reqwest` call (v1 style, no session) or as a `pi` session with `enabled_tools: []` and `no_session: true`; prefer the direct call to keep routing cheap.

---

## 7. Providers and config

- Keep `~/.botler-agent/.env` as-is (name-compatible keys where possible, e.g. `TELEGRAM_BOT_TOKEN`…), plus `BOTLER_CONFIG_DIR`. Loading: `dotenvy`-style two-level (user > source), process-env-wins, matching v1's precedence rules.
- Keep `providers.json` format (v1: per-provider `api` / `baseUrl` / `apiKey` / `models`, api ∈ openai-completions | anthropic-messages). **Bridge**: at startup translate each provider into pi's model registry. pi supports custom providers with `base_url`/`api_key` and a custom `api` string, but the *programmatic registration path from an embedder* is unverified → **spike S-1 (blocking)**. Fallback if registration is config-file-only: write pi's own config format under `BOTLER_CONFIG_DIR` at init time (one-time generation from providers.json) and map `PI_PROVIDER`/`PI_MODEL` into `SessionOptions.provider/model`.
- `CUSTOM_BASE_URL`/`CUSTOM_API_KEY` legacy fallback env vars: keep in the bridge for one release, then drop.

---

## 8. Scheduler

- Storage: same `schedules.json` schema (10KB message cap, atomic write + backup, validation) — port `scheduler/{types,store,cron}.ts` to Rust; `cron.ts` semantics (`once` watermark → `Infinity`, `holidayMode: "workday"` with China statutory-holiday calendar from `BOTLER_HOLIDAY_API_URL`, silent hours, saved-listener wake) must be ported **with the existing node:test cases as spec** (`cargo test` twins).
- Firing: engine ticks, dispatches due entries with id `schedule:<id>:<epoch>`, source `scheduler`; recipient delivery via `push::deliver` (primary channel → telegram → feishu → wechat fallback, configured + recorded contacts only). Non-blocking retry semantics of v1 (commit 1745c42) must be preserved.

---

## 9. Testing and acceptance

1. **`paths.rs`**: port every historical allowlist bug as a `proptest`/`quickcheck` case (trailing-sep prefix trick, symlink escape via deepest-existing-ancestor, relative resolution, hidden dirs). This file is the security boundary; it gets the most tests.
2. **Unit twins**: v1 `node:test` suites (scheduler cron, store, image-batch, decisions) get direct `#[cfg(test)]` ports; keep table-driven parity fixtures (same inputs, compare expected outputs as JSON files checked into `tests/fixtures/`).
3. **Behavior parity checklist** (manual, per channel): greeting reply; routing UNKNOWN clarification reply; schedule create/list/fire/push; image inbound → persisted path → cited in reply; validation-failure retry text; commit message format `agent: <60 chars>`.
4. **P0 gate**: all six spikes S-1…S-6 (§10) are **hard P0 gates** — S-5 (multi-handle embedding) cannot be deferred into P1 because the P1 session registry is built on its answer, and S-6 (license) gates shippability. S-7 is a documented non-blocking evaluation. The §10.1 behavior scenarios must also pass on the P0 skeleton. Exit evidence: written go/no-go memo with per-spike findings and per-scenario transcripts.
5. CI: `cargo +nightly-2026-08-31 clippy --locked -D warnings`, `cargo test`, `cargo fmt --check`.

---

## 10. Open spikes (must resolve before/during P0)

| ID | Question | Blocking for |
|---|---|---|
| S-1 | Programmatic custom-provider registration into pi from an embedder (env/config/API). If none: adopt the generated-config fallback in §7. | P0 hard gate (providers) |
| S-2 | Wrap-pi-tools vs hand-rolled read/write/edit: output-shape diff vs v1 tools; confirm `enabled_tools: []` + `ToolFactory` injection round-trip on the pinned version. | P0 hard gate (tools) |
| S-3 | Prove pi's `MemoryStore` can express the **three-tier namespace model plus the tier-3 visibility rule** (§4.3): per-conversation `(source, principal_or_conversation, project)`, per-project shared `(project)`, user-global **`PrincipalKey = (source, authenticated_principal_id)`** (cross-channel merge only via explicit verified account-linking → `GlobalUserId`) — each separated from **physical DB location** (config-dir fixed, opaque names per §4.8, independent of `DATA_ROOT` layout). If either fails: build our own memory adapter implementing the tier model, and re-implement the memory tools on it. Decide retain/recall content caps, cleanup command surface, and `mental_model()` injection budget. | P0 hard gate (memory) |
| S-4 | weixin-agent 2.4.6 parity vs v1's protocol behavior (context_token window, media, typing, inbound image byte access). | P0 hard gate (WeChat) |
| S-5 | Multi-session in-process embedding: N hot handles sharing one `runtime_handle`; memory footprint per handle; resume-from-file latency; eviction/reopen behavior. **P0 hard gate** — the P1 session registry is built directly on this answer. | P0 hard gate (registry) |
| S-6 | License review of pi's "MIT + Rider" terms for this use and redistribution posture. **P0 hard gate** — a no-go here kills Route A before any P1 investment. | P0 hard gate (shippability) |
| S-7 | Does pi expose a per-prompt **ephemeral (not session-persisted) context channel** for the dynamic preamble (§4.6)? "No" ships the honest persistence baseline; "yes" is an optimization, not a blocker. | P0 evaluation (non-blocking) |

The WebUI / observability port (§13.4) is **not** a P0 gate and needs no new spike: it is
a deterministic read-only surface over the dispatcher's task-log records. It is, however,
degraded by the spikes above — S-1/S-5 (no real LLM ⇒ sparse logs), S-4 (no WeChat pairing).
Its acceptance is added as a P6 milestone, not folded into the §10.1 behavioural gate.

### 10.1 P0 acceptance scenarios (behavioral gate, beyond the spikes)

These probe the risks the session-model change introduces — they matter more than routine channel parity. All run on the P0 skeleton via a fake channel adapter driving the real dispatcher + real pi sessions (no live WeChat required):

1. **Cross-project alternation in one conversation** (C1): `notes` → `vocab` → `notes`; assert histories, `AGENTS.md` context, tool `project_root`, and memory domains never interleave. Include an **id-collision case**: a Telegram conversation id equal to a Feishu/WeChat id from another channel must resolve to a different `SessionKey`, never a shared session.
2. **Two conversations writing the same project concurrently** (C2): interleaving cannot expose a half-written state to `validateState` or split a commit pair. Companion check: two rapid-fire messages in one conversation, enqueued in arrival order with routing inside the queue, must append to persistent history in that order — no reordering.
3. **Scheduler firing while an IM task runs** (C2): same queue, no bypass path.
4. **`clear_conversation_context` inside a multi-tool turn** (C4): mid-turn ack only; rotation happens after commit; post-tool session writes all land in the pre-reset session file, archived whole.
5. **Process killed after every individual WAL step** (§4.7/§4.8): run one rotation, SIGKILL the process after each of steps 1–5 in turn (five kill points), restart, and assert recovery: after step 1 → rotation redone from rename; after step 2 → index re-applied to `dst`, rotation completed; **after step 3 (index durable, intent still present) → completion proceeds without touching the index again**; after step 4 → `rot_id` completion found, only intent deletion redone, **no duplicate audit record**; after step 5 → clean state, no pending rotation. Add step 0 (kill before the intent write) as a trivially-clean control. Companion check on the plain data path: kill between write / validate / commit stages behaves per §4.7 (dirty data surfaced, no mismatched dirty-commit pair).
6. **Hot session with `AGENTS.md` or `system-prompt.md` edited mid-conversation** (§4.6): first-cut rule — every content change rotates (archive + fresh session); pure memory-digest updates are the only non-rotating case, and even those require the system-priority append proof. Never stale-policy execution.
7. **Corrupt or format-upgrade-incompatible session file on resume** (§4.7): degrade to `.corrupt` archive + fresh session + user-visible notice; no crash loop.
8. **Memory non-leakage across conversations and groups** (§4.3): retain a personal preference in DM A for project `P`; a second conversation B on `P`, another sender in group G on `P`, and the group members' `mental_model()` digests must not surface it via `recall` or injection; only an explicitly shared-namespace retain becomes visible across conversations. Negative control for tier 3: after retaining an owner fact to tier 3, a **group** session on any project must not see it via `recall` or `mental_model()`.
9. **Date-rollover continuity** (§4.6): hold one hot session across a local-date boundary (and, in a group, across senders); assert **no rotation** occurs; assert the model answers "what's today / who is speaking" from the **current turn's preamble** after the boundary — earlier turns still contain the old preamble on disk (accepted, per the honest persistence stance), but no reply may present a stale date or sender as current.
10. **Hostile identifier fuzzing** (§4.8): conversation ids containing `../`, separators, spaces, control chars, and over-long UTF-8 must produce valid opaque filenames, no traversal, and parse-rotate-journal round-trips.
11. **Cross-project association via tier 3** (§4.3 worked example): "买香蕉 2 元" retained during a `ledger` task (DM), then a `cook` task in the same DM saying "吃了一根香蕉" — `recall` must surface the purchase, the agent may state the link and the cost; companion negative check: the same cook task issued from a **group** chat must not. Include the **identity-collision negative**: a Feishu principal whose raw id string equals the Telegram owner's id must NOT read that owner's tier 3 (collision ≠ identity, per the `PrincipalKey` rule). If account linking is ever implemented, add its positive test: only a user-confirmed linked pair shares tier 3. Companion write-minimization check: a health-related mention in DM must NOT auto-appear in tier 3 recall from another project without an explicit "记住" request.

P0 exit requires: all spikes written-up **and** all eleven scenarios green.

## 11. Milestones

- **P0 — Contract, spikes & skeleton**: toolchain pin, feature pruning, `pi_bridge.rs`, hello-session example against a real provider; **all of S-1…S-6 (hard gates) and the §10.1 scenarios** resolved on the skeleton. Exit: go/no-go memo + per-scenario evidence appended here. **P1 does not start until this gate is green.**
- **P1 — Core**: config, `paths.rs` + property tests, tools (file/run/schedule/clear_conv), prompt + routing + greeting, session registry implementing C1/C2/C3 (§4.1, §4.6) + compaction + memory wiring, dispatcher (global queue, dedup, validate, retry, commit, rotation per §4.4/§4.7), CLI mode (`cargo run -- "message"`).
- **P2 — Telegram + Feishu channels**; end-to-end chat with persistent context working.
- **P3 — Scheduler & push**: engine, holiday calendar, contacts, deliver; parity tests.
- **P4 — WeChat**: weixin-agent integration, allowlist/batching/contacts/reminder ports, inbound images.
- **P5 — Hardening**: parity checklist, load check (idle RAM vs v1), restart-resume drill, **session-data retention policy** (persistent sessions store raw conversation text and project excerpts under `BOTLER_CONFIG_DIR` — a new long-lived local copy that v1 never had: configurable TTL + size cap covering active, `archived/`, and `.corrupt` files; user-visible list/inspect/purge on the same command surface as §4.3's `/memory`; rotation/archival events observable to the user), cut-over of `npm run restart` tooling to a `botlerctl` equivalent (`scripts/botlerctl.sh` v2: cargo build binary, same pidfile/log conventions), README/AGENTS.md rewrite.
- **P6 — WebUI & observability port**: v1-shaped per-day JSONL task logs appended inside the single dispatcher worker; `webui/` axum SPA + read-only API + config backup/restore + derived schedule views; `monitor/` health/metrics + standalone 8899; `task_log/`, `monitor/`, `webui/` modules; HTTP and parity tests; AGENTS.md/README/design-doc updates. Exit: the full `--features webui,channels` build/test/clippy matrix is green and the SPA renders with no JS errors. Data behind the UI stays degraded while S-1/S-4/S-5 are open (§13.4).
- v1 continues on `main` during P0–P4; v2 lands on this `v2` branch; cut over when parity checklist is green.

## 12. Failure posture

If S-1 fails and the config-generation fallback is too brittle, or S-6 reveals a licensing problem, or S-5 shows unacceptable per-handle cost: fall back to **Route B** from the earlier evaluation (hand-rolled agent loop: tokio + reqwest implementing openai-completions/anthropic-messages tool-calling, own compaction, own memory store) and keep `weixin-agent` regardless — that decision is reversible at P0 end and touches nothing outside §3's mapping.

## 13. Implementation findings (v2 branch, P1–P5 core)

Status: the deterministic core (config, paths, tools, routing/prompt, sessions, rotation, memory, dispatcher, scheduler/push, Telegram/Feishu adapters, WeChat app layer, retention, admin CLI) is implemented and green. The pi embedding and the WeChat transport are **not** linked; the binary runs against the deterministic `StubRuntime`.

### 13.1 Build and toolchain

- The core builds, tests, fmt-checks, and clippy-checks on **stable** (`cargo +stable ...`). `rust-toolchain.toml` still pins `nightly-2026-08-31` for the future pi embedding; use `+stable` explicitly, or remove the pin once S-1/S-5 resolve.
- The `channels` feature adds `axum` (Feishu webhook); Telegram uses `reqwest`. `pi-sdk` is an empty feature reserved for the embedding.
- Feishu decryption is implemented as **AES-128-CBC, key = md5(encrypt_key), IV = first 16 ciphertext bytes, PKCS#7** per the Feishu spec. v1's commented AES-256 path was not carried over; the vector test is the contract.

### 13.2 Spike status

| ID | Status | Finding |
|---|---|---|
| S-1 | **Open** | The core does not call pi; provider metadata (`providers.json`, `ProviderApi`) is parsed and modeled, but programmatic registration into pi is unverified. Embedding stays behind `pi-sdk`. |
| S-2 | **Open** | Tools are hand-rolled behind the small `Tool` trait (`name/label/description/parameters/execute`, `ToolOutput{text, mutated}`). This is the injectable shape the bridge will adapt; round-trip parity with pi's tool IO is unverified. |
| S-3 | **Resolved by building our own adapter** | pi's `MemoryStore` was not evaluated; a native store implements the three tiers, `PrincipalKey` visibility, group tier-3 exclusion, digest/recall budgets, and opaque filenames. If S-3 later proves pi's store fits, migrate behind the same API. |
| S-4 | **Open** | WeChat app layer (gate, context tokens, batching, reminder, image download/persist) is complete and tested; the `weixin-agent` transport is not linked. Server mode logs that WeChat cannot receive messages in this build. |
| S-5 | **Open** | The registry is built on the `AgentRuntime`/`AgentSession` seam with a hot pool, fingerprint freshness, eviction, and resume; per-handle cost and multi-handle behavior against real pi sessions are unmeasured. |
| S-6 | **Open** | pi licensing was not reviewed; no pi code is linked or redistributed in this build. |
| S-7 | **Not needed yet** | Dynamic preamble is rebuilt every turn and persists in history (the honest baseline). The fingerprint excludes dynamic placeholders, so date/sender changes do not rotate. |

**Go/no-go:** go for the deterministic core on `v2`; **no-go for claiming a pi-backed release, a working WeChat channel, or shippability** until S-1/S-5 (embedding), S-4 (transport), and S-6 (license) close.

### 13.3 §10.1 scenario evidence

| # | Scenario | Evidence |
|---|---|---|
| 1 | Cross-project alternation + id collision | `tests/acceptance.rs::cross_project_sessions_never_interleave_and_channels_do_not_collide`; `identity::tests::session_keys_are_channel_namespaced` |
| 2 | Arrival order, routing in queue | `tests/acceptance.rs::messages_are_enqueued_in_arrival_order` |
| 3 | Scheduler through the queue | `tests/acceptance.rs::scheduler_fires_through_the_global_queue` |
| 4 | `clear_conversation_context` mid-turn | `tests/acceptance.rs::clear_flag_rotates_the_session_after_the_turn` |
| 5 | Kill after each WAL step (0–5) | `rotation.rs` tests: `kill_before_intent_is_a_clean_control`, `recovery_redo_when_rename_never_landed`, `recovery_finalizes_after_rename_landed`, `kill_after_index_durable_does_not_rewrite_the_index`, `recovery_does_not_duplicate_audit_record`, `rotate_moves_file_and_archives_index`, `anomalous_state_quarantines_the_key` |
| 6 | Docs edited mid-conversation | `tests/acceptance.rs::editing_project_docs_rotates_the_session` |
| 7 | Corrupt resume | `tests/acceptance.rs::corrupt_resume_degrades_without_a_crash_loop`; `session_registry::tests::corrupt_resume_archives_and_starts_fresh` |
| 8 | Memory non-leakage | `memory::tests::{personal_memory_does_not_leak_across_conversations, group_cannot_read_owner_global_memory, group_cannot_write_global, project_tier_is_shared_across_conversations}`; tool-level tests for default tier and group refusal |
| 9 | Date-rollover continuity | `tests/acceptance.rs::hot_session_is_reused_across_turns_with_a_fresh_preamble` holds one hot session across turns, asserts the preamble is rebuilt each turn, and asserts no rotation; `prompt::tests::preamble_carries_only_the_current_turns_values` simulates a date/sender rollover at the preamble layer. The fingerprint contains only static inputs (template, provider, model, tool policy, memory digest, project docs), so a date or sender change cannot rotate. A true in-process midnight boundary still needs an injected clock. |
| 10 | Hostile identifiers | `paths.rs` and `identity.rs` `proptest` blocks (traversal, sibling prefix, symlink escape, control chars, over-long UTF-8, distinct keys never share a name) |
| 11 | Tier-3 cross-project + identity collision | `memory::tests::{tier3_links_projects_in_the_same_dm, identity_collision_does_not_share_tier3, group_cannot_read_owner_global_memory}`; `retain_tool_defaults_to_personal_and_writes_only_tier_one` covers write minimization |

Post-review hardening: the session index now persists `SessionMeta`, so a **cold** resume (restart or LRU eviction) is freshness-checked exactly like a hot one; a missing or changed fingerprint archives the file and starts fresh (`cold_resume_with_a_{matching,changed}_fingerprint_*`). Scheduler tasks derive their memory identity from the entry recipient (`recipient.source` + `recipient.user_id`), so tier-3 memory written by a scheduled job lands in the same namespace as that owner's IM conversation (`runner::tests::scheduler_memory_uses_the_recipients_identity`).

### 13.4 WebUI & observability port (P6)

**Why:** the v1→v2 mapping in §3 and the §1 goal list omitted `src/webui/`, `src/logging/`
and `src/monitor/`, so `AGENTS.md` and the README still listed WebUI as "not ported". A
migration to the Rust branch would otherwise silently drop the task-log UI, config editor and
health endpoints. P6 ports the surface; §1/§3 above were corrected to include it.

**Layout:** `task_log/` (always compiled; v1 `TaskLog` schema with `#[serde(rename_all =
"camelCase")]`, per-day `YYYY-MM-DD.jsonl` via `fsutil::append_jsonl_fsync`), `monitor/` and
`webui/` (both `#[cfg(feature = "webui")]`). `webui = ["dep:axum"]` is independent of
`channels`, so `default=[]` and `--features channels` compile no HTTP code. The v1 SPA is
embedded verbatim with `include_str!`; no new crate or runtime file dependency.

**Ordering:** all task-log writes happen inside the single dispatcher worker
(`process_message` / `process_script`, plus a `WorkerRequest::Duplicate` funnel for dedup
hits, which are detected in the concurrent `enqueue` path). WebUI handlers hold only
`Arc<DispatchStats>` and a cloned `TaskLogStore` path — never `Arc<Dispatcher>` or
`PathGuard` — so no handler can enqueue work or reach `DATA_ROOT`. Self-heal produces a
second `phase:"self-heal"` record, matching v1. Script tasks keep their
`schedule:<id>:<epoch>` id so the derived schedule history still works.

**Endpoint triage:** all read endpoints (`/api/logs`, `/api/summary`, `/api/usage`,
`/api/token-stats`, `/api/schedules/*`, `/api/config`, backups) and all write endpoints
(`/api/cleanup`, system-prompt and schedules PUT, backup restore) are implemented, with
`X-Botler-UI: 1` on writes, a 1 MB body cap, strict day-file cleanup, and canonical-parent +
symlink validation on restore. WeChat pairing endpoints are **blocked by S-4** and return an
explicit `unavailable` / `ok:false` payload (200, not 501), so the unmodified SPA degrades
gracefully instead of throwing. `/healthz`, `/healthz/history`, `/metrics` are served both on
the WebUI port and on the standalone loopback 8899 port, sharing one 5-second sampler.

**Sparse-record honesty:** under `StubRuntime` there are no LLM calls, so `usage`, `calls`,
`tools`, `conversation`, `modelCache`, `systemPrompt` and `routing` are zero/empty and each
record carries `degraded: true` (an extra field v1 readers ignore). `provider`/`model`
record the configured selection, not a completed call. V8 heap and event-loop-lag metrics
are zero; RSS is real only on Linux; `activeAgents` is documented as an `activeTasks` proxy.

**Build matrix:** `cargo +stable build` (default), `--features webui`, `--features channels`,
`--features webui,channels`; the same matrix for `test -p botler` and
`clippy --all-targets -- -D warnings`. `scripts/botlerctl.sh` builds with
`BOTLER_FEATURES=webui,channels` by default so a local deployment has the HTTP surface;
`WEBUI_ENABLED=1` turns the SPA on at runtime. `MONITOR_ENABLED` defaults on in `load()`.

**Correct wording:** "WebUI ported; data views degraded; WeChat pairing blocked by S-4".
Never "full parity with main". `AGENTS.md`'s red line was reworded accordingly: the pi
runtime, WeChat transport and Docker image are still forbidden claims while S-1/S-4/S-5
are open, but the WebUI itself is shipped — only its token/tool/conversation views, pairing
and Node-only metrics must not be claimed as real.
