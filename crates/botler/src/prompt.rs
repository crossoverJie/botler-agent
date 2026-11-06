//! System prompt construction, project convention loading, and routing prompts.
//!
//! The framework prompt is deliberately generic: each data subproject describes
//! its own business schema in a root `AGENTS.md` / `CLAUDE.md` /
//! `CODEBUDDY.md`, which is concatenated at execution time.

use std::fs;
use std::path::Path;

use chrono::Local;

use crate::config::{Config, USER_CONFIG_FILE_NAME};
use crate::paths::PathGuard;

pub const SCHEDULER_VIRTUAL_PROJECT: &str = "__scheduler__";
pub const RESET_CONTEXT_DECISION: &str = "RESET_CONTEXT";

const RULE_FILES: [&str; 3] = ["AGENTS.md", "CLAUDE.md", "CODEBUDDY.md"];
const RULE_MAX_CHARS: usize = 12_000;
const SUMMARY_CHARS: usize = 300;

pub const DEFAULT_SYSTEM_PROMPT: &str = r#"你是运行在个人数据目录下的轻量助手。你只在 __DATA_ROOT__ 目录内的各数据子项目中工作，禁止访问目录外的任何文件或执行命令。

你的能力：通过 read / write / edit / run 四个工具读写白名单内的文件、执行项目内脚本，完成各数据子项目中的短任务；可以用 schedule 工具创建 / 管理定时任务；可以用 retain / recall 维护跨任务的长期记忆。

# 当前环境
- 今天日期：以本轮「本轮信息」信封中的日期为准（本地时区；涉及「今天/昨天」等相对日期的任务以它为准）。系统提示词会跨天复用，因此这里不写具体日期。

# 当前数据子项目与约定
__PROJECT_CONTEXT__

# 长期记忆
- 你有三层记忆：个人会话记忆（默认）、项目共享记忆、用户全局记忆。
- retain 默认写入个人会话记忆。只有用户明确要求、或内容确实需要被其他项目共享时，才写入项目共享记忆；只有用户明确要求记住、或属于跨项目的非敏感事实（如一笔账目）时，才写入用户全局记忆。
- 健康、亲密关系、精确位置、凭证、以及超出交易汇总范围的财务细节等敏感信息，绝不自动写入用户全局记忆，必须由用户明确要求。
- 历史消息会被压缩，早期对话可能不再逐字保留。任何用户以后可能需要的结论、数字或文件路径，都应在被压缩前写入数据文件或 retain。
- 需要跨项目联想时，用 recall 检索记忆，不要直接读取其他项目的数据文件。
__MEMORY_DIGEST__

# 工作原则
- 你只能操作下面“当前数据子项目与约定”列出的项目；可以按任务需要依次操作其中多个项目，绝不能操作未列出的数据子项目。
- 修改文件前先用 read 读取现有内容，在上下文中修改后整体写回。
- 必须保持 JSON 合法、字段类型一致（数字就是数字，字符串就是字符串）。
- 写完后用平实的中文向用户汇报结果，给出关键数字。

# 本轮上下文（重要）
- 每条新消息开头都会附带一段「本轮上下文」信封，给出当前日期、发送者与当前项目。
- 只有**本轮**信封中的日期与发送者是最新事实；历史消息里的同类信息只代表当时，不得当作当前事实复述。
- 如果用户要求清空 / 忽略之前的上下文，确认后按新任务处理，不要引用旧话题。

# run 工具（执行项目内脚本）
- 仅用于执行数据子项目内**已存在**的脚本（python3 的 .py / node 的 .js/.mjs），例如项目约定中的构建/刷新脚本。
- 当项目约定要求「写数据后跑某脚本」时，写完数据后调用 run 执行它；项目没有此类脚本则跳过。
- 不经过 shell、参数直接传递；不要尝试用它执行任意命令或非脚本内容。

# schedule 工具（管理定时任务）
- 当用户表达「定时 / 提醒 / 日程」意图时，使用 schedule 工具创建定时任务，而不是文件读写。
- create 必须且只能指定四种触发器之一，并提供非空的 message（触发时发给 Agent 的指令）：
  - cron：5 字段 cron，复杂周期用，如 '0 8 * * 1-5'。
  - interval：简单间隔，如 '5m' / '2h' / '1d'，会重复。
  - at：**每天**固定时间 'HH:MM'，会每天重复。仅当用户明确说「每天 X 点」时使用。
  - once：**一次性**绝对时间（ISO 8601，必须带完整日期），如 '2026-08-20T22:00:00+08:00'，触发一次后失效。
