# LLM 调用重试与 provider 多路 fallback 设计（r4）

状态：**已实现**
r4 修正（评审意见）：**通知收口改为固定上界**。r3 一边承诺"不得增加总耗时"（§8 约束表），一边承认 `close()` 排空最多 `n × 3s`（§8.4），`all` 档三 provider 链下这是 9s——两条陈述不能同时成立。修法是给收口一个**与通知条数无关**的上界：`notifier` 自持一个 `base` ctx（脱离任务，因为提示是用户该收到的），`close()` 最多等 `notifyDrainTimeout`（2s），超时就 `cancel()` 打断在途请求并丢弃剩余队列，所以额外延迟恒 ≤ 2s，链再长也不变（§8、§8.3、§8.4、§12 Q-14）。该上界的前提是"每个 `Sender.Send` 都真的把入参 `ctx` 传给底层 HTTP 请求"，因此它与 r3 的接口改造**必须同一步发布**（§8.3、§10 P2b）。连带：`push.Notify` 不再自己 `WithoutCancel`（那会剥掉 `close()` 的取消，使打断失效），脱钩改由 `notifier.base` 承担（§8.1、§8.4）。
r3 修正（评审意见）：① `StreamError` 不再"默认可重试"，改为用 `Message` 走 §4.1 同一套分类，未知文案一律不重试（§4.3）；② 链层**始终存在**（单 provider 也包一层 `FallbackRuntime`），否则最常见的单 provider 配置永远没有 `give-up` 诊断、P3 的 `giveups` 计数也漏记（§3.2、§3.7、§10）；③ 通知改为**每任务一条 FIFO 发送队列**（单 goroutine 顺序发送），`WaitGroup` 只保证"发完"不保证"顺序"的问题消失，并用 `mu` + `closed` 把 `close()` 与 `observe()` 的入队做成原子，杜绝"任务结束后迟到的事件撞上 `send on closed channel`"（§8.3）；④ **删掉 `sendBounded`**，改为给 `push.Sender.Send` / `push.Deliver` 加 `ctx`，超时由 `context` 直接传播到 HTTP 请求，不再有"调用方不再等、goroutine 还在跑"的泄漏（§8.4、Q-12）；⑤ 移除 `botler_llm_attempts_total`——现有 `RetryEvent` 推导不出"每次 `Complete` 的结果"，为它加一条热路径事件不划算（§7.2、§9）。
r2 修正：新增 §8「用户可见的重试提示」（`RetryObserver` 走 `ctx` 注入、`push.Notify` 不降级、`notifier` 节流与 `wait()` 收口、`wechat/send.go` 的 `pushSendTimeout`），§0/§2.1/§3.3/§3.7/§5.4/§6.1/§7.2/§9/§10/§11/§12/§13 与附录 A 同步；原 §9–§12 顺延为 §10–§13。补 `ProviderChain()` / `envNonNegativeInt` / `backoff` / `sleep` / `logRetryEvent` / `sendBounded` / `newNotifier` 的实现；`FallbackRuntime` 删掉 `name` 字段，`Describe()` 改由不可变 `Chain` 纯函数推导；`RetryEvent.From` 对 `budget-exhausted` 报整条链。
r1：初稿。
范围：Go 实现（`internal/agentrt`、`internal/runner`、`internal/config`、`internal/monitor`）的模型调用链路，以及 §8 的渠道可见提示（`internal/dispatch`、`internal/push`、`internal/channels/wechat/send.go`）
不在本稿范围：渠道层重连（Telegram / WeChat 各自的 poll 循环）、scheduler 的任务级重试（`retryMax` / `retryBackoffMs`，与本文是两层，见 §1.5）、多 agent 编排、模型成本统计（`costUsd` 恒为 0 的口径不变）

---

## 0. 结论摘要

1. **现状是"零重试"**。应用层（`agentrt.Loop`、`agentrt/adk`、`runner.routeProject`）没有任何重试；唯一的重试是底层 SDK 自带的 `MaxRetries = 2`（anthropic-sdk-go / openai-go 默认值），而它**只覆盖建连阶段**——SSE 流已经开始输出后中途断流，SDK 不会再试，直接冒泡成 `error`，任务以 `⚠️ Task did not complete successfully: …` 收场。
2. **现状是"单 provider 硬绑"**。`providers.json` 可以定义任意多个 provider，但 `PI_PROVIDER` / `PI_MODEL` 只能激活一个；`runner.rtCached` 是进程级单例，`resolveRuntime()` 一旦成功就再也不会重建。当前 provider 挂了，整个进程的所有任务都挂。
3. **落点在 `agentrt.Runtime` 这个 seam**。`Complete(ctx, Request) (*Message, error)` 是一个极窄的接口，重试、provider fallback 都能用装饰器实现，**不必改 dispatch / scheduler / 日志结构**。
4. **重试必须落在 `Complete` 这一层，并把 SDK 内部重试关掉**。否则两层重试相乘（SDK 3 次 × 装饰器 3 次 = 9 次真实请求），既放大延迟也放大 429 压力。
5. **fallback 的真正缺口不是 thinking，而是"备用 provider 用哪个 model"**。跨 provider 重放历史这件事 adk-go 已经处理好了（`openaimodel` 丢弃 thought 块，§3.5），剥离 thinking 只是防御性措施；但 `PI_MODEL` 只描述主 provider，备用 provider 必须自带 `model`（§3.6），否则切过去必然 404。
6. **默认值建议**：`LLM_MAX_RETRIES` 默认 `2`（重试次数与现状 SDK 相同，但覆盖面从"仅建连"扩到"含流式断流"，这正是本方案的目的）、`LLM_REQUEST_TIMEOUT_SECONDS` 默认 `0`（= 现状不超时，见 §5.3 的取舍）、fallback **默认不生效**（只有显式给 provider 配了 `priority` 才会进链，见 §6.2）。因此**无需改任何配置**即可获得收益，且不会静默切模型。
7. **重试 / 切换 provider 要主动告知用户**（§8）。纯后台重试的体验是"发完消息后长时间沉默，然后突然出结果"；`dispatch` 又是串行队列，用户无法区分"在重试"和"进程挂了"。所以：换 provider **必须**提示（回答来自另一个模型，这是用户有权知道的事实），首次重试可选提示。提示在后台发出、搭在本来就存在的退避窗口上，`dispatch` 返回前统一收口以保证它先于最终回复到达；收口有**固定上界 `notifyDrainTimeout`（2s）**，与通知条数、provider 链长都无关（§8.4），且不改任何回复文案。

---

## 1. 现状（代码事实）

### 1.1 调用链

```
channel / scheduler / CLI
  → dispatch.Dispatch
      → runner.RunTask
          ├─ routeProject        → rt.Complete(...)   ← 无工具、无重试（runner.go:132）
          └─ agentrt.Loop.Run
                └─ l.Runtime.Complete(...)            ← 工具循环，无重试（loop.go:87）
                      └─ adk.Runtime.Complete         ← 强制流式（adk.go:194-219）
                            └─ openaimodel / anthropicmodel.GenerateContent
                                  └─ openai-go / anthropic-sdk-go（MaxRetries=2，仅建连）
```

`Runtime` 接口只有两个实现：`adk.Runtime`（真实）与 `fake.Runtime`（测试）。`runner.resolveRuntime()` 用包级变量 `rtCached` 缓存，`ResetRuntime()` 只给测试用。

### 1.2 重试现状

| 层 | 是否重试 | 说明 |
| --- | --- | --- |
| `agentrt.Loop` | **否** | `Complete` 返回 error 时直接构造 `StopReason: error` 的 assistant 消息并跳出循环（`loop.go:87-99`），一次失败 = 任务失败 |
| `runner.routeProject` | **否** | 失败只 `logWarn`，然后走"无法路由"的中文兜底（`runner.go:144`） |
| `agentrt/adk` | **否** | 流式迭代里 `err != nil` 直接 `return nil, err`（`adk.go:220-221`）；末尾 `errMessage != ""` 也直接返回 error（`adk.go:276-277`） |
| anthropic-sdk-go | **是，2 次** | `requestconfig.go:175` 默认 `MaxRetries: 2`；重试条件 = 无响应（连接错误）/ `x-should-retry` 头 / 408 / 409 / 429 / ≥500 |
| openai-go | **是，2 次** | `requestconfig.go:286` 默认 `MaxRetries: 2`；条件 = 可重放 body + 非确定性错误 + 同类状态码 |
| scheduler `retryMax` | 是，但**任务级** | 只对 `script` 类型的定时任务生效，重跑的是脚本而不是 LLM 调用 |

**关键盲区**：两路 SDK 的重试都发生在"拿到 `*http.Response` 之前"。adk-go 的两路 adapter 都是流式（`adk.go:219` 传 `true`），一旦 `200` 响应头到手、SSE 开始吐 chunk，后续的 `unexpected EOF` / `context deadline exceeded` / `stream ended without message_stop` **不会**被 SDK 重试，直接变成任务失败。这类断流在国内网关 / 代理链路上很常见。

### 1.3 provider 配置现状

| 事实 | 位置 |
| --- | --- |
| `providers.json` 支持 `providers.<id>.{api,baseUrl,apiKey,models[]}`，可定义多个 | `config.go:322` `LoadProvidersFile()` |
| 只有一个被激活：`PI_PROVIDER` / `PI_MODEL` | `config.go:162-163` |
| 解析结果缓存进全局单例，成功后不再重建 | `runtime.go:17-37` |
| 没有优先级、没有 fallback、没有健康度 | — |
| 未配置的 provider 只是"解析不到时列在 `Available models` 里" | `runtime.go:49-58` |

也就是说：`providers.json` 里写三个 provider 是"三选一"，不是"三级备份"。

### 1.4 与 v1 的对照

v1（TypeScript）的 `pi-ai` 里 `retryAssistantCall` 的 `maxRetries` 默认是 `0`，且 `openai-completions.js:195` / `anthropic-messages.js:431` 都显式给 SDK 传了 `maxRetries: 0` —— **v1 实际上一次都不重试**。Go 版现在的 2 次重试是"底层 SDK 默认值带来的意外收益"，不是设计出来的。

这带来一个 parity 判断：本方案默认保持 `LLM_MAX_RETRIES=2`（等于当前 Go 行为），因此**不构成新的 deviation**；若把默认改成 0 才需要记 deviation。

### 1.5 与 scheduler 任务级重试的关系

`Scheduler.retryMax` 重跑的是整个任务（`dispatch` 一次），LLM 重试发生在单次 `Complete` 内部。两层叠加的最坏情况是 `retryMax × (LLM_MAX_RETRIES+1)` 次模型调用。设计上要显式约束：**脚本任务的 `retryMax` 语义不变**（它明确写着"非幂等脚本保持 0"），LLM 重试只影响"这一次模型调用"，不重放任何工具执行（§3.4）。

---

## 2. 目标与非目标

### 2.1 目标

| # | 目标 |
| --- | --- |
| G-1 | 单次 LLM 调用失败（含流式中途断流）可以自动重试，重试次数 / 退避 / 总预算可配 |
| G-2 | 可配置 provider 优先级链，主 provider 不可用时自动切到下一个（每个备用自带自己的 model）。切换发生在运行期，**不需要人工重启进程**；但改 `providers.json` / `.env` 本身仍要重启才生效（`rtCached` 是进程级单例，§3.7） |
| G-3 | 重试与 fallback 都不改变工具执行语义：**已经执行过的工具不会被重放** |
| G-4 | 失败原因在 stderr 与 monitor 里可诊断（哪个 provider、第几次、什么错误、退避多久） |
| G-5 | 不引入新依赖；不改 `safePath`；不改回复抽取逻辑；不改 TaskLog 的 JSON 形状 |
| G-6 | 不配置任何新键时行为可预测：重试次数不变、**不会静默切到别的模型** |
| G-7 | 重试与切换 provider 对用户可见（§8）：用户在等待期间能看到"正在重试"或"已切换到备用模型"，而不是长时间沉默 |

### 2.2 非目标

| # | 不做 |
| --- | --- |
| N-1 | 不做"按用途选模型"（路由用便宜模型、执行用强模型）。`Runtime` 装饰器天然支持，但本期不接配置，避免一次改太多 |
| N-2 | 不做熔断器（circuit breaker）的状态机。理由：单进程、单用户、任务串行，失败重试的成本远低于实现与调试成本。若后续需要，见 §10 的 P3 |
| N-3 | 不做跨 provider 的 usage / cost 归因（`costUsd` 恒 0 的口径不变），只在指标里按 provider 维度计数 |
| N-4 | 不改 `scheduler` 的 `retryMax` / `retryBackoffMs` 语义 |
| N-5 | 不做 HTTP 层中间件式的重放（那会绕过工具循环的状态机，语义不可控） |

---

## 3. 设计：`agentrt.Runtime` 装饰器

### 3.1 为什么是 seam

`Runtime` 只有一个方法，且**无状态**（每次 `Complete` 都是一次自包含调用）。这让三件事都可以用同一种方式实现：

| 能力 | 装饰器 | 依据 |
| --- | --- | --- |
| 单次调用重试 | `RetryingRuntime` | 只需 `Complete` 的入参与返回值，不需要知道工具、路由、日志 |
| provider fallback | `FallbackRuntime` | 只需把同一个 `Request` 依次交给多个 `Runtime` |
| 按用途路由（本期不做） | `RoutedRuntime` | 按 `Request` 特征挑一个子 runtime |

`Loop` 与 `runner.routeProject` 都只持有 `agentrt.Runtime`，因此**装饰器对上层完全透明**，`dispatch` / `scheduler` / `logs` 一行都不用改。

### 3.2 组合顺序

```
resolveRuntime()
  └─ budgetRuntime{                      // §5.4：总预算（0 时不包）
         FallbackRuntime{ Chain: [       // 恒存在，哪怕链长为 1（见下）
             RetryingRuntime{ Inner: primary   },
             RetryingRuntime{ Inner: secondary },
             ...
         ] }
     }
```

顺序理由：**重试在内、fallback 在外、预算最外**。正确语义是"在主 provider 上把重试次数用尽，才认为它不可用，然后换下一个；总耗时由最外层的一个 deadline 兜住"。

反过来（`RetryingRuntime{ Inner: FallbackRuntime }`，即每轮重试都重跑整条链）看起来也能工作，但有两个毛病：

1. **每轮都要再付一次"已经挂掉的主 provider"的延迟**。主 provider 是卡死（等超时）而不是快速报错时，这个代价非常真实。
2. 日志/指标里"第几次尝试"失去意义，因为一次 attempt 里混了多个 provider。

所以采用**按 provider 顺序耗尽**：

```
func (f *FallbackRuntime) Complete(ctx context.Context, req Request) (*Message, error) {
    var lastErr error
    for i, rt := range f.Chain {
        if i > 0 { req = stripThinking(req) }         // §3.5：跨 provider 剥离 thinking
        reply, err := rt.Complete(ctx, req)           // rt 自带同 provider 重试
        if err == nil { return reply, nil }
        lastErr = err
        if _, tryNext := Classify(ctx, err); !tryNext {    // §4：换 provider 也没用
            // ctx 已结束的情况不报 give-up：那是预算/取消，由 budgetRuntime 发 "budget-exhausted"
            // 或干脆不发（Ctrl-C），两条一起发会让日志里出现两个"为什么停了"。
            if ctx.Err() == nil {
                notify(ctx, f.OnEvent, RetryEvent{Kind: "give-up", From: describe(rt), Err: err})
            }
            return nil, err
        }
        if i+1 < len(f.Chain) {
            notify(ctx, f.OnEvent, RetryEvent{Kind: "fallback", From: describe(rt), To: describe(f.Chain[i+1]), Err: err})
        }
    }
    notify(ctx, f.OnEvent, RetryEvent{Kind: "give-up", From: describe(f.Chain[len(f.Chain)-1]), Err: lastErr})
    return nil, lastErr
}
```

