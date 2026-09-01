import { test } from "node:test";
import assert from "node:assert/strict";
import { isGreeting, contextResetReply } from "./greeting.ts";

const cases: Array<[string, boolean, string]> = [
	["你好", true, "basic"],
	["你好！", true, "with punctuation"],
	["　你好　", true, "full-width spaces"],
	["你好👋", true, "trailing emoji (\\p{S})"],
	["你好❤️", true, "trailing emoji + variation selector U+FE0F (\\p{M})"],
	["你好呀", true, "tone word"],
	["您好", true, "variant"],
	["哈喽", true, "variant"],
	["喂", true, "variant (call/greeting)"],
	["hi", true, "english lowercase"],
	["Hello", true, "english mixed case"],
	["在吗？", true, "punctuation"],
	["你好，帮我记一下午饭", false, "carries content"],
	["hi 帮我查单词", false, "carries content"],
];

for (const [input, expected, note] of cases) {
	test(`isGreeting(${JSON.stringify(input)}) === ${expected} (${note})`, () => {
		assert.equal(isGreeting(input), expected);
	});
}

test("contextResetReply confirms the cleared session", () => {
	assert.match(contextResetReply(), /已清空当前会话上下文/);
});
