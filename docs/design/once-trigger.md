# 方案：新增一次性定时触发器 `once`（修订版）

> 修订记录：
> - 第一轮：补上外置提示词 `~/.botler-agent/system-prompt.md`，at-vs-once 语义区分主要写进工具 description；once 分支必须在 `compileSchedule` 之前 return；补上 `src/webui/server.ts` 预览端点；Infinity→JSON null、函数名 `scheduleSummary`、once 时禁用 silentHours；DST 脚注；修正 `update-foods-json` 已不在配置中。
> - 第二轮（写路径毒丸）：把「once 必须是将来时间」校验从 normalizeSchedules **挪出**，回到创建/修改入口（工具层 create/update 仅当本次提供 once 值时校验；WebUI save handler 仅新建或 once 值变更时校验）。store 层只做格式可解析 + 四选一互斥，且互斥校验提到 normalizeEntry（load/写一致）。这样触发后留在配置里的过期 once 不会阻塞任何后续写入。

## 1. 背景与根因

用户说「晚上十点钟提醒我更新 foods.json」，期望今天晚上 22:00 只响一次，但实际会被创建成 `{ "at": "22:00" }`，而 `at` 的语义是**每天**固定时间（编译成 `min hour * * *`），于是每天重复。

根因有两层：

1. **能力缺失**：框架只有三种周期性触发器（cron / interval / at），没有「某个绝对日期时间只响一次」的类型，agent 只能把一次性提醒勉强落到 `at`。
2. **提示词误导**：调度器的执行上下文 `schedulerContext()`（`src/prompts/system-prompt.ts`）写的是「cron / interval / at 三选一」，把 `at` 描述为「每日 HH:MM」却没有给一次性需求提供出口。注意：生产环境加载的是外置 `~/.botler-agent/system-prompt.md`，它**完全替换**内置默认提示词、且其中没有 `# schedule 工具` 段；真正在生产生效的调度指引是通过 `__PROJECT_CONTEXT__` 注入的 `schedulerContext()`，以及 schedule 工具自身的 `description`（工具描述随源码恒生效，是唯一不依赖提示词文件的渠道）。

> 当前 `~/.botler-agent/schedules.json` 已无 `update-foods-json`（仅剩 `clock-out-reminder`），可能已被清理，本方案不涉及对它的迁移。

## 2. 目标

- 新增第四种触发器 `once`：在指定的**绝对日期时间**触发**恰好一次**，之后不再触发。
- 不破坏现有 cron / interval / at 的任何行为，向后兼容已有 `schedules.json`。
- 沿用现有 watermark 机制实现「只触发一次」，**引擎本身无需改动**。
- at-vs-once 的语义区分必须写进 schedule 工具的 `description`（恒生效），并同步更新 `schedulerContext()` 与外置提示词文件。
- 同步更新 WebUI（含 server.ts 预览端点）与测试。

## 3. 数据模型改动（`src/scheduler/types.ts`）

在 `ScheduleEntry` 上新增可选字段，与 cron/interval/at 互斥：

```ts
/** One-shot absolute time, ISO 8601. Fires exactly once, then becomes inert. */
once?: string;
```

支持两种书写形式：

| 形式 | 示例 | 含义 |
|------|------|------|
| 绝对时间（带 `Z` 或 `±HH:MM` 偏移） | `2026-08-20T22:00:00+08:00` / `2026-08-20T14:00:00Z` | 直接确定的时刻 |
| 本地无时区形式 | `2026-08-20T22:00:00` 或 `2026-08-20 22:00` | 按本条目的 `timezone` 解析 |

存储时保留用户原始字符串（与 cron/interval/at 现有做法一致）。带偏移形式下 `timezone` 对该触发器本身无影响。

## 4. 触发时间计算（`src/scheduler/cron.ts`）

新增并导出：

```ts
/** Parse a `once` value to epoch ms, or null if malformed. */
export function parseOnceEpoch(s: string, tz: string): number | null
```