`give-up` 因此有两个来源，含义都是"这条链不再往下试了"：**链已耗尽**（上面最后一行）与**中途遇到不可换 provider 的错误**（`tryNext=false`，如 400）。两者的 `From` 都指向"最后失败的那个 provider"，所以日志里那句 `llm give-up at <provider>` 始终能读出"停在哪"。

**链长为 1 时也必须包 `FallbackRuntime`**（r3 修正）。r2 让 `buildRuntimeChain()` 在 `len(chain) == 1` 时直接返回 `chain[0]`（§3.7），结果是**最常见的那种配置**（只配一个 provider）永远收不到 `give-up`：`RetryingRuntime` 耗尽后只 `return nil, err`，没有任何事件，于是 P3 的 `botler_llm_giveups_total` 少记、stderr 也缺那句"停在哪个 provider"。两条修法里选了后者：

| 修法 | 问题 |
| --- | --- |
| 让 `RetryingRuntime` 在耗尽时发 `give-up` | 会和 `FallbackRuntime` 的链尾 `give-up` 重复（链长 > 1 时每次都要发两条）；且"是不是还有下一个"只有外层知道，内层发 `give-up` 语义上就是错的 |
| **链层恒存在**（采用） | 多一层接口调用（纳秒级）；`Describe()` 若不特殊处理会变成 `chain(x)`——已用下面那个纯函数退化解决，且让"事件由谁发"只有一个答案：**`give-up` 永远由链层发** |

代价是**多一层接口调用**（纳秒级）和 `Describe()` 的文案问题：若不处理，单 provider 时日志里会从 `custom/x` 变成 `chain(custom/x)`。所以 `FallbackRuntime.Describe()` 在链长为 1 时**退化为直接返回成员的 `Describe()`**（`Chain` 不可变，所以仍是纯函数）：

```go
func (f *FallbackRuntime) Describe() string {
    if len(f.Chain) == 1 { return describe(f.Chain[0]) }
    names := make([]string, len(f.Chain))
    for i, rt := range f.Chain { names[i] = describe(rt) }
    return "chain(" + strings.Join(names, ">") + ")"
}
```

`budgetRuntime.Describe()` 转发的就是它，所以 `budget-exhausted` 的 `From` 在单 provider 时也是 `custom/x`，与 `retry`/`give-up` 的行文一致。

最后那行 `give-up` 不需要再加 `ctx.Err()` 守卫：`ctx` 已结束的情况下 `Classify` 第 1 步就返回 `tryNext=false`，循环会在上面那个带守卫的分支里提前返回。所以走到链尾必然意味着"`ctx` 还活着，但所有 provider 都试过了"。

`Chain` 非空由装配侧保证（`buildRuntimeChain()` 至少有一个主 provider，否则 `buildOne` 已经把错误返回了），所以 `Complete` 里不做 `len(f.Chain) == 0` 的兜底——一个空链是装配 bug，让它 panic 在 `-race`/测试里比静默返回 `nil, nil` 更容易发现。

注意这段代码里**看不到 budget**：总预算不在这两层里各自计时，而是在整条链外面套一层 `context.WithTimeout`（`wrapBudget`，§5.4）。这样"预算耗尽"就等价于 `ctx.Err() != nil`，`Classify` 第 1 步会同时终止内层重试和外层换 provider。若两层各自维护 deadline，就会各算一遍，总时长变成 `Σ budget_i` 而不是一个上界。

`RetryingRuntime` 自己只关心 `retrySame`：

```
for attempt := 0; ; attempt++ {
    reply, err := r.Inner.Complete(ctx, req)
    if err == nil { return reply, nil }
    if retrySame, _ := Classify(ctx, err); !retrySame { return nil, err }
    if attempt == r.MaxRetries { return nil, err }     // 本 provider 用尽，交给外层 fallback
    d := r.backoff(attempt + 1)                        // §5.2，含抖动与 MaxDelay 封顶
    notify(ctx, r.OnEvent, RetryEvent{Kind: "retry", From: describe(r.Inner), Attempt: attempt + 1, MaxAttempts: r.MaxRetries, DelayMs: d.Milliseconds(), Err: err})
    if !r.sleep(ctx, d) { return nil, err }            // 退避被取消（含预算耗尽）：返回最后一次真实错误
}
```

这里也不做预算判断：预算耗尽通过 `ctx` 表达（§5.4），而 `Classify` 第 1 步已经会因 `ctx.Err() != nil` 直接返回 `retrySame=false`，所以"退避期间超预算"与"退避期间收到 Ctrl-C"走同一条路径（`sleep` 的 `false` 返回值只是兜住"恰好在 sleep 期间超时"这个窗口）。

**代价**：最坏情况总请求数 = `Σ(1 + maxRetries_i)`，3 个 provider、每个重试 2 次就是 9 次。所以 §5.4 的 `LLM_RETRY_BUDGET_SECONDS` 不是可选项的锦上添花，而是这个组合方式的必要配套。

但要注意预算**必须挂在最外层**：因为重试在内、fallback 在外，若把预算只放进 `RetryingRuntime`（内层），第一个 provider 的预算耗尽后外层照样会换下一个 provider 再来一轮，总时长是 `Σ budget_i` 而不是一个上界。实现上不新增字段，而是在整条链外面套 `wrapBudget`（§5.4）。

### 3.3 接口与新增类型

```go
// package agentrt

// RetryEvent describes one retry / fallback decision. agentrt stays free of monitor and
// logging dependencies: the caller (runner) wires the callback.
type RetryEvent struct {
	// Kind is one of the four plain strings below (plain strings, not an enum type, so a log line
	// prints it as-is): "retry" / "fallback" / "give-up" / "budget-exhausted".
	Kind string
	// From is the Describe() of the runtime that failed. For "budget-exhausted" it is the whole
	// chain (describe of budgetRuntime.inner): that event says "the deadline fired", not "this
	// provider failed", and the per-provider detail is already in the preceding events.
	From string
	To   string // Describe() of the runtime that will be tried next (empty for "retry")
	// Attempt is the 1-based RETRY index, not the call index: the very first retry reports
	// Attempt=1, and the call that just failed was call number Attempt (the loop counter itself
	// is 0-based). This is what makes "retry 1/2" and the user-facing "第 1/2 次" agree.
	Attempt int
	// MaxAttempts is the number of retries allowed (RetryingRuntime.MaxRetries), so the log reads
	// "retry Attempt/MaxAttempts". Both fields are 0 for "give-up" / "budget-exhausted": those
	// events come from FallbackRuntime / budgetRuntime, which do not track per-provider attempts.
	// Per-attempt detail is already in the "retry" events that preceded them (§7.1).
	MaxAttempts int
	// DelayMs is the backoff that will be slept before the next attempt ("retry"), or the budget
	// that just ran out ("budget-exhausted"). 0 for "fallback" / "give-up".
	DelayMs int64
	Err     error
}

// RetryingRuntime retries a failed Complete call on the same inner runtime.
type RetryingRuntime struct {
	Inner      Runtime
	MaxRetries int           // 0 = single attempt (no retry)
	BaseDelay  time.Duration // exponential backoff base
	MaxDelay   time.Duration
	OnEvent    func(RetryEvent)
	// sleepFn is the backoff primitive (nil = real timer); tests inject it to avoid waiting.
	// It reports whether the wait completed (false = ctx ended first).
	sleepFn func(ctx context.Context, d time.Duration) bool
}

// FallbackRuntime tries its members in order, each one with its own retry policy.
type FallbackRuntime struct {
	Chain   []Runtime // already wrapped in RetryingRuntime by the caller
	OnEvent func(RetryEvent)
	// No name field: Describe() derives the label from the immutable Chain (§3.3), so there is no
	// second source of truth and nothing to forget to set at construction time.
}

// budgetRuntime caps the wall-clock time of one Complete across the whole chain (§5.4).
type budgetRuntime struct {
	inner   Runtime
	d       time.Duration
	OnEvent func(RetryEvent)
}

// Describer is an optional interface so logs / warnings can name the provider that actually
// served the call. It does not change Runtime.
type Describer interface{ Describe() string }
```

`adk.Runtime` 加一个 `Describe() string` 返回 `provider/model`；`fake.Runtime` 返回 `"fake"`。`RetryingRuntime` 转发 `Describe()`。

`provider` 这个值 `adk.Runtime` 现在没有：它只存了 `modelName`。所以 `OpenAIConfig` / `AnthropicConfig` 各加一个 `Provider` 字段（由 `buildOne` 从 `ProviderTarget.Provider` 传入），`NewOpenAI` / `NewAnthropic` 把它存进 `Runtime`。这是纯增量字段，不影响现有调用点（`Provider` 为空时 `Describe()` 退化成只报 `model`）。

`FallbackRuntime.Describe()` 只能返回**静态**描述（如 `"chain(custom>ark)"`），不能返回"最近一次实际服务的 provider"——`rtCached` 是进程级单例、会被并发任务共享，记录"最近一次成功"就是一个无锁的可变字段（`go test -race` 会报）。真正"谁服务的这次调用"由 `RetryEvent` 携带（`From`/`To`），随事件写到 stderr（§7.1）和渠道提示（§8），而不是从 runtime 上读。

```go
// Describe is a pure function of the immutable Chain, so it is race-free by construction and
// needs no constructor-time field to remember. A one-element chain reports the member's own label
// (no "chain(x)" wrapper) so a single-provider setup logs exactly as it did before.
func (f *FallbackRuntime) Describe() string {
    if len(f.Chain) == 1 { return describe(f.Chain[0]) }
    names := make([]string, len(f.Chain))
    for i, rt := range f.Chain { names[i] = describe(rt) }
    return "chain(" + strings.Join(names, ">") + ")"
}
```

各代码片段里的 `describe(x)` 是一个 5 行小工具，`notify(ctx, fn, ev)` 是 §8.1 定义的扇出函数：

```go
// describe falls back to the Go type name for runtimes that do not implement Describer
// (test doubles, future implementations).
func describe(rt Runtime) string {
    if d, ok := rt.(Describer); ok { return d.Describe() }
    return fmt.Sprintf("%T", rt)
}
```

`notify` 不在这里定义，因为它同时承担"写 stderr"和"给当前任务发渠道提示"两件事，跨 §7 与 §8，完整实现见 §8.1。它的关键性质是**nil-safe**：`OnEvent` 为 nil 时不 panic，所以装饰器不需要在每个调用点判空。

### 3.4 为什么不重放工具

重试发生在**单次 `Complete` 内部**，此时 `Loop` 还没有执行任何工具（`loop.go:87` 拿到 reply 之后才走 `execTool`）。所以：

- 重试 N 次 = 最多 N 次模型调用，**0 次工具执行**
- 工具副作用（写文件、跑脚本、发消息）绝不会因为重试而重复
- 已积累的 `messages`（含此前的 tool 结果）会被完整重发，模型看到的是同一个上下文

这一点是整个方案安全性的基础，也是"重试放在 `Loop` 之外、`Complete` 之内"而不是"重跑整个 `Loop.Run`"的原因。

### 3.5 跨 provider 重放的历史

先说结论：**adk-go 已经替我们处理了大部分风险，所以这一节的工作量比想象的小，但有一条真实缺口必须补。**

已验证的事实：

| 方向 | adk-go 的行为 | 结论 |
| --- | --- | --- |
| Anthropic → OpenAI | `openaimodel/internal/completions/request.go:159` 的 `sendText := part.Text != "" && !part.Thought`，注释明确写 "Reasoning and signatures are dropped, as on the Responses path: Chat Completions has no field for either" | thought 块被静默丢弃，**不会 400** |
| OpenAI → Anthropic | `openaimodel` 从不产生 `Thought` part（`response.go` 只读 `ReasoningTokens` 计数），所以历史里本来就没有 thinking 块 | 无风险 |
| Anthropic → Anthropic（重试） | `anthropicmodel/request.go` 对无 signature 的 thought 直接 `continue`（"Strip unsigned thinking blocks when replaying history"），有 signature 则原样重放 | 无风险 |

所以 `stripThinking` 在今天是**防御性 no-op**：它的价值在于"adk-go 未来改了丢弃策略"或"新增第三种 wire protocol"时不至于突然 400，成本只有几行。仍建议实现，但**不必作为 P2 的阻塞项**。

**真正的缺口在别处**：`PI_MODEL` 只描述主 provider 的模型。切到备用 provider 时，用哪个 model id？§6.2 的 `model` 字段就是补这个洞的。

```go
func stripThinking(req Request) Request {
    out := req
    out.Messages = make([]Message, len(req.Messages))
    for i, m := range req.Messages {
        parts := make([]Part, 0, len(m.Parts))
        for _, p := range m.Parts {
            if p.Type == PartThinking { continue }
            parts = append(parts, p)
        }
        m.Parts = parts
        out.Messages[i] = m
    }
    return out
}
```

这条也解释了为什么 fallback 不能放在 `Loop` 外层"重跑整个任务"——那样虽然也解决了 thinking 问题，但会重放工具。

### 3.6 备用 provider 用哪个 model

这是 r1 漏掉的一个硬问题。`PI_MODEL` 只描述主 provider 的模型；备用 provider 的模型从哪来？三个选项：

| 方案 | 行为 | 评价 |
| --- | --- | --- |
| A. 复用 `PI_MODEL` | 把主 provider 的 model id 直接发给备用 provider | **错**。`claude-sonnet-4-5` 在 OpenAI 网关上不存在，等于必然 404 |
| B. 取备用 provider 的 `models[0]` | 每个 provider 声明多个模型时取第一个 | 可用但隐式：用户改 `models` 顺序就换了备用模型，且不一定是想要的那个 |
| C. 备用 provider 条目上显式写 `model` | `{"priority": 2, "model": "ark-code-latest", ...}` | **推荐** |

采用 C，并定两条规则：

1. 备用 provider 的 `model` **必填**（进了链就必须能唯一定位一个模型）。缺失 → 构建期 warn 并跳过该 provider（§6.3）
2. 主 provider 仍用 `PI_MODEL`，忽略自身条目的 `model` 字段（保持现有配置语义不变，避免两处来源冲突）

这样"链"的每一项都是 `(provider id, model id)` 的完整二元组，`buildOne` 的签名也自然变成 `buildOne(id, model string, c *config.Config)`。

顺带说清一个语义边界：**fallback 不改变 `PI_MODEL` 的对外可见性**。`logs` 的 `provider`/`model` 字段、`ModelName()`、`/healthz` 的 `modelCache` 仍然报配置值（v1 parity 要求）；"实际是哪个 provider 服务的"只出现在 stderr 诊断行与 P3 指标里。

### 3.7 `resolveRuntime` 的改造

