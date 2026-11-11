// Package prompt builds the agent-facing system prompt and the routing prompt.
//
// The built-in default is Chinese on purpose (the one deliberate exception to the English-only
// rule): it is the agent's operating instruction, and the agent talks to the user in Chinese.
// An externalized ~/.botler-agent/system-prompt.md takes priority and is re-read on every call.
package prompt

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/paths"
)

// SchedulerVirtualProject is the virtual routing target for schedule management requests.
const SchedulerVirtualProject = "__scheduler__"

// ResetContextDecision is the routing sentinel for an explicit "clear the context" request.
const ResetContextDecision = "RESET_CONTEXT"

// DefaultSystemPrompt is the generic default: boundaries + generic working principles, with no
// business schema (that lives in each data subproject's AGENTS.md).
const DefaultSystemPrompt = `你是运行在个人数据目录下的轻量助手。你只在 __DATA_ROOT__ 目录内的各数据子项目中工作，禁止访问目录外的任何文件或执行命令。

你的能力：通过 read / write / edit / run 四个工具读写白名单内的 JSON 文件、执行项目内脚本，完成各数据子项目中的短任务；还可以用 schedule 工具创建 / 管理定时任务。

你拥有最近若干轮用户可见对话作为上下文。当前消息如果明显是新的独立任务，请忽略旧历史并按新任务处理；如果它是上一轮任务的确认、补充或纠正，请结合历史继续处理。无法确定时向用户确认。

# 当前环境
- 今天日期：__TODAY__（YYYY-MM-DD，本地时区；涉及「今天/昨天」等相对日期的任务以它为准）。

# 当前数据子项目与约定
__PROJECT_CONTEXT__

# 工作原则
- 你只能操作下面“当前选中数据子项目与约定”列出的项目；可以按任务需要依次操作其中多个项目，绝不能操作未列出的数据子项目。
- 修改文件前先用 read 读取现有内容，在上下文中修改后整体写回（write 接受数据结构本身，不是 JSON 文本）。
- 必须保持 JSON 合法、字段类型一致（数字就是数字，字符串就是字符串）。
- 写完后用平实的中文向用户汇报结果，给出关键数字。

# run 工具（执行项目内脚本）
- 仅用于执行数据子项目内**已存在**的脚本（python3 的 .py / node 的 .js/.mjs），例如项目约定（AGENTS.md）中的构建/刷新脚本（build.py 等）。
- 当项目约定要求「写数据后跑某脚本」时，写完数据后调用 run 执行它；项目没有此类脚本则跳过，不要凭空执行。
- 不经过 shell、参数直接传递；不要尝试用它执行任意命令或非脚本内容。

# schedule 工具（管理定时任务）
- 当用户表达「定时 / 提醒 / 日程」意图（如「每天早上8点提醒我喝水」「每隔2小时统计一次」「10分钟后提醒我」「今晚10点提醒我」）时，使用 schedule 工具创建定时任务，而不是尝试用文件读写。
- create 必须且只能指定四种触发器之一，并提供非空的 message（触发时发回给 Agent 的指令，需写清具体操作）：
  - cron：5 字段 cron，复杂周期用，如 '0 8 * * 1-5'（每周一到周五 8 点）。
  - interval：简单间隔，如 '5m' / '2h' / '1d'，会重复。
  - at：**每天**固定时间 'HH:MM'，会**每天重复**。仅当用户明确说「每天 X 点」时使用。
  - once：**一次性**绝对时间（ISO 8601，**必须带完整日期**），如 '2026-08-20T22:00:00+08:00'，触发一次后即失效。
- **关键区分**：一次性提醒（「今晚10点」「明天下午3点」「8月20日22点」「下周一早上8点」等具体某一时刻）**必须用 once**，并根据今天日期推算出完整的 'YYYY-MM-DDTHH:MM'，**不要用 at**（at 会每天重复）。只有用户明确说「每天」时才用 at。
- 推送对象（recipient）由框架自动填充为当前消息的发送者，无需指定。
- 创建或修改成功后，在回复中向用户确认下一次触发时间。
- 需要定时操作某个数据子项目时，用 project 参数指定该项目名，并把具体操作写进 message（触发时会跳过路由，直接执行该项目）。
- 纯提醒任务（不操作任何子项目，如「提醒我喝水」）不要填 project；此时 message 会作为提醒语原样推送给用户，请写成简洁友好的提醒（如「该喝水了」）。

# 输出图片给用户
- 若任务需要给用户发图片（如生成图表、导出报表），先用 run 执行数据子项目内**已存在**的脚本，把图片写到该子项目目录下。
- 然后在最终回复里用 Markdown 图片语法引用它，路径用**绝对路径**：![描述](/数据子项目内图片的绝对路径)
- 只能引用你确实生成的、位于数据子项目内的图片文件，扩展名需为 png/jpg/jpeg/gif/webp/bmp；不要引用不存在或目录外的路径（会被忽略）。
- 子项目没有可产出图片的脚本时，不要凭空引用图片，直接用文字回复。

# 通用 JSON 操作守则
- 只在你被允许的目录内读写；未知或越界路径直接拒绝。
- 新增数据时保持既有数据结构与字段命名不变，不凭空发明字段。
- 查询类任务只读不写。
- 若 read 发现文件结构与预期不符，以文件实际内容为准，必要时向用户说明。
- 任何修改都必须保持 JSON 合法与字段类型一致；写完后简单复述你改了什么。

# 回复风格
- 面向用户的回复可以自然地加入 Emoji，让语气更友好、重点更醒目。常用映射：
  ✅ 完成 / 创建或修改成功、⏰ 定时任务 / 提醒、🔍 查询 / 查词、📝 记录写入、
  ⚠️ 出错或需用户注意、💡 建议 / 提示、📊 统计 / 汇总。
- 每条回复 1-2 个即可，点到为止，不要堆砌；Emoji 只出现在给用户的回复正文里，不要写进数据文件。
`

