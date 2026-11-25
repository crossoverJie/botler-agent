package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
	rt, err := buildRuntimeChain()
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

func buildRuntimeChain() (agentrt.Runtime, error) {
	c := config.C()
	onEvent := func(ev agentrt.RetryEvent) { logRetryEvent(ev) }

	targets := c.ProviderChain()
	chain := make([]agentrt.Runtime, 0, len(targets))
	for i, t := range targets {
		rt, err := buildOne(t.Provider, t.Model, c)
		if err != nil {
			if i == 0 {
				// A broken primary provider is a configuration error and must surface immediately
				// rather than silently running the user on a backup model.
				return nil, err
			}
			logWarn(fmt.Sprintf("[runner] fallback provider %q unavailable: %v", t.Provider, err))
			continue
		}
		chain = append(chain, wrapRetrying(rt, c, onEvent))
	}

	// Always wrap in FallbackRuntime, even when len(chain) == 1, so single-provider setups still
	// emit "give-up" diagnostics and increment botler_llm_giveups_total.
	rt := &agentrt.FallbackRuntime{Chain: chain, OnEvent: onEvent}
	return agentrt.WrapBudget(rt, c.RetryBudget, onEvent), nil
}

func wrapRetrying(rt agentrt.Runtime, c *config.Config, onEvent func(agentrt.RetryEvent)) agentrt.Runtime {
	if c.MaxRetries <= 0 {
		return rt
	}
	return &agentrt.RetryingRuntime{
		Inner:      rt,
		MaxRetries: c.MaxRetries,
		BaseDelay:  c.RetryBaseDelay,
		MaxDelay:   c.RetryMaxDelay,
		OnEvent:    onEvent,
	}
}

func logRetryEvent(ev agentrt.RetryEvent) {
	switch ev.Kind {
	case "retry":
		logWarn(fmt.Sprintf("[runner] llm retry %d/%d provider=%s delay=%dms: %v",
			ev.Attempt, ev.MaxAttempts, ev.From, ev.DelayMs, ev.Err))
	case "fallback":
		logWarn(fmt.Sprintf("[runner] llm fallback %s -> %s: %v", ev.From, ev.To, ev.Err))
	case "give-up":
		logWarn(fmt.Sprintf("[runner] llm give-up at %s: %v", ev.From, ev.Err))
	case "budget-exhausted":
		logWarn(fmt.Sprintf("[runner] llm budget-exhausted after %s at %s: %v",
			time.Duration(ev.DelayMs)*time.Millisecond, ev.From, ev.Err))
	}
	errMsg := ""
	if ev.Err != nil {
		errMsg = ev.Err.Error()
	}
	monitor.MarkLLMRetry(ev.Kind, ev.From, ev.To, errMsg)
}

func buildOne(id, model string, c *config.Config) (agentrt.Runtime, error) {
	if prov, ok := c.FindProvider(id); ok {
		var meta config.ModelMeta
		found := false
		var available []string
		for _, m := range prov.Models {
			available = append(available, prov.ID+"/"+m.ID)
			if m.ID == model {
				meta = m
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("Model not found: %s/%s.\nAvailable models: %s\nCheck PI_PROVIDER / PI_MODEL in .env.",
				id, model, strings.Join(available, ", "))
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
				Provider: prov.ID, BaseURL: prov.BaseURL, APIKey: apiKey, Model: meta.ID,
				MaxTokens: meta.MaxTokens, RequestTimeout: c.RequestTimeout, Vision: meta.VisionEnabled(),
			})
		case config.APIOpenAICompletions:
			return adk.NewOpenAI(context.Background(), adk.OpenAIConfig{
				Provider: prov.ID, BaseURL: prov.BaseURL, APIKey: apiKey, Model: meta.ID,
				MaxTokens: meta.MaxTokens, RequestTimeout: c.RequestTimeout, Vision: meta.VisionEnabled(),
			})
		default:
			return nil, fmt.Errorf("Unsupported provider api %q for provider %q", prov.API, prov.ID)
		}
	}
	if id == "anthropic" {
		return adk.NewAnthropic(context.Background(), adk.AnthropicConfig{
			Provider:       "anthropic",
			APIKey:         os.Getenv("ANTHROPIC_API_KEY"),
			Model:          model,
			MaxTokens:      8192,
			RequestTimeout: c.RequestTimeout,
			Vision:         true,
		})
	}
	var available []string
	for _, p := range c.CustomProviders {
		for _, m := range p.Models {
			available = append(available, p.ID+"/"+m.ID)
		}
	}
	return nil, fmt.Errorf("Model not found: %s/%s.\nAvailable models: %s\nCheck PI_PROVIDER / PI_MODEL in .env.",
		id, model, strings.Join(available, ", "))
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