```go
func buildRuntimeChain() (agentrt.Runtime, error) {
    c := config.C()

    // onEvent is the process-wide sink (§7.1 stderr + §7.2 monitor). The per-task channel notice
    // does not live here: it is installed per request through the context (§8.1).
    onEvent := func(ev agentrt.RetryEvent) { logRetryEvent(ev) }

    targets := c.ProviderChain()          // 见 §6.2，首个元素 = (PI_PROVIDER, PI_MODEL)
    chain := make([]agentrt.Runtime, 0, len(targets))
    for i, t := range targets {
        rt, err := buildOne(t.Provider, t.Model, c)
        if err != nil {
            if i == 0 {
                // 主 provider 的构建错误是配置错误，必须报出来：静默跑在备用模型上
                // 比直接失败更让人困惑（用户以为自己还在用 PI_MODEL）。
                return nil, err
            }
            logWarn(fmt.Sprintf("[runner] fallback provider %q unavailable: %v", t.Provider, err))   // 备用坏了只跳过
            continue
        }
        chain = append(chain, wrapRetrying(rt, c, onEvent))         // 每个成员自带同 provider 重试
    }

    // Always wrap, even with a single member: the chain layer is the ONLY place that emits
    // give-up (§3.2), so a one-provider setup still gets the diagnostic and the giveups metric.
    // Describe() degrades to the member's label when len == 1, so log lines are unchanged.
    rt := &agentrt.FallbackRuntime{Chain: chain, OnEvent: onEvent}
    return wrapBudget(rt, c.RetryBudget, onEvent), nil              // §5.4：预算套在最外层
}
```

`resolveRuntime()` 本身只改一行：把 `rt, err := buildRuntime()` 换成 `rt, err := buildRuntimeChain()`。缓存语义（**成功才缓存**，失败下次重解析并重新计一次 miss）与 `monitor.MarkModelCache` 的调用位置都不动——所以"改 `providers.json` 要重启"这条现状也不变（§2.1 G-2）。

要点：

1. **区分两类失败**：
   - **构建期**（`buildOne` 返回错误）= 配置写错了。主 provider 的构建错误必须立刻返回，否则用户会静默跑在备用模型上；备用 provider 的构建错误只 warn 并跳过。
   - **运行期**（`Complete` 返回错误）= 临时故障或凭据过期。交给 §4 的 `Classify` 决定是"同 provider 重试"还是"换下一个"。
2. **`rtCached` 语义不变**：仍然是"成功后缓存"，仍然是 `monitor.MarkModelCache` 的计数来源。`ModelCache` 指标口径不动（`botler_model_cache_queries_total` / `hits_total` 的 v1 名字不能改）。
3. `buildOne(id, model, c)` 抽取自现有 `buildRuntime()` 的 switch，**模型元数据校验（`Model not found` 文案）逐字保留**，因为它是用户可见的排错提示。`model` 由 `ProviderChain()` 给出：主 provider 取 `PI_MODEL`，备用取自身条目的 `model`（§3.6）。
4. `buildOne` 里把 `MaxRetries` 设为 **0**（关掉 SDK 重试，见 §5.1），并把 `RequestTimeout` 设为配置值（§5.3）。
5. `wrapRetrying` 在 `LLM_MAX_RETRIES == 0` 时直接返回 `rt`（不产生多余的一层装饰器），保持最小惊讶；`wrapBudget` 在 `LLM_RETRY_BUDGET_SECONDS == 0` 时同理。

   ```go
   func wrapRetrying(rt agentrt.Runtime, c *config.Config, onEvent func(agentrt.RetryEvent)) agentrt.Runtime {
       if c.MaxRetries <= 0 { return rt }
       return &agentrt.RetryingRuntime{
           Inner:      rt,
           MaxRetries: c.MaxRetries,
           BaseDelay:  c.RetryBaseDelay,
           MaxDelay:   c.RetryMaxDelay,
           OnEvent:    onEvent,
       }
   }
   ```
6. **`ProviderChain()` 的长度就是"用户愿意为容错付多少延迟"**：`LLM_FALLBACK_ENABLED=0` 或没有配 `priority` 时长度为 1，此时不会切任何 provider（行为与今天一致），但**仍然保留链层的 `give-up` 诊断**（§3.2 的 r3 修正）。

---

## 4. 错误分类

分类函数是纯函数，独立成 `internal/agentrt/retry.go`，便于表驱动测试：

```go
// Classify returns whether err is worth retrying on the same provider, and whether it is worth
// trying the next provider in the chain. ctx is the caller's context: a cancelled/deadlined
// caller must never be retried, while a per-request timeout (§5.3) must be.
func Classify(ctx context.Context, err error) (retrySame bool, tryNext bool)
```

`ctx` 是必需的：`LLM_REQUEST_TIMEOUT_SECONDS > 0` 时，单次请求超时同样表现为 `context.DeadlineExceeded`，但它是**可重试**的；而用户 Ctrl-C 造成的取消是**不可重试**的。两者只能靠"外层 ctx 是否已结束"区分。

### 4.1 判定顺序

| 步骤 | 判据 | retrySame | tryNext | 理由 |
| --- | --- | --- | --- | --- |
| 1 | `ctx.Err() != nil`（调用方的 ctx 已结束，含 §5.4 的预算耗尽） | 否 | 否 | 用户 Ctrl-C / 进程退出 / 预算用完，重试无意义。注意与 §5.3 的**单次请求超时**区分：后者是内层 `option.WithRequestTimeout` 产生的，外层 ctx 仍然活着，走第 7 步按可重试处理 |
| 2 | `errors.Is(err, anthropicmodel.ErrAuthentication)` 或 `*openai.Error` 的 `StatusCode ∈ {401,403}` | 否 | **是** | 这把 key 废了，换 provider 可能就好了 |
| 3 | `errors.Is(err, anthropicmodel.ErrBadRequest)` / `StatusCode == 400` / 文案含 `invalid_request`、`bad request`（§4.3：流式侧只有文案） | 否 | 否 | prompt / 参数问题，换 provider 大概率同样被拒 |
| 4 | 配额 / 计费类文案（`insufficient_quota`、`quota exceeded`、`billing`、`out of budget`、`usage limit`） | 否 | **是** | 同 pi-ai 的 `NON_RETRYABLE_PROVIDER_LIMIT_ERROR_PATTERN`：这是账户级限制，重试无用 |
| 5 | `errors.Is(err, anthropicmodel.ErrRateLimited)` / `ErrOverloaded` / `StatusCode ∈ {408,409,429}` / `>= 500` | 是 | 是 | 典型瞬时故障 |
| 6 | `context_length_exceeded` / `too many tokens` / `maximum context` | 否 | 否 | 换 provider 也一样超（不同模型窗口不同，理论上可换，但语义上应让用户看到真实原因） |
| 7 | 传输类文案（`unexpected EOF`、`connection reset`、`broken pipe`、`stream ended`、`timeout`、`EOF`、`i/o timeout`、`http2`） | 是 | 是 | **这是本方案要解决的核心盲区**：流式中途断流 |
| 8 | `*openai.Error` 且 `StatusCode == 0`（无 HTTP 响应） | 是 | 是 | 连接阶段失败 |
| 9 | 其它 | 否 | 否 | 保守：未知错误不重试，避免把确定性 bug 变成 N 倍延迟 |

### 4.2 为什么需要字符串兜底

`adk.go:220-221` 把 SDK 错误直接 `return nil, err`，`adk.go:276-277` 更狠：`fmt.Errorf("%s", errMessage)` —— **把 adk-go 的 `*genai.GenerateContentResponse` 里的 `ErrorMessage` 拍平成了裸字符串**，`errors.As` 再也拿不到结构。因此：

- Anthropic 侧的结构化 `*APIError`（含 `StatusCode` / `Type` / 哨兵）只有在**建连失败**时才保留完整链路
- 流式中途的错误事件（`resp.ErrorMessage`）只剩文本

所以 §4.1 的第 4/7 步必须靠正则文案匹配。建议直接移植 pi-ai 的两张表（`node_modules/@earendil-works/pi-ai/dist/utils/retry.js`），它们是在真实网关报错文案上迭代出来的：

| 表 | 作用 | 示例条目 |
| --- | --- | --- |
| `nonRetryableLimitPattern` | 账户级限制，重试无意义 | `insufficient_quota`、`quota exceeded`、`billing`、`available balance`、`out of budget` |
| `retryableTransportPattern` | 瞬时故障 | `overloaded`、`rate.?limit`、`too many requests`、`service.?unavailable`、`provider.?returned.?(?:an.?)?error`、`connection.?refused`、`socket hang up`、`ended without`、`stream ended before message_stop`、`timed? out`、`upstream.?connect` |

注意两张表的**判定顺序必须是"黑名单优先"**：`insufficient_quota` 的文案里通常也含 `rate limit`，先判白名单会误重试。

### 4.3 顺带修的一个小缺陷

`adk.go:276-277` 的 `fmt.Errorf("%s", errMessage)` 丢掉了 error 链，也让 `Classify` 失去结构信息。建议改为：

```go
return nil, &StreamError{Model: r.modelName, Message: errMessage}
```

`StreamError` 实现 `Error()`（文案逐字不变，仍是 `errMessage`）并 `Unwrap()` 返回 `nil`。它的作用**只是把"这是流式 error 事件"这个事实变成结构信息**，不改变分类结论：

```go
// StreamError is a failure reported by the provider inside the SSE stream. It exists so Classify
// can tell "the provider said no" apart from "we built a bad request", and it must NOT be treated
// as retryable by default: a stream event carrying 400 / invalid_request / insufficient_quota is
// exactly as permanent as the same status on the connect path.
type StreamError struct {
    Model   string
    Message string
}

func (e *StreamError) Error() string { return e.Message }
```

`Classify` 里对它的处理是**继续往下走 §4.1 的同一套规则**，只是把 `Message` 当作待匹配的文本：

```go
// A stream error has no status code and no sentinel, so its Message is the only evidence we have.
// Reuse the exact same ordered rules (blacklist before whitelist) instead of a separate path —
// that is what keeps "400 inside the stream" from being retried.
var se *StreamError
if errors.As(err, &se) {
    // fall through: the transport/limit regexes below match against se.Message
}
```

具体到 §4.1：第 4 步（配额黑名单）、第 6 步（上下文超长）、第 7 步（传输白名单）都改为匹配 `err.Error()`，对 `StreamError` 天然生效；因此：

| 流式事件里的文案 | 分类结果 |
| --- | --- |
| `invalid_request_error` / `messages: text content blocks must be non-empty` / `400` | 不重试、**不**换 provider（第 3 步语义，靠 `invalid_request` / `bad request` 文案扩展） |
| `insufficient_quota` / `billing` | 不重试、换 provider（第 4 步） |
| `context_length_exceeded` | 不重试、不换（第 6 步） |
| `overloaded` / `rate limit` / `stream ended before message_stop` / `unexpected EOF` | 重试（第 7 步） |
| 认不出来的文案 | **不重试、不换**（第 9 步"保守"） |

也就是说 §4.3 从"顺带修的一个小缺陷"降级为"顺带补的一点结构信息"：**它不新增任何重试能力**，只让日志里能区分"流式事件"与"我们自己的错误"（`%T` 可读），并让第 3/6 步的黑名单对 `Message` 也生效。原 r2 里"默认按可重试处理"的写法是错的——那会让一次 `invalid_request_error` 变成 N 次无效请求，还可能一路 fallback 到所有 provider。

为了覆盖流式侧的永久错误，第 3 步的判据要补两条文案（`invalid_request`、`bad request`），并在 §4.1 表格里注明它们与 `ErrBadRequest` 等价。


---

## 5. 退避与超时

### 5.1 关掉 SDK 内部重试

`adk.NewAnthropic` 传 `anthropicmodel.ClientConfig.MaxRetries = &zero`（该字段是 `*int`，`nil` 时默认 2）；`adk.NewOpenAI` 在 `ClientConfig.Options` 里追加 `option.WithMaxRetries(0)`（`openaimodel.go:95` 会把 `Options` append 到 client 选项之后，这是唯一入口）。

理由：

1. **避免乘数放大**：SDK 3 次 × 装饰器 3 次 = 9 次真实请求，退避还互相不可见
2. **SDK 重试对 SSE 中途断流无效**，留着只是"看起来有保护"
3. 统一由 `RetryingRuntime` 决策，日志/指标才数得准

代价：建连阶段的重试从"SDK 立刻重试"变成"装饰器退避后重试"，延迟略增（但这是对的：退避比立刻重打更能缓解 429）。

### 5.2 退避策略

| 项 | 值 | 说明 |
| --- | --- | --- |
| 算法 | 指数退避 + 抖动封顶 | `delay = min(min(base * 2^(attempt-1), max) * rand[0.5, 1.5), max)` |
| 默认 `base` | `1s` | 第 1 次重试约 0.5–1.5s |
| 默认 `max` | `15s` | 单次退避硬上限（抖动后再次 clamp 到 `max`） |
| 是否尊重 `Retry-After` | 否（本期） | 需要解析 `*http.Response`，但错误已经被拍平成字符串（§4.2）。列为 P3 |
| 可中断 | 是 | `select { case <-ctx.Done(): case <-time.After(d) }`，退避期间收到信号立刻返回（返回的是最后一次真实的上游错误，不是 `ctx.Err()`，见 §5.4） |

退避期间**必须**响应 `ctx.Done()`：`dispatch` 是串行队列，一个任务卡在 15s 的 sleep 里会拖住后面所有消息。`signal.NotifyContext` 的取消也要能穿透。

```go
// backoff returns the delay before retry number attempt (1-based), with jitter:
// base * 2^(attempt-1) capped at max, scaled by [0.5, 1.5), and clamped to max again.
func (r *RetryingRuntime) backoff(attempt int) time.Duration {
    d := r.BaseDelay << (attempt - 1)          // attempt is >= 1 and small; MaxDelay caps the rest
    if d <= 0 || d > r.MaxDelay { d = r.MaxDelay }
    jittered := time.Duration(float64(d) * (0.5 + rand.Float64()))
    if jittered > r.MaxDelay { return r.MaxDelay }
    return jittered
}

// sleep waits for d unless ctx ends first. It reports whether the wait completed, so the caller
// can return the LAST REAL upstream error instead of ctx.Err() (§5.4).
func (r *RetryingRuntime) sleep(ctx context.Context, d time.Duration) bool {
    if r.sleepFn != nil { return r.sleepFn(ctx, d) }   // test seam
    t := time.NewTimer(d)
    defer t.Stop()
    select {
    case <-t.C:
        return true
    case <-ctx.Done():
        return false
    }
}
```

`BaseDelay << (attempt-1)` 在 `attempt` 很大时会溢出，所以判断 `d <= 0` 也归到 `MaxDelay`——`LLM_MAX_RETRIES` 实际取值不会超过几位数，这只是让"配置写了个 60"不至于算出负数。

（`rand` 用 `math/rand/v2`，Go 1.22+ 无需 seed，也没有并发争用。）

### 5.3 单次请求超时

现状：`RequestTimeout` 为 `0`（不超时），`http.DefaultClient` 无 timeout，`ctx` 只来自 `signal.NotifyContext`。**一个卡死的网关可以让任务永久占住串行队列**。

建议新增 `LLM_REQUEST_TIMEOUT_SECONDS`：

