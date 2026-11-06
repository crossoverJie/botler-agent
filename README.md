# botler-agent (v2)

A general-purpose, lightweight personal agent framework. It receives messages
from **Telegram / Feishu / WeChat**, routes each message to one data subproject
under the **data root (`DATA_ROOT`)**, runs a short agent turn with a small
allowlisted toolset, validates and commits the result, and sends the reply back.
A built-in scheduler can fire entries from `schedules.json` and push the result
to a configured recipient.

v2 is a native Rust rewrite of the v1 TypeScript implementation. The design and
its acceptance contract live in
[`docs/design/rust-rewrite-v2.md`](docs/design/rust-rewrite-v2.md); v1 remains on
`main` while v2 lands on this branch.

## v2 status

| Area | State |
|------|-------|
| Deterministic core (routing, sessions, memory, tools, validation, commit, scheduler, rotation/WAL) | Complete and covered by unit, acceptance, and parity fixtures |
| Telegram (long poll) and Feishu (webhook, encrypted events) | Complete behind the `channels` feature |
| WeChat app layer (gate, context token, image batching, renewal reminder) | Complete; the `weixin-agent` transport is spike **S-4** |
| pi agent embedding (`pi::sdk`) | Pending spikes **S-1/S-5**; the shipped binary uses a deterministic stub runtime, so replies do not call a model yet |
| WebUI / Docker image / install script | Not ported yet (v1 feature) |

The deterministic core is deliberately independent of the pi SDK through the
`AgentRuntime` / `AgentSession` seam in `crates/botler/src/runtime.rs`, so the
whole pipeline is testable without a network or an LLM.

## Build and run

Requires Rust stable (workspace `rust-version = "1.90"`). The pinned
`nightly-2026-08-31` in `rust-toolchain.toml` is only needed once the pi
embedding is enabled.

```bash
cargo +stable build --release --features channels -p botler

# CLI: run one message through the real dispatcher
./target/release/botler "had a steamed bun for breakfast"

# Persistent mode: channels + scheduler + push (config from ~/.botler-agent/.env)
./target/release/botler
```

Process control is provided by `scripts/botlerctl.sh`
(`restart|start|build|stop|status|logs`): it builds the release binary, keeps
the v1 pidfile/log conventions (`.botler.pid`, `botler.log`), and never touches
`DATA_ROOT` or `~/.botler-agent/`. `restart` defaults to the Rust branch
(`BOTLER_BRANCH`, default `v2`); `restart stable` follows the latest release
tag. Do not point it at `main`, which is the v1 TypeScript tree.

## Configuration

`DATA_ROOT` and all channel/model settings come from `.env`, with the same
priority as v1:

```
process env  >  ~/.botler-agent/.env  >  source .env (dev fallback)  >  defaults
```

`BOTLER_CONFIG_DIR` overrides `~/.botler-agent`. v2-specific settings:

| Variable | Default | Purpose |
|----------|---------|---------|
| `BOTLER_SESSIONS_DIR` | `~/.botler-agent/sessions` | Persistent session files, archives, and the rotation journal |
| `BOTLER_SESSIONS_INDEX` | `~/.botler-agent/sessions-index.json` | Opaque key → file index |
| `BOTLER_MEMORY_DIR` | `~/.botler-agent/memory` | Three-tier memory namespaces |
| `BOTLER_SESSION_HOT_LIMIT` | `32` | Max live session handles kept in memory |
| `BOTLER_SESSION_RETENTION_DAYS` | `90` | TTL for live / archived / corrupt session files (`0` disables) |
| `BOTLER_SESSION_RETENTION_MAX_BYTES` | `2147483648` | Total session-data cap (`0` disables) |

`providers.json` (custom OpenAI-completions / anthropic-messages gateways) and
`system-prompt.md` keep the v1 formats. `PI_PROVIDER` / `PI_MODEL` select the
model.

## Sessions

Every IM conversation keeps one persistent session **per `(source,
conversation_id, project)`**, so switching between projects never interleaves
histories. Files are named from a digest of the key (`s-<hash32>.jsonl`) and are
never derived from raw identifiers. A content change to `AGENTS.md`,
`system-prompt.md`, the provider/model, or the memory digest rotates the session
through a five-step fsync WAL:

1. write the rotation intent;
2. rename `src → dst` and fsync both parents;
3. persist the index;
4. append the audit record;
5. delete the intent (the commit point).

Startup recovery is intent-driven and quarantines ambiguous states instead of
guessing. `clear_conversation_context` only sets a flag mid-turn; the dispatcher
rotates after the turn's validation/commit section completes.

Admin commands (the second token must be a known verb, so normal messages still
reach the agent):

```bash
botler sessions list
botler sessions inspect <file>
botler sessions purge [--older-than-days N] [--max-bytes N] [--all]
```