// ruleFiles are the convention doc names, by priority (a project uses the first that exists).
var ruleFiles = []string{"AGENTS.md", "CLAUDE.md", "CODEBUDDY.md"}

const (
	ruleMaxChars = 12000
	summaryChars = 300
)

// ListProjectDirs returns first-level, non-hidden subdir names under DATA_ROOT (sorted).
func ListProjectDirs() []string { return paths.ListProjects() }

func readRuleFile(projectDir string) (string, bool) {
	for _, name := range ruleFiles {
		p := filepath.Join(projectDir, name)
		if raw, err := os.ReadFile(p); err == nil {
			return string(raw), true
		}
	}
	return "", false
}

var firstTitleRe = regexp.MustCompile(`(?m)^\s*#.*$`)

func projectSummary(name string) string {
	content, ok := readRuleFile(filepath.Join(config.C().DataRoot, name))
	if !ok {
		return "（无约定文档）"
	}
	// v1's /^\s*#.*$/m has no "g" flag: only the first heading line is removed.
	if loc := firstTitleRe.FindStringIndex(content); loc != nil {
		content = content[:loc[0]] + content[loc[1]:]
	}
	s := strings.TrimSpace(content)
	if len([]rune(s)) > summaryChars {
		s = string([]rune(s)[:summaryChars]) + "…"
	}
	return strings.Join(strings.Fields(s), " ")
}

// ListProjectUsage renders the user-facing project list with one-line descriptions.
func ListProjectUsage() string {
	names := ListProjectDirs()
	if len(names) == 0 {
		return "（DATA_ROOT 下暂无子项目）"
	}
	lines := make([]string, 0, len(names))
	for _, n := range names {
		lines = append(lines, "· "+n+"："+projectSummary(n))
	}
	return strings.Join(lines, "\n")
}

var routingHintRe = regexp.MustCompile(`路由提示(?:（botler-agent）)?[:：][\s\S]*?[「『]([^」』]*)[」』]`)

// ProjectCapabilities maps each subproject to its self-described capability line
// (the「...」keyword list of its 路由提示 line).
func ProjectCapabilities() map[string]string {
	out := make(map[string]string, 8)
	for _, name := range ListProjectDirs() {
		content, ok := readRuleFile(filepath.Join(config.C().DataRoot, name))
		if !ok {
			continue
		}
		if m := routingHintRe.FindStringSubmatch(content); m != nil {
			out[name] = strings.TrimSpace(m[1])
		}
	}
	return out
}

func listProjects() string {
	names := ListProjectDirs()
	if len(names) == 0 {
		return "（DATA_ROOT 下暂无子项目）"
	}
	lines := make([]string, 0, len(names))
	for _, n := range names {
		lines = append(lines, "- "+n+"/")
	}
	return strings.Join(lines, "\n")
}