| 值 | 行为 |
| --- | --- |
| `0`（默认，保持现状） | 不设超时 |
| `> 0` | 通过 `anthropicmodel.ClientConfig.RequestOptions` 追加 `option.WithRequestTimeout(d)`；通过 `openaimodel.ClientConfig.Options` 追加同一个选项 |

默认取 0 而不是取一个"合理值"，理由：

- 流式长回复 + 慢网关的真实耗时很难给出安全上界，误杀一次完整任务的代价高于偶发卡死
- 默认改行为会与"不改变现状"的总体原则冲突

但**文档与 `.env.example` 里必须写清楚"0 = 永不超时"这个风险**，并给出推荐值（如 `600`）。这一条标为待决策 Q-2。

### 5.4 总预算

`LLM_RETRY_BUDGET_SECONDS`（默认 `0` = 不设总预算）。

实现方式：**不改装饰器的字段，而是在整条链外面套一层 `context.WithTimeout`**，位置在 `buildRuntimeChain()` 的最终返回值上（即 `resolveRuntime()` 拿到的那个 `Runtime` 已经带预算）：

```go
func wrapBudget(rt agentrt.Runtime, d time.Duration, onEvent func(agentrt.RetryEvent)) agentrt.Runtime {
    if d <= 0 { return rt }
    return &budgetRuntime{inner: rt, d: d, OnEvent: onEvent}
}

func (b *budgetRuntime) Complete(ctx context.Context, req agentrt.Request) (*agentrt.Message, error) {
    ctx, cancel := context.WithTimeout(ctx, b.d)
    defer cancel()
    msg, err := b.inner.Complete(ctx, req)
    if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
        // The inner chain always reports the last real upstream error; ctx only decides whether
        // to keep trying. Emit so "why did it stop with providers left" is visible in the log.
        notify(ctx, b.OnEvent, agentrt.RetryEvent{Kind: "budget-exhausted", From: describe(b.inner), DelayMs: b.d.Milliseconds(), Err: err})
    }
    return msg, err
}

func (b *budgetRuntime) Describe() string { return describe(b.inner) }
```

这样设计的三个好处：

1. **一个上界就是真的上界**：无论链里有几个 provider、每个重试几次，总耗时都不超过 `d`（外加最后一次请求本身无法中断的部分，但 `ctx` 已取消，SDK 会尽快返回）。
2. **零额外状态**：不需要在 `RetryingRuntime` / `FallbackRuntime` 里各存一个 deadline 并各自判断，避免"内层和外层各算一遍 → 总时长 `Σ budget_i`"。
3. **与 `Classify` 天然配合**：预算耗尽 = `ctx.Err() == context.DeadlineExceeded`，§4.1 第 1 步会同时终止内层重试和外层换 provider，一行专门逻辑都不用写。

这里有一条配套约束，否则用户会看到 `context deadline exceeded` 这种没有信息量的错误：**两个装饰器都返回最后一次真实的上游错误，而不是 `ctx.Err()`**。即 `RetryingRuntime` 的退避被取消时返回 `err`（最后一次 `Inner.Complete` 的错误），而不是 `ctx.Err()`；`FallbackRuntime` 返回 `lastErr`。这样预算耗尽时用户看到的仍是 `anthropicmodel: api error (status 529)` 这类可诊断的信息，`ctx` 的结束状态只用来决定"还试不试"。

一个诚实的边界：如果**正在进行的那个请求**本身因为 `ctx` 取消而返回 `context.Canceled` / `context.DeadlineExceeded`（SDK 在流式读到一半被中断时通常如此），那 `lastErr` 就是这个 ctx 错误，此时"最后一次真实错误"就是它——这条路径无法补救，因为上游没来得及报错。区别在于：**已经在退避里等待**的情况能保住真实错误，**卡在请求里**的情况保不住。这也是 `LLM_REQUEST_TIMEOUT_SECONDS`（§5.3）值得开的原因：让卡住的请求自己先超时并留下一个有意义的错误，而不是等到总预算把整个链掐断。

于是 `budgetRuntime` 只做两件事：套 `ctx`、在预算耗尽时发一个事件，**不重写错误**。

§3.2 的"按 provider 顺序耗尽"让最坏请求数变成 `Σ(1 + maxRetries_i)`，所以这个预算**不是可选项**：它把"3 个 provider × 各重试 2 次 = 9 次"收敛成"budget 内能试几次就试几次"。默认 `0` 是保守选择（不改现状），但 `.env.example` 里应推荐一个值（如 `120`），并写明"多 provider 场景下不设预算可能让一条消息等好几分钟"。

---

## 6. 配置

### 6.1 `.env` 新增键

| 键 | 默认 | 含义 |
| --- | --- | --- |
| `LLM_MAX_RETRIES` | `2` | 单次 `Complete` 在**同一 provider** 上的最大重试次数。`0` = 不重试。次数与现状 SDK 相同，但覆盖面从"仅建连"扩到"含流式断流"（§5.1 会关掉 SDK 内部重试） |
| `LLM_RETRY_BASE_MS` | `1000` | 指数退避基数（正整数，`<= 0` 回落默认，避免 `0` 退化为零间隔重试风暴） |
| `LLM_RETRY_MAX_MS` | `15000` | 单次退避硬上限（正整数，`<= 0` 回落默认） |
| `LLM_RETRY_BUDGET_SECONDS` | `0` | 一次 `Complete` 的重试总预算；`0` = 不限 |
| `LLM_REQUEST_TIMEOUT_SECONDS` | `0` | 单次 HTTP 请求超时；`0` = 不超时（见 §5.3） |
| `LLM_FALLBACK_ENABLED` | `1` | 是否启用 provider 链 fallback。`0` = 只用 `PI_PROVIDER`（等价于现状）。注意：链里除了 `PI_PROVIDER` 还需要有显式配了 `priority` 的备用 provider，否则本键无实际作用（§6.2） |
| `LLM_RETRY_NOTIFY` | `fallback` | 重试提示的渠道可见性（§8.5）：`off` / `fallback` / `all`。默认只在换 provider 时提示 |

解析沿用现有风格（`envInt` + 非法值回落默认，参考 `parseMaxToolTurns`；`LLM_RETRY_NOTIFY` 是枚举，非法值回落 `fallback` 并 warn）：

- `LLM_MAX_RETRIES` / `LLM_RETRY_BUDGET_SECONDS` / `LLM_REQUEST_TIMEOUT_SECONDS` 走 `envNonNegativeInt`（`0` 表示关闭对应功能，负数回落默认）；`LLM_RETRY_BASE_MS` / `LLM_RETRY_MAX_MS` 走 `envPositiveInt`（`<= 0` 一律回落默认，防止误配 `0` 造成零退避重试风暴）
- `LLM_FALLBACK_ENABLED` 用 `os.Getenv("LLM_FALLBACK_ENABLED") != "0"`，**不能**照抄现有的 `== "1"`（`config.go:172-180` 的 `WECHAT_ENABLED` 等都是"默认关"，这里是"默认开"，用 `== "1"` 会让未配置时变成关闭）
- 注意 `LLM_MAX_RETRIES=0` 必须是"合法且生效"的值，不能被当成"未配置"而回落成 2（这是 §11.1 里专门列的用例）

```go
// RetryNotifyMode is how much of the retry process the user gets to see (§8.5).
type RetryNotifyMode string

const (
    RetryNotifyOff      RetryNotifyMode = "off"      // stderr only
    RetryNotifyFallback RetryNotifyMode = "fallback" // default: announce provider switches only
    RetryNotifyAll      RetryNotifyMode = "all"      // also announce the first retry per provider
)

func parseRetryNotify(v string) RetryNotifyMode {
    switch RetryNotifyMode(strings.ToLower(strings.TrimSpace(v))) {
    case RetryNotifyOff:    return RetryNotifyOff
    case RetryNotifyAll:    return RetryNotifyAll
    case RetryNotifyFallback, "": return RetryNotifyFallback
    default:
        // warnf already prefixes "[config] "; do not repeat it here.
        warnf("invalid LLM_RETRY_NOTIFY %q, falling back to %q", v, RetryNotifyFallback)
        return RetryNotifyFallback
    }
}
```

```go
// envNonNegativeInt: unset / unparsable / negative all fall back to def. 0 is a legal value
// (LLM_MAX_RETRIES=0 and LLM_RETRY_BUDGET_SECONDS=0 both mean "feature off", not "unset").
func envNonNegativeInt(key string, def int) int {
    n := envInt(key, def)
    if n < 0 { return def }
    return n
}

// envPositiveInt: unset / unparsable / <= 0 all fall back to def (used for LLM_RETRY_BASE_MS
// and LLM_RETRY_MAX_MS so 0 cannot degrade into a zero-delay retry storm).
func envPositiveInt(key string, def int) int {
    n := envInt(key, def)
    if n <= 0 { return def }
    return n
}
```

字段名与 §3.7 的用法对应：`MaxRetries` / `RetryBaseDelay`（`time.Duration`，由 `LLM_RETRY_BASE_MS` 转换）/ `RetryMaxDelay` / `RetryBudget`（`time.Duration`，由秒转换）/ `RequestTimeout` / `FallbackEnabled` / `RetryNotify`。

所有键都要同步进：

- `.env.example`（带注释，风格对齐现有段落）
- `README.md` 的环境变量表
- `internal/config/config_test.go` 的解析用例

### 6.2 `providers.json` 新增可选字段

```json
{
  "providers": {
    "custom": { "priority": 1, "api": "openai-completions", "...": "..." },
    "ark":    { "priority": 2, "model": "ark-code-latest", "api": "anthropic-messages", "...": "..." },
    "backup": { "priority": 3, "model": "gpt-4o-mini", "...": "..." }
  }
}
```

| 字段 | 类型 | 缺省 | 含义 |
| --- | --- | --- | --- |
| `priority` | int | 无（`null`） | 越小越优先。**只有显式配置了 `priority` 的 provider 才会进入 fallback 链**。Go 侧字段是 `*int`：`0` 是一个合法的优先级，必须与"没配"区分 |
| `model` | string | 无 | 作为备用时使用的 model id（§3.6）。**配了 `priority` 就必须配 `model`**，否则该 provider 被跳过并 warn |
| `fallback` | bool | `true` | 是否允许作为备用候选。`false` = 只在被 `PI_PROVIDER` 显式选中时使用 |

**链的构造规则**（`Config.ProviderChain() []ProviderTarget`，`ProviderTarget{Provider, Model string}`）：

1. 第 0 位**永远**是 `(PI_PROVIDER, PI_MODEL)`（显式选择优先；否则现有用户会突然跑在别的模型上）
2. 第 1..n 位 = `providers.json` 里**显式配了 `priority`**、有 `model`、且 `fallback != false` 的 provider，按 `priority` 升序；同 `priority` 按 id 字典序（保证确定性）；`PI_PROVIDER` 自身即使配了 `priority` 也不重复出现
3. **没有 `priority` 的 provider 不进链**。这是让"默认不切模型"成立的关键：老用户的 `providers.json` 里可能本来就有多个 provider（历史遗留、试验用），如果它们自动成为备用，就会出现"没改任何配置却突然跑在另一个模型上"——这比不 fallback 更糟
4. `LLM_FALLBACK_ENABLED=0` 时链长度截断为 1（等价于现状）
5. 内置 `anthropic`（`PI_PROVIDER=anthropic` + `ANTHROPIC_API_KEY`）**只能出现在第 0 位**：它没有 `providers.json` 条目，无处挂 `priority`，所以不作为备用候选。想要"Anthropic 备用"就把它写进 `providers.json` 并配 `priority` + `model`

于是链长 = 1 + （显式配了 `priority` 的备用数量），用户对"最坏延迟"有完全的控制权。

```go
// ProviderTarget is one fully-resolved (provider, model) pair: the fallback chain's unit.
type ProviderTarget struct {
    Provider string
    Model    string
}

// ProviderChain returns the ordered fallback chain. Element 0 is always the explicit choice
// (PI_PROVIDER / PI_MODEL); the rest are providers.json entries that opted in with `priority`.
func (c *Config) ProviderChain() []ProviderTarget {
    chain := []ProviderTarget{{Provider: c.Provider, Model: c.Model}}
    if !c.FallbackEnabled {
        return chain
    }
    var rest []CustomProvider
    for _, p := range c.CustomProviders {
        if p.ID == c.Provider || p.Priority == nil { continue }   // no opt-in, or already first
        if p.Fallback != nil && !*p.Fallback { continue }
        if p.Model == "" {
            warnf("provider %q has priority but no model, skipping fallback", p.ID)
            continue
        }
        rest = append(rest, p)
    }
    sort.SliceStable(rest, func(i, j int) bool {
        if *rest[i].Priority != *rest[j].Priority { return *rest[i].Priority < *rest[j].Priority }
        return rest[i].ID < rest[j].ID                            // deterministic tie-break
    })
    for _, p := range rest {
        chain = append(chain, ProviderTarget{Provider: p.ID, Model: p.Model})
    }
    return chain
}
```

`providersTemplate`（`internal/initcmd/init.go:33`）要同步加注释说明这三个字段，但**不要在模板里给默认值**——模板里出现 `"priority": 1` 会让用户以为必须填。

### 6.3 配置校验

| 情况 | 处置 |
| --- | --- |
| `PI_PROVIDER` 不在 `providers.json` 也不等于 `anthropic` | 保持现有错误文案（`Model not found: …` + `Available models: …`），**不**静默 fallback |
| 主 provider 构建失败（`PI_MODEL` 不存在 / `api` 无法识别） | 直接返回该错误，**不**降级到备用（见 §3.7 要点 1） |
| 备用 provider 的 `baseUrl`/`apiKey` 缺失 | `LoadProvidersFile` 现有逻辑会跳过并 warn，链里自然缺一位 |
| 备用 provider 配了 `priority` 但没配 `model` | warn 后跳过（§3.6），不猜 `models[0]` |
| 备用 provider 的 `model` 不在其 `models[]` 里 | `buildOne` 报错 → warn 后跳过，链继续 |
| 备用 provider 全部构建失败 | 链退化为长度 1，等价于现状（不报错） |

---

## 7. 日志与可观测性

### 7.1 TaskLog 不动

**不加任何顶层字段**。理由：

- `scripts/parity/main.go` 的 `normalizeLog` 用 `unionKeys(n1, n3)` 逐 key 比对，新增顶层 key 会让 v1 侧缺失 → parity FAIL
- `ignoredTop`（`parity/main.go:346`）只忽略 `id/taskId/startedAt/endedAt/durationMs/modelCache/systemPrompt`，加字段要动这个白名单，等于把 parity 的解释面变宽
- 重试信息属于"运行时诊断"，不是"任务结果"，本来就不该进任务日志

替代方案：`RetryEvent` → `logWarn`（stderr，现有 `runner.go:528` 的机制）。`From`/`To` 就是 `Describe()` 的 `provider/model`，所以一行日志里不需要再拆字段：

```
[runner] llm retry 1/2 provider=custom/your-model-pro delay=1240ms: anthropicmodel: api error (status 529) [type=overloaded_error]
[runner] llm fallback custom/your-model-pro -> ark/ark-code-latest: anthropicmodel: api error (status 529) [type=overloaded_error]
[runner] llm give-up at ark/ark-code-latest: openaimodel: stream ended before message_stop
[runner] llm budget-exhausted after 2m0s at chain(custom/your-model-pro>ark/ark-code-latest): context deadline exceeded
```

