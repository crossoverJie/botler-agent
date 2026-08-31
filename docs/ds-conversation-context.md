# Botler 对话上下文方案：最近 N 轮滚动窗口

> 状态：已实现
> 原则：只保留最近 N 轮用户可见对话，不引入 TTL、摘要压缩、RAG 或长期记忆。上下文边界交给 LLM 判断。

## 1. 背景与问题

当前 Botler 每条消息都创建一个全新的 `Agent`，`initialState.messages` 为空，系统提示词也明确写着：

> 每条消息都是一次独立任务，没有跨任务记忆。

这导致 LLM 反问用户、等待二次确认、或需要用户补充信息时，用户不能只回复：

```text
好的
确认
改成 8 点
继续
刚才那条不对，改成 X
```

因为新的 Agent 不知道上一轮在做什么，用户必须重新复述完整上下文。

本方案只解决同一个会话内的短程连续交互，不追求 CodeBuddy/OpenCode 那样的完整上下文管理。

## 2. 核心设计

### 2.1 上下文单位：轮

一轮对话定义为一次用户可见的完整交换：

```text
用户消息 -> Agent 执行 -> Bot 最终回复
```

历史只保存这层可见内容，不保存 Agent 内部的 `thinking`、`toolCall`、`toolResult`。这些内部信息继续由 `task-logs` 记录，用于审计和排障，但不作为下一轮上下文。

### 2.2 滑动窗口

每个会话只保留最近 `N` 轮：

```text
最近第 N 轮
最近第 N-1 轮
...
最近第 1 轮
```

超过 `N` 轮后，最旧的一轮自然丢弃。窗口大小按“轮”而不是“消息条数”计算，避免拆散同一轮中的用户消息和 Bot 回复。

### 2.3 不设 TTL

不按时间过期上下文。允许用户昨天没有回复确认，今天再回复“确认”，Bot 仍能理解前文。

是否继续旧任务由 LLM 根据当前消息和历史共同判断：

```text
昨天：
Bot：是否确认写入昨天的午餐记录？

今天：
用户：确认
```

LLM 应判断为继续旧任务。

```text
昨天：
Bot：是否确认写入昨天的午餐记录？

今天：
用户：帮我记一下今天的早餐
```

LLM 应判断为新任务，按新任务处理。

如果判断错了，用户继续纠正即可；纠正内容也会进入最近 N 轮上下文，后续可以重新做对。

### 2.4 不引入复杂状态

不增加：

- 自动摘要。
- 上下文压缩。
- 上下文恢复。
- pending-confirmation 状态机。
- TTL。
- RAG 长期记忆。

框架只负责“读取最近历史、注入模型、写回本轮”。

## 3. 会话标识

Botler 面向个人使用，所有 IM 渠道共享同一个会话上下文，不再按渠道或用户拆分：

```text
im
```

规则：

- Telegram、Feishu、WeChat 的用户消息统一使用固定 key `im`。
- 共享上下文的假设是单用户、单使用人；跨渠道切换设备或入口时仍能延续最近对话。
- 如果未来需要多用户隔离，再把 key 拆回渠道和用户维度。
- Scheduler 触发的任务不继承用户聊天上下文。
- CLI 调试消息默认不启用会话上下文。
- Self-heal 内部修复不写入会话历史。

## 4. 存储设计

### 4.1 位置

存放在用户配置目录，不进入 `DATA_ROOT`：

```text
~/.botler-agent/conversations/im.json
```

固定文件名，不存在 `userId` 路径注入问题。

### 4.2 数据结构

```json
{
  "turns": [
    {
      "ts": 1787793138548,
      "project": "cook",
      "user": "确认写入午餐记录",
      "assistant": "已写入今天午餐记录：600 卡路里。"
    }
  ]
}
```

字段说明：

- `ts`：本轮开始时间。
- `project`：执行阶段最终路由到的项目名；如果本轮未识别出项目但产生了最终回复，则为 `null`。
- `user`：用户本轮输入文本。
- `assistant`：Bot 本轮最终回复文本。

每个 `user` 和 `assistant` 字段都做单条字符上限截断；注入模型时，整个最近对话块还受
`CONVERSATION_CONTEXT_MAX_CHARS` 总量上限约束，超限时从最旧轮开始丢弃。历史不保存
base64、图片摘要或图片引用路径；后续如需继续使用某张图片，由用户再次要求 Botler 识别。

### 4.3 写入与权限

- 写入采用原子替换或临时文件加 rename。
- 文件权限设为 `0600`。
- 当前全局 dispatch 队列已经串行化任务，因此单会话写入天然不会并发交错。

## 5. 运行时流程

```text
incoming message
  -> use fixed session key "im"
  -> load recent N turns
  -> greeting short-circuit
  -> routing with recent context
  -> execution with recent context
  -> final reply
  -> append this visible turn
```

### 5.1 路由阶段

路由阶段也加载最近 N 轮可见对话。

`buildRoutePrompt()` 增加一个可选的历史上下文块：

```text
最近对话：
用户：是否确认写入昨天的午餐记录？
Bot：请确认是否写入今天午餐记录。
```

路由模型需要同时看到历史，才能把当前消息“确认”路由到上一个任务所属的项目。

历史轮的 `project` 可能为 `null`，路由模型仍可通过其中的 `user` / `assistant` 内容判断上下文；如果仍无法确定，则输出 `UNKNOWN`。

路由提示需要明确允许模型区分新旧任务：

```text
如果当前消息是上一轮任务的简短确认或补充，优先沿用最近对话对应的项目；
如果当前消息明显是一个新的独立任务，忽略旧历史并按新任务判断；
无法确定时输出 UNKNOWN。
```

