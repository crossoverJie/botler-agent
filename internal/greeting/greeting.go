// Package greeting detects a bare greeting and renders the fixed Chinese fallback replies.
//
// A bare greeting has no subproject to route to, so it short-circuits before the routing LLM call
// (deterministic, zero cost). These are user-facing chat strings (Chinese by design); code and
// logs stay English.
package greeting

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/crossoverJie/botler-agent/internal/prompt"
)

var greetingRe = regexp.MustCompile(`(?i)^(你好|您好|喂|哈喽|嗨|hi|hello|hey|在吗|在不在)[啊呀哇哦啦哟]*$`)

// IsGreeting reports whether msg is a bare greeting with no task content.
// Leading/trailing whitespace, punctuation, symbols and combining marks (incl. full-width spaces
// and trailing emoji such as 👋 / ❤️) are stripped before matching.
func IsGreeting(msg string) bool {
	stripped := strings.TrimFunc(strings.TrimSpace(msg), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsMark(r)
	})
	return greetingRe.MatchString(stripped)
}

func projectList() string {
	caps := prompt.ProjectCapabilities()
	names := prompt.ListProjectDirs()
	if len(names) == 0 {
		return "（暂无可用的数据子项目）"
	}
	lines := make([]string, 0, len(names))
	for _, n := range names {
		if c := caps[n]; c != "" {
			lines = append(lines, "· "+n+"："+c)
		} else {
			lines = append(lines, "· "+n)
		}
	}
	return strings.Join(lines, "\n")
}

// Reply is the welcome text for a bare greeting (the routing LLM call is skipped entirely).
func Reply() string {
	return "你好👋！我是你的个人数据助手。\n\n" +
		"想做什么直接说就行，比如记账、查词、记录日常，也可以让我「创建定时任务 / 提醒」。\n\n" +
		"当前可用的子项目：\n" + projectList()
}

// ContextResetReply is the confirmation for a context-reset command.
func ContextResetReply() string {
	return "已清空当前会话上下文。接下来我会按新任务处理。"
}

// FallbackUnknownReply is the static fallback when a message cannot be routed to any subproject.
// When the message carried inbound images, it explains that the model may not see them and asks
// for a short text hint.
func FallbackUnknownReply(hasImages bool) string {
	imageHint := ""
	if hasImages {
		imageHint = "📷 我收到了你发的图片，但当前模型无法识别图片内容，所以我没确定要操作哪个子项目。\n" +
			"请补充一句文字说明，比如「记一下这顿饭」，让我知道该写到哪个项目。\n\n"
	}
	return "😅 我有点没跟上——没太确定你想操作哪个数据子项目。\n\n" +
		imageHint +
		"我是你的个人数据助手，只能在下面的子项目里读写数据、运行项目内脚本。想做什么直接说就行，" +
		"比如记账、查词、记录日常；也可以让我「创建定时任务 / 提醒」。\n\n" +
		"当前可用的子项目：\n" + projectList() + "\n\n" +
		"说得更具体一点就好～"
}