`logRetryEvent` 是 `runner` 里的 20 行 switch（`agentrt` 不能 import `monitor`，见 §7.2）：

```go
func logRetryEvent(ev agentrt.RetryEvent) {
    switch ev.Kind {
    case "retry":
        logWarn(fmt.Sprintf("[runner] llm retry %d/%d provider=%s delay=%dms: %v",
            ev.Attempt, ev.MaxAttempts, ev.From, ev.DelayMs, ev.Err))
    case "fallback":
        logWarn(fmt.Sprintf("[runner] llm fallback %s -> %s: %v", ev.From, ev.To, ev.Err))
    case "give-up":
        logWarn(fmt.Sprintf("[runner] llm give-up at %s: %v", ev.From, ev.Err))
    case "budget-exhausted":
        logWarn(fmt.Sprintf("[runner] llm budget-exhausted after %s at %s: %v",
            time.Duration(ev.DelayMs)*time.Millisecond, ev.From, ev.Err))
    }
    monitor.MarkLLMRetry(ev)   // P3，§7.2
}
```

`RetryEvent` 的 `Kind` 需要四个值：`retry` / `fallback` / `give-up` / `budget-exhausted`。最后一种由 `budgetRuntime` 发出（它在 `Complete` 返回后检查 `ctx.Err()`），否则"为什么只试了 2 个 provider 就放弃"在日志里看不出来。

注意 `give-up` 那行**不报总尝试次数**：`Attempt/MaxAttempts` 是 `RetryingRuntime` 的内部计数（§3.3），`FallbackRuntime` 并不累加各成员的尝试次数。想加就得让 `RetryingRuntime` 把次数回传给 `FallbackRuntime`，为一个诊断字段引入跨层状态不划算——前面每条 `retry` 行已经把次数打全了。

### 7.2 monitor 新增指标

**只新增，不改任何现有名字**（`health_test.go:43` 的 `TestMetricNamesMatchV1` 是硬约束）。

| 指标 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `botler_llm_retries_total` | counter | `provider`, `model` | 同 provider 重试次数（由 `retry` 事件驱动） |
| `botler_llm_fallbacks_total` | counter | `from`, `to` | 切 provider 次数（由 `fallback` 事件驱动） |
| `botler_llm_giveups_total` | counter | `provider`, `model` | 重试耗尽后放弃次数（由 `give-up` 事件驱动） |

**r3 修正：删掉原表中的 `botler_llm_attempts_total`**。它要求按"每次 `Complete` 的 `ok`/`retryable`/`fatal`"计数，但 `RetryEvent` 只覆盖 `retry`/`fallback`/`give-up`/`budget-exhausted` 四种**决策**：**首次就成功**没有任何事件，**每次失败的调用**也只有"要重试"那一次会发事件（最后一次失败直接走 `give-up`，不带 attempt 计数）。要补上就得二选一：

| 补法 | 代价 |
| --- | --- |
| 新增 `Kind: "attempt"` 事件，每次 `Complete` 都发 | 热路径（每次工具轮次都发一条）多一次分配 + 一次 fan-out；`attempts_total` 本质是 `retries + 成功次数`，而"成功次数"可以直接从 `TaskLog` 的 `status` 聚合，不值得为它给每个成功调用加事件 |
| 单独的观测钩子（不经 `RetryEvent`） | 要在 `RetryingRuntime` 里再加一个字段，且它不参与任何决策，纯为指标服务 |

本期结论：**三个计数器足够**。真正需要"每次调用结果"的排障场景，`TaskLog` 的 `status` + `durationMs` + `modelCache` 已经能回答；而"重试了几次 / 换了几个 provider / 放弃了多少次"才是本方案新增的信息。

`/healthz` 增加一个 `llm` 对象（`retries` / `fallbacks` / `giveups` / `lastError`）。`health_test.go:28-37` 的断言是"这些 key 必须存在"，新增 key 不会破坏它。


`agentrt` **不 import `monitor`**（保持 seam 无依赖）：`RetryingRuntime.OnEvent` 由 `runner` 接到 `monitor.MarkLLMRetry(...)`。这与现有 `monitor.MarkModelCache` 由 `runner.resolveRuntime` 调用的模式一致。

注意 `OnEvent` 与 §8 的渠道提示是**两个不同的消费者**：`OnEvent` 是进程级的（`buildRuntimeChain()` 装配时固定），渠道提示是任务级的（`ctx` 注入，§8.1）。§8.1 的 `notify` 把同一个 `RetryEvent` 扇出到两边，所以 monitor 计数与用户提示永远不会不一致。

### 7.3 WebUI

`internal/webui/index.html` 的指标卡片（`index.html:378`）本期不动。若要展示，等 P3。

---

## 8. 用户可见的重试提示

r1 只把 `RetryEvent` 写到 stderr（§7.1），用户侧完全无感知。这有两个实际体验问题：

1. **静默期太长**：`dispatch` 是串行队列（`acquire()`），一次「主 provider 卡住 → 重试 → 换 provider」可能持续几十秒。用户发完消息后只看到沉默，无法区分"在重试"和"进程挂了"。
2. **换了模型是用户有权知道的事实**：回答突然来自另一个模型（能力、风格、上下文窗口都不同），事后不告知等于隐瞒。

所以本节把 `RetryEvent` 接到渠道上。四条硬约束先摆出来：

| 约束 | 理由 |
| --- | --- |
| **额外延迟有固定上界（≤ 2s）** | 提示必须搭在**本来就存在的退避窗口**里发（§5.2，第 1 次退避 0.5–1.5s），而不是额外等待。实现上就是入队即返回 + `dispatch` 返回前收口，且收口本身有**与通知条数无关**的上界 `notifyDrainTimeout`（§8.3、§8.4）：最多 2s，链再长也不变 |
| **不得晚于最终回复到达** | 提示晚于答案到达就变成噪声。靠 `notifier.close()` 在 `dispatch` 返回前收口保证顺序，而不是靠"同步阻塞 runtime" |
| **不得乱序** | `retry` 必须先于它之后的 `fallback`。靠每任务一条 FIFO 队列 + 单消费者 goroutine 保证（§8.3），而不是"并发发、全部等完" |
| **不得污染 TaskLog / 最终回复文案** | `Result.Text` 与 TaskLog 形状不变（§7.1、§9），提示走独立通道 |

### 8.1 谁来发：两个 sink，两种生命周期

`RetryingRuntime.OnEvent` 是**进程级**的（`buildRuntimeChain()` 只在启动时跑一次，`rtCached` 是单例），它不知道"当前这条消息该回给谁"。而接收者 `Recipient` 是**任务级**的（`dispatch.Options.Recipient`）。

所以拆成两个 sink，由一个 `notify` 统一扇出：

```go
// notify fans one event out to the process-wide sink (stderr + monitor, §7) and, when present,
// to the request-scoped observer that dispatch installs for the current task. Both sinks are
// optional: a bare `go test` runtime with neither wired must not panic.
func notify(ctx context.Context, fn func(RetryEvent), ev RetryEvent) {
    if fn != nil { fn(ev) }
    if o := RetryObserverFrom(ctx); o != nil { o(ctx, ev) }
}
```

`agentrt` 侧新增（`retry.go`）：

```go
// RetryObserver receives retry events for ONE task. dispatch installs it through the context so
// the process-wide runtime chain can reach the current conversation without holding per-task
// state (a mutable field on the shared runtime would race under -race).
//
// ctx is the task context. The notifier does NOT hand it straight to the send: it derives its own
// base context in newNotifier (§8.3) so that "the user is owed this notice" survives the task's
// own cancellation, while close() can still abort a send that overruns its drain bound. See §8.4
// for why push.Notify must not re-derive WithoutCancel on top of that.
type RetryObserver func(ctx context.Context, ev RetryEvent)

type retryObserverKey struct{}

func WithRetryObserver(ctx context.Context, o RetryObserver) context.Context {
    return context.WithValue(ctx, retryObserverKey{}, o)
}

// RetryObserverFrom returns nil when no observer is installed (CLI, scheduler, self-heal, tests).
// Passing a nil observer to WithRetryObserver is therefore a valid way to disable one.
func RetryObserverFrom(ctx context.Context) RetryObserver {
    o, _ := ctx.Value(retryObserverKey{}).(RetryObserver)
    return o
}
```

用 `ctx` 而不是像 `tools.SetTaskContext` 那样的包级变量，原因就是 §3.3 已经踩过的那个坑：包级可变状态在 `-race` 下不成立，而 `ctx` 天然是每任务一份。

**改动点**：§3.2 与 §5.4 代码片段里的事件出口是 `notify(ctx, xxx.OnEvent, ev)` 而不是直接调 `xxx.OnEvent(ev)`。原因是同一个事件有两个消费者（进程级 stderr/monitor 与任务级渠道提示），`notify` 负责扇出并做 nil 判断；装饰器本身不需要知道有哪些消费者。

### 8.2 谁该收到：只在"有人在等"的场景发

| 场景 | `Source` | 有 `Recipient` | 是否提示 |
| --- | --- | --- | --- |
| 微信 / Telegram 入站消息 | `wechat` / `telegram` | 是 | **是**（用户在等） |
| CLI 调试 | `cli` | 否 | 否（stderr 已有，§7.1） |
| scheduler 定时任务 | `scheduler` | 目前**没有**（`scheduler_adapter.go` 只传 `ID`/`Source`/`ProjectHint`；`schedules.json` 的 `recipient` 由 engine 在推送最终结果时用，不经 `dispatch.Options`） | **否**。用户不在等，一条"正在重试"是纯噪声；任务成功与否由最终推送体现 |
| WebUI 触发 | 视实现 | — | 否（本期 WebUI 不跑 LLM 任务） |

判定放在 `dispatch` 里，注入点紧跟 `runner.RunTask` 之前：

```go
// dispatch.Dispatch — the gate (isInteractiveSource below). The notifier's shape and its
// throttle rules are §8.3.
var n *notifier
if r := opts.Recipient; r != nil && isInteractiveSource(source) {
    n = newNotifier(ctx, *r, config.C().RetryNotify)   // nil when mode == off
}
if n != nil {
    ctx = agentrt.WithRetryObserver(ctx, n.observe)
    defer n.close()   // drains the FIFO queue with a fixed bound: no notice may outlive the task (§8.3)
}
res, err := runner.RunTask(ctx, message, &runner.LogContext{...})
```

`ctx` 必须在 `newNotifier` 之前就已确定（`Dispatch` 里它是入参或早期 `context.WithTimeout` 的产物），因为 `base` 由它派生。若顺序反过来（先建 notifier 再改 ctx），`base` 会挂在一个与任务无关的 ctx 上，§8.3 表格第一行的"预算耗尽仍要发"就失效了。

```go
// isInteractiveSource reports whether a human is on the other end waiting for this reply.
// Hardcoded on purpose: "has a Recipient" is NOT the same question (schedules.json entries carry
// one too, they just do not reach dispatch.Options today).
func isInteractiveSource(src string) bool {
    return src == "wechat" || src == "telegram"
}
```

写死比"有 `Recipient` 就发"更安全：`schedules.json` 的条目本身就带 `recipient`（`store.go:280`），今天只是因为 adapter 没把它塞进 `Options` 才没进来——将来若有人顺手补上（很自然的一步），"有 Recipient 就发"就会让定时任务开始给用户发"正在重试"。

**self-heal 重试不装 observer**：`dispatch.go:211` 的第二次 `runner.RunTask` 用的是同一个 `ctx`，若不处理会继承 observer，导致自愈阶段再发一轮提示（`sent` 计数也已用掉）。做法是在 self-heal 分支显式用 `agentrt.WithRetryObserver(ctx, nil)` 重建 ctx —— `RetryObserverFrom` 返回 nil 时 `notify` 自然不发，语义正好。

注意 `defer n.close()` 仍会等在跑的那一条（自愈开始前的那条），这是对的：它是用户应该看到的那条。

### 8.3 发什么：措辞、频率、去重

通知器是**每任务一个实例**，节流状态天然是任务级的：

