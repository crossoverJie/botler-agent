//! Greeting short-circuit and routing-failure replies.
//!
//! A bare greeting carries no subproject, so routing would return `UNKNOWN` and
//! a confusing template. These replies are deterministic, zero-cost, and
//! intentionally written in Chinese to match the agent's interaction language.

use crate::paths::PathGuard;
use crate::prompt::project_capabilities;

fn project_list(guard: &PathGuard) -> String {
    let caps = project_capabilities(guard);
    if caps.is_empty() {
        return "（暂无可用的数据子项目）".into();
    }
    caps.into_iter()
        .map(|(name, cap)| match cap {
            Some(c) => format!("· {name}：{c}"),
            None => format!("· {name}"),
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// True for a bare greeting. Whitespace, punctuation, symbols and combining
/// marks are stripped from both ends first (covers full-width spaces and
/// trailing emoji such as 👋 / ❤️).
pub fn is_greeting(msg: &str) -> bool {
    let stripped = strip_edges(msg);
    matches!(
        stripped.to_lowercase().as_str(),
        "你好" | "您好" | "喂" | "哈喽" | "嗨" | "hi" | "hello" | "hey" | "在吗" | "在不在"
    ) || {
        // Trailing mood particles: 啊/呀/哇/哦/啦/哟
        let core = stripped.trim_end_matches(['啊', '呀', '哇', '哦', '啦', '哟']);
        matches!(
            core.to_lowercase().as_str(),
            "你好" | "您好" | "喂" | "哈喽" | "嗨" | "hi" | "hello" | "hey" | "在吗" | "在不在"
        ) && core.len() != stripped.len()
    }
}

fn strip_edges(msg: &str) -> String {
    msg.trim()
        .trim_matches(|c: char| {
            c.is_whitespace()
                || c.is_ascii_punctuation()
                || is_unicode_punctuation(c)
                || is_symbol_or_mark(c)
        })
        .to_string()
}

fn is_unicode_punctuation(c: char) -> bool {
    // A pragmatic subset: CJK / full-width punctuation used in chat.
    matches!(
        c,
        '，' | '。'
            | '！'
            | '？'
            | '、'
            | '；'
            | '：'
            | '“'
            | '”'
            | '‘'
            | '’'
            | '（'
            | '）'
            | '【'
            | '】'
            | '《'
            | '》'
            | '…'
            | '—'
            | '～'
            | '·'
    )
}

fn is_symbol_or_mark(c: char) -> bool {
    // Emoji / symbols / combining marks. `is_control` is included so a bare
    // control-character-wrapped greeting still matches.
    c.is_control()
        || matches!(
            c,
            '👋' | '❤' | '️' | '😀' | '😊' | '🙂' | '😄' | '✨' | '～' | '~'
        )
        || ('\u{FE00}'..='\u{FE0F}').contains(&c)
        || ('\u{200D}'..='\u{200D}').contains(&c)
        || ('\u{1F300}'..='\u{1FAFF}').contains(&c)
        || ('\u{0300}'..='\u{036F}').contains(&c)
}

pub fn greeting_reply(guard: &PathGuard) -> String {
    format!(
        "你好👋！我是你的个人数据助手。\n\n\
想做什么直接说就行，比如记账、查词、记录日常，也可以让我「创建定时任务 / 提醒」。\n\n\
当前可用的子项目：\n{}",
        project_list(guard)
    )
}

pub fn context_reset_reply() -> &'static str {
    "已清空当前会话上下文。接下来我会按新任务处理。"
}

pub fn fallback_unknown_reply(guard: &PathGuard, has_images: bool) -> String {
    let image_hint = if has_images {
        "📷 我收到了你发的图片，但当前模型无法识别图片内容，所以我没确定要操作哪个子项目。\n\
请补充一句文字说明，比如「记一下这顿饭」，让我知道该写到哪个项目。\n\n"
    } else {
        ""
    };
    format!(
        "😅 我有点没跟上——没太确定你想操作哪个数据子项目。\n\n\
{image_hint}我是你的个人数据助手，只能在下面的子项目里读写数据、运行项目内脚本。想做什么直接说就行，\
比如记账、查词、记录日常；也可以让我「创建定时任务 / 提醒」。\n\n\
当前可用的子项目：\n{}\n\n\
说得更具体一点就好～",
        project_list(guard)
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_greetings_with_noise() {
        for msg in [
            "你好",
            " 你好！",
            "您好👋",
            "hi",
            "Hi!",
            "哈喽～",
            "在吗？",
            "你好啊",
            "hello ❤️",
        ] {
            assert!(is_greeting(msg), "{msg:?} should be a greeting");
        }
    }

    #[test]
    fn rejects_task_messages() {
        for msg in [
            "你好，帮我记账",
            "hello world",
            "记一下午饭",
            "hi 查词 apple",
            "",
        ] {
            assert!(!is_greeting(msg), "{msg:?} should not be a greeting");
        }
    }
}
