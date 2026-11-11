# Botler v3 评估：用 Go + ADK-Go 复刻 `main`（v1）

状态：评估稿（r7）
r7 修正：§6 数据兼容表去掉 r2 遗留的"providers.json 新增可选 cost 字段"（与"成本恒 0"冲突），改为"不新增任何字段"；§10.2 把三处指标简写展开成全名，并补"不要用前缀简写"的说明。
r6 修正：附录 A 把微信协议层/业务层拆成两行并写明逐文件行数（773 / 1240，去掉"约 1200"的模糊口径）；§2.5 错别字「胜了」→「脏了」。
r5 修正（对照四轮 review）：§8.2 拆分 `usage.*` 通配符（与 `calls[]` 逐字段写法对齐）并补上 `routing.usage.costUsd` 归属；§2.1.1 补 `wechat-logout` 归入契约 1、修正 `--version` 的措辞、把首句的"ESM 惰性"改为"启动器条件动态 import"；§3.8 #7 与 §4.3/§12/§13 统一口径。
r4 修正（对照三轮 review）：新增 §2.1.1「启动顺序契约」（Go 无 lazy import，`init`/`--version` 必须在 `paths` 校验之前返回）；修正 pi-ai 图片工具结果的真实形态（独立 `role:"user"` 消息 + 固定前缀 + 条件性假 assistant）；归一化表把 `costUsd` 移回"必须逐字相等并断言 = 0"、拆分 `images` 的 https/本地文件名；统一"照抄 SDK 轮询逻辑"的定性；补行号与错别字。
r3 修正（对照一轮 review）：成本不是自建项（v1 对 providers.json 单价硬编码 0）；耦合面更正为 1531 行/7722 行；**adk-go 的 model 层发不出 v1 的纯文本工具结果**（新增 §3.3，方案 A/B/C 重排，A 不再声称保真）；补全 v1 的英文错误文案、commit/push 按选中项目收窄、两套图片扩展名白名单、holidays 备份与 saved-listener、指标名精确写法、`preview` 不校验 UI 头等；所有外部库断言 pin 到版本/commit 并新增 §3.8 P0 核对清单。
范围：`main` @ v0.5.1（commit `c948ab7`）中 **iLink（微信）与 Telegram** 两条渠道的完整功能集，后端由 TypeScript + pi SDK 换成 Go + `adk-go`；配置目录需与现网 `/Users/chenjie/.botler-agent` 完全兼容
不在本稿范围：飞书（见 §2.6.1）、新功能（记忆仅做方案评估）、多 agent 编排、新渠道

---

## 0. 结论摘要

1. **可行，耦合面按实测：非测试 9253 行，真正 import pi 的 1531 行**（`runner.ts` 624 / `tools/schedule.ts` 334 / `logging/collect.ts` 230 / `tools/run.ts` 105 / `tools/write.ts` 60 / `providers.ts` 58 / `tools/edit.ts` 38 / `tools/clear-conversation.ts` 34 / `tools/read.ts` 28 / `tools/index.ts` 20），其余 7722 行是纯 Node 标准库逻辑。结论不变，但 **`logging/collect.ts` 属于要改写的部分**（它依赖 pi-ai 的 `Usage` / `contentText` / `AgentMessage`），不能归为纯移植。
2. **微信不需要自己写协议。** `github.com/openilink/openilink-sdk-go` v0.6.0（MIT，无第三方依赖）覆盖 v1 手写的 iLink 协议面：扫码登录、长轮询、context_token、CDN AES-128-ECB、图片/语音/文件、会话过期。`WeClawBot-API` 只是推送示例，能力不足；`openilink-hub` 是它的生产级消费方。**这些 API 名称需在 P0 逐项复核**（见 §3.8）。
3. **adk-go 有工具循环，但发不出 v1 形态的工具结果（r3 修正）。** 循环在 `llmagent` + `runner` 的 `internal/llminternal/base_flow.go`；而 `openaimodel/internal/completions/request.go` 的 `newToolMessage` 固定为 `json.Marshal(fr.Response)`，而 pi-ai 的 completions 适配器把 toolResult 的文本块拼成**纯文本** `role:"tool"` 内容。所以“自研循环就能保住 v1 工具结果文本形态”不成立：要么接受 JSON 形态（A/B 都需要重测提示词），要么自己写两种 wire protocol（方案 C）。见 §3.3。
4. **许可链条方向正确，但需要写清范围。** adk-go = Apache-2.0，openilink-sdk-go = MIT，本项目 AGPL-3.0-only，直接依赖兼容；但 adk-go 会带入 `cloud.google.com/go/*`、`genai`、两个官方 SDK 等一大串间接依赖，供应链审计范围应按 `go.sum` 全量看，不能只看三个直接依赖。
5. **永久记忆：接口是 adk-go 的，实现和策略是自己的。** 但注意：`memory.Service` 与 `preloadmemorytool` **只在 B 方案（runner + session）下能直接接**；A/C 自研循环只能复用它们的注入思路。见 §11。
6. **成本不是自建项（r3 修正）。** v1 确实调用 pi-ai 的 `calculateCost(model.cost, usage)`，但 `providers.ts:22` 给 providers.json 的每个模型硬编码 `cost:{0,0,0,0}` → 现网配置下 `costUsd` **恒为 0**。v3 对等做法就是恒 0；只有走内置 `anthropicProvider()`（没有 providers.json 的全新安装）才有真实价格表。
7. **工作量**：单人 **28–40 人日（约 5.5–8 周）** 到达 iLink + Telegram 的功能对等（含测试与部署；含 P0 的 stub harness 与工具结果实验）。真正的时间黑洞是 §13 里的三行：工具结果形态（JSON vs 纯文本）、`custom`/`ark` 的 provider 端点、微信协议版本漂移；**不是** §7 的既有缺陷（D-1/D-2 已推迟到飞书期，其余是微项）。
8. **现网配置已逐文件核对**（`/Users/chenjie/.botler-agent`，采样 2026-11，见 §6.1）。两个兼容点必须提前处理：adk-go 的 `openaimodel` **默认走 Responses API**，必须显式设 `APIChatCompletions`；模型缺 `vision` 字段时必须按“支持图片输入”处理。
9. **所有对外部库的断言都必须在 P0 重新核对并 pin。** 本仓库无 Go 依赖、无 go.mod/vendor，本稿核对的是 adk-go `e98671f`（2026-11-08）与 go mod cache 里的 `openilink-sdk-go v0.6.0`。adk-go 处于 `/v2` 快速演进期，符号与默认值都可能已变；P0 第一件事是写一份逐项核对表（§3.8）。

---

## 1. 目标与非目标

### 目标

1. **行为对等**：`main` 的每一条用户可见行为，包括容易被忽略的细节（5 分钟去重、20 轮工具上限、60 字提交信息、日志字段名、英文错误文案），全部在 Go 侧复现。
   - 固定用户文案的语言要分清：**中文**只出现在 `greeting.ts`（欢迎语 / 未知项目兜底 / 重置确认）与注入的中文 system prompt；**错误与兜底路径全是英文**（`runner.ts` 的 `⚠️ Task did not complete successfully: ...` / `⚠️ No reply generated`、`dispatcher.ts` 的 `⚠️ Could not save safely; please check the data: <fix>` 等）。§2.3/§2.2 已逐字列出。
   - 已知且已声明的例外：监控的 `event_loop_lag_*` 一期恒为 0（§10.1），属于 WebUI/Prometheus 可见的已知偏差。
2. **配置与数据对等**：磁盘格式保持字节级兼容（`.env` / `providers.json` / `system-prompt.md` / `schedules.json` / `holidays.json` / `contacts.json` / `wechat/*.json` / `conversations/im.json` / `task-logs/YYYY-MM-DD.jsonl` / `.backups/`）。**就数据而言**切换只需换二进制；但仓库层脚本（`install.sh` / `botlerctl.sh` / `Makefile`）与源码目录在 v3 分支会变，回滚到 v1 需要在 `main` 上重新 checkout（见 §9.2）。
3. **界面对等**：`src/webui/index.html` 原样复用（`go:embed`），只重写后端 JSON API。这样 UI 不需要重写，也不会漂移。
4. **部署对等**：Docker 镜像、`/config` + `/data` 挂载、`ENV` 默认值、HEALTHCHECK、`GIT_PUSH=1` 的 commit+push 语义、`botlerctl.sh` 的 pidfile/log 约定。
5. **保留安全红线**：`safePath` 白名单、无任意 shell、`schedule` 只写固定文件、app/data 分离。

### 非目标

- 不引入多 agent 编排、子 agent、workflow、A2A 等 adk-go 能力。
- 不改数据项目协议（`DATA_ROOT` 下的 `AGENTS.md` 仍由数据项目自描述）。
- **v3.0 不实现飞书**（`src/channels/feishu.ts`，见 §2.6.1）；`FEISHU_*` 配置项保留可解析但不启动，避免现网 `.env` 报错。
- 本期不实现记忆功能（仅 §11 评估）。
- v3.0 不做监控新特性，只按 §10 的分期方案落地第一期。
- 允许的少量新增（需在 README/CHANGELOG 登记，不算"新功能"）：`--version` 子命令（镜像 smoke 与运维排查用）、`BOTLER_PPROF`（三期）。
- 不做 v1/v2 并存的双实现；Go 版本独立成 `v3` 分支，`main` 继续维护。

---

## 2. 必须逐条复刻的行为契约

这一节是实现的验收口径。左列是 v1 位置，右列是不能改的数值/语义。

### 2.1 入口与配置

| 契约 | 值 / 语义 | v1 位置 |
|---|---|---|
| 无参数启动 | 按 `.env` 启动已配置的渠道；一个都没配则退出码 1 并提示 | `src/index.ts` |
| CLI 单条模式 | `botler "消息"` → 直接 dispatch，打印 `text` 与 `[images]`，退出 | 同上 |
| 子命令 | `init` / `wechat-login` / `wechat-logout` | `bin/botler.mjs`, `index.ts` |
| 配置优先级 | 进程环境 > `~/.botler-agent/.env` > 源目录 `.env` > 内置默认 | `src/config.ts` |
| `BOTLER_CONFIG_DIR` | 只能来自真实进程环境（防止 `.env` 自引用） | 同上 |
| `providers.json` | `providers.<id>.{api,baseUrl,apiKey,models[]}`；`api` 接受**缺省 + 4 个显式字符串**（两两同义）：缺省/`"openai"`/`"openai-completions"` → chat completions，`"anthropic"`/`"anthropic-messages"` → anthropic messages；无法识别的值 → 跳过该 provider 并 warn（`parseProviderApi`） | `src/config.ts:160-165` |
| apiKey 解析链 | `credential.key` > `providers.json` 的 `apiKey` > 环境变量 `<PROVIDER_ID>_API_KEY`（id 转大写、`-`→`_`，如 `ARK_API_KEY`） | `src/providers.ts:47-51` |
| 旧版回退 | `CUSTOM_BASE_URL` / `CUSTOM_API_KEY` + 内置 `CUSTOM_MODELS`；**前置条件：`PI_PROVIDER !== "anthropic"`**（正好是默认值，所以全新安装不会读 `CUSTOM_*`）；且 providers.json 非空时优先用文件 | `src/config.ts:226` |
| 默认模型 | `PI_PROVIDER=anthropic`，`PI_MODEL=claude-sonnet-4-5` | 同上 |
| `vision` 缺省 | 未显式 `false` 即按支持图片输入处理 | `src/providers.ts` |
| 全部 env 键 | 除 §6.1 列出的以外，v1 还解析 `TELEGRAM_ALLOW_FROM`、`FEISHU_ALLOW_FROM`、`WEBUI_HOST`、`MONITOR_PORT`、`BOTLER_LOG_DIR`、`BOTLER_SCHEDULES_FILE`、`BOTLER_HOLIDAYS_FILE`、`BOTLER_HOLIDAY_API_URL`、`CONVERSATION_CONTEXT_ENABLED`、`CUSTOM_BASE_URL`、`CUSTOM_API_KEY`、`TG_PROXY`、`HTTPS_PROXY`/`HTTP_PROXY`（含小写） | `src/config.ts:287-325` |
| 源目录 `.env` | v1 用 `import.meta.url` 定位 `APP_ROOT`，读 `<repo>/.env` 作为最后一级回退 | `src/config.ts:7`。**v3 决策：去掉这一级**（二进制没有"源目录"概念，按可执行文件目录读会误拾部署目录的杂散文件）；dev 场景把 `.env` 放 `~/.botler-agent/.env`，或用 `BOTLER_CONFIG_DIR` 指向临时目录 |
| 空 allowlist | Telegram/Feishu 允许所有发送者（个人部署默认） | `src/channels/allowlist.ts` |

#### 2.1.1 启动顺序契约（Go 必须显式设计，没有 lazy import 可用）

v1 靠**启动器里的条件动态 import**让部分入口不触发 `tools/paths.ts` 的模块期校验：`bin/botler.mjs` 按参数只 `await tsImport(...)` 其中一条——`init` 加载 `../src/init.ts`（它只 import `config.ts` + `system-prompt.ts`，从不加载 `paths.ts`），其余加载 `../src/index.ts`。注意这不是 ESM 的包级惰性求值（静态 `import` 仍会整图加载），而是"启动器决定加载哪张图"。Go 没有等价物——所有包的 `init()` 都在 `main()` 之前跑完，`cmd/botler` 一旦 import `internal/tools`，白名单校验就排在任何子命令之前。