### 5.2 执行阶段

执行阶段把最近 N 轮可见对话作为上下文注入。

推荐通过系统提示词增加上下文块，而不是把完整 `AgentMessage[]` 重新构造回 `initialState.messages`：

```ts
const historyBlock = formatRecentTurns(recentTurns);
const systemPrompt = `${loadSystemPrompt(project)}\n\n${historyBlock}`;
```

这样可以避免恢复历史 assistant 消息时携带内部工具调用和 usage 元数据，实现更简单。

系统提示词需要从“没有跨任务记忆”改为：

```text
你拥有最近若干轮用户可见对话作为上下文。当前消息如果明显是新的独立任务，请忽略旧历史并按新任务处理；如果它是上一轮任务的确认、补充或纠正，请结合历史继续处理。无法确定时向用户确认。
```

### 5.3 写回时机

只要本轮正常执行并产生了最终 assistant 回复，就写入 `user` + `assistant`；`project` 可以为 `null`。

写回条件：

- `phase === "execute"`
- 存在最终 assistant 回复文本
- `project` 可为 `null`
- `TaskStatus` 属于 `success` / `auto-fixed` / `unknown-project`
- 不写入 `duplicate` / `validation-failed` / `error`
- `self-heal` 不读取、不写入

当前代码中的 `phase` 枚举只有 `execute | self-heal`；`aborted` / `timeout` 属于 `stopReason` 或工具轮次上限，不是 `phase`。这些没有最终 assistant 回复的情况由“存在最终 assistant 回复”条件排除。

查询任务虽然没有修改文件，但用户和 Bot 的问答仍然是有意义的上下文，因此也写入。

## 6. 代码改造点

### 6.1 新增会话存储模块

新增 `src/conversation/store.ts`：

```ts
export interface ConversationTurn {
  ts: number;
  project: string | null;
  user: string;
  assistant: string;
}

export function loadRecentTurns(
  sessionKey: string,
  maxTurns: number,
): ConversationTurn[];

export function appendTurn(
  sessionKey: string,
  turn: ConversationTurn,
  maxTurns: number,
): void;

export function clearSession(sessionKey: string): void;
```

### 6.2 扩展 `RunLogContext`

`src/runner.ts` 的 `RunLogContext` 增加：

```ts
sessionKey?: string;
```

### 6.3 渠道传递会话信息

Telegram、Feishu、WeChat 在调用 `dispatch()` 时传入同一个固定 key：

```ts
sessionKey: "im",
```

Scheduler 和 CLI 不传。

### 6.4 路由和执行注入

`routeProject()` 和 `runTask()` 接收最近轮次，分别拼入路由提示和执行系统提示。

## 7. 配置

新增配置：

```text
# 最近几轮用户可见对话会作为上下文注入
CONVERSATION_CONTEXT_ENABLED=1
CONVERSATION_CONTEXT_TURNS=5
CONVERSATION_TURN_MAX_CHARS=4000
CONVERSATION_CONTEXT_MAX_CHARS=12000
```

默认值：

```text
enabled = true
maxTurns = 5
maxCharsPerMessage = 4000
maxCharsPerContext = 12000
```

`CONVERSATION_TURN_MAX_CHARS` 是单条 `user` 或 `assistant` 消息上限，不是 N 轮总上限；先保持 4000，如果后续观察到正常回复被截断，再调高。

`CONVERSATION_CONTEXT_MAX_CHARS` 是注入上下文的总字符上限；当最近 N 轮超过该值时，从
最旧一轮开始丢弃，避免叠加项目约定文档后挤爆长系统提示词。

不新增 TTL 配置。

## 8. 边界场景

| 场景 | 处理 |
|------|------|
| 用户只回复“确认/好的” | 路由阶段看到历史，优先落到上一项目 |
| 用户开始明确的新任务 | 路由模型忽略旧历史，按新任务判断 |
| 模型误判 | 用户纠正，纠正消息进入上下文，后续可修正 |
| 历史超过 N 轮 | 最旧一轮滚动丢弃 |
| 昨天问、今天确认 | 没有 TTL，仍可继续 |
| greeting / 无数据子项目短路回复 | 不写入历史 |
| Scheduler 消息 | 不加载用户聊天上下文 |
| Self-heal | 不读取、不写入聊天上下文 |
| CLI 调试 | 默认不启用上下文 |
| 超长单轮回复 | 按字符上限截断 |

## 9. 测试范围

至少覆盖：

- 最近 N 轮加载顺序。
- 超过 N 轮后旧轮淘汰。
- 单条消息字符截断。
- 最近对话总量超过 `CONVERSATION_CONTEXT_MAX_CHARS` 时从最旧轮开始丢弃。
- `phase === "self-heal"` 不读取、不写入。
- `duplicate`、`validation-failed`、`error` 不写入。
- `unknown-project` 且存在最终 assistant 回复时写入，`project` 为 `null`。
- Scheduler/CLI 不加载历史。
- greeting / 无数据子项目短路回复不写入历史。
- 历史路由提示能包含旧任务信息。
- 多项目历史下，“确认”能路由到最近一个有 `project` 的轮。
- 上下文注入后的 token 增长不会导致长系统提示词被截断；必要时截断历史或降低 N。

## 10. 后续演进

当前阶段不实现，但后续如果出现长任务或跨很多轮确认需求，可以在此基础上逐步增加：

- 按项目过滤历史。
- 只保留“最近一次未完成问题”作为轻量 pending 状态。
- 使用 `pi-agent-core` 已有的 compaction 能力做摘要，而不是回到完整 CodeBuddy 架构。
