// Package config loads the two-level .env configuration, providers.json and every env key
// the framework understands. It mirrors v1's src/config.ts (see docs/design/go-rewrite-adk-go.md
// §2.1) so an existing ~/.botler-agent keeps working byte-for-byte.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ModelMeta describes one model of a custom provider (providers.json).
type ModelMeta struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Reasoning     bool   `json:"reasoning"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
	// Vision reports whether the model accepts image input. A missing value means "supported"
	// (v1 treats an absent field as text+image); only an explicit false turns images off.
	Vision *bool `json:"vision,omitempty"`
}

// VisionEnabled reports whether this model accepts image input.
func (m ModelMeta) VisionEnabled() bool { return m.Vision == nil || *m.Vision }

// Wire protocols a custom provider may speak.
const (
	APIOpenAICompletions = "openai-completions"
	APIAnthropicMessages = "anthropic-messages"
)

// CustomProvider is one entry of providers.json.
type CustomProvider struct {
	ID      string
	API     string
	BaseURL string
	APIKey  string
	Models  []ModelMeta
}

// Config is the resolved runtime configuration.
type Config struct {
	DataRoot string
	Provider string
	Model    string

	CustomProviders []CustomProvider

	TelegramToken     string
	TelegramAllowFrom string

	FeishuAppID             string
	FeishuAppSecret         string
	FeishuVerificationToken string
	FeishuEncryptKey        string
	FeishuPort              int
	FeishuAllowFrom         string

	WeChatEnabled            bool
	WeChatAllowFrom          string
	WeChatReminderHours      int
	WeChatImageBatchSeconds  int
	GitPush                  bool
	WebUIEnabled             bool
	WebUIPort                int
	WebUIHost                string
	SchedulerEnabled         bool
	SchedulesFile            string
	HolidaysFile             string
	HolidayAPIURL            string
	LogDir                   string
	MonitorEnabled           bool
	MonitorPort              int
	MaxToolTurns             int
	ConversationContextOn    bool
	ConversationContextTurns int
	ConversationTurnMaxChars int
	ConversationContextMax   int
	ConversationDir          string
}

var (
	current   *Config
	currentMu sync.Mutex
)

// C returns the process configuration (set by Load).
func C() *Config {
	currentMu.Lock()
	defer currentMu.Unlock()
	if current == nil {
		current = Load()
	}
	return current
}

// Set replaces the process configuration (tests only).
func Set(c *Config) {
	currentMu.Lock()
	current = c
	currentMu.Unlock()
}

// UserConfigDir is the externalized config directory: BOTLER_CONFIG_DIR, else ~/.botler-agent.
// It can only come from a real process env var (not from .env itself) to avoid a chicken-and-egg
// situation. The value is read on every call so an overriding process env var always wins.
func UserConfigDir() string {
	if v := os.Getenv("BOTLER_CONFIG_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".botler-agent"
	}
	return filepath.Join(home, ".botler-agent")
}

// ParseDotEnv loads key=value lines from path into the process environment. Values already set
// in the environment (including by a higher-priority .env) are never overwritten.
func ParseDotEnv(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq == -1 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		value := strings.TrimSpace(trimmed[eq+1:])
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, ok := os.LookupEnv(key); !ok {
			_ = os.Setenv(key, value)
		}
	}
}

// Load reads the user-level .env (only; v3 deliberately drops the source-dir fallback, see the
// design doc §2.1 Q-10) and resolves every configuration key.
func Load() *Config {
	ParseDotEnv(filepath.Join(UserConfigDir(), ".env"))
	c := &Config{
		DataRoot:                 os.Getenv("DATA_ROOT"),
		Provider:                 envOr("PI_PROVIDER", "anthropic"),
		Model:                    envOr("PI_MODEL", "claude-sonnet-4-5"),
		TelegramToken:            os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramAllowFrom:        os.Getenv("TELEGRAM_ALLOW_FROM"),
		FeishuAppID:              os.Getenv("FEISHU_APP_ID"),
		FeishuAppSecret:          os.Getenv("FEISHU_APP_SECRET"),
		FeishuVerificationToken:  os.Getenv("FEISHU_VERIFICATION_TOKEN"),
		FeishuEncryptKey:         os.Getenv("FEISHU_ENCRYPT_KEY"),
		FeishuPort:               envInt("FEISHU_PORT", 3000),
		FeishuAllowFrom:          os.Getenv("FEISHU_ALLOW_FROM"),
		WeChatEnabled:            os.Getenv("WECHAT_ENABLED") == "1",
		WeChatAllowFrom:          os.Getenv("WECHAT_ALLOW_FROM"),
		WeChatReminderHours:      parseWeChatReminderHours(),
		WeChatImageBatchSeconds:  parseWeChatImageBatchSeconds(),
		GitPush:                  os.Getenv("GIT_PUSH") == "1",
		WebUIEnabled:             os.Getenv("WEBUI_ENABLED") == "1",
		WebUIPort:                envInt("WEBUI_PORT", 8900),
		WebUIHost:                envOr("WEBUI_HOST", "127.0.0.1"),
		SchedulerEnabled:         os.Getenv("SCHEDULER_ENABLED") == "1",
		SchedulesFile:            envOr("BOTLER_SCHEDULES_FILE", filepath.Join(UserConfigDir(), "schedules.json")),
		HolidaysFile:             envOr("BOTLER_HOLIDAYS_FILE", filepath.Join(UserConfigDir(), "holidays.json")),
		HolidayAPIURL:            envOr("BOTLER_HOLIDAY_API_URL", "https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/{year}.json"),
		LogDir:                   envOr("BOTLER_LOG_DIR", filepath.Join(UserConfigDir(), "task-logs")),
		MonitorEnabled:           os.Getenv("MONITOR_ENABLED") != "0",
		MonitorPort:              parseMonitorPort(),
		MaxToolTurns:             parseMaxToolTurns(),
		ConversationContextOn:    os.Getenv("CONVERSATION_CONTEXT_ENABLED") != "0",
		ConversationContextTurns: parseConversationContextTurns(),
		ConversationTurnMaxChars: parseConversationTurnMaxChars(),
		ConversationContextMax:   parseConversationContextMaxChars(),
		ConversationDir:          filepath.Join(UserConfigDir(), "conversations"),
	}
	c.CustomProviders = buildCustomProviders(c.Provider)
	return c
}

func envOr(key, def string) string {
	// v1 uses `process.env.X ?? default`: an explicitly empty value is a value, not "unset".
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}

func parseMonitorPort() int {
	n := envInt("MONITOR_PORT", 8899)
	if n <= 0 {
		return 8899
	}
	return n
}

// parseNumber mirrors Number(value): a float, plus ok=false for NaN / non-numeric input.
func parseNumber(raw string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// parseMaxToolTurns falls back to 20 for invalid values (non-integer / < 1).
func parseMaxToolTurns() int {
	f, ok := parseNumber(envOr("MAX_TOOL_TURNS", "20"))
	if !ok || f != math.Trunc(f) || f < 1 {
		return 20
	}
	return int(f)
}

// parseConversationContextTurns: 0 disables injection, invalid values fall back to 5.
func parseConversationContextTurns() int {
	f, ok := parseNumber(envOr("CONVERSATION_CONTEXT_TURNS", "5"))
	if !ok || f != math.Trunc(f) || f < 0 {
		return 5
	}
	return int(f)
}

func parseConversationTurnMaxChars() int {
	f, ok := parseNumber(envOr("CONVERSATION_TURN_MAX_CHARS", "4000"))
	if !ok || f < 1 {
		return 4000
	}
	return int(math.Floor(f))
}

func parseConversationContextMaxChars() int {
	f, ok := parseNumber(envOr("CONVERSATION_CONTEXT_MAX_CHARS", "12000"))
	if !ok || f < 1 {
		return 12000
	}
	return int(math.Floor(f))
}

// parseWeChatReminderHours: "0" disables reminders; 1-24 active (>24 clamps to 24);
// anything else falls back to the default 23.
func parseWeChatReminderHours() int {
	raw, ok := os.LookupEnv("WECHAT_REMINDER_HOURS")
	if ok && raw == "0" {
		return 0
	}
	if !ok || raw == "" {
		return 23
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 {
		return 23
	}
	if n > 24 {
		return 24
	}
	return int(n)
}

// parseWeChatImageBatchSeconds: "0" disables batching; a non-negative integer sets the window;
// anything invalid falls back to 60.
func parseWeChatImageBatchSeconds() int {
	raw, ok := os.LookupEnv("WECHAT_IMAGE_BATCH_SECONDS")
	if ok && raw == "0" {
		return 0
	}
	f, ok := parseNumber(envOr("WECHAT_IMAGE_BATCH_SECONDS", "60"))
	if !ok || f < 0 {
		return 60
	}
	return int(math.Floor(f))
}

// ParseProviderAPI normalizes a provider's `api` value; ok=false means unsupported (skip it).
func ParseProviderAPI(raw string) (api string, ok bool) {
	switch raw {
	case "", "openai", "openai-completions":
		return APIOpenAICompletions, true
	case "anthropic", "anthropic-messages":
		return APIAnthropicMessages, true
	}
	return "", false
}

// legacyCustomModels mirrors v1's built-in CUSTOM_MODELS list for the legacy CUSTOM_* env fallback.
var legacyCustomModels = []ModelMeta{
	{ID: "your-model-pro", Name: "Your Model Pro", Reasoning: true, ContextWindow: 1048576, MaxTokens: 393216},
	{ID: "your-model-flash", Name: "Your Model Flash", Reasoning: false, ContextWindow: 1048576, MaxTokens: 393216},
}

// LoadProvidersFile parses <UserConfigDir>/providers.json. Malformed entries are skipped with a
// warning; a missing / unusable file returns nil so the caller can fall back.
func LoadProvidersFile() []CustomProvider {
	file := filepath.Join(UserConfigDir(), "providers.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var doc struct {
		Providers map[string]struct {
			API     any             `json:"api"`
			BaseURL any             `json:"baseUrl"`
			APIKey  any             `json:"apiKey"`
			Models  json.RawMessage `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		warnf("failed to parse %s, falling back to CUSTOM_* env config: %v", file, err)
		return nil
	}
	var out []CustomProvider
	for id, p := range doc.Providers {
		baseURL, _ := p.BaseURL.(string)
		apiKey, _ := p.APIKey.(string)
		if baseURL == "" || apiKey == "" {
			continue
		}
		rawAPI := ""
		if s, ok := p.API.(string); ok {
			rawAPI = s
		}
		api, ok := ParseProviderAPI(rawAPI)
		if !ok {
			warnf("skipping provider %q: unsupported api %q", id, rawAPI)
			continue
		}
		var models []ModelMeta
		if len(p.Models) > 0 {
			if err := json.Unmarshal(p.Models, &models); err != nil {
				continue
			}
		}
		kept := make([]ModelMeta, 0, len(models))
		for _, m := range models {
			if m.ID == "" {
				continue
			}
			if m.Name == "" {
				m.Name = m.ID
			}
			kept = append(kept, m)
		}
		if len(kept) == 0 {
			continue
		}
		out = append(out, CustomProvider{ID: id, API: api, BaseURL: baseURL, APIKey: apiKey, Models: kept})
	}
	return out
}