所以这三条要写成规格：

| # | 启动顺序契约 |
|---|---|
| 1 | `internal/paths` 的白名单**惰性计算**（`sync.Once`），首次使用时才校验 `DATA_ROOT`；所有经 `index.ts` 的入口（无参数启动、CLI 单条、`wechat-login`、**`wechat-logout`**）在 `main()` 里**显式调用** `paths.Validate()`，保住 v1 的"启动即失败"体验。`wechat-logout` 在 v1 里同样走 `index.ts → dispatcher → runner → paths`，漏掉它会静默跳过校验 |
| 2 | `init` / `--version` 在 `Validate()` 之前返回，完全不触碰 `DATA_ROOT`（`init` 与 v1 行为一致；`--version` 是新增入口，同样不需要数据目录） |
| 3 | 服务入口内部顺序与 v1 相同：`startHealthServer()` → 无渠道 `exit 1` → `startWebui()` → `startScheduler()`；注意 WebUI 在退出判断**之后**，所以"没配渠道但开了 WebUI"在 v1 也是退出 |

反面教材就是 §9.2 smoke 的第 1 步：若把校验放在包 `init()` 里，`botler init` 会因为镜像里 `/data` 为空而直接抛 `DATA_ROOT has no subproject directories`——v1 不会，因为 `init.ts` 根本加载不到 `paths.ts`。

### 2.2 Dispatcher（`src/dispatcher.ts`）

| 契约 | 值 |
|---|---|
| 去重 | 按 `id`，窗口 5 分钟；命中返回哨兵文本 `(duplicate message ignored)`，记 `status: "duplicate"` |
| 串行队列 | 进程内单链，写入永不并发；任务抛错后链继续 |
| 只读任务 | `mutated=false` → 不校验、不提交；`projects` 为空记 `unknown-project` |
| 写任务 | `validateState(projects)` → 失败则**一次**自愈重试（新 agent，自包含 fix 指令）→ 再失败记 `validation-failed` 且不提交 |
| 自愈成功 | 回复追加 `\n\n(auto-fixed and saved)`，状态 `auto-fixed` |
| 提交信息 | 聊天任务：`agent: <用户消息压平空白后前 60 字>`（超长加 `…`）；**脚本任务：`agent: scheduled script <id>`** |
| 错误文案（逐字） | `⚠️ Could not save safely; please check the data: <fix>`；`⚠️ Task execution error: <msg>`；`⚠️ <scriptErr>Script produced invalid data (left as-is, not committed): <fix>`；`⚠️ Script task error: <msg>` |
| 去重表 | `seen` 只 `set` 不 `delete`，**进程内无 TTL 淘汰**；“5 分钟”只在判断时生效。Go 若加清理，内存行为就不对等，需显式决策 |
| 脚本任务日志 | `dispatchScriptedTask` 也写 TaskLog：`phase:"execute"`、`userMessage:"run <script>"`、`mutated = ran`（脚本实际跑过即 true，非仅成功）、`project` 由 `projectOf(res.cwd)` 反推 |
| 任务日志 | 每次 dispatch 追加一行 JSONL；自愈额外一条 `phase: "self-heal"` |
| 对话记录 | 仅 `execute` 阶段、有 `sessionKey`、状态 ∈ {success, auto-fixed, unknown-project}、且 `recordConversationTurn !== false` |
| 永不 reject | 所有异常转为用户可读文本 + 日志 |

### 2.3 Runner（`src/runner.ts`）

两阶段，顺序不可换：

1. **路由**：`listProjectDirs()` 为空 → 返回 `⚠️ No data subprojects under DATA_ROOT; please check your config.`；单项目 → 短路；否则一次无工具的 LLM 调用（prompt 只含“项目名 + AGENTS.md 前 300 字 **+ `…`**”），返回项目集 / `attachmentProject` / `resetContext` / `UNKNOWN`。
2. **执行**：把**选中项目**的约定文档全文（`AGENTS.md` > `CLAUDE.md` > `CODEBUDDY.md`，截断 12000 字）拼进 system prompt，新建 agent 执行。

| 契约 | 值 |
|---|---|
| 问候短路 | 无 `projectHint`、非 scheduler、无入站图片、`isGreeting()` 为真 → 不调用任何 LLM，返回固定欢迎语 |
| 图片消息 | **永不**当作问候；路由 `UNKNOWN` 且有图时返回带"请补充文字描述"的兜底文案 |
| 历史注入 | 最近 `CONVERSATION_CONTEXT_TURNS`（默认 5）轮用户可见对话，作为 system prompt 文本块注入；scheduler / CLI / self-heal 不读不写 |
| 历史裁剪 | 单轮 `user`/`assistant` 各截断 `CONVERSATION_TURN_MAX_CHARS=4000`；整块上限 `CONVERSATION_CONTEXT_MAX_CHARS=12000`，从最旧开始丢；**如果最新一轮本身就超 12000，则保留这一段的前 12000 字（`slice(0, maxChars)`），而不是丢空** |
| 工具轮次上限 | `MAX_TOOL_TURNS=20`；到 `max-1` 时注入一段“停止调用工具、直接总结”的 user 消息；到 `max` 硬停 |
| 硬停兜底（逐字） | `⚠️ Reached the tool-call limit (<N> turns); the task was aborted with no final reply.\n\nLast step: <tool>< (errored)?>\nLast result: <text or \"(no text result)\">\n\nTo raise the limit, set MAX_TOOL_TURNS in .env (default 20).` |
| 回复抽取 | 三分支：① 最后一条是 assistant → 取 text 块；② 最后一条是 toolResult 且 `toolTurnCapHit` → 硬停文案；③ 否则 → `⚠️ No reply generated`。绝不做 delta 拼接；路由用的 `lastAssistantText` 是另一套逻辑（error/aborted 时返回空串） |
| 错误回复（逐字） | `stopReason ∈ {error, aborted}` → `⚠️ Task did not complete successfully: <errorMessage ?? stopReason>` |
| 图片引用 | 正文里的 `![alt](path)` 提取为图片列表：`^https://` 的**直接放行（不过 `safePath`）**；本地路径需过 `safePath` + **出站**扩展名白名单 `png/jpe?g/gif/webp/bmp` + 文件存在；非法引用静默丢弃。**合法引用也会从正文里剃掉**（`text.replace(IMAGE_REF_RE, "").trim()`），否则 Telegram/CLI 会显示裸 markdown |
| 入站图片 | 先持久化到 `DATA_ROOT/<project>/photos/<date>-<rand>.<ext>`（经 `safePath`），再把相对路径以中文注记追加到用户消息。**入站扩展名白名单只有 `jpg/png/gif/webp`（不含 bmp），未知 MIME 兜底 `jpg`**——与出站不同 |
| 变更判定 | 存在 `write`/`edit` 的 toolResult，或持久化了入站图片 |

### 2.4 工具（`src/tools/*`）

| 工具 | 关键语义 |
|---|---|
| `read` | 返回文件原文文本 |
| `write` | 入参是**数据结构本身**；内部 `JSON.stringify(v, null, 2)`；父目录自动创建；若模型传了字符串，尝试 `JSON.parse` 一次（失败则报可操作的错）；非 object/array 报错 |
| `edit` | 精确字符串替换；`oldText` 不存在则报错（含前 60 字）；`all=true` 全替换 |
| `run` | 仅执行项目内**已存在**的 `.py`→`python3`、`.js/.mjs`→`node`；参数数组直传不走 shell；`cwd` 锁定项目根；60s 超时；输出上限 1MB；非零退出**不抛异常**，把 stdout/stderr 作为文本返回 |
| `schedule` | 无路径参数；只写固定 `schedules.json`；`action ∈ {list,create,update,delete,enable,disable}`；recipient 由框架注入，模型不可指定 |
| `clear_conversation_context` | 仅 IM 执行态注册；只标记当前任务，不碰文件 |

**工具返回串与 details 必须逐字对齐**（这是 A/B/C 分歧的核心，§3.3）：

| 工具 | 返回文本 | details |
|---|---|---|
| `read` | 文件原文（不截断） | `{path, bytes}` |
| `write` | `Wrote <path> (<n> bytes, valid JSON)` | `{path, bytes, recovered}` |
| `edit` | `Updated <path>`；失败抛 `oldText not found in the original file (first 60 chars): <...>` | `{path}` |
| `run` | 成功：stdout（空输出为 `(script ran successfully, no output)`）；运行时失败：`Script failed (<msg>):\n<stdout+stderr>`；前置失败（路径/不存在/类型）**抛错** | `{script, cwd, failed}` |

注：v1 把这些作为 `[{type:"text",text:...}]` 文本块发给模型；adk-go 的 model 层固定把它们序列化成 JSON（见 §3.3），**这是否需要逐字对等是 Q-1 的真正分歧点**。

**`safePath`（安全边界，`src/tools/paths.ts`）**

- 白名单 = `DATA_ROOT` 下**一级**非隐藏子目录，启动时计算一次；`DATA_ROOT` 未设/不存在/无子目录 → 启动即失败。
- 相对路径按 `DATA_ROOT` 解析；绝对路径原样保留但仍需在界内。
- 用 `root + separator` 前缀匹配（防 `/agent2`、`/agent-bak`）。
- 不 `realpath` 目标文件（新文件会 ENOENT），而是 `realpath` **最深的已存在祖先**再拼回剩余部分（防 symlink 逃逸）。
- 任务级子集：当 task-context 注入了选中项目，路径还必须落在其中之一。

### 2.5 校验与提交（`src/safety/*`）

| 契约 | 值 |
|---|---|
| 校验范围 | 选中项目下所有 `*.json`，跳过 `node_modules/.git/dist/public/.pi/.codebuddy` 与隐藏项 |
| 失败文案 | `<rel> is not valid JSON (parse failed: <msg>). Read the file, fix the syntax error, and write it back. Do not create a new file.` |
| 提交范围 | **仅选中项目**：`commitIfChanged(msg, log.projects)` 内部先 `if (projects && !projects.includes(d.name)) continue`；有 `.git` 且 `git status --porcelain` 非空 → `add -A` + `commit`。未选中但**脏了**（有未提交改动）的项目**不碰** |
| push | `GIT_PUSH=1` 时每个仓库用 `rev-list --count @{u}..HEAD` 判断（仅本地对象），有领先才 push；**循环与 `projects` 过滤同源** → 只补推本轮选中项目里的 un-pushed commit（AGENTS.md 的“下一轮自愈”只在本轮又选中该仓库时成立） |
| 首次 push | `push -u origin HEAD`（建立 tracking，不改写已有 tracking） |
| 跳过条件 | detached HEAD / 无 `origin` / 已配置但不可解析的上游 / 未有提交的分支 → **按 `(reason, 仓库名)` 每进程各警告一次**（不是全局一次），不阻塞 |
| 失败姿态 | push 失败只记首行 stderr，绝不失败任务、绝不当轮重试 |

### 2.6 渠道

| 渠道 | 契约 |
|---|---|
| Telegram | 长轮询；代理优先级 `TG_PROXY` > `HTTPS_PROXY` > `https_proxy` > `HTTP_PROXY` > `http_proxy`；allowlist 候选身份 = `from.id` / `username` / `chat.id`；去重 id = `<chatId>:<messageId>`；只发文本；纯图片回复时发 `(image generated, but the Telegram channel does not support sending images yet)` |
| Feishu | **v3.0 不实现**（配置保留可解析）。将来实现时按以下契约：HTTP webhook（`FEISHU_PORT=3000`）；`url_verification` 回 `{challenge}`；`encrypt` 事件解密（AES-128-CBC，见 D-1）；`im.message.receive_v1` 文本消息 → dispatch → 用 tenant_access_token 回文本；allowlist 候选 = `chat_id`/`open_id`/`user_id`/`union_id`；token 缓存提前 60s 过期；纯图片回复占位文本 |
| WeChat | 见 §4（协议由 SDK 承担，业务层逐条复刻） |

#### 2.6.1 飞书后置的影响

- `deliver()` 的降级链 `telegram → feishu → wechat` 在 v3.0 中只包含 `telegram → wechat`（跳过未实现的渠道，不报错）。
- `FEISHU_*` 仍从 `.env` 解析（否则现网配置会报未知项），但启动时只记一行 `[feishu] not implemented in this build; skipped`。
- D-1（AES-256 错密钥）与 D-2（不校验来源）随之推迟；实现飞书时必须按修正后的做法做，不要照抄 v1。

### 2.7 调度器（`src/scheduler/*`）

