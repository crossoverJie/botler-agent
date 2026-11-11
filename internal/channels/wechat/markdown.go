package wechat

import (
	"regexp"
	"strings"
)

// WeChat renders plain text only, so a markdown-formatted model reply is stripped before sending.
// Port of v1's markdownToPlainText (src/channels/wechat/send-media.ts); the SDK's own filter is
// not used because its behaviour differs.
var (
	codeFenceRe   = regexp.MustCompile("(?s)```[^\\n]*\\n?(.*?)```")
	imageRefRe    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	linkRe        = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	tableSepRe    = regexp.MustCompile(`(?m)^\|[\s:|-]+\|$`)
	tableRowRe    = regexp.MustCompile(`(?m)^\|(.+)\|$`)
	boldRe        = regexp.MustCompile(`\*\*(.+?)\*\*`)
	italicRe      = regexp.MustCompile(`\*(.+?)\*`)
	boldUnderRe   = regexp.MustCompile(`__(.+?)__`)
	italicUndersc = regexp.MustCompile(`_(.+?)_`)
	strikeRe      = regexp.MustCompile(`~~(.+?)~~`)
	inlineCodeRe  = regexp.MustCompile("`(.+?)`")
)

// MarkdownToPlainText converts a markdown-formatted model reply into plain text for WeChat.
func MarkdownToPlainText(text string) string {
	result := text
	// Code blocks: strip fences, keep code content.
	result = codeFenceRe.ReplaceAllStringFunc(result, func(m string) string {
		sub := codeFenceRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		return strings.TrimSpace(sub[1])
	})
	// Images: remove entirely.
	result = imageRefRe.ReplaceAllString(result, "")
	// Links: keep display text only.
	result = linkRe.ReplaceAllString(result, "${1}")
	// Tables: remove separator rows, then strip pipes.
	result = tableSepRe.ReplaceAllString(result, "")
	result = tableRowRe.ReplaceAllStringFunc(result, func(m string) string {
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "|"), "|")
		cells := strings.Split(inner, "|")
		for i, cell := range cells {
			cells[i] = strings.TrimSpace(cell)
		}
		return strings.Join(cells, "  ")
	})
	// Strip inline markdown formatting.
	result = boldRe.ReplaceAllString(result, "${1}")
	result = italicRe.ReplaceAllString(result, "${1}")
	result = boldUnderRe.ReplaceAllString(result, "${1}")
	result = italicUndersc.ReplaceAllString(result, "${1}")
	result = strikeRe.ReplaceAllString(result, "${1}")
	result = inlineCodeRe.ReplaceAllString(result, "${1}")
	return result
}
