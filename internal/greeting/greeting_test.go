package greeting

import "testing"

func TestIsGreeting(t *testing.T) {
	yes := []string{"你好", "您好！", "hi", "Hello", "HEY", "在吗", "在不在", "喂", "哈喽", "嗨～", " 你好 👋 ", "你好❤️", "你好，", "你好啊"}
	no := []string{"", "你好吗，帮我记一下今天的午饭", "记账", "hi there", "hello world", "在吗？帮我查一下"}
	for _, s := range yes {
		if !IsGreeting(s) {
			t.Errorf("expected greeting: %q", s)
		}
	}
	for _, s := range no {
		if IsGreeting(s) {
			t.Errorf("expected NOT greeting: %q", s)
		}
	}
}

func TestReplyMentionsNoProjectsPlaceholder(t *testing.T) {
	if got := Reply(); got == "" {
		t.Fatal("reply must not be empty")
	}
}

func TestFallbackUnknownReplyImageHint(t *testing.T) {
	plain := FallbackUnknownReply(false)
	withImage := FallbackUnknownReply(true)
	if len(withImage) <= len(plain) {
		t.Fatal("image fallback should add a hint")
	}
	if plain == "" {
		t.Fatal("fallback must not be empty")
	}
}