```go
type notifier struct {
    r       push.Recipient
    mode    config.RetryNotifyMode // "fallback" | "all" ("off" never installs a notifier)
    retried map[string]bool        // provider ("From") whose retry was already announced
    sent    int                    // notices announced
    closed  bool                   // set by close(); guards against a late event after the task ended
    mu      sync.Mutex             // guards retried/sent/closed AND the queue send (see observe)

    // base is the context every notice is sent under. It is detached from the task ctx on
    // purpose: a notice is something the user is already owed, so the task's own cancellation
    // (budget exhausted, Ctrl-C) must not swallow it. close() cancels it when the drain bound
    // expires, which is what actually aborts an in-flight HTTP request (r4).
    base   context.Context
    cancel context.CancelFunc

    // queue is the per-task FIFO. ONE goroutine drains it, so notices reach the user in the order
    // the events happened — a WaitGroup would only tell us "all done", not "in order" (r3 fix).
    queue chan notice
    done  chan struct{}  // closed by sender() when the queue is drained and closed
}

type notice struct {
    text string
}

// maxNoticesPerTask is a hard stop, not a policy: `fallback` can only fire len(chain)-1 times,
// so this only guards against a pathological chain or a future event kind. It doubles as the
// queue capacity, which is what makes the send in observe() non-blocking.
const maxNoticesPerTask = 3

// notifyDrainTimeout bounds how long close() may delay the final reply. It is a FIXED bound, not
// maxNoticesPerTask × per-notice timeout: the whole point is that the extra latency does not grow
// with the number of notices or the length of the provider chain (r4 fix).
//
// 2s is chosen so that the common case (a healthy channel, one text send of tens of ms) always
// finishes well inside it, while a sick channel cannot hold the reply hostage for long. It is
// deliberately smaller than notifyTimeout (3s): the drain bound, not the per-send bound, is what
// the user actually feels.
const notifyDrainTimeout = 2 * time.Second

// newNotifier returns nil for RetryNotifyOff so dispatch installs no observer at all
// (no ctx value, no goroutine, no allocation on the hot path). Otherwise it starts the one
// sender goroutine that owns this task's notice ordering.
func newNotifier(ctx context.Context, r push.Recipient, mode config.RetryNotifyMode) *notifier {
    if mode == config.RetryNotifyOff { return nil }
    base, cancel := context.WithCancel(context.WithoutCancel(ctx))
    n := &notifier{
        r:       r,
        mode:    mode,
        retried: map[string]bool{},
        base:    base,
        cancel:  cancel,
        queue:   make(chan notice, maxNoticesPerTask),
        done:    make(chan struct{}),
    }
    go n.sender()
    return n
}

// sender is the single consumer: strictly FIFO, one notice at a time. A slow channel therefore
// delays later notices instead of letting them overtake — which is exactly the guarantee we want
// ("retrying…" must never arrive after "switched to backup…").
func (n *notifier) sender() {
    defer close(n.done)
    for m := range n.queue {
        // close() gave up on the drain and cancelled base: drop the rest silently instead of
        // logging one "context canceled" line per remaining notice.
        if n.base.Err() != nil { break }
        if err := push.Notify(n.base, n.r, m.text); err != nil {   // best-effort
            // The in-flight send was the one close() cancelled: that is expected, and close()
            // already logged the "dropped" line, so do not add a confusing "failed" line here.
            if n.base.Err() != nil { break }
            fmt.Fprintf(os.Stderr, "[dispatch] retry notice failed: %v\n", err)
        }
    }
}

// observe never blocks the retry: it only hands the text to the sender goroutine, so the send
// overlaps the backoff sleep that follows this event (§8.4). Ordering is the queue's job.
//
// The mutex is held across render + send so close() can never close the channel underneath us
// (a late event would otherwise panic with "send on closed channel"). Holding it is safe because
// the send itself is non-blocking: renderLocked() caps notices at maxNoticesPerTask, which is
// exactly the queue capacity, so at most maxNoticesPerTask sends can ever happen and the buffer
// can never be full.
//
// The ctx parameter is unused on purpose: it exists because RetryObserver's signature carries it,
// and the actual send context is n.base (r4). Keeping the signature uniform is worth more than
// dropping the parameter — a per-event ctx here is exactly what r3 got wrong.
func (n *notifier) observe(_ context.Context, ev agentrt.RetryEvent) {
    n.mu.Lock()
    defer n.mu.Unlock()
    if n.closed { return }                     // the task is over; a late event is dropped silently
    text := n.renderLocked(ev)
    if text == "" { return }
    n.queue <- notice{text: text}
}

// close stops accepting notices, waits at most notifyDrainTimeout for the queue to drain, and
// returns. dispatch calls it with defer, so no notice can outlive the task and none can arrive
// after the final reply.
//
// The bound is what keeps §8's latency promise true: waiting for the full queue would cost
// n × notifyTimeout (3 notices × 3s = 9s in `all` mode with three providers), which grows with the
// chain. Here the cost is a constant.
//
// On timeout n.cancel() aborts the in-flight send. Note that `range n.queue` still walks the
// remaining buffered items after the channel is closed, but sender() breaks out on the first
// n.base.Err() != nil, so the second <-n.done below is still prompt — the remaining notices are
// "dropped" in the sense that matters (they never reach the user), not by being unlinked from the
// channel.
func (n *notifier) close() {
    n.mu.Lock()
    if !n.closed { n.closed = true; close(n.queue) }
    n.mu.Unlock()

    select {
    case <-n.done:                             // drained in time: every notice went out, in order
    case <-time.After(notifyDrainTimeout):
        n.cancel()                             // aborts the in-flight send and every queued one
        <-n.done                               // sender drains the rest fast: ctx is cancelled
        fmt.Fprintf(os.Stderr, "[dispatch] retry notices dropped: drain exceeded %s\n", notifyDrainTimeout)
    }
    n.cancel()                                 // idempotent; releases base either way
}

// renderLocked must be called with mu held.
func (n *notifier) renderLocked(ev agentrt.RetryEvent) string {
    // The cap is checked FIRST: marking `retried[ev.From]` and only then discovering we are over
    // the cap would record a notice that never went out, and a later event from the same provider
    // would be silently dropped for the wrong reason.
    if n.sent >= maxNoticesPerTask { return "" }
    switch ev.Kind {
    case "retry":
        if n.mode != config.RetryNotifyAll { return "" }
        if n.retried[ev.From] { return "" }    // one notice per provider, not per attempt
        n.retried[ev.From] = true
    case "fallback":
        // every fallback is announced (§8.3 表格): it is the fact the user has a right to know
    default: // "give-up" / "budget-exhausted"
        return ""
    }
    n.sent++
    if ev.Kind == "retry" {
        return fmt.Sprintf("⏳ 模型响应异常，正在重试（第 %d/%d 次），请稍候…", ev.Attempt, ev.MaxAttempts)
    }
    return fmt.Sprintf("🔄 主模型不可用，已切换到备用模型（%s），请稍候…", ev.To)
}
```

`renderLocked` 里的状态（`retried` / `sent`）只会被 `observe` 碰到，而 `observe` 由 runtime 在 `dispatch` 的串行队列里调用，天然不会并发——`sender` 只读 `queue`，不读这三个字段。所以这把锁**今天**没有争用；但它不是可选的装饰，而是承载了两个正确性职责：

1. **`closed` 与 `close(n.queue)` 的原子性**：`close()` 在 `mu` 内关闭 channel，`observe` 在 `mu` 内检查 `closed` 再发送。少了这把锁，self-heal 或将来任何"任务结束后仍有事件"的路径就会撞上 `send on closed channel` 的 panic——而 `dispatch` 的 panic recover 会把它变成一个失败的任务。
2. **上限与队列容量的一致性**：`renderLocked` 保证最多 `maxNoticesPerTask` 次 `sent++`，而队列容量正是这个值，所以持锁发送**永远不会阻塞**，不会把渠道延迟传染给重试循环。

**这也是本设计里唯一需要锁的地方**，比给整个 runtime 加可变字段干净得多。

**为什么 `base` 必须是 `WithoutCancel(ctx)` + 自己的 `cancel`（r4）**：这两个需求是相反的，只有一个 ctx 无法同时满足。

| 需求 | 用任务 ctx 直接发 | 用 `context.Background()` 发 | `WithoutCancel(ctx)` + 自持 `cancel` |
| --- | --- | --- | --- |
| 任务预算耗尽时提示仍要发出（用户该收到） | ✗ 被一起取消 | ✓ | ✓ |
| `close()` 超时后能打断在途请求 | ✓ | ✗ 无法取消，`close()` 只能干等 | ✓ |
| 进程退出时能尽快收敛 | ✓ | ✗ | ✓（`dispatch` 的 `defer` 一定会走到 `cancel`） |

`WithoutCancel` 只去掉父 ctx 的**取消信号与 deadline**，值仍然继承；`cancel` 由 `notifier` 自己持有，所以"保住该发的"和"能打断超时的"不再互斥。这也是 §8.4 里 `push.Notify` **不能**再叠一层 `WithoutCancel` 的原因——那会把 `close()` 的 `cancel()` 一起剥掉，打断立刻失效，回退到 r3 的"干等 n × 3s"。

`n.cancel()` 调用两次是安全的（`context.CancelFunc` 幂等），所以 `close()` 末尾那次无条件调用只是保证"无论走哪条分支都释放 base"，不需要额外标志。

**这个 2s 上界有一个前提，必须写出来**：它成立于"所有 `Sender.Send` 都真的尊重 `ctx`"。`close()` 超时后先 `cancel()` 再 `<-n.done`，若某个 `Send` 实现忽略了 `ctx`（自建 `context.Background()`、或在 DNS/建连阶段不检查取消），那个 `<-n.done` 就会退化成"等到底"，2s 上界立刻失效、回退到 r3 的形态。所以 **r3 的接口改造（`Sender.Send` 加 `ctx`）是 r4 上界的前置条件，而不是可以分开发布的两件事**——§10 里 P2b 把两者放在同一步，原因就在这里。落地时还要守住一条纪律：`Send` 的每个实现都必须把入参 `ctx` 传给底层 HTTP 请求（`context.WithTimeout(ctx, ...)` 派生，而不是另起一个根 ctx），否则这个上界会静默失效。§11.1 的 `notify_test.go` 用"永久阻塞的 fake sender"测的正是这条纪律的调用方一侧；实现一侧靠 code review 与 §8.4 的改动面表格兜住。

`dispatch` 侧收口的完整形态就是 §8.2 的那段代码（`defer n.close()` 与注入写在同一个 `if` 块里）。之所以用 `defer` 而不是在每个返回点各写一次，是因为 `Dispatch` 有十几个提前返回的分支。

**顺序保证（两层）**：

1. **提示之间**：单消费者 FIFO（`sender` 顺序处理 `queue`），所以 `retry` 一定先于它之后的 `fallback` 到达。r2 用"每条通知一个 goroutine + `WaitGroup`"只能保证"全部发完"，慢渠道下 `fallback` 可能反超 `retry`——那是真实的 UX 缺陷，因为用户会先看到"已切换"再看到"正在重试"。
2. **提示先于最终回复**：`runner.RunTask` 返回 → `defer n.close()` → `Dispatch` 返回 → 渠道发回复。`defer` 也覆盖了 §8.2 提到的 self-heal 分支与 `dispatch.go:163` 的 panic recover。注意这条保证在**超时分支**下依然成立：`close()` 宁可丢弃剩余通知，也不会提前返回让回复超车。

代价是**串行 + 一个固定上界**：`sender` 一条一条发，所以最坏情况是"每条都慢"；但 `close()` 只在 `notifyDrainTimeout`（2s）内等，超时就把在途请求 `cancel()` 掉、丢弃剩余队列。于是**额外延迟恒 ≤ 2s，与通知条数、链长无关**（r4）。r3 写的是 `n × notifyTimeout`（3 条 × 3s = 9s），那个上界会随链长增长，与 §8 约束表第一行直接矛盾。

实际几乎走不到这个上界：正常渠道一条文本发送是几十到几百毫秒，2s 足够发完 3 条；只有渠道已经病态（TCP 半开、网关 hang）时才会触发丢弃，而那正是"宁可少一条提示、也不要拖住回复"的场景。丢弃时会留一行 stderr，可观测。

一个细节：`newNotifier` 返回的实例要能同时被 `observe` 和 `close` 用，所以它由 `dispatch` 局部持有即可，**不需要**从 ctx 里再取回来（`notifierFrom` 那样做只会引入一个额外的 ctx key）。

四种事件的取舍：

| `Kind` | 是否提示 | 文案（中文，用户可见，符合项目"聊天文案可用中文"的例外） |
| --- | --- | --- |
| `retry` | 仅 `mode=all`，且同一 provider **只提示第一次** | `⏳ 模型响应异常，正在重试（第 1/2 次），请稍候…` |
| `fallback` | **每次都提示**（这是本节的核心价值） | `🔄 主模型不可用，已切换到备用模型（ark/ark-code-latest），请稍候…` |
| `give-up` | 否 | 最终回复里的 `⚠️ Task did not complete successfully: …` 已经说明，再发一条是重复 |
| `budget-exhausted` | 否 | 同上 |

`retry` 按 provider 去重是必要的：3 个 provider × 各重试 2 次 = 6 条 `retry` 事件，去重后仍是 3 条（每个 provider 一条），加上 2 条 `fallback` 就是 5 条——全发出去就是刷屏。去重键用 `ev.From`（`Describe()`）而非 `Attempt`，因为用户关心"哪个模型在抽风"，不关心"第几次"；`Attempt/MaxAttempts` 只用于文案里的数字。`maxNoticesPerTask = 3` 是硬上限：默认档 `fallback` 最多 `n-1` 条（3 个 provider 时 2 条），触不到；`all` 档理论上有 `2n-1` 条，靠这个上限截断到 3 条。

**`ev.To` 的文案细节**：`To` 是 `describe(next)`，即备用 provider 的静态描述。想显示 `ark/ark-code-latest` 就在装配时让每个成员的 `Describe()` 返回 `provider/model`——§3.6 的 `buildOne(id, model, c)` 手上正好有这两个值，`adk.Runtime` 加一个 `Describe()` 即可（§3.3）。**不要**为了这句文案给 `Describe()` 加"最近一次"状态（§3.3 已说明那是 `-race` 陷阱）。

**为什么"换 provider 必须提示、重试可选"**：重试是"同一件事再试一次"，结果不变时用户无感更好；换 provider 改变了回答的来源，属于用户应当知情的事实。这也正好对应配置默认值（§8.5）。

### 8.4 怎么发：`push.Notify`（不降级、超时靠 `ctx` 传播）

不能复用 `push.Deliver`：它的设计是"主渠道失败就降级到 telegram → feishu → wechat"（`deliver.go` 的 `degrade`）。一条"正在重试"的临时提示**降级到别的渠道**只会更让人困惑（用户在微信里说话，提示跑到 Telegram），而且会污染 `contacts` 之外的语义。所以新增一个不降级的入口。

**r3 修正：给 `push.Sender.Send` 加 `ctx`，删掉 `sendBounded`**。r2 的写法是"goroutine + `time.After` 停止等待"，但那**只是调用方不再等**：`Sender.Send` 没有 `ctx` 参数，底层 HTTP 请求照样跑完。telegram 的 `Send` 自己套了 30s（`telegram.go:43`），而提示只等 3s，于是渠道故障时每次重试都会留下一个最长 30s 的 goroutine；多任务下会累积。正确的做法是让取消能真正到达 HTTP 请求：

```go
// internal/push/types.go — Sender gains ctx. Every caller already has one; the implementations
// simply pass it to the HTTP request, so cancelling the caller actually aborts the send.
type Sender interface {
    Configured() bool
    Send(ctx context.Context, address string, p Payload, withImages bool) error
}
```

改动面是明确的（2 个生产实现 + 2 个 `Send` 调用点 + 2 个 `Deliver` 调用点 + 测试替身）：

| 位置 | 改动 |
| --- | --- |
| `internal/channels/wechat/send.go` 的 `pushSender.Send` | 用 `context.WithTimeout(ctx, wechatSendTimeout)` 替换 `ctx := context.Background()`（`send.go:80`），并新增常量 `wechatSendTimeout = 30 * time.Second`（该文件目前没有任何超时常量；`sendText` / `sendImageFile` 本来就接收 `ctx`，所以这一个改动同时给文本与图片发送上了界。刻意不复用 r2 的 `pushSendTimeout` 名字——那个属于 `push.Notify` 层且已被删除，同名会让人误以为上界仍由调用方给）。**两个作用**：让取消从调用方传播进来（这是 r3 的核心修正），以及顺手修掉"微信推送完全没有超时"这个原有缺陷——注意 `push.Deliver` 的调用点传的是无 deadline 的进程级 `ctx`，光透传并不能给它上界 |
| `internal/channels/telegram/telegram.go` 的 `(*Channel).Send` | 把 `context.WithTimeout(context.Background(), requestTimeout)`（`telegram.go:103`）改成 `context.WithTimeout(ctx, requestTimeout)`：**继承调用方的取消，同时保留自己的 30s 上界**。不能直接透传 `ctx`——`push.Deliver` 的两个调用点传的是进程级 `ctx`（无 deadline），去掉这层上界等于让一次推送无限期挂着 |
| `internal/push/deliver.go` 的 `Deliver` / `degrade` | 都加 `ctx context.Context` 首参，透传给 `Send` |
| `internal/scheduler/engine.go:428`、`internal/channels/wechat/reminder.go:125` | 传各自的 `ctx`（两处都已经有） |
| 测试替身（`push_test.go`、`engine_test.go`、`wechat/lifecycle_test.go`） | 签名补 `ctx`，其余不动 |

两个实现的模式一致：**`ctx` 只用来接收取消，上界仍由每个实现自己给**。于是"谁在等"与"等多久"分开了——取消从调用方传播到 HTTP（`ctx` 参数），而每一层保留合理的上界（`Notify` 3s、渠道层 30s），后者永远不会成为前者的短板（3s < 30s）。`Notify` 的实现因此可以非常短：