- **关键区分**：一次性提醒（「今晚10点」「明天下午3点」「8月20日22点」等）必须用 once，并根据今天日期推算完整日期；不要用 at。
- 推送对象（recipient）由框架自动填充为当前消息的发送者，无需指定。
- 创建或修改成功后，在回复中确认下一次触发时间。
- 纯提醒任务不要填 project；此时 message 会作为提醒语原样推送，请写成简洁友好的提醒。
- 需要定时执行项目内脚本时，用 script 动作（相对 DATA_ROOT 的 .py/.js/.mjs 路径），不要用 message 绕一圈。

# 输出图片给用户
- 若任务需要给用户发图片，先用 run 执行项目内**已存在**的脚本，把图片写到该项目目录下。
- 然后在最终回复里用 Markdown 图片语法引用，路径用**绝对路径**：![描述](/数据子项目内图片的绝对路径)
- 只能引用确实生成的、位于数据子项目内的图片文件（png/jpg/jpeg/gif/webp/bmp）。

# 回复风格
- 面向用户的回复可以自然地加入少量 Emoji，让语气更友好，每条 1-2 个即可。
"#;

/// Extract the "路由提示" keyword list from a project's conventions.
fn extract_routing_hint(content: &str) -> Option<String> {
    // Mirrors /路由提示(?:（botler-agent）)?[:：][\s\S]*?[「『]([^」』]*)[」』]/
    let idx = content.find("路由提示")?;
    let rest = &content[idx..];
    let colon = rest.find([':', '：'])?;
    let colon_len = rest[colon..].chars().next()?.len_utf8();
    let after = &rest[colon + colon_len..];
    let start = after.find(['「', '『'])?;
    let start_len = after[start..].chars().next()?.len_utf8();
    let tail = &after[start + start_len..];
    let end = tail.find(['」', '』'])?;
    Some(tail[..end].trim().to_string()).filter(|s| !s.is_empty())
}

pub fn list_project_dirs(guard: &PathGuard) -> Vec<String> {
    guard.project_names()
}

fn read_rule_file(project_dir: &Path) -> Option<String> {
    for name in RULE_FILES {
        let path = project_dir.join(name);
        if path.is_file() {
            return fs::read_to_string(path).ok();
        }
    }
    None
}