| 契约 | 值 |
|---|---|
| 触发形态 | `cron`（5 字段）/ `interval`（`5m`/`2h`/`1d`）/ `at`（每日 `HH:MM`）/ `once`（ISO 8601 一次性），四选一 |
| 时区 | IANA，默认 `Asia/Shanghai` |
| 动作 | `message`（走 LLM 管线）或 `script`（直接执行，绕过 LLM），二选一 |
| `once` | 严格晚于水位线才触发；过点后 `Infinity`；**不应用** `silentHours` |
| `holidayMode: "workday"` | 仅中国法定工作日；忽略 cron 的日期字段，只用时分；法定假日跳过、调休补班照常；不能与 `once` 组合 |
| 节假日数据 | `BOTLER_HOLIDAY_API_URL`（默认 holiday-cn），缓存到 `BOTLER_HOLIDAYS_FILE`；启动缺失时阻塞拉取（~2×10s），之后每 24h 刷新；任何失败保留缓存并退化为周一至周五 |
| `silentHours` | 落在窗口内的触发顺延到窗口结束；`from==to` 视为无窗口；最多 366 次迭代后放弃并警告 |
| 水位线 | 每个条目记录"上次触发/首次出现"时刻，`nextFireEpoch` 严格大于它；**默认不补跑** |
| 轮询延迟 | 无待触发 → 60s；否则到最近触发点；下限 1s，上限 2e9 ms |
| 保存唤醒 | 两个 listener：`setSchedulesSavedListener(reloadSchedules)` 与 **`setHolidaysSavedListener(reloadSchedules)`**（日历 24h 刷新落盘也要立即重算 workday 日程） |
| 触发 id | `schedule:<id>:<epochMs>`（天然绕过 5 分钟去重） |
| 重试 | `retry.max ≤ 10`、`retry.backoffMs ≤ 1h`；**不阻塞主循环**（定时器另行触发）；退避期间条目的动作/触发/项目被改写则取消重试；`script` 条目只在 `error` 重试，`validation-failed` 不重试（避免重复副作用） |
| unknown-project 特例 | 无 `project` 的消息型条目，路由失败时把 `message` 原文当提醒语推送 |
| 推送 | 有 `recipient` 则 `deliver()`；失败只记日志 |

### 2.8 推送 / 会话 / 日志 / 监控

| 模块 | 契约 |
|---|---|
| contacts | 记录各渠道到访地址（`~/.botler-agent/contacts.json`，0600）；微信重新登录时清空该渠道 |
| deliver | 主渠道优先；失败按降级链自动尝试，仅尝试“已配置 + 有记录地址”的渠道，逐个地址试到成功；降级只发文本；图片仅主渠道为微信时发送。v3.0 的链 = `telegram → wechat`（飞书未实现，跳过） |
| 会话窗口 | `~/.botler-agent/conversations/im.json`；固定 key `im`；只存 `{ts, project, projects, user, assistant}`；原子写 + fsync 后 rename；损坏视为空 |
| 任务日志 | `~/.botler-agent/task-logs/YYYY-MM-DD.jsonl`，按 `startedAt` 分日；字段全 camelCase（见 `src/logging/types.ts`）；读侧容忍坏行 |
| 日志查询 | 时间范围 / 项目 / 来源 / 状态 / 关键字 / 分页；`summary`、`token-stats`（hour/day/week，按 project/source 分组）、`usage`、`cleanup`（只删匹配 `^\d{4}-\d{2}-\d{2}\.jsonl$` 的文件） |
| 监控 | 5s 采样、容量 720 点的历史环；`/healthz`、`/healthz/history`、`/metrics`（Prometheus 文本）；**指标名以 `src/monitor/health.ts` 为准**（特别注意 `botler_scheduler_next_fire_epoch_seconds` / `botler_scheduler_last_fire_epoch_seconds` 与 `botler_schedules_enabled_total` 的前缀差异，见 §10.2） |
| 日志截断 | `collect.ts` 的 `TEXT_MAX = 2000`：工具结果、thinking、conversation 文本都截到 2000 字（`userMessage` / `replyText` 不截）；`resultChars` 记录截断前长度 |
| 配置备份 | `.backups/` 有**两套命名**：配置类 `(.env|system-prompt.md|schedules.json).YYYYMMDDTHHMMSS`（每文件最多 10 份），以及 **`holidays.json.<epochMs>`**（假日日历自己的 10 份）；`listBackups`/prune 必须分开处理，不能把假日备份当脏数据删掉 |
| WebUI 写接口保护 | **修改配置的接口**要求 `X-Botler-UI: 1`（cleanup、system-prompt PUT、schedules PUT、backup restore、wechat pair start/cancel）。**`POST /api/config/schedules/preview` 是只读的，不校验该头**（UI 自己带了而已）；按 HTTP 方法一刀切会把 preview 从 200 变成 403 |

### 2.9 WebUI 接口清单（必须逐一实现）

```
GET  /                              index.html（原样）
GET  /healthz  /healthz/history  /metrics
GET  /api/logs                     ?from&to&project&source&status&q&limit&offset
GET  /api/logs/:id                  未命中返回 200 + null（不是 404）
GET  /api/summary
GET  /api/usage
GET  /api/token-stats              ?granularity=hour|day|week&groupBy=project|source
POST /api/cleanup                  {beforeDays?|all?}
GET  /api/schedules/overview
GET  /api/schedules/history-index
GET  /api/schedules/:id/runs       ?limit&offset
GET  /api/schedules/:id/stats
GET  /api/config                   {systemPrompt, schedules, backups, wechat, pairing}
GET  /api/config/system-prompt     {content}
PUT  /api/config/system-prompt     {content}（非空、≤100KB）
GET  /api/config/schedules
PUT  /api/config/schedules         {schedules[]}
POST /api/config/schedules/preview {cron|interval|at|once, timezone, holidayMode}
GET  /api/config/backups
POST /api/config/backups/restore   {name, file}
POST /api/wechat/pair
GET  /api/wechat/pair/status
POST /api/wechat/pair/cancel
```

---

## 3. 选型：Go + ADK-Go 的分工

### 3.1 版本与 pin（先看这一节，再读后面的断言）

本稿涉及外部库的结论，核对于 **2026-11-08**，对应版本如下。P0 的第一件事是把这些 pin 下来并逐项重跑（清单见 §3.8）。

| 依赖 | 版本 / commit | 说明 |
|---|---|---|
| `google.golang.org/adk/v2` | 本地 `/Users/chenjie/Documents/dev/github/adk-go` @ `e98671f`（main） | `/v2` 仍在快速演进，符号与默认值都可能变 |
| `github.com/openilink/openilink-sdk-go` | v0.6.0（go mod cache） | MIT，无第三方依赖 |
| `@earendil-works/pi-ai` / `pi-agent-core` | 仓库 `node_modules` 实际安装版本 | 本稿对 v1 行为与行数的结论来源 |
| Go 工具链 | 本机 go1.27.3；adk-go 的 `go.mod` 声明 `go 1.26.10` | 见下 |

**工具链口径统一为 go 1.27.x**（满足 adk-go 的 1.26.10 下限）：`go.mod` 的 `go` 行、Dockerfile 的 `golang:1.27-alpine`、CI 的 `go-version: '1.27.x'` 三处必须一致；若 adk-go 后续抬高下限，以它的 `go.mod` 为准并同步这三处。

依赖体积与编译时间本稿**不做预估**（没有可靠出处）：`go.sum` 行数、二进制大小、首次构建耗时在 P0 固定版本后实测，写入决策记录。许可方面：adk-go = Apache-2.0、openilink-sdk-go = MIT，与本项目 AGPL-3.0-only 的直接依赖兼容；但 adk-go 会拉入 `cloud.google.com/go/*`、`genai`、`anthropic-sdk-go`、`openai-go` 等间接依赖，供应链审计范围应按 `go.sum` 全量看。

### 3.2 adk-go 提供了什么 / 缺什么（核对于上述 pin）

| 需求 | adk-go 现状 | 结论 |
|---|---|---|
| OpenAI 兼容网关（自定义 `baseUrl`） | `openaimodel.ClientConfig.BaseURL` | 满足 |
| **OpenAI 兼容网关的默认 API** | `ClientConfig.API` 零值 = **Responses API**（`/v1/responses`） | **必须显式设 `APIChatCompletions`**，否则现网 `custom`（`.../v1`）会打错端点 |
| Anthropic 自定义端点 | `anthropicmodel.ClientConfig.BaseURL`（官方 SDK + `option.WithBaseURL`，路径仍 `/v1/messages`） | 与 v1 一致；`ark` 的 `.../api/plan` 需 P0 实测 |
| 网关不支持 `developer` role | completions 适配器把 system instruction 放进 `system` 消息 | 天然满足 |
| 多 provider 注册表 | `model/registry.go` | **A/C 方案用不到**，直接构造 model 即可；B 也不需要 registry |
| **agent 循环** | `llmagent` + `runner` 的 `Flow.Run` → `runOneStep` → `callLLM` → `handleFunctionCalls` | 有；属于 runner/session 体系（§3.5） |
| **工具结果形态** | `completions/request.go` 的 `newToolMessage` 固定 `json.Marshal(fr.Response)` | **硬限制**：发不出 v1 的纯文本 tool 消息（§3.3） |
| 工具声明 | `functiontool.New`（结构体推 schema，或显式 `InputSchema`） | 满足；`write.content` 需显式 schema |
| 图片输入 | `genai.Part{InlineData}` | 满足 |
| 每轮工具上限 + 倒数第二轮注入提示 | `RunConfig.MaxLLMCalls` 数的是 LLM 调用总数；注入需 `BeforeModelCallback` | **需自建**（语义不同） |
| stopReason / 错误归一化 | 无 pi 等价物 | **需自建** |
| 成本（USD） | 只给 `UsageMetadata` 的 token 数 | **恒 0**（见下），不自建单价 |
| 会话持久化 / 压缩 | `session.Service` / `session/compaction` | 仅 B 方案直接用；v1 不需要 |
| 长期记忆 | `memory.Service{AddSessionToMemory, SearchMemory}` + 两个记忆工具 | 仅 B 方案能直接接（§11.5） |

**成本这一行要按事实写**：pi-ai 确实有 `calculateCost(model.cost, usage)` 并写进 `usage.cost.total`；但 v1 的 `providers.ts:22` 给 providers.json 的每个模型硬编码 `cost:{input:0,output:0,cacheRead:0,cacheWrite:0}`，所以**现网配置下 `costUsd` 恒为 0**。只有走内置 `anthropicProvider()`（没有 providers.json 的全新安装）时，pi-ai 才有真实价格表。v3 对等做法：恒 0；若以后要支持内置目录，再加价格表。

### 3.3 硬约束：adk-go 的 model 层发不出 v1 形态的工具结果

这条决定 A/B/C 怎么选，必须先摆事实：

- **v1（pi-ai）**：`openai-completions` 适配器把 toolResult 的 text 块 join 成字符串，发成 `{role:"tool", content:"<纯文本>"}`；`anthropic-messages` 发成 `tool_result` 文本块。
- **adk-go**：`convertContent` 遇到 `FunctionResponse` 就调 `newToolMessage` → `json.Marshal(fr.Response)` → `{role:"tool", content:"{\"result\":\"Wrote ...\"}"}`；Anthropic 路径同构。而且它要求 call id 能被 tracker 解析。
- **结论**：只要用 adk-go 的 model 层，"自研循环就能保住 v1 工具结果文本"这个说法不成立。要么接受 JSON 形态，要么自己写 wire protocol。
- 补充：即便选 C，"文本对等"也不只是 `join("\n")`。pi-ai 在 `openai-completions.js:1065-1124` 的实际形态是：① 空文本按有无图片回落成 `(no tool output)` / `(see attached image)`；② `compat.requiresToolResultName` 为真时补 `name`；③ **有图片时不是给 tool 消息加块，而是另起一条 `role:"user"` 消息**，内容 = 固定文本 `Attached image(s) from tool result:` + 若干 `image_url` 块；④ 若 `compat.requiresAssistantAfterToolResult` 为真，在这条 user 消息之前再插一条伪造的 `role:"assistant"` 消息 `I have processed the tool results.`，并把 `lastRole` 置为 `user`；⑤ 文本过 `sanitizeSurrogates`。C 方案要复刻字节级提示词，这五条都得进规格。

三个方案的真实差异：

| | 循环 | 工具结果形态 | 成本 | stopReason | 后续 session/compaction/记忆 | 代码量 |
|---|---|---|---|---|---|---|
| **A** | 自写 ~350 行 | JSON（**偏差**） | 恒 0 | 自写 | 需重做 | 中 |
| **B** | adk-go runner | JSON（**偏差**） | 恒 0 | 自写 | **迁移成本最低**（接口已就位；v3.0 仍是每任务空 session，compaction/记忆本期不会真正启用） | 少（~150 行胶水） |
| **C** | 自写 ~350 行 | **v1 纯文本（对等）** | 恒 0 | 自写 | 需重做 | 多（两种 wire protocol，每种 300–400 行） |

判断依据：

- 如果能接受"模型看到的工具结果是 JSON"，A/B 都行；**B 更划算**（少写循环，事件/usage 现成，并为之后的持久会话预留接口——但 compaction 与记忆在 v3.0 不会被真正启用）。
- 如果第一版要求提示词级对等（差分测试更干净），选 **C**；但此时 adk-go 只剩"genai 类型 + 工具 schema"，引入它的理由要重新论证——`net/http` + `encoding/json` 复刻两个协议并不比自己写循环难多少。
- 诚实的成本：JSON 形态对现代模型通常可用（工具结果被包一层 `{"result": ...}`），但**无法用单测证明不影响质量**，只能靠固定用例实测。

