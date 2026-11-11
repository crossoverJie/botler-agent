# Go rewrite — status and decisions

Companion to [`go-rewrite-adk-go.md`](./go-rewrite-adk-go.md) (the evaluation) and to the `v2-golang`
branch itself. It records the decisions that document left open (§14), every deliberate deviation
from v1, how far verification has actually gone, and what is still missing.

Branch `v2-golang` · module `github.com/crossoverJie/botler-agent` · Go 1.27 · 2026-11

## 1. Decisions taken (design §14)

| ID | Decision | Consequence |
|---|---|---|
| Q-1 engine | **adk-go model layer + self-implemented loop.** `google.golang.org/adk/v2` v2.7.1 provides the wire protocols (`openaimodel` with `APIChatCompletions`, `anthropicmodel`); the loop lives in `internal/agentrt/loop.go` | tool results reach the model as adk-go's JSON envelope (`{"result": "..."}`), not v1's plain text; see §2. The seam is one package, so switching to `llmagent`+`runner` later does not touch routing/logs/scheduler |
| Q-2 `run` `.js/.mjs` | kept; image ships `nodejs` | interpreter table unchanged (`python3` / `node`) |
| Q-3 monitoring | phase one only | `/healthz` keys and `/metrics` names match v1 exactly; event-loop lag is reported as `0` until phase two |
| Q-4 memory | not implemented | `agentrt` has no memory hooks yet; nothing else assumes memory |
| Q-5 branch | `v2-golang`, v1 stays on `main` | both source trees coexist on this branch for the parity harness; the Go pipeline is `ci-go.yml` |
| Q-6 WebUI | v1 `index.html` reused byte-for-byte via `go:embed` | only the JSON API was reimplemented |
| Q-7 Feishu | not implemented | `FEISHU_*` stays parseable; startup logs `[feishu] not implemented in this build; skipped`; the push fallback chain is `telegram → wechat` |
| Q-8 pins | adk-go **v2.7.1** (a release, not the commit quoted in the design), `genai` v1.76.0, `openilink-sdk-go` v0.6.0 | `go.sum` is the source of truth; `go mod tidy` also pulls the adk-go indirect tree |
| Q-9 v1 quirks | copied, except where noted below | dedup table never pruned, English error strings, `preview` without the UI header, commit/push narrowed to the selected projects |
| Q-10 source-dir `.env` | dropped | only `$BOTLER_CONFIG_DIR/.env` (default `~/.botler-agent/.env`) is read; `botler init` embeds the template |

## 2. Deliberate deviations from v1

| # | Deviation | Why | Visible to |
|---|---|---|---|
| D-1 | Tool results are sent to the model as JSON (`{"result": "..."}`) instead of plain text (design §3.3) | adk-go's model layer marshals `FunctionResponse.Response` | model behaviour only; the task log still records the plain text |
| D-2 | JSON written by the `write` tool has **sorted** keys, because adk-go hands tool arguments to the loop as `map[string]any` | same layer as D-1 | data-file diffs; the parity harness reports it as an explained difference |
| D-3 | `DATA_ROOT` is canonicalized (`EvalSymlinks`) before the allowlist is computed | v1 rejects any `DATA_ROOT` under a symlinked path (e.g. macOS `/var → /private/var`) with `Path out of bounds (symlink escape)`; the parity harness reproduced this | nothing, except that such a DATA_ROOT now works |
| D-4 | Conversation JSON is fsynced and its directory fsynced before/after rename | design D-8; strictly safer, format unchanged | nothing |
| D-5 | An out-of-range `FINISH_REASON` (safety, malformed function call, …) is logged as `length` | adk-go has no pi-ai equivalent; `error` would hide the usage | task-log `stopReason` |
| D-6 | `botler --version` exists | new entry point used by the container smoke test (design §9.2) | new flag only |
| D-7 | Length limits (`TEXT_MAX`, `CONVERSATION_TURN_MAX_CHARS`, summary truncation) count **runes**, v1 counts UTF-16 code units | Go has no UTF-16 strings; identical for BMP text (incl. Chinese), differs only for astral characters (emoji) | truncation points in task logs / prompts when emoji are present |

## 3. Verification status

Green: `go build ./...`, `go vet ./...`, `gofmt -l .` (empty), `go test ./...` (all packages).

| Area | Evidence | Coverage |
|---|---|---|
| security boundary (`paths`) | unit tests for prefix tricks, hidden dirs, symlink escape, task subset, new files | 82.5% |
| cron / scheduler | unit tests for cron fields, interval/at, `once` watermark, silent hours, workday gating, store validation, history | 83.4% |
| task logs | query/filter/paging, summary, hour/day/week buckets, cleanup name validation, collect fidelity | 85.9% |
| conversation window | sliding window, per-turn truncation, whole-window cap incl. oversized newest turn | 80.6% |
| dispatch pipeline | end-to-end with a fake runtime: write task, dedup, empty reply, greeting, tool-turn hard stop, provider error, scripted task | 65.6% |
| telegram | allowlist, proxy precedence, update decoding, token redaction | 79.1% |
| webui | endpoints, header rule, backup naming/pruning, traversal rejection | 63.2% |
| wechat | batching coordinator, markdown stripping, AES, account/context files | 36.8% |
| **v1 parity** | `go run ./scripts/parity` — scripted stub drives v1 and v3; task log, cost fields, conversation history and data tree match (one explained difference: D-2) | 1 scenario |
| **live model, openai-completions** | `custom` gateway + `deepseek-v4-pro`: routing-only call, then a read-only task with 2 tool calls, 3 LLM calls, cache-read tokens reported | manual |
| **live model, anthropic-messages** | `ark` gateway (`…/api/plan`) + `ark-code-latest`: read a data file and answer from it | manual |
| **live Telegram** | real token: `@… online`, `/healthz` shows `channels.telegram.up=true` | manual |

## 4. Still missing

1. **WeChat end-to-end.** Code is complete (SDK + business layer), but no QR login / receive / send /
   reminder cycle has been run against the real iLink service. The stored session in the reference
   install currently answers `session expired (errcode -14)`, so a re-pair is needed first.
2. **A real Telegram message round trip.** Startup, channel-up and the model pipeline are verified
   separately; nobody has yet typed into the bot and watched the reply come back through
   `dispatch` → `sendMessage`.
3. **WebUI in a browser.** The JSON API is covered by `httptest`; the page has not been opened, and
   the pairing/backup/restore flows have not been clicked through.
4. **More parity scenarios.** Only `basic` (read → write → reply) exists. Greeting, routing failure,
   validation self-heal, tool-turn hard stop, `conversation/im.json` accumulation, and a scheduled
   script entry are still uncovered.
5. **Docker image.** The Dockerfile and CI job exist, but no multi-arch build has been run
   (`docker buildx` + the container smoke on both platforms).
6. **CI.** `ci-go.yml` has never run on GitHub (the branch is not pushed).
7. **Thin test coverage** in `config`, `prompt`, `monitor`, `push`, `images`, `initcmd`,
   `agentrt` (loop) and `runner` (19%): the wiring is exercised end-to-end, the units are not.
8. **Renewal/retry edges** that only appear in production: scheduler retry backoff cancellation,
   WeChat 24h REMINDER timing, `GIT_PUSH` against a real remote (`docs/docker-git-push.md` flow).
9. **`botlerctl stable`** resolves the latest GitHub release, which currently still points at a v1
   tag; the first Go release has to be tagged before that path is meaningful.
