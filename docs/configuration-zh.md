# 配置与参考手册

> 🌐 [English](configuration.md) · [返回 README](../README-ZH.md)

所有用户级配置与运行状态均存放在 `~/.botler-agent/`（可用 `BOTLER_CONFIG_DIR` 覆盖）。首次执行 `botler init` 会生成模板文件，且不会覆盖已有配置。

## 目录

- [配置目录结构与加载优先级](#配置目录结构与加载优先级)
- [自定义模型供应商（`providers.json`）](#自定义模型供应商providersjson)
- [自定义系统提示词（`system-prompt.md`）](#自定义系统提示词system-promptmd)
- [消息渠道（Telegram / 飞书 / 微信）](#消息渠道telegram--飞书--微信)
- [定时调度器（`schedules.json`）](#定时调度器schedulesjson)
- [WebUI 与健康度监控](#webui-与健康度监控)
- [任务生命周期与数据约定](#任务生命周期与数据约定)
- [环境变量全表](#环境变量全表)

---

## 配置目录结构与加载优先级

| 文件 | 作用 |
|------|------|
| `.env` | 用户级配置（`DATA_ROOT`、模型选择、渠道凭据、`GIT_PUSH`、`SCHEDULER_ENABLED`、`WEBUI_ENABLED`、`MONITOR_ENABLED` 等），权限 `600` |
| `providers.json` | 自定义模型供应商（OpenAI-completions / Anthropic-messages 网关）：每个 provider 含 `api` / `baseUrl` / `apiKey` / `models` |
| `system-prompt.md` | 外置系统提示词，支持 `__DATA_ROOT__` / `__PROJECTS__` / `__PROJECT_CONTEXT__` / `__TODAY__` 占位符 |
| `schedules.json` | 定时任务配置；可由聊天、WebUI 或手动编辑管理 |
| `contacts.json` | 每渠道已知地址存储（微信 `context_token` + 已记录地址），供推送使用 |
| `task-logs/` | 按日分片的 JSONL 任务日志（供 WebUI 读取） |
| `wechat/account.json` | 微信 iLink 登录凭据（切勿提交到版本库） |

配置加载优先级（从高到低）：

```text
进程环境变量  >  ~/.botler-agent/.env  >  源码目录 .env（开发回退）  >  内置默认值
```

可通过进程环境变量 `BOTLER_CONFIG_DIR` 覆盖 `~/.botler-agent` 的位置（适用于 Docker `/config` 挂载、测试或多套隔离配置）。

---

## 自定义模型供应商（`providers.json`）

自定义 OpenAI-completions / Anthropic-messages 兼容网关（自建 / 内部端点，如自建 OpenAI-completions 网关、火山方舟 Ark 编码端点）定义在 `~/.botler-agent/providers.json`（`botler init` 会生成模板）。当该文件缺失时，旧版 `CUSTOM_BASE_URL` / `CUSTOM_API_KEY` 环境变量仍可作为兜底。

`api` 字段按供应商选择通信协议：`"openai-completions"`（默认）或 `"anthropic-messages"`。

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
      "priority": 10,
      "model": "ark-code-latest",
      "models": [
        { "id": "ark-code-latest", "name": "…", "reasoning": false, "contextWindow": 524288, "maxTokens": 32768, "vision": false }
      ]
    }
  }
}
```

- **选择主模型**：在 `.env` 里将 `PI_PROVIDER` 设为某个 provider id、`PI_MODEL` 设为其中某个模型 id，然后重启（模型按进程缓存）。
- **内置 Anthropic**：设 `PI_PROVIDER=anthropic`，通过 `ANTHROPIC_API_KEY` 或 `ANTHROPIC_AUTH_TOKEN` 鉴权即可（无需配置 `providers.json`）。
- **新增供应商 / 模型**：直接编辑此文件，无需修改框架代码。格式有误的条目会被跳过并打印告警。
- **`api` 协议**：`"openai-completions"`（OpenAI Chat Completions，默认）或 `"anthropic-messages"`（Anthropic Messages API，会在 `baseUrl` 后追加 `/v1/messages`）。
- **多供应商自动 Fallback 链（可选）**：在备用 provider 条目上设置 `"priority"`（整数，数值越小优先级越高）和 `"model"`（备用模型 id，设置 `priority` 时必填），即可在主模型（`PI_PROVIDER` / `PI_MODEL`）失败时自动按优先级降级切换。设置 `"fallback": false` 可将该条目排除出备用链；未设置 `priority` 的条目不会参与自动切换。
- **模型字段**：
  - `id`：发送给网关的模型标识
  - `name`：显示名称
  - `reasoning`：是否支持推理思考
  - `contextWindow` / `maxTokens`：上下文窗口与最大输出 token 数
  - `vision`：是否接受图片输入（**省略时默认为 `true`，即默认开启识图**）。仅在纯文本模型上显式设置 `"vision": false`；切勿对多模态模型误设为 `false`，否则微信入站图片和视觉任务会被拒绝。

---

## 自定义系统提示词（`system-prompt.md`）

`botler init` 会生成 `~/.botler-agent/system-prompt.md`，每次任务执行时按需重读，修改无需重启：

1. 保留四个运行时占位符：`__DATA_ROOT__`、`__PROJECTS__`、`__PROJECT_CONTEXT__`、`__TODAY__`。
2. 可按个人习惯调整全局语气或公共规则，也可让 AI 助手直接生成，例如：

   > 我的 botler-agent 数据目录下有子项目 `notes/`（每日笔记）和 `vocab/`（生词本）。请写一份 `~/.botler-agent/system-prompt.md`，遵循「先 read 再 write、保持 JSON 合法、字段类型一致、写后中文汇报」，并保留 `__DATA_ROOT__` / `__PROJECTS__` / `__PROJECT_CONTEXT__` / `__TODAY__` 占位符。

> **安全说明**：目录白名单、受控工具集合、禁止任意 shell 等边界由代码强制执行，提示词内容无法突破工具边界。

---

## 消息渠道（Telegram / 飞书 / 微信）

- **Telegram**（长轮询）：配置最简单。需要 `TELEGRAM_BOT_TOKEN`；大陆网络环境下需配置 `TG_PROXY`（如 `http://127.0.0.1:7890`）。可设 `TELEGRAM_ALLOW_FROM`（逗号分隔的用户 id 或用户名）限制发送者，留空则全部放行。
- **飞书**（事件订阅 Webhook，支持加密）：需要 `FEISHU_APP_ID` / `FEISHU_APP_SECRET`；可选 `FEISHU_VERIFICATION_TOKEN` / `FEISHU_ENCRYPT_KEY`；默认监听 `FEISHU_PORT`（`3000`）。可用 `FEISHU_ALLOW_FROM`（逗号分隔的 `open_id`/`user_id`/`union_id` 或 chat id）限制发送者。
- **微信（iLink / ClawBot 官方 Bot API）**：私聊渠道。先运行 `botler wechat-login`（终端打印二维码，用微信扫码后凭据存入 `~/.botler-agent/wechat/account.json`），再设 `WECHAT_ENABLED=1`。
  - **白名单与续期提醒**：扫码主号始终允许；`WECHAT_ALLOW_FROM` 可额外放行其他 `ilink_user_id`。`WECHAT_REMINDER_HOURS`（默认 `23`）会在主号 24h `context_token` 窗口过期前主动提醒刷新。
  - **入站图片（视觉输入 + 批处理合并）**：微信发来的图片会解码作为视觉输入传给模型，并将原图持久化到 `DATA_ROOT/<project>/photos/`。由于微信「选图即发、文字后到」，图片会短暂暂存（`WECHAT_IMAGE_BATCH_SECONDS`，默认 `60` 秒；设 `0` 关闭），让随后补发的文字说明及多张图片合并为同一个任务执行。若当前模型不支持识图（`vision: false`），机器人会用中文提示补充文字说明。
  - **出站图片**：仅微信渠道会发送回复中引用的图片（`![](...)`，须位于 `DATA_ROOT` 允许路径内）；其他渠道会自动剥离图片 Markdown 仅发文本。
- **IM 渠道公共特性**：
  - **问候短路**：纯问候消息（如 `你好` / `hi`）跳过路由 LLM 调用，零成本返回中文欢迎语并列出当前可用子项目。
  - **最近对话记忆与重置**：IM 渠道共享受限的最近可见对话窗口（`CONVERSATION_CONTEXT_TURNS`，默认 `5` 轮）以支持简短追问；当用户明确要求开启新任务或重置上下文时，模型会调用 `clear_conversation_context` 清空窗口。

---

## 定时调度器（`schedules.json`）

设 `SCHEDULER_ENABLED=1` 启动进程内调度器。可通过聊天指令（`schedule` 工具）、WebUI 或直接编辑 `schedules.json` 管理定时任务，保存后会立即唤醒调度循环。

每个条目在 `cron` / `interval` / `at` / `once` 四种触发方式中**四选一**：

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

- **触发器类型**：
  - `cron`：5 字段表达式（`分 时 日 月 周`）。
  - `interval`：简单间隔如 `"5m"` / `"2h"` / `"1d"`（分钟粒度、对齐墙钟）。
  - `at`：每日固定时刻 `"HH:MM"`（基于 `timezone`）。
  - `once`：一次性绝对时间，ISO 8601 格式（如 `"2026-08-20T22:00:00+08:00"` 或 `"2026-08-20T14:00:00Z"`）。在该时刻**精确触发一次**后自动失效，且**不受 `silentHours` 免打扰限制**。
- **`timezone`**：IANA 时区，默认 `Asia/Shanghai`。
- **`message` 与 `project`**：到期时将 `message` 送入正常执行流水线；指定有效的 `project` 子项目名可跳过路由 LLM 调用。
- **`recipient` 与多渠道兜底推送**：设置后通过 `deliver()` 推送执行结果——优先主渠道，失败时按 `telegram → feishu → wechat` 顺序在已配置且有记录联系地址（`contacts.json`）的渠道上兜底。
- **`retry` 与 `silentHours`**：可选失败重试策略与免打扰时间窗（落入窗口内的触发会延后到窗口结束）。
- **`holidayMode: "workday"`（中国法定工作日门控）**：仅适用于 `cron` / `interval` / `at`（不可与 `once` 组合）。开启后**仅在中国法定工作日触发**——自动跳过法定节假日，并在调休补班日正常触发；此时忽略 cron 的日期字段，仅提取 `hour:minute` 时刻。节假日日历在启动时及每 24 小时从 `BOTLER_HOLIDAY_API_URL` 拉取并缓存至 `BOTLER_HOLIDAYS_FILE`，网络异常时保留缓存并平滑退化为普通周一至周五。
- **聊天路由**：创建/管理定时任务的消息（或含关键词 `定时` / `提醒` / `日程`）会路由到虚拟项目 `__scheduler__`；`schedule` 工具在所有执行上下文中均可用。

---

## WebUI 与健康度监控

- **WebUI**：设 `WEBUI_ENABLED=1` 启动任务日志界面，监听 `WEBUI_HOST:WEBUI_PORT`（宿主机默认 `127.0.0.1:8900`，Docker 镜像默认 `0.0.0.0:8900`）。读取 `task-logs/` 下的每日 JSONL 日志并提供监控面板，唯一的写操作是清理任务日志。
- **健康度监控**：默认监听 `127.0.0.1:MONITOR_PORT`（默认 `8899`，避开飞书 `3000` 与 WebUI `8900`），暴露 `/healthz`（JSON）、`/metrics`（Prometheus 文本）和 `/healthz/history`（趋势采样）。设 `MONITOR_ENABLED=0` 仅关闭独立采集端口，不影响 WebUI 内的监控视图。

---

## 任务生命周期与数据约定

1. **接收与去重**：渠道调用 `dispatch(text, { id, source, recipient })`；5 分钟内相同 `id` 的重复消息会被忽略（调度器触发的任务绕过去重）。
2. **串行队列**：所有任务在单队列中顺序执行，彻底避免并发写入损坏同一数据文件。
3. **两阶段运行（`路由 → 执行`）**：
   - *路由*：用轻量提示词（`子项目名 + AGENTS.md 前 300 字摘要`）判断目标子项目或 `__scheduler__`（单子项目直接跳过路由；意图模糊则直接回复请用户说明）。
   - *执行*：创建全新独立的 `Agent`，按需注入选中子项目的 `AGENTS.md`（或 `CLAUDE.md` / `CODEBUDDY.md`），受 `MAX_TOOL_TURNS` 轮次上限保护。
4. **写后校验与自愈**：写操作结束后校验所有数据 JSON 语法合法。若 `edit` 破坏了 JSON 语法，框架会拉起一次新 `Agent` 按自包含修复指令原地修正；仍不通过则返回错误且不提交。
5. **自动 Git 提交（及推送）**：`commitIfChanged()` 遍历并提交 `DATA_ROOT` 下有改动的独立 Git 子仓库；开启 `GIT_PUSH=1` 时会推送所有含未推送提交的子仓库（容器配置详见 [docker-git-push.md](docker-git-push.md)）。
6. **日志落盘**：无论成败，每次任务都会向 `task-logs/` 追加一条 JSONL 记录。

---

## 环境变量全表

可直接参考仓库根目录的模板文件 [`.env.example`](../.env.example)。

| 变量 | 说明 |
|------|------|
| `DATA_ROOT` | 被 Agent 操作的数据根目录（白名单为其一级非隐藏子目录） |
| `BOTLER_CONFIG_DIR` | 覆盖用户级配置目录（默认 `~/.botler-agent`） |
| `PI_PROVIDER` | 主模型供应商 id（`anthropic` 或 `providers.json` 中定义的 id） |
| `PI_MODEL` | 主模型 id（如 `claude-sonnet-4-5` 或 `your-model-flash`） |
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | 内置 Anthropic 鉴权（当 `PI_PROVIDER=anthropic` 时） |
| `CUSTOM_BASE_URL` / `CUSTOM_API_KEY` | `providers.json` 缺失时的旧版单网关兜底环境变量 |
| `LLM_MAX_RETRIES` | 单次 LLM 调用在同一供应商上的最大重试次数，含流式中断（`0` 关闭；默认 `2`） |
| `LLM_RETRY_BASE_MS` | 指数退避基础延迟毫秒数（`> 0`，默认 `1000`） |
| `LLM_RETRY_MAX_MS` | 单次重试退避延迟上限毫秒数（`> 0`，默认 `15000`） |
| `LLM_RETRY_BUDGET_SECONDS` | 单次调用跨供应商链的总重试时间预算秒数（`0` 不限时，默认 `0`；配置多供应商时建议 `120`） |
| `LLM_REQUEST_TIMEOUT_SECONDS` | 单次 HTTP 请求超时秒数（`0` 不超时，默认 `0`；建议 `600`） |
| `LLM_FALLBACK_ENABLED` | `=0` 禁用供应商链自动降级，仅使用 `PI_PROVIDER`（默认 `1`） |
| `LLM_RETRY_NOTIFY` | 微信 / Telegram 渠道上的重试提示级别：`fallback`（默认，仅切换供应商时提示）、`all` 或 `off` |
| `TELEGRAM_BOT_TOKEN` | Telegram BotFather token；留空则不启动 Telegram |
| `TG_PROXY` | Telegram API 代理地址（如 `http://127.0.0.1:7890`） |
| `TELEGRAM_ALLOW_FROM` | 可选逗号分隔的 Telegram 发送者白名单（用户 id 或用户名）；留空则全部放行 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 飞书应用凭据；留空则不启动飞书 |
| `FEISHU_VERIFICATION_TOKEN` | 飞书事件订阅验证 token（可选） |
| `FEISHU_ENCRYPT_KEY` | 飞书消息加密 key（留空则仅处理明文事件） |
| `FEISHU_PORT` | 飞书 Webhook 监听端口（默认 `3000`） |
| `FEISHU_ALLOW_FROM` | 可选逗号分隔的飞书发送者白名单（`open_id`/`user_id`/`union_id` 或 chat id）；留空则全部放行 |
| `WECHAT_ENABLED` | `=1` 启动微信 iLink 渠道（需先完成 `botler wechat-login`） |
| `WECHAT_ALLOW_FROM` | 可选逗号分隔的额外 `ilink_user_id` 白名单（主号始终允许） |
| `WECHAT_REMINDER_HOURS` | 微信续期提醒阈值：`0`=关闭，`1`–`24`=静默 N 小时后提醒（默认 `23`） |
| `WECHAT_IMAGE_BATCH_SECONDS` | 微信入站图片批处理窗口秒数（`0`=立即分发；默认 `60`） |
| `GIT_PUSH` | `=1` 时提交后自动 `git push`（默认 `0`；容器配置见 [docker-git-push.md](docker-git-push.md)） |
| `WEBUI_ENABLED` | `=1` 启动本地任务日志 WebUI |
| `WEBUI_HOST` | WebUI 绑定地址（宿主机默认 `127.0.0.1`，Docker 镜像默认 `0.0.0.0`） |
| `WEBUI_PORT` | WebUI 监听端口（默认 `8900`） |
| `BOTLER_LOG_DIR` | 任务日志 JSONL 目录（默认 `~/.botler-agent/task-logs/`） |
| `MONITOR_ENABLED` | `!=0` 启用独立健康/指标端口（默认 `1`；`=0` 仅关闭独立端口） |
| `MONITOR_PORT` | 健康/指标服务端口（默认 `8899`，避开 `3000` / `8900`） |
| `SCHEDULER_ENABLED` | `=1` 启动进程内定时调度器 |
| `BOTLER_SCHEDULES_FILE` | 调度配置文件路径（默认 `~/.botler-agent/schedules.json`） |
| `BOTLER_HOLIDAY_API_URL` | 中国法定节假日日历源 URL（支持 `{year}` 占位符） |
| `BOTLER_HOLIDAYS_FILE` | 缓存的节假日日历文件路径（默认 `~/.botler-agent/holidays.json`） |
| `MAX_TOOL_TURNS` | 单任务最大工具调用轮次（默认 `20`） |
| `CONVERSATION_CONTEXT_ENABLED` | `=0` 关闭最近对话上下文注入（默认 `1`） |
| `CONVERSATION_CONTEXT_TURNS` | 每个 IM 会话保留的最近可见轮数（`0` 关闭；默认 `5`） |
| `CONVERSATION_TURN_MAX_CHARS` | 单条存储消息最大字符数（默认 `4000`） |
| `CONVERSATION_CONTEXT_MAX_CHARS` | 注入上下文总字符数上限，超限优先丢弃最旧轮（默认 `12000`） |