**这个实验属于 P0**：用两个假 provider 跑同一组工具调用，比较 A 与 C 发出的请求体；再用真实模型跑 10 条固定用例（读→改→写→跑脚本），比较工具成功率。实验结果出来再定 A/B/C。

### 3.4 A 方案：adk-go 的 model 层 + 自研工具循环

```
internal/agentrt
  ├─ Runtime 接口（沿用 v2 的 seam 思路）
  │    Complete(ctx, Request) → Reply{Text, ToolCalls, Usage, StopReason, Err}
  ├─ adk/        adk-go model 适配器 + 循环实现
  └─ fake/       测试用确定性实现（无网络）
```

自研循环的职责（约 300–400 行）：拼 system + messages → 调 `model.LLM.GenerateContent` → 有 tool calls 则顺序执行并追加 **function response** → 计数工具轮、`max-1` 注入收尾提示、`max` 硬停 → 返回最终 assistant 文本与逐次 usage。

相对 runner 真正省掉的是这三件事（工具结果形态已经不是差异点）：

| 对等点 | v1 行为 | runner 里的做法 | 自研循环 |
|---|---|---|---|
| 轮次上限 | 数"含 toolCall 的 assistant 消息" | `MaxLLMCalls` 语义不同 + 回调注入 | 循环里计数，直接注入 |
| 最终回复 | 最后一条 assistant 的 text 块 | 从事件流重建消息序列 | 手里就是消息序列 |
| usage / stopReason | pi-ai 归一化后给出 | 两者都要自建 | 同一处实现 |

代价：放弃 runner/session/compaction；将来做持久会话要重做（§11 预留注入点只是不锁死，不是已经能用）。

### 3.5 B 方案：`llmagent` + `runner`

- 优点：循环、事件、session、回调、`MaxLLMCalls`、压缩、记忆都能直接用；未来上多 agent/workflow 更顺。
- 胶水约 150 行：① 每任务建一个内存 session（`session.InMemoryService` + `AutoCreateSession`）；② 6 个工具包成 `functiontool`（`write.content` 需显式 `InputSchema`）；③ 消费 `runner.Run` 事件流，重建"最后一条 assistant 文本 / 工具调用序列 / 每次 usage"；④ `BeforeModelCallback` 注入收尾提示 + `MaxLLMCalls` 硬停；⑤ provider 错误映射成 v1 的两句英文兜底。
- 代价：工具结果是 JSON（与 A 相同）；成本仍需自己写（恒 0 即可）。
- 建议：选 B 也先做 `Runtime` seam + 差分测试，避免 runner 语义扩散到 business 层。

### 3.6 C 方案：自写 wire protocol

- 内容：各自实现 `openai-completions`（`/chat/completions`）与 `anthropic-messages`（`/v1/messages`），含 tool_calls / tool_result / 图片 inline / usage / 错误码。
- 优点：工具结果文本 100% 对等；不依赖 adk-go 的演进；成本/stopReason/usage 全部可控。
- 代价：要自己维持两套协议的边角（`tool_choice`、reasoning/thinking 字段、cache_control、错误归类、流式）。这些正是 pi-ai 已经解决的。
- 触发条件：§3.3 的实验显示 JSON 工具结果会导致可观测的质量下降，或第一版坚持字节级提示词对等。

### 3.7 各方案如何实现 `CONVERSATION_CONTEXT_TURNS`（结论：完全一样）

这个问题容易误解：`CONVERSATION_CONTEXT_TURNS` **不是**模型上下文窗口管理，也不属于 agent 运行时。它是“一个文件 + 一段拼进 system prompt 的文本”，属于 dispatcher/runner 层，因此 **A/B/C 三种方案的实现一字不差**。

数据流（v3.0 必须与 v1 一致）：

```
dispatcher（execute 阶段，有 sessionKey）
  → conversation.Store 读 ~/.botler-agent/conversations/im.json 的最近 N 轮
  → runner 把历史格式化为一段文本，拼进 system prompt
  → agent（仍然无状态：不把历史当 chat messages，不带工具调用元数据）
  → 完成后 dispatcher 把本轮（用户原文 + 最终回复）追加回 im.json，保留最近 N 轮
```

必须保留的四个不变量：

| 不变量 | 值 |
|---|---|
| 写入时机 | 仅 `execute` 阶段、有 `sessionKey`、状态 ∈ {success, auto-fixed, unknown-project}、且 `recordConversationTurn !== false`；scheduler / CLI / self-heal 不读不写 |
| 存储内容 | 只存用户可见文本：`{ts, project, projects, user, assistant}`；用户消息**不带**框架追加的图片注记，助手消息**不带** `(auto-fixed and saved)` 修饰 |
| 裁剪 | `CONVERSATION_CONTEXT_TURNS=5`（0 = 关闭）；单轮 `CONVERSATION_TURN_MAX_CHARS=4000`；整块 `CONVERSATION_CONTEXT_MAX_CHARS=12000`，超出从最旧开始丢 |
| 注入形态 | **文本块，不是消息**。路由阶段：`最近对话：\n<...>\n`，每轮带 `（项目：x）`；执行阶段：`\n\n# 最近对话\n<...>\n\n以上是最近若干轮用户可见对话。...`（后接一段“新任务则忽略历史”的指令） |

两种方案的落点：

- **A / C（自研循环）**：历史文本作为 `Request.SystemPrompt` 的一部分传入，循环本身对历史无感知。
- **B（`llmagent` + `runner`）**：历史文本作为 `llmagent.Config.Instruction` 的一部分；**每个任务必须新建一个空 session**（`session.InMemoryService` + 每个任务唯一 sessionID）。绝不把 `im.json` 的历史塞进 session 的历史里——那会让模型看到工具调用元数据、token 成本翻倍，且与 v1 行为不同。

执行阶段还有一段容易漏的内容：IM 会话专属的 `clearToolInstruction`（`runner.ts:480-483`，告诉模型可用 `clear_conversation_context` 工具，且仅在用户明确要求清空时才调用）会拼在 system prompt 末尾；非 IM 会话没有这段。

另外两个容易漏的点：

1. 会话 key 固定为 `im`，**Telegram 与微信共享同一份历史**（v1 行为，别“顺手修”成按渠道隔离）。
2. 历史在 `runTask` 入口就加载，并且**早于**问候短路与路由：路由 prompt 里也要用它（带项目名），否则“上一轮的补充”会被路由到错误项目。

**对“无限上下文”的影响（v3.1+）**：A 方案需要自己写摘要/召回（§11.3 的 M1+M2）；B 方案可直接接 `session/compaction` 或同样的 M1。两者都不影响 v3.0 的对等交付。

---

### 3.8 P0 核对清单（逐项打勾才能开始 P1）

对 adk-go（pin 到 commit）与 openilink-sdk-go（pin 到 v0.6.0）：

| # | 核对项 | 不通过时的回退 |
|---|---|---|
| 1 | `openaimodel` 显式 `APIChatCompletions` + 现网 `custom` provider 实打一次 | 无（自己写 completions 客户端，即方案 C） |
| 2 | `anthropicmodel` + `ark` 的 `.../api/plan` 实打一次，记录实际请求 URL 与响应 | 自己拼 `net/http`（方案 C 的 anthropic 部分） |
| 3 | `functiontool` 的 `write.content` 任意 JSON（显式 `InputSchema`）往返 | 手写 `tool.Tool` 实现（不依赖 functiontool） |
| 4 | usage：`UsageMetadata` 的 `PromptTokenCount` / `CandidatesTokenCount` / `CachedContentTokenCount` / `ThoughtsTokenCount` 映射到 v1 的 `input/output/cacheRead/reasoning` | 按 provider 响应原字段自行解析 |
| 5 | 图片 inline（微信入站 jpg → model） | 同 4 |
| 6 | **工具结果形态实验**（§3.3）：A 与 C 的请求体对比 + 真实模型 10 条用例质量 | 决定 A/B/C |
| 7 | `openilink-sdk-go`：真实扫码 → 收发文本 → 收图落盘 → 发图；`Monitor` 的重试/退避与会话过期行为 | 对齐其数值；若行为对不上，按计划切到自写 120 行轮询循环（P4 的计划内分支，见 §4.3 风险 2） |
| 8 | `go.sum` / 二进制体积 / 首次构建耗时实测 | 记录即可 |

### 3.9 依赖清单（Go 侧，尽量少）

| 用途 | 建议 |
|---|---|
| 微信协议 | `github.com/openilink/openilink-sdk-go`（MIT，零依赖） |
| Telegram | `github.com/go-telegram-bot-api/telegram-bot-api/v6`（或直接写 Bot API HTTP，代码量相近、依赖更少） |
| 模型/工具声明 | `google.golang.org/adk/v2`（**仅 A/B 方案需要**；若 Q-1 选 C，这一行整条删除，改为标准库自写两个协议） |
| wire protocol（仅 C） | 标准库 `net/http` + `encoding/json`，无第三方依赖 |
| QR 图 | `github.com/skip2/go-qrcode`（输出 PNG → data URL，对齐 v1 的 `qrcode.toDataURL`） |
| cron | 不用库，移植 `cron.ts`（语义多且已被测试固定，第三方库行为不一样） |
| SQLite（记忆阶段二） | `modernc.org/sqlite`（纯 Go，CGO-free） |
| 其余 | 标准库（`net/http`、`os/exec`、`encoding/json`、`time` + `time.LoadLocation`） |

## 4. 微信方案评估（openilink-sdk-go v0.6.0）

### 4.1 结论

**满足现有功能，可以直接替换 v1 手写的协议层。** 下面每一项能力都需在 P0 用真实扫码复核（§3.8 #7），本表核对于 v0.6.0：

| v1 手写 | SDK 对应 | 说明 |
|---|---|---|
| `login.ts`（取二维码 + 轮询状态） | `FetchQRCode` / `PollQRStatus` / `LoginWithQR(callbacks)` | 回调含 `OnQRCode` / `OnScanned` / 结果 `BotID` |
| `api.ts` 的 `getUpdates` 长轮询 | `Monitor(ctx, handler, opts)` | 内建重试与退避、动态超时；`OnBufUpdate` 持久化 `sync_buf` |
| `getupdates` 游标持久化 | `MonitorOptions.OnBufUpdate` | 对应 `wechat/sync.json` |
| `sendMessage` 文本 | `SendText` / `Push`（自动缓存 contextToken） | 对应 `send-media.ts` |
| `sendMessage` 图片 | `UploadFile` + `SendImage` | 对应 `upload.ts` + `send-media.ts` |
| CDN AES-128-ECB 解密下载 | `DownloadMedia` / `DecryptAESECB` / `ParseAESKey` | 对应 `aes-ecb.ts` + `download.ts` |
| `types.ts` 消息结构 | `WeixinMessage` / `MessageItem` / `CDNMedia` | 字段一致（含 `ContextToken`、`FromUserID`） |
| 语音转文字 | `ExtractText`（含 voice text） | v1 也只是读 `voice_item.text` |
| 输入状态 | `SendTyping` | v1 未用，可保留不启用 |
| 会话过期 `-14` | `APIError.IsSessionExpired()` | 对齐 v1 的 `SESSION_EXPIRED_ERRCODE` |

### 4.2 仍由我们实现（业务层，逐条移植）

- owner（扫码人 `ilink_user_id`）+ `WECHAT_ALLOW_FROM` 白名单；
- `context_token` 的**持久化**（SDK 只在内存缓存）+ `lastMsgAt` / `lastRemindedAt`；
- 24h 窗口续期提醒循环（`WECHAT_REMINDER_HOURS`，默认 23，`0` 关闭）；
- 选图即发/文字后到的图片批量合并（`WECHAT_IMAGE_BATCH_SECONDS`，默认 60，`0` 关闭）；
- 入站图片 → `safePath` 校验 → `DATA_ROOT/<project>/photos/...` 落盘；
- 回复前的 markdown → 纯文本（对齐 `markdownToPlainText`；不要依赖 SDK 的过滤器，行为不一致）；
- `contacts.json` 记录；
- WebUI 配对状态机（`idle/pending/scanned/confirmed/expired/error`、8 分钟 TTL、二维码刷新上限、扫码后免重启上线）；
- 重新配对与登出时的状态清理（清 context_token、清该渠道 contacts、清 sync 游标）。

### 4.3 风险

1. **协议版本漂移。** SDK v0.6.0 对应 iLink 的某个版本；v1 的 `CHANNEL_VERSION` 是 `0.1.0`。上线前需要一次真实扫码 + 收发 + 收图 + 发图验证（这是 §12 的 P4 出口条件，无法用单测替代）。
2. **`Monitor` 的控制粒度。** v1 的重试参数是 `DEFAULT_LONG_POLL_TIMEOUT_MS=35_000`、`MAX_CONSECUTIVE_FAILURES=3`、`BACKOFF_DELAY_MS=30_000`、`RETRY_DELAY_MS=2_000`、`SESSION_EXPIRED_ERRCODE=-14`（`monitor.ts:19-23`）。SDK 的内建策略可能不同：对齐这些数值，或干脆用自己写的 120 行轮询循环（零依赖、行为确定）。**这是 P4 的计划内分支，不是失败后的退路**（§3.8 #7 / §12 P4 / §13 同一口径）。
3. **字节级媒体访问。** SDK 已提供 `DownloadMedia` 返回明文字节，满足"落盘后再给模型相对路径"的 v1 语义。