- 以 `Z` 或 `±HH:MM` 结尾 → `Date.parse` 原生解析（绝对时刻）。
- 否则匹配 `YYYY-MM-DD[T ]HH:MM(:SS)?` → 用现有 `wallToEpoch(wallClock, tz)` 按条目时区解析。
- 其余形式返回 `null`。

**正确性陷阱**：`nextFireEpoch` 的 once 分支必须在 `compileSchedule(e)` **之前** return。现有 `nextFireEpoch`（cron.ts:327 附近）第一行就是 `const cron = compileSchedule(e)`，若把 once 分支放在其后，once-only 条目会先被 `compileSchedule` 抛「must specify one of cron/interval/at」。正确结构：

```ts
export function nextFireEpoch(e: ScheduleEntry, afterEpoch: number): number {
    if (e.once) {
        const t = parseOnceEpoch(e.once, e.timezone || DEFAULT_TZ);
        if (t === null) throw new Error(`schedule "${e.id}" invalid once "${e.once}"`);
        // 严格晚于水印才算待触发；已到点/过期 => Infinity（永不）。
        // 注意：silentHours 对 once 不生效——精确一次性提醒不应被 DND 顺延。
        return t > afterEpoch ? t : Infinity;
    }
    const cron = compileSchedule(e);
    // ... 既有 cron/interval/at 逻辑不变
}
```

关键决策：

1. **`silentHours` 不适用于 `once`**。一次性提醒是用户明确指定的精确时刻，DND 把它延后到次日会违背预期；once 分支在调用 `applySilentHours` 之前直接 return。
2. `compileSchedule` 保持只服务 cron/interval/at，不改动。

> 脚注（DST）：无时区形式依赖 `wallToEpoch` 的单次校正近似（cron.ts:242-250），在有 DST 的时区、spring-forward 间隙理论上可能差一小时。这是既有 at/cron 的共同限制，非 once 新增；Asia/Shanghai 无 DST，实际不受影响。

## 5. 校验与存储（`src/scheduler/store.ts`）

### 校验边界（重要）

**normalizeEntry / normalizeSchedules 只做「格式可解析」与「四选一互斥」校验，绝不做「是否为将来时间」校验。**

原因：`saveSchedules`（store.ts:189-193）每次都是**全量写**，`normalizeSchedules`（store.ts:171-181）是 **all-or-nothing**；而 §8 设计了触发后的 once 保持 `enabled:true`、inert 地留在配置里（engine 不删除/不禁用）。一旦在写路径校验「once 必须是将来」，那么任意一条 once 触发后变成过去时间，之后**任何一次写**（新建别的提醒、改另一个任务、甚至 disable 这条已触发的 once）都会把整份列表送进 normalizeSchedules 并因这条过期 once 而整单抛错——整个调度配置变成只读。这是必然发生的（once 用一次即过期），不是边角情况。

因此「将来时间」是**创建/修改入口的 UX 约束**，放在工具层与 WebUI（§6、§9），不放在 store 层。

### normalizeEntry（load 与写路径共用）

- 触发器检测增加 `hasOnce`，缺触发器时报错文案更新为 `needs one of cron/interval/at/once`。
- **新增四选一互斥校验**（这是**新增**校验——现有代码只有「至少一个」检查 store.ts:63-65，多填会被 `compileSchedule` 的 cron>interval>at 优先级静默覆盖）。统计四个 `has*` 标志，若不等于 1 则抛错。放在 normalizeEntry（而非仅 normalizeSchedules）是为了让手改配置文件产生的 cron+once 双触发器在 **load 路径也被拒绝并 warning**，保持 load/写一致性。历史数据均只填一个，风险可接受。
- once 分支：
  ```ts
  if (hasOnce) {
      if (parseOnceEpoch(o.once as string, tz) === null)
          throw new Error(`schedule "${id}" invalid once "${o.once}"`);
      entry.once = o.once as string;
  }
  ```
  （注意读的是原始对象 `o.once`，不是结果 `entry.once`。）