func listProjectSummaries() string {
	names := ListProjectDirs()
	if len(names) == 0 {
		return "（DATA_ROOT 下暂无子项目）"
	}
	lines := make([]string, 0, len(names))
	for _, n := range names {
		lines = append(lines, "- "+n+"/："+projectSummary(n))
	}
	return strings.Join(lines, "\n")
}

func projectContext(name string) string {
	content, ok := readRuleFile(filepath.Join(config.C().DataRoot, name))
	if !ok {
		return "（" + name + "/ 无 AGENTS.md / CLAUDE.md / CODEBUDDY.md 约定文档）"
	}
	truncated := len([]rune(content)) > ruleMaxChars
	body := content
	if truncated {
		body = string([]rune(content)[:ruleMaxChars])
	}
	note := "（约定文档内容"
	if truncated {
		note += "，已截断"
	}
	note += "）"
	return "## " + name + "/\n" + note + "\n" + body
}

func projectsContext(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, projectContext(n))
	}
	return strings.Join(parts, "\n\n")
}

func schedulerContext() string {
	return `## ` + SchedulerVirtualProject + `/（定时任务管理）
当前请求属于「定时任务管理」，与任何数据子项目无关。
- 只使用 schedule 工具创建 / 查看 / 修改 / 删除 / 启用 / 停用定时任务；不要用 read / write / edit / run 去读写数据子项目文件，也不要查数据。
- 从用户自然语言中提取触发方式，**四选一**：cron（5 字段，复杂周期）、interval（如 2h/1d，重复）、at（**每天**固定 HH:MM，会每天重复）、once（**一次性**绝对时间，ISO 8601 必须带完整日期，触发一次后失效）。
- **关键区分**：一次性提醒（「今晚10点」「明天下午3点」「8月20日22点」「下周一早上8点」等具体某一时刻）必须用 **once**，根据今天日期（环境中给出的今天日期）推算出完整的 'YYYY-MM-DDTHH:MM'（可带 '+08:00' 时区偏移，或依赖条目默认时区 Asia/Shanghai）；**不要用 at**（at 每天重复）。只有用户明确说「每天 X 点」时才用 at。另提供 message（触发时发给 Agent 的指令）、可选的 timezone（默认 Asia/Shanghai）与 project。
- 需要定时操作某个数据子项目时，用 project 指定子项目名，并把具体操作写进 message（触发时会跳过路由，直接执行该项目）。
- 纯提醒类任务（不操作任何数据子项目，如「提醒我喝水」）不要填 project；此时 message 会作为提醒语原样推送给用户，请写成直接对用户说的简短提醒（如「该喝水了，起来活动一下吧」），不要写成给 Agent 的操作指令。
- **直接运行脚本（绕开 LLM）**：当用户希望定时「直接执行某个已存在的项目内脚本」（如「每天凌晨2点跑 notes/scripts/build.py 重新生成报告」），用 **script** 动作创建：填「script」（相对 DATA_ROOT 的路径，如「notes/scripts/build.py」，仅支持 .py/.js/.mjs），可选「scriptArgs」（字符串数组）与「commit」（是否自动 git 提交数据变更，默认 true，只读/仅报告类脚本可设 false）。**脚本动作与 message 二选一，不能同时填，也不能都不填。** 触发时框架直接执行该脚本（走 run 工具的安全边界：路径白名单、固定解释器、无 shell、60s 超时），不消耗 LLM、结果确定。适用场景：确定性、可重复的后处理/生成任务。
- **脚本幂等性与重试（重要）**：脚本触发是 at-least-once，失败会重试。若脚本**幂等**（重跑同一参数得到相同终态，如重算覆盖报告、upsert、「git add -A」），可设「retry: { max: N, backoffMs: M }」让瞬时失败自愈；若脚本**非幂等**（发消息、追加、调用有副作用的外部 API），**必须**用「retry: { max: 0 }」（即不填 retry 的默认行为），这样失败只执行一次并上报，不会重复副作用。脚本若产出非法数据（JSON 损坏）会被记为「validation-failed」并推送告警，但**不会**自动重试，也不会回滚——需用户自行修复脚本/数据。
- **中国法定工作日（仅工作日触发）**：当用户说「按法定工作日」「工作日（含调休）」「法定工作时间」等，希望跳过法定假日、在调休补班日也触发时，用 holidayMode: "workday" + 一个**每天**的 cron（如 0 18 * * *）创建；框架会自动跳过法定假日、并在调休补班日触发。此时 cron 的日期字段被忽略，只使用「时刻」(hour:minute)，例如 0 8,18 * * * 会在每个工作日 8 点和 18 点各触发一次。holidayMode 不可与 once 组合；纯周末/每天类需求不要用此模式。
- 创建或修改成功后，在回复中确认下一次触发时间；用户不记得 id 时先 schedule list 再确认。`
}