---

## 5. 目录与模块映射

单 module，`internal/` 按 v1 的职责划分（不做多 module，YAGNI）。

```
cmd/botler/main.go                 # init / cli / wechat-login / wechat-logout / server
internal/config                    # 两级 dotenv、providers.json、全部 env 解析（含默认值）
internal/prompt                    # 默认中文 system prompt、项目清单/摘要、路由 prompt、模板占位符
internal/greeting                  # isGreeting / 固定中文文案
internal/paths                     # safePath（安全边界）+ 惰性 allowlist + Validate() + property tests
internal/agentrt                   # Runtime 接口、Request/Reply、InboundImage
internal/agentrt/adk               # adk-go 适配 + 工具循环 + usage（仅 A/B；选 C 时换成 internal/agentrt/openai + /anthropic）
internal/agentrt/fake              # 测试替身
internal/tools                     # read/write/edit/run/schedule/clear + 注册表 + task-context
internal/runner                    # 两阶段路由 + 执行 + 图片抽取 + 兜底文案
internal/dispatch                  # 去重、串行队列、校验、自愈、提交、日志、统计
internal/safety                    # validate / git(commit+push)
internal/scheduler                 # types / store / cron / holidays / engine / poll
internal/logs                      # TaskLog 结构、JSONL 存储、查询/汇总/时间序列/清理
internal/conversation              # im.json 窗口
internal/push                      # contacts / deliver
internal/channels/telegram
internal/channels/feishu           # v3.0 不实现（占位，留接口）
internal/channels/wechat           # account / context / login / pair / monitor / batch / download / send
internal/webui                     # server + go:embed(index.html) + config-store + schedule-history
internal/monitor                   # stats / health / metrics / 采样
internal/version
internal/testutil
testdata/fixtures/*.json           # 复用 v2 分支的 greeting / image_batch / cron / log_query 夹具
```

对应关系（与 v2 的 §3 表同构）：

| v1 | Go |
|---|---|
| `src/index.ts` | `cmd/botler/main.go` |
| `src/config.ts`、`src/providers.ts` | `internal/config`、`internal/agentrt/adk` |
| `src/dispatcher.ts` | `internal/dispatch` |
| `src/runner.ts`、`runner/decisions.ts` | `internal/runner` |
| `src/prompts/system-prompt.ts`、`greeting.ts` | `internal/prompt`、`internal/greeting` |
| `src/tools/*` | `internal/tools`、`internal/paths` |
| `src/safety/*` | `internal/safety` |
| `src/scheduler/*` | `internal/scheduler` |
| `src/push/*` | `internal/push` |
| `src/channels/telegram.ts`、`feishu.ts` | `internal/channels/*`（v3.0 只做 telegram） |
| `src/channels/wechat/*`（协议层） | 删除 → `openilink-sdk-go` |
| `src/conversation/store.ts` | `internal/conversation` |
| `src/tools/task-context.ts` | `internal/tools`（注入 recipient / conversationSessionKey / projects 三者；其中 sessionKey 只在 IM 执行态注入，决定 `clear_conversation_context` 是否注册——属于安全红线的一部分） |
| `src/logging/*` | `internal/logs` |
| `src/monitor/*` | `internal/monitor` |
| `src/webui/server.ts`、`config-store.ts` | `internal/webui` |
| `src/webui/index.html` | 原样复制 + `go:embed` |
| `bin/botler.mjs`、`init.ts` | `cmd/botler/main.go` 的子命令（`bin/botler.mjs` 已在 v2 落地后删除——不再分发 npm 包，只发 Go 二进制与 Docker 镜像） |

---

## 6. 数据兼容与回滚

**目标：Go 二进制可以直接跑在现有 v1 安装上（数据与配置零迁移）；回滚 = 恢复 v1 的仓库树（`git checkout main`）后照旧 `npm start`。**

> 注：npm 包与 `bin/botler.mjs` 启动器已于 v2 合并到 `main` 后删除（只分发 Go 二进制与 Docker 镜像）；回滚走仓库内的 `src/` + `npm start`（`tsx src/index.ts`）。上文关于启动器条件动态 import 的描述仅作历史记录。

| 文件 | 处理 |
|---|---|
| `~/.botler-agent/.env`、`providers.json`、`system-prompt.md` | 原样读取；**不新增任何字段**（成本恒 0，见 §0.6 / §3.2 / §8.2）；`providers.json` 里出现未知字段时忽略而不是报错 |
| `schedules.json` | **外层是对象：`{"schedules": [...]}`**（不是裸数组）；字段名与校验规则不变；写入仍是“全量规范化 + 10KB 上限 + 原子写 + 备份” |
| `holidays.json` | 不变 |
| `contacts.json`、`wechat/account.json`、`wechat/sync.json`、`wechat/contexts.json` | 字段与嵌套结构不变（token 值可直接沿用）。注意 `contexts.json` 的 key 是渠道 userId（含 `@`/`.`），v1 只把它当 **JSON 对象 key**，不拼文件名；只有 Go 侧改用"一个 key 一个文件"时才需要哈希——这是 v3 新设计，不是 v1 行为 |
| `conversations/im.json` | 不变（key 固定 `im`，`sessionFile` 另有 `^[a-z0-9_-]+$` 校验）；`{turns:[{ts,project,projects,user,assistant}]}`；写 tmp → fsync → rename（v1 无目录 fsync，Go 补齐） |
| `task-logs/*.jsonl` | 写入字段与顺序不变，**camelCase**；Go 结构体用 `json:"startedAt"` 风格 tag；数字用 `int64`；**不要新增字段**（v2 加过 `degraded`，v3 不加）。`TEXT_MAX=2000` 的截断（工具结果/thinking/conversation 文本）也必须一致，否则差分测试会因截断长度不同产生噪声 |
| `.backups/` | **两套命名**：配置类 `^(.env|system-prompt\.md|schedules\.json)\.\d{8}T\d{6}$`（`config-store.ts`，每文件 10 份）与 `holidays.json.<epochMs>`（`holidays.ts`，10 份）。`listBackups` / prune 必须分开处理，不能把假日备份当脏数据删掉 |

实现约束：

- `TaskLog` 里的 `details`、`arguments`、`systemPrompt` 等字段是"任意 JSON"，Go 侧用 `json.RawMessage`/`any`，避免重新序列化改变字段顺序（WebUI 只按 key 读，但 diff 测试会受影响）。
- 时间统一用毫秒 int64；`durationMs` 由 `endedAt - startedAt` 计算，不要用 wall-clock 差值。
- 并发写 JSONL 只发生在 dispatcher 单链内，与 v1 一致；日志写入失败不得影响主流程。
- `task-logs` 已有 28MB / 24 个日文件（采样 2026-11）：`queryLogs` 必须**逐行流式读取**，不得整文件读入后一次性反序列化到切片。
- 时间字段的取整要照抄：`durationMs` 在正常路径是 `endedAt - startedAt`，但 `minimalLog` / `scriptTaskLog` 里是两次独立 `Date.now()`，可能差 1ms；`nextFireAtSec` / `lastFireAtSec` 用 `Math.round(ms/1000)`。差分归一化要把这些算进去。

### 6.1 现网配置快照（`/Users/chenjie/.botler-agent`，2026-11 核对）

| 项 | 实际内容 | 对 Go 实现的约束 |
|---|---|---|
| `.env` 已用键 | `DATA_ROOT` / `PI_PROVIDER` / `PI_MODEL` / `TG_PROXY` / `TELEGRAM_BOT_TOKEN` / `FEISHU_*`（5 个）/ `WECHAT_ENABLED` / `GIT_PUSH` / `WEBUI_ENABLED` / `WEBUI_PORT` / `SCHEDULER_ENABLED` | 未出现的键（`MONITOR_*`、`MAX_TOOL_TURNS`、`CONVERSATION_*`、`WECHAT_ALLOW_FROM`、`WECHAT_REMINDER_HOURS`、`WECHAT_IMAGE_BATCH_SECONDS`）必须走与 v1 完全相同的默认值 |
| `providers.json` | 两个 provider：① `custom`（**无 `api` 字段** → 默认 openai-completions，`baseUrl` 以 `/v1` 结尾，含 2 个模型）② `ark`（`api = anthropic-messages`，`baseUrl = https://ark.cn-beijing.volces.com/api/plan`，含 4 个模型） | ① 必须映射为 `APIChatCompletions`（不能用 adk-go 默认的 Responses）② 必须支持 anthropic-messages + 自定义 `baseUrl`；两种协议都要在 P0 实测 |
| 模型字段 | 多数模型无 `vision` 字段，`ark` 中只有 `doubao-seed-2.1-lite` 显式 `vision: true` | 缺省必须按 **支持图片** 处理，否则微信收图时图片会被静默丢弃 |
| `schedules.json` | 外层是 `{"schedules": [...]}`；采样时 17 条，字段全集实测为 `at, commit, cron, enabled, holidayMode, id, message, once, project, recipient, retry, script, timezone`（当前样本**没有** `interval` / `silentHours` / `scriptArgs`，但解析器必须支持它们） | 四种触发形态 + 脚本动作 + 重试 + 假日模式都要能解析与回写；`recipient.userId` 原样保留（形如 `o9cq...@im.wechat`） |
| `wechat/contexts.json` | 以 `@im.wechat` 结尾的 userId 为 key，含 `token` / `lastMsgAt` / `lastRemindedAt` | 键名不透明、含 `@` 与 `.` → 任何持久化文件名不得直接用它（需哈希） |
| `task-logs/` | 24 个 `YYYY-MM-DD.jsonl`，共 28MB；字段与 `src/logging/types.ts` 一致 | 兼容测试拿真实文件做输入：查询/汇总/分页结果与 v1 逐一对比 |
| `.backups/` | 44KB，**两类文件共存**（5 个 `holidays.json.*` + 5 个 `schedules.json.2026...`） | 两套命名与排序规则都要一致，否则"恢复"找不到旧备份或误删假日备份 |

**兼容验收做法**（P1 硬门禁，覆盖面要写全）：复制现网配置到临时目录，`DATA_ROOT` 指向数据目录副本，逐项对比 v1 与 Go：`/api/logs`（含分页与 28MB 真实日志下的耗时）、`/api/logs/:id`、`/api/summary`、`/api/usage`、`/api/token-stats`（hour/day/week × project/source）、`/api/schedules/overview|history-index|:id/runs|:id/stats`、`/api/config`（含 backups 列表与 wechat 状态）；再跑一轮只读任务，对比 TaskLog（除 id/时间戳）。

---

## 7. v1 既有缺陷：必须显式决策

这些是"照抄 = 把 bug 也复刻"的地方。建议在 P1 开始前逐条定调。

| # | 问题 | 证据 | 建议 |
|---|---|---|---|
| D-1 | **（推迟到飞书期）** Feishu 加密事件解密会抛错：用 `aes-256-cbc` 但 key 是 16 字节的 `md5(encryptKey)`，Node 会抛 `ERR_CRYPTO_INVALID_KEYLEN`（已实测），被外层 catch 吞掉 → 加密事件被静默丢弃 | `src/channels/feishu.ts` | v3.0 不涉及。实现飞书时按规范用 **AES-128-CBC**（key 16B、IV 取密文前 16B、PKCS#7）+ 向量测试；同时在 v1 上开 issue |
| D-2 | **（推迟到飞书期）** Feishu 不校验事件来源（`verification_token` / 签名） | 同上 | 实现飞书时：配了 `FEISHU_VERIFICATION_TOKEN` 就校验，未配置则 warn 后保持兼容 |
| D-3 | `run` 工具支持 `.js/.mjs`（需要 node），而 Go 镜像默认不装 node | `src/tools/run.ts`、`Dockerfile` | 三选一：①镜像继续装 node（**推荐**，但注意：v1 的 node 来自 `node:20-alpine` 基础镜像，`apk add nodejs` 拿到的是 alpine 仓库的版本，**不是"零行为变化"**；若要严格对齐就固定 `nodejs=20.x` 或用 node 基础镜像只装二进制）；②只支持 `.py`（破坏性）；③配置项 `RUN_JS_ENABLED` 默认开、镜像用 node 变体 |
| D-4 | 监控指标是 Node 专属（V8 heap、event loop lag） | `src/monitor/health.ts` | 见 §10：分三期，第一期保接口、第二期换 Go 运行时指标、第三期加 pprof |
| D-5 | `MAX_TOOL_TURNS` 的"轮"= 含工具调用的 assistant 消息数，与 LLM 调用次数不等价 | `src/runner.ts` | Go 侧按消息计数实现，不要用 `MaxLLMCalls` 类比 |
| D-6 | 工具轮上限触发后的兜底文案依赖"最后一条是 toolResult"这一 Node 侧消息形态 | 同上 | Go 循环需保留同等的消息序列，否则文案分支会变 |
| D-7 | `edit` 的 `all` 参数用 `split/join`，`newText` 含 `$&` 等不会出问题；但 `replace` 单次替换在 JS 里会对 `$` 展开 | `src/tools/edit.ts` | Go 用 `strings.Replace`，**语义更正确**；注意这是微偏差（用户几乎不可能触发），记录即可 |
| D-8 | 会话文件 renaming/fsync 时序：v1 是"写 tmp → fsync → rename"，没有目录 fsync | `src/conversation/store.ts` | Go 侧补齐目录 fsync（更安全，不影响可读性） |