```go
// notifyTimeout bounds ONE notice. A notice must never become the slowest part of a retry: if the
// channel is unreachable the request is cancelled and the task proceeds.
const notifyTimeout = 3 * time.Second

// Notify sends a short, best-effort notice on the recipient's OWN channel only.
// It deliberately does NOT degrade across channels: a transient "retrying" hint appearing on a
// different channel than the conversation is worse than the silence it replaces.
//
// ctx comes from notifier.base (§8.3), which is already detached from the task's cancellation, so
// a budget-exhausted task still delivers the notice the user is owed. Do NOT re-derive
// context.WithoutCancel here: that would also strip the notifier's own cancel, and close() would
// lose the only way to abort an overrunning send — back to waiting n × 3s (r4).
func Notify(ctx context.Context, r Recipient, text string) error {
    s, ok := SenderFor(r.Source)
    if !ok { return fmt.Errorf("%s: channel not available", r.Source) }
    if !s.Configured() { return fmt.Errorf("%s: channel not configured", r.Source) }
    if r.UserID == "" { return errors.New("recipient address is empty") }

    ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
    defer cancel()
    return s.Send(ctx, r.UserID, Payload{Text: text}, false)   // text only, never images
}
```

对比 r2 的两点改进：

1. **取消真的能到 HTTP**：`http.NewRequestWithContext` 链路上的 `ctx` 一旦过期，连接立刻被 abort，goroutine 不会滞留。
2. **错误有信息量**：渠道慢时报的是 `context deadline exceeded`（或 SDK 包好的 `send text: context deadline exceeded`），而不是 r2 那句自造的 `notice timed out after 3s`。

`context.WithoutCancel` **上移到了 `notifier.base`**（§8.3），不再出现在 `Notify` 里。语义完全一样——提示是用户已经该收到的东西，任务预算耗尽不该把它一起掐掉——但位置从"每次发送现算"变成"每任务算一次"。这不只是整洁：`Notify` 里再叠一层会把 `close()` 的 `cancel()` 剥掉（`WithoutCancel` 不继承取消），使 §8.3 的 2s 收口上界失效，退回 r3 的"干等 n × 3s"。

**微信侧的前置条件不变**：入站消息在 `monitor.go:163` 已经 `UpdateContext` + `RecordContact`，所以 `pushSender.Send` 能拿到 `context_token`（`send.go:75` 的 `GetContext`）。`context_token` 缺失时 `pushSender.Send` 直接报错，提示会静默丢掉（失败处理见 §8.6）。

**提示不得拖长整条链**，两条具体措施：

| 事件 | 提示发出的时机 | 额外耗时 |
| --- | --- | --- |
| `retry` | **与退避 sleep 并发**（`sleep` 本来就是死时间），`close()` 在 `dispatch` 返回前收口 | ≈ 0（退避 0.5–1.5s 已覆盖一次文本发送） |
| `fallback` | 同样入队后立即返回，与下一个 provider 的首次请求并发 | ≈ 0（下一个 provider 的调用本身就要几秒，`notifyTimeout` 远小于它） |

所以 §8.3 的 `observe` 只往队列里塞一条就返回：它必须**立刻**把控制权交还给 `RetryingRuntime`（退避 sleep 或下一个 provider 的调用），否则"发提示"就成了"在重试之外再加一段等待"。而 `close()` 在 `dispatch` 侧收口保证了顺序与"先于最终回复"（§8.3）。

**唯一真正的代价是 `dispatch` 返回前的那一次 `close()`，而上界是固定的 `notifyDrainTimeout`（2s）**。这里必须把话说准，因为 r3 的说法自相矛盾过：

| | r3（错） | r4（本稿） |
| --- | --- | --- |
| `close()` 等多久 | 排空整个队列 → `n × notifyTimeout` | `min(排空时间, notifyDrainTimeout)` |
| `all` 档 3 provider 链（3 条通知） | 最多 **9s** | 最多 **2s** |
| 链长增加时 | 线性增长 | **不变** |
| 与 §8 约束表"额外延迟有固定上界" | 矛盾 | 一致 |

代价是**可能少一条提示**：渠道病态时，超时之后仍在队列里的通知会被丢弃（stderr 记一行）。这个取舍是明确的——提示是过程插播，最终回复才是用户要的东西；为了多发一条"正在重试"而让答案晚到几秒，方向反了。反过来，健康渠道下 2s 足够发完 3 条，用户该看到的都能看到。

**为什么不改 `Deliver` 的降级语义**：`Deliver` 的 `ctx` 只用来**限制单次尝试**，不改变"主渠道失败就换下一个"的逻辑。它的两个调用点（scheduler、微信续期提醒）都是后台任务，传的是进程级 `ctx`，行为与今天一致。


### 8.5 配置：`LLM_RETRY_NOTIFY`

| 值 | 行为 |
| --- | --- |
| `off` | 完全不发提示（只留 stderr） |
| `fallback`（**默认**） | 只在换 provider 时提示（§8.3 的核心价值，不刷屏） |
| `all` | 首次重试 + 每次换 provider 都提示（调试期推荐） |

默认 `fallback` 的理由：换模型是用户有权知道的事实，而"重试了一次"多数时候用户根本不需要知道——默认值应该取"信息价值 / 打扰成本"最高的那一档。

`newNotifier` 的构造与 `off` 的短路见 §8.3（`off` 时返回 `nil`，`dispatch` 因此不装 observer：无 ctx value、无 goroutine、热路径零分配）。

**对 parity 无影响**：`scripts/parity` 以 CLI 模式跑（`source=cli`，无 `Recipient`），§8.2 的门槛直接把它排除在外，`resultText` 一字不变。

### 8.6 发不出去怎么办

提示是 **best-effort**，任何失败都只写 stderr，绝不影响任务：

| 情况 | 表现 | 处理 |
| --- | --- | --- |
| 微信 `context_token` 缺失/过期 | `pushSender.Send` 返回 `wechat: no cached context_token for this user` | stderr 一行（`[dispatch] retry notice failed: …`），任务继续。下一次入站消息会刷新 token |
| 用户重新登录了微信（`clearAccount` 清空 token 与 contacts） | 同上 | 同上 |
| 渠道未配置（如只开了 wechat，`Recipient` 却是 telegram） | `channel not configured` | 同上 |
| 发送超时 | `context deadline exceeded`（或 SDK 包好的 `send text: context deadline exceeded`） | 同上；请求由 `notifyTimeout` 真正取消，无 goroutine 残留（§8.4） |
| 收口超时（渠道整体病态） | `close()` 等满 `notifyDrainTimeout`（2s），`cancel()` 打断在途请求，队列里剩下的通知丢弃 | stderr 一行 `retry notices dropped: drain exceeded 2s`；**最终回复照常发出**，只是可能少一条提示（§8.3、§8.4） |
| 任务本身已失败/取消 | `give-up` / `budget-exhausted` 不产生提示（§8.3 表格）；已经入队的 `retry`/`fallback` 仍会发出，因为 `base` 已脱离任务取消（§8.3） | 无需处理 |

**为什么不在提示失败时降级到别的渠道**：见 §8.4 的开头。用户是在微信里问的，答案在微信里，一条跑到 Telegram 的"正在重试"比没有更糟。

### 8.7 与"最终回复"的关系

提示是**追加**的，不替代任何东西：

```
用户: 帮我把今天的体重记一下
  ← ⏳ 模型响应异常，正在重试（第 1/2 次），请稍候…      (§8.3, retry，仅 LLM_RETRY_NOTIFY=all)
  ← 🔄 主模型不可用，已切换到备用模型（ark/…），请稍候…   (§8.3, fallback)
  ← ✅ 已记录：体重 72.5kg（今天）                      (原有回复，文案不变)
```

失败时同理，只是最后一条变成原有的 `⚠️ Task did not complete successfully: …`。

三条边界必须写清楚，否则实现时容易走偏：

1. **绝不拼进 `Result.Text`**。`Result.Text` 是"任务的回答"，而提示是"过程的插播"。拼进去会同时破坏 parity（`resultText` 比对）和 `TaskLog.replyText` 的语义。
2. **不产生 `TaskLog` 条目**。`dispatch` 一次任务只写一条日志（§7.1），提示不是任务。
3. **CLI 与 scheduler 一条都不发**（§8.2）。CLI 的 stderr 已经有完整信息；scheduler 的用户不在等。注意 `silentHours` 帮不上忙：它的语义是"把落在免打扰窗口内的**触发时刻**推迟到窗口结束"（`cron.go:617` 的 `applySilentHours` 在算 `nextFireEpoch` 时生效），不是"发送时抑制"。任务一旦开跑就已经在窗口外了，此时任何提示都会照常发出。所以 §8.2 的 `isInteractiveSource` 门槛是唯一防线——这也是它必须写死 `wechat`/`telegram` 而不是"有 Recipient 就发"的原因。

---

## 9. 与 v1 parity 的关系

| 影响面 | 结论 |
| --- | --- |
| TaskLog JSON | 不变（§7.1）。注意 `provider`/`model` 仍报**配置值**，不是实际服务的 provider（§3.6） |
| `/healthz` keys | 只增不减；`parity` 不比对 healthz |
| `/metrics` 名字 | 现有名字逐字不变；新增 3 个（`attempts_total` 已按 §7.2 移除） |
| 回复文案 | 不变。重试成功后用户看到的是正常回复；重试耗尽后仍是 `⚠️ Task did not complete successfully: <msg>` |
| 渠道提示（§8） | **对 parity 无影响**：`scripts/parity` 以 `source=cli` 运行且无 `Recipient`，§8.2 的门槛直接排除，`resultText` 一字不变 |
| 路由失败兜底 | 不变（`runner.go:144` 仍走中文"无法路由"） |
| 默认行为 | 请求次数上界从"SDK 3 次仅建连"变成"3 次含流式断流"，**这是唯一的行为变化，也是本方案的目的**；fallback 需要显式 `priority` + `model`，默认不切模型；`LLM_REQUEST_TIMEOUT_SECONDS=0` 保持不超时 |
| 需要记 deviation 吗 | 不需要（v1 无重试，Go 现状已有 2 次；本方案只是把重试的覆盖面扩大，不新增用户可见的回复文案差异）。若最终把 `LLM_MAX_RETRIES` 默认改成 0，则要在 `go-rewrite-status.md` 记一条（v1 无重试） |

一个要留意的细节：`scripts/parity` 用 stub 网关，stub 若按"第 N 次请求"返回不同内容，重试会让两边的调用次数不一致。因此 **parity 运行时应显式设 `LLM_MAX_RETRIES=0`**（或确认 stub 永不返回可重试错误），并在 `parity/main.go` 的配置准备处写明。

---

## 10. 分阶段实施

| 阶段 | 内容 | 风险 | 可否独立发布 |
| --- | --- | --- | --- |
| **P0** | `internal/config` 新增 7 个键 + 解析 + 测试（**只解析，不改变任何现有行为**） | 低 | 是 |
| **P1** | `internal/agentrt/retry.go`：`Classify` + 两张正则表 + `RetryingRuntime` + `FallbackRuntime` + `wrapBudget` + `StreamError`；`buildOne` 注入 `MaxRetries=0` + `RequestTimeout`；`resolveRuntime` 改调 `buildRuntimeChain`（链层恒存在，§3.2） | 中（错误分类需要真实网关样本验证） | 是（fallback 未接，链长度 1，但 `give-up` 诊断已在） |
| **P2** | `providers.json` 的 `priority` / `model` / `fallback` + `ProviderChain()` + thinking 剥离（`FallbackRuntime` 的骨架已在 P1 落地） | 中（§3.6 的备用模型解析） | 是 |
| **P2b** | §8 的渠道提示：`agentrt.WithRetryObserver` + `dispatch` 注入 + `push.Notify` + `notifier` 的 FIFO 队列；**同时**给 `push.Sender.Send` / `push.Deliver` 加 `ctx`（§8.4，否则超时只是"不再等"，goroutine 会滞留，且 §8.3 的 2s 收口上界会失效） | 低（纯增量，失败只写 stderr；接口改动的编译期即可覆盖） | 是（`LLM_RETRY_NOTIFY=off` 即完全关闭） |
| **P3**（可选） | monitor 三个计数器 + `/healthz` 的 `llm` 块；`Retry-After` 解析；熔断器 | 低 | 是 |

**P0 必须与 P1 同一个 PR 上线**，这一点容易做错：如果先单独发 P0 的"关掉 SDK 重试"（`MaxRetries=0`），而 `RetryingRuntime` 还没接上，结果是**重试能力从"2 次"掉到"0 次"**——一个纯粹的能力回退。所以 P0 拆出来的唯一价值是"配置解析可以先写完并单测"，合入顺序必须是 P1 紧随其后（或干脆合并为一个 PR）。

P0 + P1 就能拿到 80% 的收益（流式断流不再一次失败），建议先做这两步。

P2b 单独列为一步，因为它虽然价值高但**与重试/fallback 的正确性完全解耦**：它只消费 `RetryEvent`，不参与任何决策。把它拆开的实际好处是——P1/P2 可以先用 `LLM_RETRY_NOTIFY=off` 上线验证稳定性，等日志确认重试行为符合预期后，再打开提示。反过来如果一起上，一次"重试风暴"会直接变成用户的聊天窗口刷屏，排查时更难分辨是重试逻辑错了还是提示节流错了。

---

## 11. 测试计划

### 11.1 单元测试

| 文件 | 用例 |
| --- | --- |
| `internal/config/config_test.go` | 7 个新键的解析：默认值、非法值回落、`0` 的语义（`LLM_MAX_RETRIES=0` 不能回落成 2） |
| `internal/config/providers_test.go`（新） | `ProviderChain()`：`PI_PROVIDER` 恒在第 0 位、未配 `priority` 的 provider 不进链、`priority` 排序、同 priority 字典序、`PI_PROVIDER` 不重复、配了 `priority` 但缺 `model` 被跳过、`fallback:false` 排除、`LLM_FALLBACK_ENABLED=0` 截断 |
| `internal/agentrt/retry_test.go` | `Classify` 表驱动：每类错误的 `(retrySame, tryNext)`；黑名单优先于白名单（`insufficient_quota` 文案里含 `rate limit`）；`ctx.Canceled` 不重试；`StreamError` 的 `Message` 走同一套规则（`invalid_request` 不重试、`stream ended` 重试、未知文案不重试） |
| `internal/agentrt/retry_runtime_test.go` | `RetryingRuntime`：成功一次不重试、前两次失败第三次成功、耗尽后返回最后一次错误、退避可被 `ctx` 中断（注入 `sleepFn` 避免真实等待）、`OnEvent` 的调用次数与 `Attempt` 值；`wrapBudget`：超预算后不再换 provider、返回的仍是最后一次真实上游错误而不是 `context deadline exceeded` |
| `internal/agentrt/fallback_test.go` | `FallbackRuntime`：主成功不碰备用、主 fatal（如 400）直接返回且**不消耗备用**、主重试耗尽后才试备用、全失败返回最后一个错误、每个成员各自用满自己的 `MaxRetries`（断言主被调用 `1+maxRetries` 次）、thinking 被剥离（断言备用 runtime 收到的 `Request` 里没有 `PartThinking`）、**链长为 1 时耗尽仍发一次 `give-up`**（r3 修正）且 `Describe()` 不含 `chain(` |
| `internal/runner/runtime_test.go` | 链构建：备用 provider 配错被跳过且主链仍可用；主 provider 配错**不**降级；全错时错误文案与现状一致；**单 provider 时返回的是 `FallbackRuntime`（链层恒存在）** |
| `internal/dispatch/notify_test.go` | §8 的提示：`cli` / `scheduler` 源**不**发提示（断言 sender 调用 0 次）；`wechat` 源在 `mode=all` 下发且文案含 `第 1/2 次`；`fallback` 事件在 `mode=fallback` 下每次都发、`retry` 在 `mode=all` 下同 provider 只发一次；`maxNoticesPerTask` 触顶后停发；`LLM_RETRY_NOTIFY=off` 时 0 次（`newNotifier` 返回 nil）；发送失败只 warn 不影响任务返回；**`close()` 返回后不再有在途发送，且顺序与事件顺序一致**（用会阻塞的 fake sender 断言 `retry` 先于 `fallback`）；**`close()` 之后再来事件不 panic**（r3：`closed` 标志吞掉它，配合 `-race` 跑）；**r4 收口上界**：注入一个永久阻塞的 fake sender，断言 `close()` 在 `notifyDrainTimeout` 附近返回（允许 ±200ms 抖动）、返回后 fake 收到 ctx 取消、队列里剩余通知被丢弃且 stderr 有 `dropped` 一行；**r4 条数无关性**：3 条通知 + 慢渠道（每条 1.5s）时 `close()` 仍在 2s 上界内返回，而不是等 4.5s；**r4 预算耗尽仍发**：父 ctx 在 `observe` 之后、`sender` 发送之前被取消，断言 sender 仍收到非取消的 `ctx`（`base` 生效） |
| `internal/push/push_test.go` | `Notify`：**不**降级（主渠道失败时其他渠道 0 次调用，与 `Deliver` 行为相反）、未配置渠道返回错误、空地址返回错误、**`ctx` 超时能让阻塞的 fake sender 立即返回**（r3：`Sender.Send` 带 `ctx`） |

