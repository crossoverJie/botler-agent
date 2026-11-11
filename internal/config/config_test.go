package config

import (
	"os"
	"testing"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		if err := os.Setenv(k, v); err != nil {
			t.Fatal(err)
		}
		key := k
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, old)
				return
			}
			_ = os.Unsetenv(key)
		})
	}
	configDir := t.TempDir() // no .env inside
	t.Setenv("BOTLER_CONFIG_DIR", configDir)
}

// v1 uses `process.env.X ?? default`: an explicitly empty value is a value, not "unset".
func TestExplicitEmptyValueIsKept(t *testing.T) {
	withEnv(t, map[string]string{"PI_PROVIDER": "", "WEBUI_HOST": "", "PI_MODEL": "m"})
	c := Load()
	if c.Provider != "" {
		t.Fatalf("empty PI_PROVIDER must stay empty, got %q", c.Provider)
	}
	if c.WebUIHost != "" {
		t.Fatalf("empty WEBUI_HOST must stay empty (v1 binds all interfaces), got %q", c.WebUIHost)
	}
}

func TestNumericParsingMatchesV1(t *testing.T) {
	withEnv(t, map[string]string{
		"WECHAT_IMAGE_BATCH_SECONDS":  "1.5", // Number(1.5) → floor → 1
		"WECHAT_REMINDER_HOURS":       "Infinity",
		"CONVERSATION_CONTEXT_TURNS":  "2.9", // not an integer → default 5
		"CONVERSATION_TURN_MAX_CHARS": "1.9", // floor → 1
		"MAX_TOOL_TURNS":              "0",   // < 1 → default 20
	})
	c := Load()
	if c.WeChatImageBatchSeconds != 1 {
		t.Fatalf("WECHAT_IMAGE_BATCH_SECONDS=1.5 → %d, want 1", c.WeChatImageBatchSeconds)
	}
	if c.WeChatReminderHours != 23 {
		t.Fatalf("WECHAT_REMINDER_HOURS=Infinity → %d, want 23", c.WeChatReminderHours)
	}
	if c.ConversationContextTurns != 5 {
		t.Fatalf("non-integer turns → %d, want 5", c.ConversationContextTurns)
	}
	if c.ConversationTurnMaxChars != 1 {
		t.Fatalf("1.9 → %d, want 1", c.ConversationTurnMaxChars)
	}
	if c.MaxToolTurns != 20 {
		t.Fatalf("MAX_TOOL_TURNS=0 → %d, want 20", c.MaxToolTurns)
	}
}

func TestProviderAPIParsing(t *testing.T) {
	cases := map[string]string{
		"":                   APIOpenAICompletions,
		"openai":             APIOpenAICompletions,
		"openai-completions": APIOpenAICompletions,
		"anthropic":          APIAnthropicMessages,
		"anthropic-messages": APIAnthropicMessages,
	}
	for raw, want := range cases {
		got, ok := ParseProviderAPI(raw)
		if !ok || got != want {
			t.Errorf("ParseProviderAPI(%q) = %q,%v want %q", raw, got, ok, want)
		}
	}
	if _, ok := ParseProviderAPI("gemini"); ok {
		t.Error("an unknown api must be rejected")
	}
}

// A model without the `vision` field supports images; only an explicit false disables them.
func TestVisionDefaultsToTrue(t *testing.T) {
	f := false
	var unset *bool
	if !(ModelMeta{Vision: unset}).VisionEnabled() {
		t.Fatal("missing vision must mean supported")
	}
	if (ModelMeta{Vision: &f}).VisionEnabled() {
		t.Fatal("vision:false must mean unsupported")
	}
}
