package conversation

import (
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
)

func withConfig(t *testing.T, turnMax, ctxMax int) {
	t.Helper()
	config.Set(&config.Config{
		ConversationDir:          t.TempDir(),
		ConversationTurnMaxChars: turnMax,
		ConversationContextMax:   ctxMax,
		ConversationContextOn:    true,
		ConversationContextTurns: 5,
	})
}

func turn(i int) Turn {
	return Turn{TS: int64(i), User: "u" + itoa(i), Assistant: "a" + itoa(i)}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}

func TestAppendKeepsSlidingWindow(t *testing.T) {
	withConfig(t, 4000, 12000)
	for i := 1; i <= 7; i++ {
		if err := AppendTurn(IMSessionKey, turn(i), 5); err != nil {
			t.Fatal(err)
		}
	}
	got := LoadRecentTurns(IMSessionKey, 5)
	if len(got) != 5 {
		t.Fatalf("got %d turns, want 5", len(got))
	}
	if got[0].User != "u3" || got[4].User != "u7" {
		t.Fatalf("unexpected window: %v", got)
	}
}

func TestAppendTruncatesEachSide(t *testing.T) {
	withConfig(t, 10, 12000)
	long := strings.Repeat("x", 100)
	if err := AppendTurn(IMSessionKey, Turn{TS: 1, User: long, Assistant: long}, 5); err != nil {
		t.Fatal(err)
	}
	got := LoadRecentTurns(IMSessionKey, 5)
	if len(got[0].User) != 10 || len(got[0].Assistant) != 10 {
		t.Fatalf("expected 10-char truncation, got %d/%d", len(got[0].User), len(got[0].Assistant))
	}
}

func TestClearSessionRemovesTurns(t *testing.T) {
	withConfig(t, 4000, 12000)
	if err := AppendTurn(IMSessionKey, turn(1), 5); err != nil {
		t.Fatal(err)
	}
	ClearSession(IMSessionKey)
	if got := LoadRecentTurns(IMSessionKey, 5); len(got) != 0 {
		t.Fatalf("expected empty history, got %v", got)
	}
}

func TestFormatRecentTurnsKeepsNewestWithinCap(t *testing.T) {
	withConfig(t, 4000, 40)
	turns := []Turn{
		{TS: 1, User: "old-question", Assistant: "old-answer"},
		{TS: 2, User: "new-question", Assistant: "new-answer"},
	}
	got := FormatRecentTurns(turns, FormatOptions{})
	if strings.Contains(got, "old-question") {
		t.Fatalf("oldest turn should be dropped: %q", got)
	}
	if !strings.Contains(got, "new-question") {
		t.Fatalf("newest turn must survive: %q", got)
	}
}

func TestFormatRecentTurnsKeepsHeadWhenNewestExceedsCap(t *testing.T) {
	withConfig(t, 4000, 5)
	turns := []Turn{{TS: 1, User: "abcdefghij", Assistant: ""}}
	got := FormatRecentTurns(turns, FormatOptions{})
	if got == "" {
		t.Fatal("a single oversized turn must not be dropped to empty")
	}
	if len([]rune(got)) > 5 {
		t.Fatalf("expected cap of 5 runes, got %q", got)
	}
}

func TestFormatRecentTurnsProjectOnlyInRouting(t *testing.T) {
	withConfig(t, 4000, 12000)
	p := "cook"
	turns := []Turn{{TS: 1, Project: &p, User: "u", Assistant: "a"}}
	if got := FormatRecentTurns(turns, FormatOptions{}); strings.Contains(got, "（项目：") {
		t.Fatalf("execution phase must not render the project label: %q", got)
	}
	if got := FormatRecentTurns(turns, FormatOptions{IncludeProject: true}); !strings.Contains(got, "（项目：cook）") {
		t.Fatalf("routing phase must render the project label: %q", got)
	}
}

func TestShouldRecordTurn(t *testing.T) {
	cases := []struct {
		phase    string
		status   string
		key      string
		record   bool
		expected bool
	}{
		{"execute", "success", "im", true, true},
		{"execute", "auto-fixed", "im", true, true},
		{"execute", "unknown-project", "im", true, true},
		{"execute", "error", "im", true, false},
		{"execute", "validation-failed", "im", true, false},
		{"self-heal", "success", "im", true, false},
		{"execute", "success", "", true, false},
		{"execute", "success", "im", false, false},
	}
	for _, c := range cases {
		if got := ShouldRecordTurn(c.phase, c.status, c.key, c.record); got != c.expected {
			t.Errorf("ShouldRecordTurn(%q,%q,%q,%v)=%v want %v", c.phase, c.status, c.key, c.record, got, c.expected)
		}
	}
}

func TestShouldLoadRecentTurns(t *testing.T) {
	if !ShouldLoadRecentTurns(RecentTurnLoadContext{Enabled: true, Phase: "execute", Source: "telegram", SessionKey: "im"}) {
		t.Fatal("IM execute runs should load history")
	}
	for _, src := range []string{"scheduler", "cli"} {
		if ShouldLoadRecentTurns(RecentTurnLoadContext{Enabled: true, Phase: "execute", Source: src, SessionKey: "im"}) {
			t.Fatalf("%s must not load history", src)
		}
	}
	if ShouldLoadRecentTurns(RecentTurnLoadContext{Enabled: false, Phase: "execute", Source: "telegram", SessionKey: "im"}) {
		t.Fatal("disabled conversation context must not load history")
	}
}
