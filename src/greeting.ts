/**
 * Greeting detection + friendly fallback text for the routing layer.
 *
 * A bare greeting (e.g. "你好") has no subproject to route to, so instead of sending it
 * through the routing LLM (which would return UNKNOWN and a confusing English template),
 * we short-circuit with a deterministic, zero-cost welcome. Replies here are user-facing
 * chat text (kept in the user's language, Chinese), while all code/comments stay English.
 */

import { projectCapabilities } from "./prompts/system-prompt.ts";

/**
 * Returns true for a bare greeting that carries no task content.
 * Leading/trailing whitespace, punctuation, symbols and combining marks (incl. full-width
 * spaces and trailing emoji such as 👋 / ❤️) are stripped first, then the remaining core
 * word is matched against a small greeting set.
 */
export function isGreeting(msg: string): boolean {
	const stripped = msg
		.trim()
		.replace(/^[\s\p{P}\p{S}\p{M}]+|[\s\p{P}\p{S}\p{M}]+$/gu, "");
	return /^(你好|您好|喂|哈喽|嗨|hi|hello|hey|在吗|在不在)[啊呀哇哦啦哟]*$/i.test(stripped);
}

/**
 * Returns true for an explicit request to clear the shared conversation context.
 * Like greetings, this is matched only when the message contains no additional task content.
 */
export function isContextResetRequest(msg: string): boolean {
	const stripped = msg
		.trim()
		.replace(/^[\s\p{P}\p{S}\p{M}]+|[\s\p{P}\p{S}\p{M}]+$/gu, "");
	return /^(新任务|新对话|忽略上文|忽略历史|重置上下文|重置会话|清空上下文|new task|ignore previous|ignore context|reset context|clear context)$/i.test(
		stripped,
	);
}

/** Compact, IM-friendly list of subprojects with a one-line "what it does" each. */
function projectList(): string {
	const caps = projectCapabilities();
	const names = Object.keys(caps);
	if (names.length === 0) return "（暂无可用的数据子项目）";
	return names.map((n) => (caps[n] ? `· ${n}：${caps[n]}` : `· ${n}`)).join("\n");
}

/** Welcome reply for a bare greeting; the routing LLM call is skipped entirely. */
export function greetingReply(): string {
	return (
		"你好👋！我是你的个人数据助手。\n\n" +
		"想做什么直接说就行，比如记账、查词、记录日常，也可以让我「创建定时任务 / 提醒」。\n\n" +
		`当前可用的子项目：\n${projectList()}`
	);
}

/** Confirmation for a context-reset command. The previous conversation is already cleared. */
export function contextResetReply(): string {
	return "已清空当前会话上下文。接下来我会按新任务处理。";
}

/**
 * Static fallback when a message cannot be routed to any subproject.
 * When the message carried inbound images, guide the user explicitly: image content only helps
 * if the model can see it, so suggest adding a short text hint (and mention the model caveat).
 */
export function fallbackUnknownReply(hasImages = false): string {
	const imageHint = hasImages
		? "📷 我收到了你发的图片，但当前模型无法识别图片内容，所以我没确定要操作哪个子项目。\n" +
			"请补充一句文字说明，比如「记一下这顿饭」，让我知道该写到哪个项目。\n\n"
		: "";
	return (
		"😅 我有点没跟上——没太确定你想操作哪个数据子项目。\n\n" +
		imageHint +
		"我是你的个人数据助手，只能在下面的子项目里读写数据、运行项目内脚本。想做什么直接说就行，" +
		"比如记账、查词、记录日常；也可以让我「创建定时任务 / 提醒」。\n\n" +
		`当前可用的子项目：\n${projectList()}\n\n` +
		"说得更具体一点就好～"
	);
}