- 末尾的 `compileSchedule(entry)` sanity 检查改为 `if (!hasOnce) compileSchedule(entry)`。
- **不校验「是否为将来时间」**：normalizeEntry 在 load 时也会被调用，一个曾经合法、现在已过期的 once 必须能被正常读回，否则会被丢弃并报警告，且会触发上面描述的「写路径毒丸」问题。

### normalizeSchedules

- 保持现有「重复 id」检查即可；触发器互斥与格式校验已在 normalizeEntry 完成。
- **不做将来时间校验**（理由见上）。

## 6. 工具层（`src/tools/schedule.ts`）

- schema 增加 `once` 可选字符串参数，描述写清「一次性绝对时间 ISO 8601，如 `2026-08-20T22:00:00+08:00`；触发一次后即失效」。
- **`description` 是根因修复的主阵地**（它随工具恒生效，不依赖任何提示词文件）。必须在其中写明四种触发器，并显著区分：
  - `at`：**每天**固定时间重复（HH:MM）。
  - `once`：**某个具体日期时间只响一次**（必须带完整日期）。当用户说「今晚/明天/某天 X 点」「X 月 X 日 X 点」「下周一 X 点」等一次性语义时，必须用 `once` 并按当前日期推算出完整的 `YYYY-MM-DDTHH:MM`，**不要用 `at`**（那会每天重复）。
  - 周期性重复（每天/每周/每隔）才用 at/cron/interval。
- `triggerOf(e)`：增加 `if (e.once) return \`once ${e.once}\``。
- create 路径：triggers 数组收集四个字段，要求恰好一个；recipient 注入等其余逻辑不变。
- **将来时间校验放在工具层（入口校验）**：
  - create 且 `args.once` 时：`parseOnceEpoch(args.once, tz)` 必须 `> Date.now()`，否则抛错 `"once time must be in the future (use a full date-time, e.g. 2026-08-20T22:00:00+08:00)"`，错误会透传给 agent 并可自纠正。
  - update 且本次提供了非空 `args.once`（即新建/切换/修改 once 值）时：同样校验。
  - update 仅改 message/timezone/enabled 而不动 once 值（如 disable 一个已过期的 once）时**不校验**，避免 §5 描述的写路径毒丸。
- update 路径：切换触发器时删除其余三个字段（cron/interval/at/once），避免残留。
- `nextFireOf(e)` / list 输出：当 `nextFireEpoch` 返回 `Infinity`（once 已过期/已触发）时，标注 `(expired)`，不要显示 `Invalid Date`。

## 7. 系统提示词（`src/prompts/system-prompt.ts` + 外置文件）

三处都要覆盖，缺一不可：

1. **工具 `description`（§6，已述）——唯一恒生效渠道，是根因修复的主阵地。**
2. **`schedulerContext()`（system-prompt.ts:134-142）**：它通过 `__PROJECT_CONTEXT__` 注入，外置提示词文件第 19 行有该占位符，因此**会随源码改动在生产生效**。把「cron / interval / at 三选一」改为「cron / interval / at / once 四选一」，并写明 at=每日重复、once=一次性（必须带完整日期）、不要把一次性提醒建成 at。这是当前误导 agent 的直接来源，必须改。
3. **外置文件 `~/.botler-agent/system-prompt.md`**：它完全替换内置默认提示词，且当前没有 `# schedule 工具` 段。需要新增一段 schedule 使用说明（四触发器 + at/once 区分），与内置默认提示词的对应段落保持一致。
   - 同时同步更新**内置默认提示词** `DEFAULT_SYSTEM_PROMPT` 的 `# schedule 工具` 段（system-prompt.ts:31-37），保持内置/外置一致，供 `npm run init` 新用户和无外置文件场景使用。