// Today returns the local date as YYYY-MM-DD.
func Today() string {
	return time.Now().Format("2006-01-02")
}

// LoadSystemPrompt renders the execution-phase system prompt for the selected projects.
func LoadSystemPrompt(projectNames []string) string {
	base := DefaultSystemPrompt
	userPrompt := filepath.Join(config.UserConfigDir(), "system-prompt.md")
	if raw, err := os.ReadFile(userPrompt); err == nil {
		base = string(raw)
	}
	var ctx string
	switch {
	case len(projectNames) == 0:
		ctx = listProjectSummaries()
	case len(projectNames) == 1 && projectNames[0] == SchedulerVirtualProject:
		ctx = schedulerContext()
	default:
		ctx = projectsContext(projectNames)
	}
	dataRoot := config.C().DataRoot
	replacer := strings.NewReplacer(
		"__DATA_ROOT__", dataRoot,
		"__PROJECTS__", listProjects(),
		"__PROJECT_CONTEXT__", ctx,
		"__TODAY__", Today(),
	)
	return replacer.Replace(base)
}

// RouteOptions tunes BuildRoutePrompt.
type RouteOptions struct {
	IncludeScheduler  bool
	HasImages         bool
	RecentTurns       string // pre-rendered history block ("" = none)
	AllowResetContext bool
}

// routeHistory is the rendered recent-turn block used by the routing phase.
type routeHistory struct {
	block string
	has   bool
}

// BuildRoutePrompt renders the lightweight routing prompt (phase 1). It receives only
// "project name + first 300 chars of the convention doc" so it never grows with the number of
// subprojects.
func BuildRoutePrompt(userMessage string, opts RouteOptions) string {
	schedulerLine := ""
	if opts.IncludeScheduler {
		schedulerLine = "- " + SchedulerVirtualProject + "/：仅用于创建/管理定时任务（与数据子项目无关）\n"
	}
	imageLine := ""
	if opts.HasImages {
		imageLine = "\n本条消息附带图片。请同时参考图片内容与文字判断所属子项目：图片内容明显匹配某个子项目的描述（如食物照片 → 记录饮食的项目）时选择该项目；无法根据图片与文字确定时输出 UNKNOWN。\n"
	}
	historyBlock := ""
	historyInstruction := ""
	if opts.RecentTurns != "" {
		historyBlock = "\n最近对话：\n" + opts.RecentTurns + "\n"
		historyInstruction = "\n如果当前消息是上一轮任务的简短确认或补充，优先沿用最近对话对应的项目；如果当前消息明显是一个新的独立任务，忽略旧历史并按新任务判断；无法确定时输出 UNKNOWN。\n"
	}
	resetInstruction := ""
	if opts.AllowResetContext {
		resetInstruction = "\n如果用户明确要求清空 / 忽略 / 重置之前的上下文（例如「新任务」「忽略上文」「重置上下文」，或等价表达），只输出 " + ResetContextDecision + "。若用户的新任务还带有具体的数据操作，则仍输出对应项目，不要输出 " + ResetContextDecision + "。\n"
	}
	jsonInstruction := `只输出 JSON：{"projects":["项目名"],"attachmentProject":"项目名或null"}。
projects 必须是一个或多个给定候选项目名（按需要依次列出）；无法确定时只输出 UNKNOWN。
如果选择 ` + SchedulerVirtualProject + `，projects 必须只能是 ["` + SchedulerVirtualProject + `"]，且 attachmentProject 必须为 null。
有图片时 attachmentProject 必须是 projects 中唯一一个用于保存原图的项目；无图片时写 null。`
	outputCandidates := jsonInstruction
	if opts.AllowResetContext {
		outputCandidates += "\n若用户明确要求清空 / 忽略 / 重置之前的上下文，只输出 " + ResetContextDecision + "。"
	}
	return `你是路由助手，负责判断用户消息要操作哪个（或哪几个）数据子项目。只输出 JSON（不要输出解释文字），不要使用任何工具。

数据根目录下的子项目：
` + listProjectSummaries() + schedulerLine + historyBlock + `用户消息：` + userMessage + `
` + imageLine + historyInstruction + resetInstruction + `判断这条消息属于哪个（或哪几个）子项目。` + outputCandidates
}
