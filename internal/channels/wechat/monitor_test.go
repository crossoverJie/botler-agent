package wechat

import (
	"testing"

	ilink "github.com/openilink/openilink-sdk-go"
)

func TestBodyFromItemListPrefersTextOverVoice(t *testing.T) {
	// A text item always wins, even when a voice transcript is present in the same message.
	items := []ilink.MessageItem{
		{Type: ilink.ItemImage},
		{Type: ilink.ItemVoice, VoiceItem: &ilink.VoiceItem{Text: "voice transcript"}},
		{Type: ilink.ItemText, TextItem: &ilink.TextItem{Text: "typed text"}},
	}
	if got := bodyFromItemList(items); got != "typed text" {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestBodyFromItemListFallsBackToVoiceTranscript(t *testing.T) {
	items := []ilink.MessageItem{
		{Type: ilink.ItemVoice, VoiceItem: &ilink.VoiceItem{}}, // no transcript yet
		{Type: ilink.ItemVoice, VoiceItem: &ilink.VoiceItem{Text: "transcript"}},
	}
	if got := bodyFromItemList(items); got != "transcript" {
		t.Fatalf("unexpected body: %q", got)
	}
}

func TestBodyFromItemListEmpty(t *testing.T) {
	cases := [][]ilink.MessageItem{
		nil,
		{},
		{{Type: ilink.ItemImage}},
		{{Type: ilink.ItemText, TextItem: &ilink.TextItem{Text: ""}}},
		{{Type: ilink.ItemText}}, // malformed: no payload
		{{Type: ilink.ItemVoice, VoiceItem: &ilink.VoiceItem{Text: ""}}}, // empty transcript
	}
	for _, items := range cases {
		if got := bodyFromItemList(items); got != "" {
			t.Fatalf("expected an empty body, got %q", got)
		}
	}
}

func TestIsAllowed(t *testing.T) {
	cases := []struct {
		name      string
		sender    string
		owner     string
		allowFrom string
		want      bool
	}{
		{"owner", "wx-1", "wx-1", "", true},
		{"owner with allowlist", "wx-1", "wx-1", "other", true},
		{"listed sender", "wx-2", "wx-1", "wx-2", true},
		{"listed among several", "wx-2", "wx-1", "wx-9, wx-2 ,wx-8", true},
		{"stranger", "wx-3", "wx-1", "wx-2", false},
		{"empty allowlist blocks stranger", "wx-3", "wx-1", "", false},
		{"whitespace entries ignored", "wx-3", "wx-1", " , ,", false},
		// No owner recorded (not logged in yet): only the explicit allowlist can pass.
		{"no owner, listed", "wx-2", "", "wx-2", true},
		{"no owner, stranger", "wx-2", "", "", false},
		// An empty sender never matches, even when the allowlist contains an empty entry.
		{"empty sender", "", "", " ,", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAllowed(c.sender, c.owner, c.allowFrom); got != c.want {
				t.Fatalf("isAllowed(%q,%q,%q) = %v, want %v", c.sender, c.owner, c.allowFrom, got, c.want)
			}
		})
	}
}

func TestTruncateIsRuneAware(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"ab", 3, "ab"},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abc"},
		// Byte-level slicing would produce mojibake here; rune slicing keeps the log readable.
		{"今天喝水了吗", 4, "今天喝水"},
		{"今天", 10, "今天"},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Fatalf("truncate(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestOrEmpty(t *testing.T) {
	if got := orEmpty(""); got != "(empty)" {
		t.Fatalf("unexpected placeholder: %q", got)
	}
	if got := orEmpty("wx-1"); got != "wx-1" {
		t.Fatalf("unexpected passthrough: %q", got)
	}
}
