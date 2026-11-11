package wechat

import (
	"strings"
	"testing"
)

func TestMarkdownToPlainTextStripsCodeFences(t *testing.T) {
	got := MarkdownToPlainText("before\n```json\n{\"a\": 1}\n```\nafter")
	if strings.Contains(got, "```") {
		t.Fatalf("code fence kept: %q", got)
	}
	if !strings.Contains(got, `{"a": 1}`) {
		t.Fatalf("code content lost: %q", got)
	}
}

func TestMarkdownToPlainTextRemovesImagesAndUnwrapsLinks(t *testing.T) {
	got := MarkdownToPlainText("看图 ![图](/data/cook/photos/a.jpg) 和 [链接](https://example.com)")
	if strings.Contains(got, "![") || strings.Contains(got, "https://example.com") {
		t.Fatalf("markdown not stripped: %q", got)
	}
	if !strings.Contains(got, "链接") {
		t.Fatalf("link text lost: %q", got)
	}
}

func TestMarkdownToPlainTextFlattensTables(t *testing.T) {
	in := "| 项目 | 数量 |\n| --- | --- |\n| 苹果 | 2 |"
	got := MarkdownToPlainText(in)
	if strings.Contains(got, "|") || strings.Contains(got, "---") {
		t.Fatalf("table markup kept: %q", got)
	}
	if !strings.Contains(got, "苹果") || !strings.Contains(got, "2") {
		t.Fatalf("cell content lost: %q", got)
	}
}

func TestMarkdownToPlainTextStripsInlineFormatting(t *testing.T) {
	got := MarkdownToPlainText("**粗体** *斜体* ~~删除~~ `代码` __下划__ _也是斜体_")
	for _, marker := range []string{"**", "~~", "`", "__"} {
		if strings.Contains(got, marker) {
			t.Fatalf("%s kept: %q", marker, got)
		}
	}
	if !strings.Contains(got, "粗体") || !strings.Contains(got, "代码") {
		t.Fatalf("text lost: %q", got)
	}
}

func TestMarkdownToPlainTextLeavesPlainTextAlone(t *testing.T) {
	in := "✅ 已记账：苹果 2 个，共 12.5 元"
	if got := MarkdownToPlainText(in); got != in {
		t.Fatalf("got %q, want %q", got, in)
	}
}

func TestReminderText(t *testing.T) {
	got := ReminderText(23.4)
	if !strings.Contains(got, "over 23 hours") || !strings.Contains(got, "about 1 hours") {
		t.Fatalf("text = %q", got)
	}
	got = ReminderText(30)
	if !strings.Contains(got, "over 30 hours") || !strings.Contains(got, "about 0 hours") {
		t.Fatalf("text = %q", got)
	}
}