Retention is applied at startup and every 24 hours. `list` exposes live,
archived, and `.corrupt` files, which is also how rotation/archival events are
observed; `inspect` prints a bounded excerpt.

## Memory

Three tiers, with an explicit write boundary:

- **Tier 1 (personal conversation)** — default `retain` target, keyed by
  `(source, principal_or_conversation, project)`.
- **Tier 2 (project-shared)** — requires `tier: "project"`.
- **Tier 3 (user-global)** — requires `tier: "global"`, keyed by
  `PrincipalKey = (source, authenticated_principal_id)`; group chats never read
  or write it.

`recall` and the system-prompt digest search only the acting context's visible
union. Credential-shaped spans are redacted before storage, and the digest is
length-bounded. CLI cleanup surface:

```bash
botler memory list [project]
botler memory delete <id> [project]
botler memory clear <personal|project|global> [project]
```

## Architecture

```
channels (Telegram / Feishu / WeChat app layer)
   │  enqueue inline at receive (global arrival order)
   ▼
Dispatcher: dedup → single process-wide serial worker
   ▼
Runner: route → session selection (fingerprint check / resume / rotate)
   → tools → validate → one self-heal retry → git commit
   ▼
AgentRuntime (pi SDK in production, stub in tests)
```

- The queue boundary starts at routing: a later message can never overtake an
  earlier one in the same conversation.
- Sessions are keyed per project, and project alternation in one chat keeps
  separate histories, prompts, and memory domains.
- `safePath` remains the security boundary: first-level, non-hidden
  subdirectories of `DATA_ROOT`, prefix matching with `root + separator`,
  deepest-existing-ancestor canonicalization to block symlink escapes, and an
  optional per-task project subset.
- Tools: `read`, `write`, `edit`, `run` (existing in-project python3/node
  scripts only, no shell), `schedule` (fixed `schedules.json`, no path
  parameter), `retain`, `recall`, and the IM-only
  `clear_conversation_context`.
- Data subprojects describe themselves in their root `AGENTS.md`; the framework
  hardcodes no business schema. Writes are validated (all data JSON must parse)
  and committed per independent git repo.

## Channels and scheduler

- **Telegram**: long polling; `TELEGRAM_BOT_TOKEN`, optional
  `TELEGRAM_ALLOW_FROM`. `TG_PROXY` is **not** wired into the Rust client yet;
  use a system-level proxy / VPN instead.
- **Feishu**: webhook on `FEISHU_PORT`; `FEISHU_VERIFICATION_TOKEN` is required
  whenever the Feishu app is enabled (startup fails otherwise) and every event
  must carry it. With `FEISHU_ENCRYPT_KEY`, only encrypted events are accepted
  and each request must pass the `X-Lark-Signature` check (AES-128-CBC event
  decryption); sender allowlisting is also implemented.
- **WeChat**: app layer complete (`WECHAT_ENABLED=1`, owner gate, context-token
  tracking, `WECHAT_IMAGE_BATCH_SECONDS` batching, `WECHAT_REMINDER_HOURS`
  renewal nudges). The transport itself is pending spike S-4.
- **Scheduler**: `SCHEDULER_ENABLED=1`; entries support `cron` / `interval` /
  `at` / `once`, plus `retry`, `silentHours`, `holidayMode: "workday"` (China
  legal workdays, including 调休 makeup days), and `recipient` push delivery
  with `telegram → feishu → wechat` fallback.

## Tests

```bash
cargo +stable test -p botler --features channels            # unit tests
cargo +stable test -p botler --test acceptance              # §10.1 scenarios
cargo +stable test -p botler --test parity                  # v1 parity fixtures
cargo +stable fmt --all -- --check
cargo +stable clippy --all-targets --features channels -- -D warnings
```

Acceptance coverage includes cross-project alternation and channel id
collisions, arrival-order serialization, scheduler-through-queue, reset-flag
rotation, config-change rotation, corrupt-resume recovery, hot-session reuse,
and WAL kill points. Parity fixtures under `crates/botler/tests/fixtures/`
pin greeting detection, image batching, and cron/once/workday firing.

## Security boundaries

- App and data are separate: this repo (and `.env`) never live inside
  `DATA_ROOT`.
- The path allowlist is enforced in code; do not relax it or add arbitrary
  shell execution.
- Secrets stay in `.env` / `providers.json`; never hardcode credentials.
- Persistent sessions and memory contain raw conversation text and must remain
  under `BOTLER_CONFIG_DIR` with opaque names; use the retention and memory
  admin commands to list and purge them.

## License

[AGPL-3.0](LICENSE). Third-party dependency licenses are reproduced in
[`THIRD-PARTY-LICENSES`](THIRD-PARTY-LICENSES).