---

## 8. 验证方案

### 8.1 确定性逻辑：夹具直译

v1 的 21 个 `node:test` 文件（2677 行）是规格书。**v2 分支已经把最关键的几组抽成 JSON 夹具**（`crates/botler/tests/fixtures/{greeting,image_batch,cron,log_query}.json`），Go 侧直接复制这份夹具 + 写 Go 表驱动测试即可，不必重读 TS 断言。

必测清单（按风险排序）：

1. `paths`：历史 bug 全量（前缀 trick、symlink 逃逸、最深已存在祖先、相对路径、隐藏目录、任务子集）。这是安全边界，测试密度最高。
2. `scheduler/cron`：cron 字段解析、`interval`/`at` 换算、`once` 水位线、`silentHours` 跨窗、`holidayMode` 与假日表组合。
3. `image_batch`：单图、多图、图+文字、超窗、`0` 关闭。
4. `greeting`：中文/英文、全角空格、尾部 emoji、带图不带图。
5. `runner/decisions`：路由集合判定、`resetContext`、是否记录对话。
6. `validate`：坏 JSON 的 `fix` 文案必须逐字一致。
7. `logs`：查询过滤、分页、汇总、`token-stats` 分桶、`cleanup` 的严格文件名。
8. `git`：无改动不提交、无 `.git` 跳过、首次 push 用 `-u`、上游不可解析时**按 `(reason, 仓库名)` 每进程各警告一次**（用临时仓库构造）、`projects` 为空集合时什么都不做。
9. `webui/config-store`：备份名正则、10 份上限、restore 的路径穿越拒绝；**假日备份（`holidays.json.<epochMs>`）不能被 prune 掉**。
10. `dispatchScriptedTask`：precondition / runtime / validation-failed 三个分支的状态、文案、是否提交、是否重试，以及脚本 TaskLog 的形态（`userMessage: "run <script>"`、`mutated = ran`）。
11. `conversation/store`：单轮超 12000 字时保留前 12000（不是丢空）、整块裁剪从最旧丢、`formatRecentTurns` 的 `（项目：x）` 只在路由阶段出现。
12. `schedules/preview`：**不带** `X-Botler-UI` 也返回 200（它不修改任何东西）。

### 8.2 差分测试（对等门禁）

搭建一次性脚本 `scripts/parity.sh`：

1. 准备一个临时 `BOTLER_CONFIG_DIR` 与 `DATA_ROOT`（含 2–3 个数据项目 + `AGENTS.md` + 1 个 `build.py`）。
2. **让 v1 变确定**：v1 在模块加载时就把 `models.streamSimple` 绑死了，没有可替换的 runtime seam。可行做法是写一个本地 OpenAI 兼容 stub server，把临时 `providers.json` 指向它（`api: "openai-completions"`, `baseUrl: "http://127.0.0.1:<port>/v1"`, 任意 apiKey），**不改 v1 源码**。stub 按脚本返回固定的 assistant 文本与 tool_calls 序列。**注意 pi-ai 走的是流式接口（`streamSimple`），stub 必须实现 SSE 分块响应**，否则连请求都发不出来。
3. 同一组输入分别驱动 v1（`tsx src/index.ts "..."`）与 Go（`./botler "..."`），两边都指向同一个 stub。
4. 归一化后比较：`task-logs/*.jsonl`、`conversations/im.json`、`schedules.json`、`git log --format=%s`。
5. 差异必须逐条解释，允许清单（如 D-7）写在脚本里。

**字段处置表**（脚本里按这张表逐项实现，不允许"看起来差不多"就放过）：

| 处置 | 字段 |
|---|---|
| 忽略（非确定或进程级） | `id`、`taskId`、`startedAt`、`endedAt`、`durationMs`、`calls[].timestamp`、`conversation[].timestamp`、`tools[].startedAt`、`tools[].durationMs`、`modelCache`、`systemPrompt`、`routing.prompt`、`routing.candidates`、`usage.{input,output,cacheRead,cacheWrite,reasoning,total}`、`calls[].{input,output,cacheRead,cacheWrite,reasoning,total}`、`routing.usage.{input,output,cacheRead,cacheWrite,reasoning,total}`（stub 与真实 provider 的 token 数不同）、`images` 里的**本地**文件名（随机） |
| 必须逐字相等 | `status`、`replyText`、`userMessage`、`project`、`projects`、`mutated`、`phase`、`source`、`provider`、`model`、`routing.decision`、`inboundImageCount`、`images` 里的 **`https://` 引用**（确定值，也是 §2.3 放行分支的唯一门禁）、`conversation[].{role,text,thinking,chars,stopReason,toolName,isError}`、`tools[].{name,arguments,resultText,resultTruncated,resultChars,isError,details}`、`calls[].{stopReason}` |
| 必须恒等于 0 | `usage.costUsd`、`calls[].costUsd`、**`routing.usage.costUsd`**（`collect.ts` 在路由跑过时也写这个字段）—— 三者都是 `calculateCost(全 0 单价)` 的结果，**确定性常量**，不是非确定值。若被忽略，"v3 对等做法 = 恒 0"这个最重要的结论就失去了门禁（Go 侧接真实单价或写错都会被放过） |
| 数量必须相等（值忽略） | `conversation`、`tools`、`calls` 三个数组的长度 |

`arguments` / `details` 是任意 JSON：Go 侧必须用 `json.RawMessage` 原样搬运（键序不能变），否则差分脚本会因序列化顺序产生假差异。

这是唯一能抓住"文本/字段/顺序"级偏差的机制，优先级高于任何单测。

### 8.3 WebUI 契约测试

`index.html` 原样复用，所以只需保证 API 形状：

- 用 `httptest` 对每个端点断言：状态码、`Content-Type`、顶层 key 集合、关键字段类型。
- 用固定日志夹具跑一遍：`/api/logs` 分页、`/api/summary` 的 9 张卡片、`/api/token-stats` 的分桶。
- 修改型接口缺 `X-Botler-UI: 1` 必须 403；`/api/config/schedules/preview` 例外（200）。
- `GET /api/logs/<不存在>` 必须是 200 + `null`。
- 静态资源：`go:embed` 后 `GET /` 返回的 HTML 与 v1 的 `src/webui/index.html` 字节一致（这是"原样复用"的验证）；SPA 里所有 `fetch` 都是相对路径，`go:embed` 不能改路径前缀。

### 8.4 人工验收（每个渠道一次）

问候回复 → 路由失败文案 → 读/写/编辑/跑脚本 → 坏 JSON 自愈 → `git commit` 信息 → 创建定时任务并触发 → 推送降级 → 微信收图落盘并在回复中引用 → 微信图片批量。

---

## 9. 部署

### 9.1 镜像

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 编译跑在宿主架构，产物是目标架构的静态二进制
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/botler ./cmd/botler

FROM alpine:3.22
RUN apk add --no-cache git openssh-client tzdata python3 nodejs ca-certificates
COPY --from=build /out/botler /usr/local/bin/botler
# 与 v1 相同的默认值（端口也必须相同）
ENV BOTLER_CONFIG_DIR=/config DATA_ROOT=/data WEBUI_HOST=0.0.0.0 \
    WEBUI_ENABLED=1 WEBUI_PORT=8900 \
    SCHEDULER_ENABLED=1 MONITOR_ENABLED=1 MONITOR_PORT=8899