fn project_summary(guard: &PathGuard, name: &str) -> String {
    let Some(root) = guard.project_root(name) else {
        return "（无约定文档）".into();
    };
    let Some(content) = read_rule_file(root) else {
        return "（无约定文档）".into();
    };
    let mut s = strip_first_heading(&content);
    if s.chars().count() > SUMMARY_CHARS {
        s = s.chars().take(SUMMARY_CHARS).collect::<String>() + "…";
    }
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn strip_first_heading(content: &str) -> String {
    // Remove the first line that is a markdown heading, then trim.
    let mut out = String::new();
    let mut removed = false;
    for line in content.lines() {
        if !removed && line.trim_start().starts_with('#') {
            removed = true;
            continue;
        }
        out.push_str(line);
        out.push('\n');
    }
    out.trim().to_string()
}

pub fn project_capabilities(guard: &PathGuard) -> Vec<(String, Option<String>)> {
    guard
        .project_names()
        .into_iter()
        .map(|name| {
            let hint = guard
                .project_root(&name)
                .and_then(read_rule_file)
                .and_then(|c| extract_routing_hint(&c));
            (name, hint)
        })
        .collect()
}

fn list_projects_block(guard: &PathGuard) -> String {
    let names = guard.project_names();
    if names.is_empty() {
        return "（DATA_ROOT 下暂无子项目）".into();
    }
    names
        .iter()
        .map(|n| format!("- {n}/"))
        .collect::<Vec<_>>()
        .join("\n")
}

fn list_project_summaries(guard: &PathGuard) -> String {
    let names = guard.project_names();
    if names.is_empty() {
        return "（DATA_ROOT 下暂无子项目）".into();
    }
    names
        .iter()
        .map(|n| format!("- {n}/：{}", project_summary(guard, n)))
        .collect::<Vec<_>>()
        .join("\n")
}

fn project_context(guard: &PathGuard, name: &str) -> String {
    let Some(root) = guard.project_root(name) else {
        return format!("（{name}/ 不在允许目录中）");
    };
    let Some(content) = read_rule_file(root) else {
        return format!("（{name}/ 无 AGENTS.md / CLAUDE.md / CODEBUDDY.md 约定文档）");
    };
    let truncated = content.chars().count() > RULE_MAX_CHARS;
    let body: String = if truncated {
        content.chars().take(RULE_MAX_CHARS).collect()
    } else {
        content
    };
    format!(
        "## {name}/\n（约定文档内容{}）\n{body}",
        if truncated { "，已截断" } else { "" }
    )
}

pub fn scheduler_context() -> String {
    format!(
        r#"## {SCHEDULER_VIRTUAL_PROJECT}/（定时任务管理）
当前请求属于「定时任务管理」，与任何数据子项目无关。
- 只使用 schedule 工具创建 / 查看 / 修改 / 删除 / 启用 / 停用定时任务；不要读写数据子项目文件，也不要查数据。
- 从用户自然语言中提取触发方式，**四选一**：cron、interval、at（每天固定 HH:MM）、once（一次性绝对时间，ISO 8601 必须带完整日期）。
- **关键区分**：一次性提醒必须用 once；at 每天重复。只有用户明确说「每天 X 点」时才用 at。
- 需要定时操作某个数据子项目时，用 project 指定子项目名，并把具体操作写进 message。
- 纯提醒类任务不要填 project；message 写成直接对用户说的简短提醒。
- **直接运行脚本（绕开 LLM）**：用 script 动作，填相对 DATA_ROOT 的 .py/.js/.mjs 路径，可选 scriptArgs 与 commit。脚本动作与 message 二选一。
- **中国法定工作日**：用户说「按法定工作日」「工作日（含调休）」时，用 holidayMode: "workday" + 一个每天的 cron；只使用时刻字段，自动跳过法定假日、在调休补班日触发。不可与 once 组合。
- 创建或修改成功后，确认下一次触发时间。"#
    )
}

/// Load the base template: externalized `system-prompt.md` first, built-in fallback.
pub fn load_base_template(config: &Config) -> String {
    let path = config.user_config_dir.join(USER_CONFIG_FILE_NAME);
    match fs::read_to_string(path) {
        Ok(s) if !s.trim().is_empty() => s,
        _ => DEFAULT_SYSTEM_PROMPT.to_string(),
    }
}

/// Render the execution-phase system prompt.
pub fn build_system_prompt(
    guard: &PathGuard,
    config: &Config,
    projects: &[String],
    memory_digest: Option<&str>,
) -> String {
    let base = load_base_template(config);
    let ctx = if projects.is_empty() {
        list_project_summaries(guard)
    } else if projects.len() == 1 && projects[0] == SCHEDULER_VIRTUAL_PROJECT {
        scheduler_context()
    } else {
        projects
            .iter()
            .map(|p| project_context(guard, p))
            .collect::<Vec<_>>()
            .join("\n\n")
    };
    let digest = match memory_digest {
        Some(d) if !d.trim().is_empty() => format!("\n# 记忆摘要\n{d}\n"),
        _ => String::new(),
    };
    // The rendered prompt is persisted with the session and reused across
    // days, so it must never embed a concrete date. `__TODAY__` stays
    // supported in external templates but resolves to a stable pointer at the
    // per-turn envelope.
    base.replace("__DATA_ROOT__", &guard.data_root().display().to_string())
        .replace("__PROJECTS__", &list_projects_block(guard))
        .replace("__PROJECT_CONTEXT__", &ctx)
        .replace("__MEMORY_DIGEST__", &digest)
        .replace("__TODAY__", "（见本轮「本轮信息」中的日期）")
}

/// v2 routing output. `attachment_project` is used when inbound images must be
/// persisted into exactly one selected project.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RouteDecision {
    pub projects: Vec<String>,
    pub attachment_project: Option<String>,
    pub reset_context: bool,
}