> 实现备注：外置文件是用户配置、不在仓库内，实施时直接编辑 `~/.botler-agent/system-prompt.md`；内置默认的改动提交到仓库。

## 8. 调度引擎（`src/scheduler/engine.ts`）——无需改动

现有 watermark 机制天然支持 once：

| 场景 | 行为 |
|------|------|
| 新建一个未来的 once | watermark=now；`nextFireEpoch` 返回 once 时刻；循环 sleep 到该时刻；`next <= now` 成立 → 触发；触发后 watermark 推进到 now，下次计算 once ≤ now → `Infinity`，不再触发 |
| 触发后进程不重启 | 立即变 inert，不会重复 |
| 触发后重启 | watermark 重置为 now（> once 时刻）→ `Infinity` → 不补触发 |
| 触发前重启 | watermark 重置为新 now（< once 时刻）→ 仍在 once 时刻触发 |
| once 时刻在停机期间过去 | watermark=now > once → `Infinity` → 跳过（符合既有「默认不补触发」策略） |
| 长任务阻塞越过 once 时刻 | 该 once 的 watermark 仍是首次见到的旧值 < once → `nextFireEpoch` 返回 once 时刻；解锁后 `next <= now` 成立 → 立即补触发（「late but not lost」） |
| once 触发失败 + retry | retry 在同一次 fire 内退避重试；耗尽后 watermark 推进，不再重触发 |
| **禁用后重新启用一个已过期的 once** | `pruneWatermarks`（engine.ts:58-61）在禁用时删掉 watermark，重新启用后 watermark 重置为 now > once → `Infinity` → 静默永不触发。与默认不补触发策略一致，可接受；列表会显示 `(expired)`，用户可据此删除 |

因此不需要新增「已触发」持久化字段，也不需要引擎在触发后写文件删除/禁用条目。过期 once 条目 inert 地留在配置里（可被删除），与 disabled 条目同理。

## 9. WebUI

### `src/webui/index.html`

- 触发器单选增加 `once (one-time date-time)`。
- `scheduleSummary(e)`（index.html:691-696，注意函数名不是 triggerLabel）增加 `if(e.once) return "once: "+e.once;`。
- 编辑器 `mode` 判定、构造 entry 时处理 once；spec 输入框 placeholder 增加 once 示例（`2026-08-20T22:00+08:00`）。
- **`mode=once` 时禁用 silentHours 输入字段**（once 不应用 DND，见 §4），避免误导。
- **保存时做将来时间校验（入口校验，与工具层一致）**：save handler 中，当 `mode==="once"` 且（新建条目，或 once 值与原条目不同）时，解析 once epoch，若 `<= Date.now()` 则在模态框内提示错误并阻止保存。仅改 message/enabled 而不改 once 值时不校验，避免阻止用户 disable 已过期的 once（§5 所述写路径毒丸）。
- 预览「Next fire」：`/api/config/schedules/preview` 对过期 once 返回的 `next` 是 `Infinity`，经 `JSON.stringify` 序列化为 **`null`**（不是 0）。前端判断必须写 `r.next === null || r.next === 0`，此时显示 `(expired)`，不能直接 `new Date(r.next)`（那会得到 1970 年）。

### `src/webui/server.ts`（预览端点，必须改）

`/api/config/schedules/preview`（server.ts:244-270）当前只从 body 取 cron/interval/at，构造的 entry 无 once。若前端 POST `{ mode: "once", ... }`，服务端会构造出无触发器条目 → `nextFireEpoch` 抛错 → 预览报错。必须：

- body 类型加 `once?: string`。
- 构造 entry 时加 `...(b.once ? { once: b.once } : {})`。

（保存端点走通用 config 写入 + `normalizeSchedules`，无需额外改动。）

## 10. 测试

- `cron.test.ts`：
  - `parseOnceEpoch`：绝对形式、无时区形式、非法形式、跨时区换算正确性。
  - `nextFireEpoch` with once：未来时刻返回该时刻；已过期/水印晚于该时刻返回 `Infinity`；silentHours 不被应用（once 分支早于其 return）。
