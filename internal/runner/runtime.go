package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/agentrt/adk"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/monitor"
)

var (
	rtCached agentrt.Runtime
)

// resolveRuntime resolves (and caches) the model client for PI_PROVIDER / PI_MODEL.
// A misconfiguration returns a usable error instead of crashing the process.
func resolveRuntime() (agentrt.Runtime, error) {
	if rtCached != nil {
		monitor.MarkModelCache(true)
		return rtCached, nil
	}
	monitor.MarkModelCache(false)
	// v1 caches only a successful resolution: a misconfiguration is re-resolved (and re-counted as a
	// miss) on the next task instead of being pinned for the life of the process.
	rt, err := buildRuntime()
	if err != nil {
		return nil, err
	}
	rtCached = rt
	return rt, nil
}

// ResetRuntime clears the cached client (tests).
func ResetRuntime() {
	rtCached = nil
}

func buildRuntime() (agentrt.Runtime, error) {
	c := config.C()
	if prov, ok := c.FindProvider(c.Provider); ok {
		var meta config.ModelMeta
		found := false
		var available []string
		for _, m := range prov.Models {
			available = append(available, prov.ID+"/"+m.ID)
			if m.ID == c.Model {
				meta = m
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("Model not found: %s/%s.\nAvailable models: %s\nCheck PI_PROVIDER / PI_MODEL in .env.",
				c.Provider, c.Model, strings.Join(available, ", "))
		}
		apiKey := prov.APIKey
		if apiKey == "" {
			envKey := strings.ToUpper(strings.ReplaceAll(prov.ID, "-", "_")) + "_API_KEY"
			apiKey = os.Getenv(envKey)
		}
		// The model layer is adk-go's; the tool loop stays in agentrt.Loop (design doc §3.3, Q-1).
		switch prov.API {
		case config.APIAnthropicMessages:
			return adk.NewAnthropic(context.Background(), adk.AnthropicConfig{
				BaseURL: prov.BaseURL, APIKey: apiKey, Model: meta.ID,
				MaxTokens: meta.MaxTokens, Vision: meta.VisionEnabled(),
			})
		case config.APIOpenAICompletions:
			return adk.NewOpenAI(context.Background(), adk.OpenAIConfig{
				BaseURL: prov.BaseURL, APIKey: apiKey, Model: meta.ID,
				MaxTokens: meta.MaxTokens, Vision: meta.VisionEnabled(),
			})
		default:
			return nil, fmt.Errorf("Unsupported provider api %q for provider %q", prov.API, prov.ID)
		}
	}
	if c.Provider == "anthropic" {
		return adk.NewAnthropic(context.Background(), adk.AnthropicConfig{
			APIKey:    os.Getenv("ANTHROPIC_API_KEY"),
			Model:     c.Model,
			MaxTokens: 8192,
			Vision:    true,
		})
	}
	var available []string
	for _, p := range c.CustomProviders {
		for _, m := range p.Models {
			available = append(available, p.ID+"/"+m.ID)
		}
	}
	return nil, fmt.Errorf("Model not found: %s/%s.\nAvailable models: %s\nCheck PI_PROVIDER / PI_MODEL in .env.",
		c.Provider, c.Model, strings.Join(available, ", "))
}

// ModelName returns the configured provider/model pair for logging.
func ModelName() (string, string) {
	c := config.C()
	return c.Provider, c.Model
}

var errNoRuntime = errors.New("model runtime is not configured")

// SetRuntimeForTest installs a fixed runtime (tests only).
func SetRuntimeForTest(rt agentrt.Runtime) {
	rtCached = rt
}
