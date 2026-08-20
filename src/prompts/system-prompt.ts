import { readFileSync, existsSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { CONFIG, USER_CONFIG_DIR } from "../config.ts";

/**
 * Built-in generic default system prompt: only describes boundaries and generic working principles, with no specific business schema.
 * Each data subproject's conventions (AGENTS.md / CLAUDE.md, etc.) are concatenated at runtime via __PROJECT_CONTEXT__ as needed.
 * This file is also the source of the externalized ~/.botler-agent/system-prompt.md template.
 */
export const DEFAULT_SYSTEM_PROMPT = `你是运行在个人数据目录下的轻量助手。你只在 __DATA_ROOT__ 目录内的各数据子项目中工作，禁止访问目录外的任何文件或执行命令。

你的能力：通过 read / write / edit / run 四个工具读写白名单内的 JSON 文件、执行项目内脚本，完成各数据子项目中的短任务；还可以用 schedule 工具创建 / 管理定时任务。每条消息都是一次独立任务，没有跨任务记忆。

# 当前环境
- 今天日期：__TODAY__（YYYY-MM-DD，本地时区；涉及「今天/昨天」等相对日期的任务以它为准）。

# 当前数据子项目与约定
__PROJECT_CONTEXT__

# 工作原则
- 你已被路由到上面的数据子项目，直接按该项目约定完成任务即可；不要操作其他子项目。
- 修改文件前先用 read 读取现有内容，在上下文中修改后整体写回（write 接受数据结构本身，不是 JSON 文本）。
- 必须保持 JSON 合法、字段类型一致（数字就是数字，字符串就是字符串）。
- 写完后用平实的中文向用户汇报结果，给出关键数字。

# run 工具（执行项目内脚本）
- 仅用于执行数据子项目内**已存在**的脚本（python3 的 .py / node 的 .js/.mjs），例如项目约定（AGENTS.md）中的构建/刷新脚本（build.py 等）。
- 当项目约定要求「写数据后跑某脚本」时，写完数据后调用 run 执行它；项目没有此类脚本则跳过，不要凭空执行。
- 不经过 shell、参数直接传递；不要尝试用它执行任意命令或非脚本内容。

# schedule 工具（管理定时任务）
- 当用户表达「定时 / 提醒 / 日程」意图（如「每天早上8点提醒我喝水」「每隔2小时统计一次」「10分钟后提醒我」）时，使用 schedule 工具创建定时任务，而不是尝试用文件读写。
- create 必须且只能指定 cron / interval / at 之一，并提供非空的 message（触发时发回给 Agent 的指令，需写清具体操作）。
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
`;

/** Common Agent convention doc names (by priority; a project uses the first one that exists). */
const RULE_FILES = ["AGENTS.md", "CLAUDE.md", "CODEBUDDY.md"];

/** Max characters of a single convention doc to concatenate, to avoid huge docs blowing up the context. */
const RULE_MAX_CHARS = 12000;

/** Max characters of a project summary for routing. */
const SUMMARY_CHARS = 300;

/** First-level, non-hidden subdir names under DATA_ROOT (stable sort). */
export function listProjectDirs(): string[] {
	if (!CONFIG.dataRoot) return [];
	try {
		return readdirSync(CONFIG.dataRoot, { withFileTypes: true })
			.filter((d) => d.isDirectory() && !d.name.startsWith("."))
			.map((d) => d.name)
			.sort((a, b) => a.localeCompare(b));
	} catch {
		return [];
	}
}

/** Read a project's root convention doc content (RULE_FILES by priority). */
function readRuleFile(projectDir: string): string | null {
	for (const n of RULE_FILES) {
		const p = join(projectDir, n);
		if (existsSync(p)) {
			try {
				return readFileSync(p, "utf8");
			} catch {
				return null;
			}
		}
	}
	return null;
}

/** Project summary (for routing): the convention doc with its first-title line removed, then truncated. */
function projectSummary(name: string): string {
	const content = readRuleFile(join(CONFIG.dataRoot, name));
	if (!content) return "（无约定文档）";
	let s = content.replace(/^\s*#.*$/m, "").trim();
	if (s.length > SUMMARY_CHARS) s = `${s.slice(0, SUMMARY_CHARS)}…`;
	return s.replace(/\s+/g, " ").trim();
}

/** User-facing list of available projects with their descriptions (used in the "couldn't decide" hint). */
export function listProjectUsage(): string {
	const names = listProjectDirs();
	if (names.length === 0) return "（DATA_ROOT 下暂无子项目）";
	return names.map((n) => `· ${n}：${projectSummary(n)}`).join("\n");
}

/** Short project-name list (string, for the __PROJECTS__ placeholder, compatible with externalized prompts). */
function listProjects(): string {
	const names = listProjectDirs();
	if (names.length === 0) return "（DATA_ROOT 下暂无子项目）";
	return names.map((n) => `- ${n}/`).join("\n");
}

/** Project list + summaries (used in the routing phase, small). */
function listProjectSummaries(): string {
	const names = listProjectDirs();
	if (names.length === 0) return "（DATA_ROOT 下暂无子项目）";
	return names.map((n) => `- ${n}/：${projectSummary(n)}`).join("\n");
}

/** The full convention doc of a given project (used in the execution phase). */
function projectContext(name: string): string {
	const content = readRuleFile(join(CONFIG.dataRoot, name));
	if (!content) return `（${name}/ 无 AGENTS.md / CLAUDE.md / CODEBUDDY.md 约定文档）`;
	const truncated = content.length > RULE_MAX_CHARS;
	return `## ${name}/\n（约定文档内容${truncated ? "，已截断" : ""}）\n${truncated ? content.slice(0, RULE_MAX_CHARS) : content}`;
}

/**
 * Virtual "project" context for schedule management requests (no data subproject involved).
 * Routed to when the message is about creating/managing scheduled tasks.
 */
export const SCHEDULER_VIRTUAL_PROJECT = "__scheduler__";

function schedulerContext(): string {
	return `## ${SCHEDULER_VIRTUAL_PROJECT}/（定时任务管理）
当前请求属于「定时任务管理」，与任何数据子项目无关。
- 只使用 schedule 工具创建 / 查看 / 修改 / 删除 / 启用 / 停用定时任务；不要用 read / write / edit / run 去读写数据子项目文件，也不要查数据。
- 从用户自然语言中提取：触发方式（cron 5 字段 / interval 如 2h / at 每日 HH:MM，三选一）、message（触发时发给 Agent 的指令）、可选的 timezone（默认 Asia/Shanghai）与 project。
- 需要定时操作某个数据子项目时，用 project 指定子项目名，并把具体操作写进 message（触发时会跳过路由，直接执行该项目）。
- 纯提醒类任务（不操作任何数据子项目，如「提醒我喝水」）不要填 project；此时 message 会作为提醒语原样推送给用户，请写成直接对用户说的简短提醒（如「该喝水了，起来活动一下吧」），不要写成给 Agent 的操作指令。
- 创建或修改成功后，在回复中确认下一次触发时间；用户不记得 id 时先 schedule list 再确认。`;
}

/**
 * Routing prompt (phase 1): decide which data subproject the user message targets.
 * Outputs only a project name or UNKNOWN, uses no tools; input is just "name + summary", very small.
 * The virtual `__scheduler__` project is included as a candidate for schedule-management messages,
 * unless `includeScheduler` is false (scheduler-fired reminders are never schedule-management requests).
 */
export function buildRoutePrompt(userMessage: string, includeScheduler = true): string {
	const schedulerLine = includeScheduler
		? `- ${SCHEDULER_VIRTUAL_PROJECT}/：仅用于创建/管理定时任务（与数据子项目无关）\n`
		: "";
	return `你是路由助手，负责判断用户消息要操作哪个数据子项目。只输出一个项目名（不含斜杠），无法确定时只输出 UNKNOWN。不要使用任何工具。

数据根目录下的子项目：
${listProjectSummaries()}${schedulerLine}
用户消息：${userMessage}

判断这条消息属于哪个子项目。只输出项目名（如 my-project）或 UNKNOWN。`;
}

/**
 * Load the execution-phase system prompt (phase 2):
 * 1. ~/.botler-agent/system-prompt.md exists → use the externalized one; otherwise fall back to the built-in default;
 * 2. for a real data subproject, concatenate only that project's convention doc in full, by projectName;
 *    for the virtual `__scheduler__` project, use the schedule-management context instead (no data rules);
 * 3. uniformly replace placeholders.
 */
export function loadSystemPrompt(projectName?: string): string {
	let base = DEFAULT_SYSTEM_PROMPT;
	const userPrompt = join(USER_CONFIG_DIR, "system-prompt.md");
	if (existsSync(userPrompt)) {
		try {
			base = readFileSync(userPrompt, "utf8");
		} catch {
			// Fall back to built-in on read failure
		}
	}
	const ctx =
		projectName === SCHEDULER_VIRTUAL_PROJECT
			? schedulerContext()
			: projectName
				? projectContext(projectName)
				: listProjectSummaries();
	return base
		.replaceAll("__DATA_ROOT__", CONFIG.dataRoot)
		.replaceAll("__PROJECTS__", listProjects())
		.replaceAll("__PROJECT_CONTEXT__", ctx)
		.replaceAll("__TODAY__", today());
}

/** Today's date in the local timezone (YYYY-MM-DD). */
function today(): string {
	const d = new Date();
	const m = String(d.getMonth() + 1).padStart(2, "0");
	const day = String(d.getDate()).padStart(2, "0");
	return `${d.getFullYear()}-${m}-${day}`;
}