VOLUME ["/config", "/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8899/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["botler"]
```

- 与 v1 相比：去掉 node 运行时与 `npm ci`，加上 `botler` 本体；`nodejs` 仅为 `run` 工具的 `.js/.mjs`（见 D-3）。
- 构建阶段固定 `FROM --platform=$BUILDPLATFORM` + `ARG TARGETOS TARGETARCH` + `CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH`：编译不跑在 QEMU 上，多架构构建时间从十几分钟降到分钟级。运行阶段的 `apk add` 仍需 QEMU（或直接放弃 alpine，但那会丢掉 git/python/node）。
- `GIT_PUSH` 的宿主机挂载/环境变量约定**完全不变**，`docs/docker-git-push.md` 只需把“镜像里有 `ssh`”换成“镜像里有 `git` + `openssh-client`”（本来就一样）。

### 9.2 CI / 发布

现有 5 个工作流个个都要动，但大部分只需改触发条件与语言相关步骤：

| 工作流 | 现状 | v3 处置 |
|---|---|---|
| `ci.yml` | PR/push to `main`：node typecheck + test；`docker-smoke`（build 镜像 + `init`） | **改写**：`gofmt -l` / `go vet` / `golangci-lint` / `go test -race` / `CGO_ENABLED=0 go build`。`-race` 需要 gcc + CGO，可以和静态构建同一个 job（ubuntu-latest 有 gcc）；真正需要单独拆的是**交叉编译产物 job**（不同 GOOS/GOARCH、不带 race）。`docker-smoke` 保留，但入口从 `tsx` 换成 `botler`，并补一条真实启动 + `curl /healthz`（见下） |
| `docker-publish.yml` | push `main` → `edge` + `sha-<short>`，QEMU 多架构 | **改触发与标签**：v3 在 `v3` 分支开发时推 `edge-go` + `sha`，避免把 Go 镜像推成 `edge` 覆盖 v1；合并到 `main` 后再恢复 `edge` |
| `docker-release.yml` | tag `v*` → `latest` + semver | **几乎不变**：仍由 tag 触发；若 v3 先独立发版，镜像名用 `botler-agent-go` 或 tag 用 `v3.*`，避免和 v1 的 `latest` 撞车 |
| `npm-audit.yml` | 每日 `npm audit` + signatures | **v3 不再需要**（无 npm 依赖）。若 `main` 继续维护 v1 则保留；v3 分支上新增 `govulncheck` 替代 |
| `release.yml` | tag → validate(node) → draft → publish → cleanup | **保留整体结构**（draft/publish/cleanup 与语言无关），只改 `validate` 为 Go 校验，并**新增 `build-artifacts` job** 把 6 个交叉编译产物 + `sha256sums.txt` 传到 draft；否则 `install.sh` 没东西可下 |
| *(新增)* `govulncheck.yml` | — | 每日 + PR 跑 `govulncheck ./...`；Dependabot 开 `gomod` 生态 |
| *(新增，可选)* `codeql.yml` | — | Go 静态扫描；不是必须，但现网有 `npm-audit` 每日扫描，换成它比较对等 |

关键片段（可以直接拿去改）：

```yaml
# ci.yml
jobs:
  check:
    steps:
      - uses: actions/checkout@<pinned-sha>
      - uses: actions/setup-go@<pinned-sha>
        with: { go-version: '1.27.x', cache-dependency-path: go.sum }
      - run: test -z "$(gofmt -l .)" && go vet ./...
      - run: golangci-lint run
      - run: go test -race ./...
      - run: CGO_ENABLED=0 go build -trimpath ./cmd/botler

  container-smoke:
    steps:
      - run: docker build -t botler-agent:smoke .
      # 1) 二进制 + 脚手架能跑
      #    前提：§2.1.1 契约 2——这两个入口在 paths.Validate() 之前返回，不碰 DATA_ROOT。
      #    若实现成"包 init() 里校验"，这两步必须挂 -v /tmp/smoke/data:/data 才能过，
      #    而那会把"启动顺序写错"这个 bug 掩盖掉。
      - run: docker run --rm botler-agent:smoke --version
      - run: docker run --rm botler-agent:smoke init
      # 2) 真能起进程并回答健康检查
      #    - monitor 只绑 127.0.0.1（v1 契约）→ 必须从容器内探，不能端口映射后从宿主 curl
      #    - 「没有任何可用渠道则 exit 1」（§2.1）→ 必须给一个占位 channel，否则进程立刻退出
      - run: |
          mkdir -p /tmp/smoke/data/proj /tmp/smoke/cfg
          docker run -d --name smoke \
            -v /tmp/smoke/cfg:/config -v /tmp/smoke/data:/data \
            -e DATA_ROOT=/data \
            -e TELEGRAM_BOT_TOKEN=000000:dummy \
            -e WEBUI_ENABLED=0 -e SCHEDULER_ENABLED=0 -e MONITOR_ENABLED=1 \
            botler-agent:smoke
          for i in $(seq 1 30); do
            docker exec smoke wget -qO- http://127.0.0.1:8899/healthz >/dev/null && exit 0
            sleep 1
          done
          docker logs smoke && exit 1
```

上面这个 smoke 有两个坑，都是 v1 契约决定的，不是实现问题：

- `src/index.ts` 的顺序是 `startHealthServer()` → "没有渠道就 `process.exit(1)`" → `startWebui()`。空 `/config` + 无 token 必然命中退出分支，容器立刻死 → 必须给一个占位 channel（dummy token 会让长轮询报错，但进程存活并标记渠道 down，正好验证容错路径）。
- `src/monitor/health.ts:233` 是 `listen(port, "127.0.0.1")`，宿主 `-p 18899:8899` 到不了容器 loopback → 用 `docker exec` 从容器内探。
- 备选：`WEBUI_ENABLED=1` + `WEBUI_HOST=0.0.0.0` + `-p 18900:8900`，WebUI 端口同样注册了 `/healthz`，可从宿主直接 curl。
- **`/tmp/smoke/data/proj` 在主路径和备选路径都不能省**：`src/index.ts` 在模块加载期静态 import `webui/server.ts` → `scheduler/engine.ts` → `tools/paths.ts`（`scheduler/poll.ts` 的注释也点明了这条传递链），所以 `DATA_ROOT` 校验与 `WEBUI_ENABLED` 无关；只有 §2.1.1 契约 2 的 `init` / `--version` 不受影响。

另外 `--version` 在 v1 里**不存在**（`bin/botler.mjs` 只认 `init`，`index.ts` 把首个非 `--` 参数当消息），Go 加它是合理新增，已在 §1 的"允许的少量新增"里登记。

三个容易踩的点：

1. **`docker-smoke` 的覆盖必须补。** v1 跑 `init` 能抓到"缺 tsx / 依赖没装"这类错误；Go 静态二进制不会缺库，`init` 只能证明能启动。上面加了"真实启动 + 容器内抓 `/healthz`"，否则 `DATA_ROOT` 处理、端口绑定、`safePath` 启动校验这类回归在镜像层面完全抓不到。
2. **动作必须继续用 SHA pin**（仓库现有习惯），不要为了省事改成 `@v4`。
3. **分支保护里的 required checks 名字会变**（`check-typecheck-test` → `check`），改完 CI 必须同步仓库 ruleset，否则 PR 会永远绿/永远等不到合并条件。

版本号与脚本：

- 唯一来源从 `package.json` 改为 git tag + `internal/version`（`-ldflags "-X .../internal/version.Version=$TAG"`）；`Makefile` 不再用 `node -p` 读版本，`make release` 只负责打 tag + 推送。
- `v3` 分支建议**删掉 v1 的 `src/` 与 `package.json`**（保留在 `main`），否则两边 CI 会互相干扰（`npm-audit` 找不到 lockfile、`go vet` 扫到无关目录）。

### 9.3 脚本

- `install.sh`：改为下载 release 二进制（`BOTLER_VERSION` 默认 latest）+ 可选 `sha256sum -c`，保留 `botler init`；不再 `npm install`。需要 §9.2 的 `build-artifacts` job 先产出 6 个平台的产物。
- `scripts/botlerctl.sh`：`restart [main|stable]` 语义保留，但 `stable` 拉的是 release tag 的**二进制**；`.botler.pid` / `botler.log` / `nohup` 语义不变，`stop/status/logs` 不变。注意 v1 的 `restart main` 会 `git pull` + `npm install`，Go 侧要改成"下载二进制"还是"保留源码构建"，需要显式决策（涉及回滚路径，见 §1 目标 2）。
- `Makefile`：`release VERSION=x.y.z` 改为"打 tag + 交叉编译 + 推送"，版本号唯一来源改为 `internal/version`。

---

## 10. 监控方案（分期）

v1 的监控是一层“零依赖、只读、只绑 127.0.0.1”的边缘能力：内存统计 + 两个出口（WebUI 端口内嵌一份、独立 8899 一份），5s 采样、720 点环形历史，WebUI 的 Monitor 页签直接轮询这三个接口。Go 侧分三期落地。

### 10.1 目标与原则

- **一期（v3.0）：形状不变。** `/healthz` 的 JSON key、`/metrics` 的指标名与标签、`/healthz/history` 的样本结构全部与 v1 一致 → 现有 WebUI、现网 Prometheus 抓取、告警规则都不用改。
- **二期（v3.1）：换成语义正确的 Go 指标**，改名而不是伪造数值；文档与看板同步。
- **三期（v3.2，可选）：深度诊断**（pprof、慢任务归因）。
- 任何一期都遵守 v1 的三条铁律：只读、不碰 `DATA_ROOT`、监控自身失败不得影响 agent（listen 失败只记日志，handler panic 恢复为 500）。

### 10.2 指标映射

| v1 指标 | 一期取值 | 二期处置 |
|---|---|---|
| `botler_uptime_seconds` | `time.Since(startedAt)` | 保留 |
| `botler_memory_rss_bytes` | Linux 读 `/proc/self/statm` × page size；其他平台回退 `MemStats.Sys` | 保留（真实 RSS） |
| `botler_memory_heap_used_bytes` | `MemStats.HeapAlloc` | 保留，但**语义换轨**（V8 heapUsed → Go HeapAlloc）：看板曲线切换日会阶跃，需在 release note 与看板备注里写明 |
| `botler_memory_heap_total_bytes` | `MemStats.HeapSys` | 同上（V8 heapTotal → HeapSys） |
| `botler_memory_external_bytes` | `MemStats.Sys - HeapSys` | 二期删除（Go 无此概念） |
| `botler_memory_array_buffers_bytes` | `0` | 二期删除，新增 `botler_go_heap_inuse_bytes` |
| `botler_event_loop_lag_p50_ms`、`botler_event_loop_lag_p90_ms`、`botler_event_loop_lag_p99_ms` | `0` | **二期删除**，新增 `botler_go_gc_pause_p99_ms`（`MemStats.PauseNs` 环）、`botler_go_goroutines`、`botler_go_allocs_total` |
| `botler_queue_depth` / `botler_active_tasks` | dispatcher 计数器 | 保留 |
| `botler_active_agents` | 当前在跑的模型调用数 | 保留（语义对齐 v1 的 Agent.prompt 数量） |
| `botler_dispatches_total`、`botler_duplicates_total`、`botler_failures_total` | 计数器 | 保留 |
| `botler_model_cache_queries_total`、`botler_model_cache_hits_total` | 模型解析缓存计数 | 保留 |
| `botler_tasks_total{status}` | 按最终状态分桶 | 保留 |
| `botler_schedules_total`、`botler_schedules_enabled_total`、`botler_scheduler_next_fire_epoch_seconds`、`botler_scheduler_last_fire_epoch_seconds` | 调度器快照 | 保留。**前缀不要写错**：前两个是 `schedules_`，后两个是 `scheduler_`（v1 即如此）；写错会直接让现网告警规则失效 |
| `botler_channel_up{channel}` | `telegram` / `wechat`（v3.0 无 feishu） | 保留 |

这张表里的指标名**一律写全名**（对照 `health.ts` 的 `emit(...)` 调用），不要用"同一前缀简写"：`hits_total` 展开成 `botler_hits_total` 是错的（正确是 `botler_model_cache_hits_total`），`enabled_total` 展开成 `botler_enabled_total` 也是错的（正确是 `botler_schedules_enabled_total`）。§10.1 承诺"现网告警规则不用改"，写错一个名字就等于破坏这个承诺。

`/healthz` 必须保持这些 key：

```
ok, pid, uptimeSec, startedAt,
memory{rss,heapUsed,heapTotal,external,arrayBuffers},
eventLoopLagMs{p50,p90,p99},
queueDepth, activeTasks, activeAgents,
totalDispatched, duplicatesTotal, totalFailed, statusCounts,
modelCache{queries,hits,hitRate},
schedulesTotal, schedulesEnabled, nextFireAtSec, lastFireAtSec, lastFireId,
channels{<name>:{up,firstUpAt?,lastUpAt?,lastErrorAt?,lastError?}}
```

`/healthz/history`：`{intervalSec: 5, capacity: 720, samples: [{ts,rss,heapUsed,queueDepth,activeTasks,lagP50,lagP99}]}`。**样本只有 `lagP50` 和 `lagP99` 两个延迟字段（没有 P90）**；一期它们恒为 0，WebUI 曲线是一条平线，不报错。`rss` 与 `/healthz` 的 `memory.rss` 同源（同一采样函数），不要一个读 `/proc` 一个读 `MemStats`。

### 10.3 实现要点

- 单个 `sampler` goroutine：`time.Ticker(5s)` + 长度 720 的环形缓冲；写者推进索引，读者在锁内复制一份切片再序列化，避免持锁做 JSON 编码。
- 计数器用 `atomic.Int64`/`Uint64`；`statusCounts` 用 `sync.Map`（key 数量小且固定）；快照在一段短 `RLock` 内组装，避免读到“半更新的组合”。
- 两个出口共用同一组 handler：WebUI 的 `ServeMux` 注册 `/healthz`、`/healthz/history`、`/metrics`；8899 的独立 server 复用完全相同的 handler。**`startedAt` 只有一个来源**（进程启动时赋值），两个端口必须读到同一个值，不要各自 `time.Now()`。
- 8899 绑 `127.0.0.1`（同 v1）；WebUI 绑 `WEBUI_HOST`（容器内默认 `0.0.0.0`）。两者都不鉴权，与 v1 一致。
- `MONITOR_ENABLED != "0"` 才启动 8899；WebUI 启动时也启动采样（对齐 v1 的 `ensureMonitorSampling`，避免 `MONITOR_ENABLED=0` 时 Monitor 页签无数据）。
- 指标文本输出自己拼（`# HELP` + `# TYPE` + `name{labels} value`），不引入 Prometheus 客户端库。

### 10.4 三期：可选深度诊断（不进 v3.0）

- `net/http/pprof` 挂在 8899，仅当 `BOTLER_PPROF=1`；默认关闭，不引入依赖。
- 每任务分段计时：`dispatchWaitMs`（入队 → 开始）、`agentMs`、`toolMs`（按工具名分桶），在 `/healthz` 多一个 `lastTask` 对象——定位“慢在模型还是工具”。
- 可选 OTLP 导出（`OTEL_EXPORTER_OTLP_ENDPOINT`），默认关闭。

### 10.5 明确不做

- 不做 Prometheus 推送网关、不自带时序库（现网已有抓取）。
- 监控数据不落盘（v1 也是纯内存，历史只保留约 1 小时）。
- 不为了“对齐”而给 Node 指标伪造数值：无等价的就写 0 并在二期删除。

---

## 11. 永久记忆：方案评估

### 11.1 v1 的现状

- 只有"最近 5 轮用户可见对话"（`conversations/im.json`），**旧轮次被物理覆盖**，不是压缩，是丢弃。
- 任务之间没有任何共享状态，system prompt 每次重建。
- 因此"上周说过的事"、"跨项目关联（记账里买了香蕉 → 烹饪里吃掉）"都做不到。

### 11.2 候选方案

| 方案 | 内容 | 成本 | 收益 | 风险 |
|---|---|---|---|---|
| **M0** | 保持现状（本期基线） | 0 | 无 | 无 |
| **M1** | 历史**追加**而非覆盖 + 滚动摘要（LLM 压缩旧轮次为短摘要） | 低：1 张表/文件 + 1 次额外 LLM 调用 | 跨重启连续、长对话不失忆 | 摘要必然有损；需定义"摘要不进 git" |
| **M2** | 显式事实库：`remember/forget/recall` 工具 + 分层命名空间 | 中：工具 + 存储 + 检索 | 精确、可审计、可删除、跨项目关联 | 依赖模型主动写入；需要写入门槛规则 |
| **M3** | SQLite + FTS5 全量历史索引，按当前消息检索 top-K | 中：`modernc.org/sqlite`（CGO-free） | 无需向量服务即可做关键词召回，容量大 | 中文分词质量；FTS5 对中文需 `tokenize=unicode61`（效果一般）或 trigram |
| **M4** | 向量检索（embedding + 暴力余弦或 sqlite-vec） | 高：需 embedding 端点 + 调优 | 语义召回最好 | 自建网关未必有 embedding；sqlite-vec 需 CGO；成本与延迟 |
| **M5** | 用 adk-go 的 `memory.Service` 做接口，后端实现 M2/M3 | 低（接口只有 2 个方法） | 未来可接 runner/compaction | **只有 B 方案能直接复用 adk-go 的两个记忆工具**；A/C 只能借鉴注入思路。接口很薄，收益主要是"不锁死" |

### 11.3 建议路线

**本期（v3.0）：M0 + 预留注入点。** 只把 `agentrt.Request` 里预留 `MemoryDigest string` 与 `Recall func(query) []Snippet`（均为空），不实现搜索与存储；保证记忆接入不触碰 dispatcher/runner。注意这只是"以后好接"，**不等于**已经能用 adk-go 的 `memory.Service` 或两个记忆工具（那需要 B 方案的 session/工具接口）。

**v3.1：M1 + M2（同时实现 M5 的接口层，便于以后切 B 方案）。**

- 存储放 `BOTLER_CONFIG_DIR/memory/`（**不在 `DATA_ROOT`**，不进 git、不参与校验/提交）。
- 三层命名空间（沿用 v2 已定稿的模型，原文见 `v2` 分支 `docs/design/rust-rewrite-v2.md` §4.3，实现参照 `crates/botler/src/memory.rs`；与 v1 的一次性 agent 不冲突）：
  - tier 1 个人会话（默认写入目标）：`(source, conversation_or_principal, project)`
  - tier 2 项目共享：`(project)`，需显式参数才写
  - tier 3 用户全局：`(source, principal_id)`，需显式参数 + 提示词约束（仅"用户明确要求记住"或"任务必需的非敏感事实"；健康/关系/精确定位/凭据永不自动写入）
- 群聊永不读 tier 3，也永不写 tier 3。
- 注入预算：摘要 + 召回结果合计 ≤ 1500 字符，超出按时间倒序截断，并加显式标记 `[truncated]`。
- 提供 `/memory list|delete|clear` 与 WebUI 的一个只读页签（可选）。
- **M3 的 FTS5 需要先确认 `modernc.org/sqlite` 的默认构建是否带 FTS5**（若需要 build tag，会牵动 §9.1 的静态构建）；不确认就先用 M2 的朴素关键词匹配。

**v3.2：M3。** 全量历史入 SQLite，召回走 FTS5；tier 过滤在 SQL 层完成；`modernc.org/sqlite` 保持 CGO-free（静态二进制不受影响）。

**M4 暂不做。** 触发条件：出现"关键词召回明显不够用"的真实抱怨，且用户确认可用的 embedding 端点。

### 11.4 与“每任务新建 Agent”的关系

不需要改架构：记忆在 **prompt 构建时** 读取（摘要 + 召回片段作为独立段落注入），agent 仍然是无状态的一次性实例。这保持了 v1 的核心纪律（"自愈指令必须自包含"），也避免引入 v2 那套会话生命周期、新鲜度指纹、WAL 轮转的复杂度。

### 11.5 adk-go 到底给了什么（核对于 §3.1 所列 pin）

| 能力 | adk-go | 能否直接用 |
|---|---|---|
| **接口** | `memory.Service`：`AddSessionToMemory(ctx, session.Session)` + `SearchMemory(ctx, *SearchRequest)` | 可直接实现，只有两个方法 |
| **内存实现** | `memory.InMemoryService()`：按 `(appName, userID)` 存，从 session events 摄取，关键词打分（非 ASCII 支持子串匹配），最多 10 条 | **不能用于永久记忆**：进程重启即丢，不能按项目/群聊分层，无清理接口 |
| **云实现** | `memory/vertexai`、`memory/vertexairag` | 需要 GCP/Vertex RAG，与“本地优先”冲突 |
| **记忆工具** | `tool/loadmemorytool`（模型可调 `load_memory`）+ `tool/preloadmemorytool`（每次请求自动注入到 system instructions，带 `<PAST_CONVERSATIONS>` 包装） | **可复用**（如果走 B 方案或自己实现 `tool.Tool`） |
| **上下文压缩** | `session/compaction`（滑窗 + LLM 摘要） | 与“长期记忆”是两件事；v1 的 agent 是每任务新建，本期不需要 |
| **存储/策略** | 无 | **必须自己做**：持久化、三层命名空间、写入门槛、注入预算、删除/审计 |

结论：**接口和两个工具是 adk-go 的，实现和策略是我们的**——而且**只有 B 方案能直接用**：`memory.Service` 吃 `session.Session`，两个记忆工具要求 `agent.Context`/`tool.Tool`。A/C 自研循环只能借鉴 `preloadmemorytool` 的注入思路（往 system instruction 里拼一段带长度上限的文本），检索质量仍取决于我们自己实现的后端。

### 11.6 记忆的隐私与运维红线

1. 记忆文件权限 0600，目录 0700；路径使用不透明 ID（SHA-256），不拼接渠道原始 ID（**v3 新设计**：v1 没有记忆，`wechat/contexts.json` 直接用 userId 做 JSON key 是安全的，因为没有把它拼进文件名）。
2. 记忆读写在 `BOTLER_CONFIG_DIR`，**不进入** `DATA_ROOT` 的校验与 git 提交。
3. 必须可删：用户能按 tier/项目/单条清除，且清除后重启不再出现。
4. 日志里不记录记忆原文（只记 `retained=<id> tier=<n>`）。
5. 群聊场景默认只读 tier 1(group) + tier 2。

---

## 12. 里程碑与工作量

单人估时，含测试；不含记忆功能与监控二期/三期。

| 阶段 | 内容 | 估时 | 出口条件 |
|---|---|---|---|
| **P0** | §3.8 的 8 项核对；**先交付 `scripts/parity.sh` 的 v1 stub harness**（否则 P1/P8.2 无法验收）；**工具结果形态实验（A vs C）**；`openilink-sdk-go` 真实扫码收发一次 | 3–4 天 | §3.8 全部打勾 + A/B/C 定案 + 真实请求日志 |
| **P1** | 确定性核心：config（含现网 `.botler-agent` 兼容）、启动顺序契约（§2.1.1）、paths、tools、prompt/greeting/routing、agent 循环、dispatch、validate/git、logs、conversation、CLI | 7–9 天 | §6.1 兼容验收 + §8.2 差分（stub harness）通过；§8.1 全部 12 项绿 |
| **P2** | Telegram | 1–2 天 | 长轮询/代理/白名单/去重端到端 |
| **P3** | Scheduler + push | 3–4 天 | cron/once/workday/silentHours/retry 全绿；降级链 `telegram → wechat` 用例通过 |
| **P4** | WeChat：SDK 接入 + 业务层（白名单/批量/token 持久化/提醒/落盘/发送）+ 配对状态机。**外部不确定性最高**，按 6–9 天留缓冲；若 `Monitor` 行为对不上，直接走"照抄 SDK 轮询逻辑"的分支（计划内，不是退路） | 6–9 天 | 真实微信：文本、图片、定时推送、WebUI 扫码 |
| **P5** | WebUI + monitor（一期）：`go:embed` index.html、全部 API、备份恢复、独立 8899、指标形状不变 | 3–4 天 | §8.3 契约测试全绿；SPA 无 JS 报错 |
| **P6** | 打包与部署：Dockerfile、CI、install/botlerctl、release、README/AGENTS 更新 | 2–3 天 | 多架构镜像 + tag 发布 + NAS 实测 GIT_PUSH |
| — | 差分验证与收尾 | 3–5 天 | 全量差分无未解释差异 |

合计 **28–40 人日**（约 5.5–8 周），**不含**：飞书配置兼容测试、现网 28MB 日志的基准调优、双实现并存期的 bug 双修（按每周 0.5 天预留）。

后续期（不在 v3.0）：飞书渠道（按 §2.6.1 的修正版契约）、监控二期/三期（§10.2/§10.4）、记忆（§11.3）。

---

## 13. 风险表

| 风险 | 影响 | 缓解 |
|---|---|---|
| **工具结果形态（JSON vs 纯文本）导致模型行为变化** | 工具调用成功率下降、回复风格漂移，且单测抓不到 | §3.3 的 10 条真实用例实验；不可接受就转 C 方案 |
| 模型层其他差异（stopReason、usage 字段） | 日志字段漂移，难排查 | 差分测试 + §3.8 #4 的字段映射表 |
| **`custom` provider 打错端点**（adk-go 默认 Responses API） | 所有请求 404/400，看起来像“网关不兼容” | P0 硬门禁：显式 `APIChatCompletions` + 用现网 `providers.json` 实测 |
| **`ark`（anthropic-messages 自定义 baseUrl）路径拼接** | 模型全部不可用 | P0 硬门禁：用真实 key 打一次并记录实际 URL。官方 SDK 固定追加 `/v1/messages`，若网关要求 `/api/plan/v1/messages` 之外的形状，"加一层归一化"救不了，得自己拼 HTTP（方案 C） |
| 微信协议版本漂移 | 收不到消息 / 发图失败 | P0 真实扫码验证；若 `Monitor` 行为对不上，按计划切到自写轮询循环（§4.3 风险 2） |
| 现网 28MB 日志的查询性能 | WebUI 卡顿/内存毛刺 | 流式逐行扫 + 分页上限；P1 用真实日志做基准 |
| 中文分词的 FTS 效果 | 记忆召回差 | M3 用 trigram/`unicode61` 双方案压测；不行就只做 M2 关键词匹配 |
| adk-go 依赖体积与升级节奏 | 编译时间、供应链审计 | 只在 `internal/agentrt/adk` 内 import；升级只影响该包 |
| Go 版本要求 | CI/开发机不一致 | 三处统一 1.27.x（§3.1）：`go.mod`、Dockerfile、CI |
| **图片链路静默丢图**：两套扩展名白名单（出站含 bmp、入站不含）、`https://` 引用放行、SDK 解密字节是否与 v1 一致 | 用户发了图，模型没看到 | P0/P4 各跑一次真实收图 + 发图；白名单按 §2.3 分别实现 |
| **现网 `FEISHU_*` 已配置但 v3.0 无飞书** | 若存在 `recipient.source = "feishu"`，切换后推送降级链里没有它，等于少一条投递路径 | 切换前检查 `schedules.json` 的 recipient 与 `contacts.json`，确认都落在 telegram/wechat；风险表与 release note 明写 |
| `run` 工具的 node 依赖（D-3） | 数据项目脚本失效或行为变化（node 20 → alpine 默认版本） | 默认镜像保留 `nodejs`，并固定主版本；或用 node 基础镜像 |
| 双实现并存期 | 修 bug 要改两遍 | 明确冻结窗口：P1–P5 期间 `main` 只修安全/数据问题 |

---

## 14. 待决策事项

| ID | 问题 | 选项 | 建议 |
|---|---|---|---|
| Q-1 | agent 引擎 | A：model 层 + 自研循环 / B：`llmagent` + `runner` / C：自写两个 wire protocol | **等 §3.3 的实验结果再定**：能接受 JSON 工具结果 → B（代码最少，事件/usage 现成，为后续持久会话预留接口）；要求提示词级对等 → C；两者都不满意才选 A |
| Q-8 | 外部依赖 pin 与回退 | pin 到 commit + P0 核对 / 不 pin | **pin**，并把 §3.8 作为 P0 出口条件；若 P0 发现符号或默认 API 不符，按 §3.8 最后一列回退 |
| Q-9 | v1 的"非缺陷但显眼"行为 | 照抄 / 修正 | **照抄**：去重表不清理（避免内存行为漂移）、英文错误文案、`preview` 不校验 UI 头、commit/push 按选中项目收窄。若要改，单独提 issue，不混进对等版 |
| Q-10 | 源目录 `.env` 回退 | 保留（按可执行文件目录）/ 去掉 | **去掉**（见 §2.1）：二进制没有源目录概念，误读部署目录的文件比少一级回退更危险 |
| Q-2 | `run` 是否保留 `.js/.mjs` | 保留（镜像装 node）/ 只留 python | **保留** |
| Q-3 | 监控分期节奏 | 只做一期 / 一期+二期一起 / 按 §10 三期走 | **先一期，二期在看板迁移后做** |
| Q-4 | 记忆路线 | 本期不做 / M1+M2 / 直接 M3 | **本期不做，v3.1 做 M1+M2** |
| Q-5 | 分支与发布 | `v3` 分支独立发布 / 直接替换 `main` | **`v3` 分支先跑通，再决定是否替换** |
| Q-6 | WebUI index.html | 原样复用 / 顺手重写 | **原样复用**（零 UI 风险） |
| Q-7 | 飞书时点 | 永不 / 下一期 / 待定 | **下一期**，实现时按 §2.6.1 的修正版契约 |

---

## 附录 A：v1 依赖与 Go 替代对照（实现时照抄）

| v1（npm） | 用途 | Go |
|---|---|---|
| `@earendil-works/pi-agent-core` | Agent 循环、工具类型 | 自研循环（§3.4）或方案 C（§3.6） |
| `@earendil-works/pi-ai` | provider、schema、usage、`calculateCost` | `adk-go` model 层（A/B）；成本恒 0；工具结果形态见 §3.3 |
| `grammy` | Telegram Bot API | `go-telegram-bot-api` 或直接 HTTP |
| `https-proxy-agent` | TG 代理 | `http.Transport.Proxy` 自定义函数：`TG_PROXY` 是**自定义键**（`ProxyFromEnvironment` 不认识），要手工注入并按 `TG_PROXY > HTTPS_PROXY > https_proxy > HTTP_PROXY > http_proxy` 取第一个 |
| `qrcode` / `qrcode-terminal` | WebUI 配对返回 **PNG data URL**（`qrcode.toDataURL`）；CLI 登录在终端打 **ASCII 二维码** | `skip2/go-qrcode`（PNG → base64 data URL）+ 终端 ASCII 两个出口都要保留 |
| `tsx` | 运行时 | 静态二进制 |
| 微信协议层（`api.ts` 186 + `aes-ecb.ts` 47 + `types.ts` 128 + `login.ts` 138 + `upload.ts` 158 + `download.ts` 116 = **773 行**） | iLink | 协议层 → `openilink-sdk-go` |
| 微信业务层（`monitor.ts` 373 + `pair.ts` 251 + `image-batch.ts` 144 + `account.ts` 132 + `send-media.ts` 104 + `reminder.ts` 98 + `context.ts` 84 + `index.ts` 54 = **1240 行**） | 白名单 / 批量 / 配对 / token 持久化 / 提醒 / markdown 剥离 / 落盘 | 全部自写（§4.2 逐条） |
| `node:test` | 测试 | `go test` + `testdata/fixtures` |