- `store.test.ts`：
  - `normalizeEntry` 接受合法 once；拒绝非法 once 字符串；缺触发器报错文案包含 once。
  - 四触发器互斥：同时填两个（如 cron+once）报错（load 与写路径共用 normalizeEntry）。
  - **normalizeEntry/normalizeSchedules 不做将来时间校验**：过期 once 能被正常 normalize（不抛错）——加一条用例锁定这个边界，防止以后有人把将来校验加回 store 层重现写路径毒丸。
- 工具层/WebUI 的将来时间校验为 UI 逻辑，不单测（或仅在有现成工具单测框架时补）。

## 11. 兼容性与迁移

- 旧 `schedules.json`（只有 cron/interval/at）完全不受影响。
- 本方案不硬编码迁移任何具体业务条目。`update-foods-json` 当前已不在 schedules.json 中，无需清理。
- 上线后，用户用自然语言「今晚十点提醒我……」即可验证：应生成 `once` 而非 `at`，且 next fire 为今天的对应时刻。

## 12. 改动文件清单

| 文件 | 改动 |
|------|------|
| `src/scheduler/types.ts` | 新增 `once?: string` 字段及注释 |
| `src/scheduler/cron.ts` | 新增 `parseOnceEpoch`；`nextFireEpoch` 在 `compileSchedule` **之前**加 once 分支（跳过 silentHours） |
| `src/scheduler/store.ts` | `normalizeEntry` 支持 once、once 跳过 compileSchedule；**四选一互斥校验放在 normalizeEntry**（load/写一致）；**不做将来时间校验**（避免写路径毒丸） |
| `src/tools/schedule.ts` | schema/description（at-vs-once 主阵地）/triggerOf/create/update 清理/list 过期展示；**将来时间校验放在 create/update 入口**（仅当本次提供 once 值时） |
| `src/prompts/system-prompt.ts` | 内置默认提示词 schedule 段 + `schedulerContext()` 同步四触发器与选择规则 |
| `~/.botler-agent/system-prompt.md` | **外置文件**新增 schedule 段（生产实际加载），与内置保持一致 |
| `src/webui/index.html` | 触发器选项、`scheduleSummary`、编辑器、once 时禁用 silentHours、保存时将来校验（仅新建/once 值变更）、预览 null/0 过期判断 |
| `src/webui/server.ts` | `/api/config/schedules/preview` body 与 entry 构造增加 once（不做将来校验） |
| `src/scheduler/cron.test.ts` | once 解析/nextFireEpoch 用例 |
| `src/scheduler/store.test.ts` | once 格式校验/四选一互斥；**过期 once 可被 normalize（锁定不做将来校验）** |

## 13. 实施步骤

1. types.ts 加字段。
2. cron.ts 加 `parseOnceEpoch` 与 once 分支（注意在 compileSchedule 之前 return），配单测。
3. store.ts：normalizeEntry 支持 once 与四选一互斥；**不加将来时间校验**，配单测（含「过期 once 可被 normalize」用例锁定边界）。
4. tools/schedule.ts：schema/description/triggerOf/create/update/list；**将来时间校验放在 create 与 update（仅当本次提供非空 once 值时）**。
5. system-prompt.ts：更新内置默认段与 `schedulerContext()`。
6. 编辑 `~/.botler-agent/system-prompt.md` 新增 schedule 段。
7. webui/index.html + server.ts：once 选项、预览、silentHours 禁用、保存时将来校验（仅新建/once 值变更）。
8. `npm run typecheck` + `npm test` 全绿。
9. CLI 本地验证：`npm start -- "今晚22点提醒我测试"`，检查 schedules.json 生成 once 而非 at、next fire 正确；再测过期 once 被 create 拒绝；最后验证触发后（或手动填一个过去的 once）仍能成功创建另一条提醒，确认写路径未被毒丸锁定。