`fake.Runtime` 需要一个小扩展：支持"第 N 次调用返回错误"，或直接写一个 5 行的 `scriptedRuntime` 测试替身（不改 `fake` 包，避免影响现有测试的语义）。

### 11.2 集成测试

`internal/agentrt/adk` 侧用一个 `httptest.Server`：

1. 第一次请求返回 `200` + 半截 SSE 然后关闭连接 → 断言 `Classify` 判为可重试
2. 第二次返回 `200` + 完整 SSE → 断言 `RetryingRuntime` 成功且只调用 2 次
3. 返回 `401` → 断言不重试、`tryNext=true`
4. 返回 `400` → 断言不重试、`tryNext=false`

（adk-go 的 `openaimodel.ClientConfig.HTTPClient` 与 `anthropicmodel.ClientConfig.HTTPClient` 都支持注入，可指向 `httptest`。）

### 11.3 回归

- `go test ./...` / `go test -race ./...`
- `gofmt -l .` 为空、`go vet ./...` 干净
- `make cover-check` 不下降（新增代码需要配套测试，见 §11.1 的测试清单）
- `go run ./scripts/parity`（记得 `LLM_MAX_RETRIES=0`，见 §9）

### 11.4 手工验证（自动化覆盖不到的部分）

- 真实网关：把 `baseUrl` 指向一个不存在的端口，确认 fallback 生效且 stderr 有三行诊断
- 真实网关：`PI_MODEL` 写错，确认错误文案与现在一致（**不** fallback 到备用模型）
- 真实网关：备用 provider 的 `model` 写错，确认只 warn 跳过、主 provider 照常工作
- 微信渠道：断流重试成功后，用户收到的是正常回复而不是 `⚠️ Task did not complete successfully`
- 微信渠道：**提示真的到了用户手里**（这是自动化测不到的）——断网触发重试，确认微信里先收到「⏳ 正在重试」，再收到正常回复；`context_token` 过期时应只有 stderr 的 `retry notice failed` 警告，任务本身不受影响
- 微信渠道：确认提示**没有**出现在降级渠道（故意让微信发送失败，验证 Telegram 不会收到「正在重试」）
- 微信渠道：**收口上界可观测**——把微信 `context_token` 换成一个必然 hang 的值（或指向黑洞地址的 `baseUrl`），确认最终回复仍在约 2s 内到达、stderr 有 `retry notices dropped: drain exceeded 2s`。这条验证的是 §8.3 那个前提"所有 `Sender.Send` 都真的尊重 `ctx`"，自动化只能测到调用方一侧
- scheduler：定时任务重试时**不应**给用户发提示（`source=scheduler`），只有最终结果被推送
- Ctrl-C：在退避 sleep 中按 Ctrl-C，进程应立刻退出而不是等满 15s
- 长时间对话：主 provider 用到第 3+ 轮工具调用时手动断开网络，确认 fallback 后模型仍能读懂前文（验证 §3.5 的历史重放）

---

## 12. 待决策事项

| ID | 问题 | 选项 | 建议 |
| --- | --- | --- | --- |
| Q-1 | `LLM_MAX_RETRIES` 默认值 | 2（= 现状 SDK）/ 0（= v1 对等） | **2**。现状已经是 2，改 0 是能力回退；"重试 2 次"正是本方案要保住的东西 |
| Q-2 | `LLM_REQUEST_TIMEOUT_SECONDS` 默认值 | 0（不超时）/ 300 / 600 | **0**，但在 `.env.example` 里推荐 `600` 并写明"0 = 永不超时，网关卡死会占住串行队列"。若运维侧更怕卡死，选 600 |
| Q-3 | 跨 provider fallback 时是否剥离 thinking | 剥离 / 保留 | **剥离**，但降级为"防御性"而非阻塞项：adk-go 的 `openaimodel` 已经丢弃 thought 块（§3.5），今天不剥离也不会 400 |
| Q-3b | 备用 provider 的 model 从哪来 | 复用 `PI_MODEL` / 取 `models[0]` / 条目上显式 `model` | **显式 `model` 且必填**（§3.6）。复用 `PI_MODEL` 会必然 404 |
| Q-4 | `400` 是否 fallback | 否 / 是 | **否**。prompt 问题换 provider 也大概率被拒，只会把一次清晰的错误变成 N 次延迟 |
| Q-5 | fallback 是否也作用于**路由**调用 | 是 / 否 | **是**。路由失败目前静默降级成"无法路由"，用户看到的是莫名其妙的兜底文案；重试能显著改善体验，且成本极低（路由 prompt 很小）。实现上零成本：`routeProject` 与 `Loop` 拿的是同一个 `rt` |
| Q-6 | 是否本期做按用途选模型（N-1） | 做 / 不做 | **不做**。装饰器已经留好位置，等有实际成本诉求再接 |
| Q-7 | parity 是否显式关重试 | 是 / 否 | **是**（§9）。否则 stub 的调用计数会让差分测试变得脆弱 |
| Q-8 | 重试提示的默认档位 | `off` / `fallback` / `all` | **`fallback`**（§8.5）。换 provider 是用户有权知道的事实，"重试了一次"多数时候是噪声 |
| Q-9 | 重试提示是否降级到其它渠道 | 降级（复用 `Deliver`）/ 不降级（`Notify`） | **不降级**（§8.4）。用户在微信说话，提示跑到 Telegram 比沉默更糟 |
| Q-10 | 是否提示 `give-up` / `budget-exhausted` | 提示 / 不提示 | **不提示**。最终回复的 `⚠️ Task did not complete successfully: …` 已说明结果，再发一条是重复 |
| Q-11 | 提示文案是否含 provider 名 | 含（`ark/ark-code-latest`）/ 只写"备用模型" | **含**。用户配置了多个 provider 时，"切到哪个"是排障的关键信息，且不涉及任何密钥 |
| Q-12 | 如何给 `push.Sender.Send` 加超时 | 改接口加 `ctx` / 用 `sendBounded` 包一层 | **改接口加 `ctx`**（r3 修正，§8.4）。`sendBounded` 只能让调用方停止等待，底层 HTTP 请求照样跑完：telegram 是 30s、微信原本无超时，渠道故障时每次重试都滞留一个 goroutine。改动面是 2 个生产实现 + 全部调用点（`deliver.go` 的 `Send`、`scheduler`/`reminder` 的 `Deliver`）+ 几个测试替身，编译期就能全部发现，比留下一个泄漏源划算 |
| Q-13 | 提示与最终回复是否合并成一条 | 分开发 / 合并 | **分开**（§8.7）。合并就必须改写最终回复文案，直接撞 parity 与 `replyText` 语义 |
| Q-14 | `close()` 排空队列的上界 | 排空到底（`n × 3s`）/ 固定上界（2s，超时丢弃剩余）/ 完全不收口（异步发） | **固定上界 2s**（r4，§8.3、§8.4）。排空到底会让额外延迟随链长线性增长（`all` 档 3 provider = 9s），与 §8 约束表冲突；完全不收口则会让提示晚于答案到达。固定上界同时满足"额外延迟可预测"与"提示先于回复"，代价是渠道病态时可能丢一条提示——这是正确的取舍方向。**前提**：每个 `Sender.Send` 实现都必须把入参 `ctx` 传给底层 HTTP 请求，否则 `<-n.done` 会退化成等到底；所以它与 Q-12 的接口改造绑定在同一步发布 |

---

## 13. 与项目红线的关系

| 红线 | 本方案是否触碰 |
| --- | --- |
| 不放宽 `safePath` 白名单 | 否。完全不涉及 `internal/paths` |
| 不给 agent 加 bash / 任意命令 | 否 |
| 不把所有 `text_delta` 拼成回复 | 否。回复抽取（`runner.go:414-448`）一行不改；`RetryingRuntime` 返回的仍是聚合好的单条 `Message` |
| 不硬编码密钥 / 路径 / 模型凭据 | 否。全部走 `.env` + `providers.json` |
| app / data 分离 | 否 |
| 不在框架里硬编码子项目业务 schema | 否 |
| 不引入新依赖 | 否。只用标准库 + 已在 `go.mod` 里的 SDK（`option.WithMaxRetries` / `option.WithRequestTimeout` 都来自现有 SDK） |
| 代码 / 注释 / 日志英文 | 遵守。stderr 诊断行、注释、指标名全英文；用户可见的聊天回复不变（仍是现有中文兜底）。§8 的提示文案是**用户可见的聊天文本**，按项目既有例外走中文（同 `greeting.go` 的兜底文案） |

---

## 附录 A：涉及文件清单

| 文件 | 改动 |
| --- | --- |
| `internal/config/config.go` | `Config` 新增 7 个字段 + `Load()` 解析（次数/预算/请求超时 3 个键走 `envNonNegativeInt`，退避基数/上限 2 个键走 `envPositiveInt`，`LLM_FALLBACK_ENABLED` 用 `os.Getenv(...) != "0"`，`LLM_RETRY_NOTIFY` 走 `parseRetryNotify`）+ `ProviderChain()` / `ProviderTarget` |
| `internal/config/config_test.go` | 新键解析用例（含 `LLM_MAX_RETRIES=0` 不被回落） |
| `internal/config/providers_test.go`（新） | `ProviderChain()` 的排序 / 跳过 / 截断用例 |
| `internal/config/config.go`（`CustomProvider`） | 新增 `Priority *int` / `Model string` / `Fallback *bool`（`Priority` 必须是 `*int`：`0` 与"未配置"必须可区分，否则 `"priority": 0` 会被当成没配）；`LoadProvidersFile` 的匿名 struct 同步加这三个字段 |
| `internal/initcmd/init.go` | `providersTemplate` 加 `priority` / `model` / `fallback` 的说明性注释 |
| `internal/agentrt/retry.go`（新） | `Classify`、两张正则表、`RetryEvent`、`RetryingRuntime`、`FallbackRuntime`（含链长为 1 的 `Describe()` 退化）、`wrapBudget`、`stripThinking`、`StreamError`、`notify`、`WithRetryObserver` / `RetryObserverFrom` |
| `internal/agentrt/retry_test.go`（新） | `Classify` 表驱动用例（含 `StreamError.Message` 的正/负例） |
| `internal/agentrt/retry_runtime_test.go`（新） | `RetryingRuntime` / `wrapBudget` 行为 |
| `internal/agentrt/fallback_test.go`（新） | `FallbackRuntime` 行为 + thinking 剥离 + 链长为 1 的 `give-up` |
| `internal/agentrt/fake/fake.go` | 加 `Describe() string` 返回 `"fake"`（纯增量，不影响现有测试） |
| `internal/agentrt/adk/adk.go` | `OpenAIConfig` / `AnthropicConfig` 加 `Provider` 字段 + `Runtime.Describe()`；`MaxRetries=0`、`RequestTimeout` 注入、`StreamError` |
| `internal/runner/runtime.go` | `buildRuntimeChain()`、`buildOne(id, model, c)`、装饰器装配、`OnEvent` → `monitor` |
| `internal/runner/runtime_test.go` | 链构建与跳过用例（含单 provider 仍包链层） |
| `internal/dispatch/dispatch.go` | §8.2：`Recipient` 存在且源可交互时注入 `agentrt.WithRetryObserver` + `defer n.close()`；self-heal 分支用 nil observer |
| `internal/dispatch/notify.go`（新） | `notifier`：文案渲染、按 provider 去重、`maxNoticesPerTask` 上限、FIFO 队列 + 单消费者 `sender()`、`base` ctx（`WithoutCancel` + 自持 `cancel`）、`close()` 的 `notifyDrainTimeout` 固定上界 |
| `internal/dispatch/notify_test.go`（新） | 源过滤、节流、上限、发送失败只 warn、`close()` 后不再有在途发送、发送顺序与事件顺序一致、`close()` 后的迟到事件不 panic、`close()` 的固定上界（阻塞 fake sender 下 ≤ 2s 返回并丢弃剩余）、条数与上界无关、父 ctx 取消后仍能发出已入队的通知 |
| `internal/push/notify.go`（新） | `Notify`：单渠道、不降级、`context.WithTimeout(ctx, notifyTimeout)`（**不**再自己 `WithoutCancel`，脱钩在 `notifier.base`，见 §8.3/§8.4） |
| `internal/push/types.go` | `Sender.Send` 加 `ctx context.Context` 首参（r3，§8.4） |
| `internal/push/deliver.go` | `Deliver` / `degrade` / `deliverToChannel` 加 `ctx` 并透传 |
| `internal/push/push_test.go` | `Notify` 的不降级 / 未配置渠道 / 空地址 / `ctx` 超时用例（追加到现有文件），测试替身补 `ctx` |
| `internal/channels/wechat/send.go` | `pushSender.Send` 加 `ctx` 参数，内部改 `context.WithTimeout(ctx, wechatSendTimeout)`（替换 `send.go:80` 的 `context.Background()`），新增常量 `wechatSendTimeout = 30 * time.Second` |
| `internal/channels/telegram/telegram.go` | `(*Channel).Send` 改 `context.WithTimeout(ctx, requestTimeout)`（`telegram.go:103`）：继承调用方取消，保留自己的 30s 上界 |
| `internal/scheduler/engine.go` / `internal/channels/wechat/reminder.go` | `push.Deliver` 调用点补 `ctx` |
| `internal/monitor/stats.go` / `health.go` | P3：3 个计数器 + `llm` 快照块 |
| `.env.example` / `README.md` | 新键文档（含 `LLM_RETRY_NOTIFY`）+ `providers.json` 新字段说明 |
| `scripts/parity/main.go` | 运行配置里显式 `LLM_MAX_RETRIES=0`（§9） |