// buildCustomProviders prefers providers.json; if it is missing/unusable, the legacy
// CUSTOM_BASE_URL / CUSTOM_API_KEY env vars apply — but only when PI_PROVIDER !== "anthropic"
// (the default), so a fresh install never reads CUSTOM_*.
func buildCustomProviders(provider string) []CustomProvider {
	if fromFile := LoadProvidersFile(); len(fromFile) > 0 {
		return fromFile
	}
	if provider == "anthropic" {
		return nil
	}
	baseURL := os.Getenv("CUSTOM_BASE_URL")
	apiKey := os.Getenv("CUSTOM_API_KEY")
	if baseURL == "" || apiKey == "" {
		return nil
	}
	id := provider
	if id == "" {
		id = "custom"
	}
	return []CustomProvider{{ID: id, API: APIOpenAICompletions, BaseURL: baseURL, APIKey: apiKey, Models: legacyCustomModels}}
}

// Provider returns the custom provider with the given id.
func (c *Config) FindProvider(id string) (CustomProvider, bool) {
	for _, p := range c.CustomProviders {
		if p.ID == id {
			return p, true
		}
	}
	return CustomProvider{}, false
}

var warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[config] "+format+"\n", args...)
}

// SetWarnf replaces the warning sink (tests).
func SetWarnf(f func(format string, args ...any)) { warnf = f }