pub fn build_route_prompt(
    guard: &PathGuard,
    user_message: &str,
    include_scheduler: bool,
    has_images: bool,
    allow_reset: bool,
) -> String {
    let scheduler_line = if include_scheduler {
        format!("- {SCHEDULER_VIRTUAL_PROJECT}/：仅用于创建/管理定时任务（与数据子项目无关）\n")
    } else {
        String::new()
    };
    let image_line = if has_images {
        "\n本条消息附带图片。请同时参考图片内容与文字判断所属子项目；无法确定时输出 UNKNOWN。\n"
    } else {
        ""
    };
    let reset_instruction = if allow_reset {
        format!(
            "\n如果用户明确要求清空 / 忽略 / 重置之前的上下文，只输出 {RESET_CONTEXT_DECISION}。若新任务仍带有具体数据操作，则输出对应项目。\n"
        )
    } else {
        String::new()
    };
    format!(
        "你是路由助手，负责判断用户消息要操作哪个（或哪几个）数据子项目。只输出 JSON（不要输出解释文字），不要使用任何工具。\n\n\
数据根目录下的子项目：\n{}{scheduler_line}\n用户消息：{user_message}\n{image_line}{reset_instruction}\n\
只输出 JSON：{{\"projects\":[\"项目名\"],\"attachmentProject\":\"项目名或null\"}}。\n\
projects 必须是一个或多个给定候选项目名（按需要依次列出）；无法确定时只输出 UNKNOWN。\n\
如果选择 {SCHEDULER_VIRTUAL_PROJECT}，projects 必须只能是 [\"{SCHEDULER_VIRTUAL_PROJECT}\"]，且 attachmentProject 必须为 null。\n\
有图片时 attachmentProject 必须是 projects 中唯一一个用于保存原图的项目；无图片时写 null。",
        list_project_summaries(guard)
    )
}

/// Current local date (YYYY-MM-DD). Shared with inbound-image persistence so
/// filenames match the agent's notion of "today".
pub fn today() -> String {
    Local::now().format("%Y-%m-%d").to_string()
}

/// Fresh per-turn envelope. The system prompt declares only the current turn's
/// preamble authoritative; older preambles remain in persisted history, which
/// is the honest persistence stance of §4.6.
pub fn build_preamble(source: &str, sender: Option<&str>, project: &str) -> String {
    preamble_on(&today(), source, sender, project)
}

/// Date-injected form used to simulate a rollover without touching the clock.
pub(crate) fn preamble_on(date: &str, source: &str, sender: Option<&str>, project: &str) -> String {
    let sender = sender.unwrap_or("(unknown)");
    format!(
        "[本轮信息] 日期：{date}；渠道：{source}；发送者：{sender}；当前项目：{project}。\n\
仅以本轮信息为准；不要根据历史轮次中的旧日期或旧发送者推断当前值。\n\n"
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extracts_routing_hint() {
        let doc = "# Notes\n路由提示（botler-agent）：「记账、开销、消费」\nmore";
        assert_eq!(
            extract_routing_hint(doc).as_deref(),
            Some("记账、开销、消费")
        );
        assert_eq!(extract_routing_hint("# no hint"), None);
    }

    #[test]
    fn scheduler_context_is_virtual_project() {
        assert!(scheduler_context().contains(SCHEDULER_VIRTUAL_PROJECT));
    }

    #[test]
    fn rendered_system_prompt_carries_no_concrete_date() {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(root.path().join("notes")).unwrap();
        let guard = PathGuard::new(root.path()).unwrap();
        let config = Config::with_paths(root.path(), root.path());
        let rendered = build_system_prompt(&guard, &config, &[], None);
        assert!(!rendered.contains("__TODAY__"));
        assert!(!rendered.contains(&today()));
    }

    #[test]
    fn preamble_carries_only_the_current_turns_values() {
        let before = preamble_on("2026-11-03", "telegram", Some("alice"), "notes");
        let after = preamble_on("2026-11-04", "telegram", Some("bob"), "notes");
        assert!(before.contains("日期：2026-11-03"));
        assert!(before.contains("发送者：alice"));
        assert!(after.contains("日期：2026-11-04"));
        assert!(after.contains("发送者：bob"));
        assert!(!after.contains("2026-11-03"));
        assert!(!after.contains("alice"));
    }
}
