package config

import (
	"os"
	"testing"
	"time"
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

func TestLLMRetryConfigDefaults(t *testing.T) {
	for _, k := range []string{
		"LLM_MAX_RETRIES", "LLM_RETRY_BASE_MS", "LLM_RETRY_MAX_MS",
		"LLM_RETRY_BUDGET_SECONDS", "LLM_REQUEST_TIMEOUT_SECONDS",
		"LLM_FALLBACK_ENABLED", "LLM_RETRY_NOTIFY",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("BOTLER_CONFIG_DIR", t.TempDir())

	c := Load()
	if c.MaxRetries != 2 {
		t.Fatalf("MaxRetries = %d, want 2", c.MaxRetries)
	}
	if c.RetryBaseDelay != 1000*time.Millisecond {
		t.Fatalf("RetryBaseDelay = %s, want 1s", c.RetryBaseDelay)
	}
	if c.RetryMaxDelay != 15000*time.Millisecond {
		t.Fatalf("RetryMaxDelay = %s, want 15s", c.RetryMaxDelay)
	}
	if c.RetryBudget != 0 {
		t.Fatalf("RetryBudget = %s, want 0", c.RetryBudget)
	}
	if c.RequestTimeout != 0 {
		t.Fatalf("RequestTimeout = %s, want 0", c.RequestTimeout)
	}
	if !c.FallbackEnabled {
		t.Fatal("FallbackEnabled must default to true")
	}
	if c.RetryNotify != RetryNotifyFallback {
		t.Fatalf("RetryNotify = %q, want %q", c.RetryNotify, RetryNotifyFallback)
	}
}

func TestLLMRetryConfigParsingAndFallbacks(t *testing.T) {
	// Explicit zero must NOT fall back to 2; LLM_FALLBACK_ENABLED=0 must disable fallback.
	withEnv(t, map[string]string{
		"LLM_MAX_RETRIES":             "0",
		"LLM_RETRY_BASE_MS":           "250",
		"LLM_RETRY_MAX_MS":            "5000",
		"LLM_RETRY_BUDGET_SECONDS":    "90",
		"LLM_REQUEST_TIMEOUT_SECONDS": "300",
		"LLM_FALLBACK_ENABLED":        "0",
		"LLM_RETRY_NOTIFY":            "ALL",
	})
	c := Load()
	if c.MaxRetries != 0 {
		t.Fatalf("LLM_MAX_RETRIES=0 must stay 0, got %d", c.MaxRetries)
	}
	if c.RetryBaseDelay != 250*time.Millisecond || c.RetryMaxDelay != 5000*time.Millisecond {
		t.Fatalf("unexpected delays: base=%s max=%s", c.RetryBaseDelay, c.RetryMaxDelay)
	}
	if c.RetryBudget != 90*time.Second || c.RequestTimeout != 300*time.Second {
		t.Fatalf("unexpected timeouts: budget=%s req=%s", c.RetryBudget, c.RequestTimeout)
	}
	if c.FallbackEnabled {
		t.Fatal("LLM_FALLBACK_ENABLED=0 must disable fallback")
	}
	if c.RetryNotify != RetryNotifyAll {
		t.Fatalf("RetryNotify = %q, want %q", c.RetryNotify, RetryNotifyAll)
	}

	// Negative and unparsable numbers fall back to defaults; unknown notify mode falls back to "fallback".
	// Zero backoff delays (LLM_RETRY_BASE_MS=0 / LLM_RETRY_MAX_MS=0) also fall back to defaults so they
	// cannot trigger a zero-delay retry storm.
	withEnv(t, map[string]string{
		"LLM_MAX_RETRIES":             "-1",
		"LLM_RETRY_BASE_MS":           "0",
		"LLM_RETRY_MAX_MS":            "0",
		"LLM_RETRY_BUDGET_SECONDS":    "-10",
		"LLM_REQUEST_TIMEOUT_SECONDS": "nope",
		"LLM_FALLBACK_ENABLED":        "1",
		"LLM_RETRY_NOTIFY":            "loud",
	})
	c2 := Load()
	if c2.MaxRetries != 2 || c2.RetryBaseDelay != time.Second || c2.RetryMaxDelay != 15*time.Second {
		t.Fatalf("invalid values did not fall back: %+v", c2)
	}
	if c2.RetryBudget != 0 || c2.RequestTimeout != 0 {
		t.Fatalf("invalid timeouts did not fall back: budget=%s req=%s", c2.RetryBudget, c2.RequestTimeout)
	}
	if !c2.FallbackEnabled {
		t.Fatal("LLM_FALLBACK_ENABLED=1 must keep fallback enabled")
	}
	if c2.RetryNotify != RetryNotifyFallback {
		t.Fatalf("invalid LLM_RETRY_NOTIFY must fall back to %q, got %q", RetryNotifyFallback, c2.RetryNotify)
	}

	withEnv(t, map[string]string{"LLM_RETRY_NOTIFY": "off"})
	if got := Load().RetryNotify; got != RetryNotifyOff {
		t.Fatalf("LLM_RETRY_NOTIFY=off -> %q", got)
	}
}
