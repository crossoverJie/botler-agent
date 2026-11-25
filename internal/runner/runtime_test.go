package runner

import (
	"errors"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/monitor"
)

func withProvider(t *testing.T) {
	t.Helper()
	config.Set(&config.Config{
		Provider: "custom",
		Model:    "test-model",
		CustomProviders: []config.CustomProvider{{
			ID: "custom", API: config.APIOpenAICompletions,
			BaseURL: "http://127.0.0.1:1/v1", APIKey: "k",
			Models: []config.ModelMeta{{ID: "test-model", Name: "test"}},
		}},
	})
	ResetRuntime()
	t.Cleanup(ResetRuntime)
}

// v1 marks the first resolution as a miss and only later ones as hits; the counters drive
// /healthz modelCache and the task log.
func TestModelCacheCountsFirstCallAsMiss(t *testing.T) {
	withProvider(t)
	beforeQ, beforeH := monitor.ModelCacheSnapshot()

	if _, err := resolveRuntime(); err != nil {
		t.Fatal(err)
	}
	q1, h1 := monitor.ModelCacheSnapshot()
	if q1-beforeQ != 1 || h1-beforeH != 0 {
		t.Fatalf("first call must be a miss: queries=%d hits=%d", q1-beforeQ, h1-beforeH)
	}
	if _, err := resolveRuntime(); err != nil {
		t.Fatal(err)
	}
	q2, h2 := monitor.ModelCacheSnapshot()
	if q2-beforeQ != 2 || h2-beforeH != 1 {
		t.Fatalf("second call must hit: queries=%d hits=%d", q2-beforeQ, h2-beforeH)
	}
}

// A misconfiguration is not cached: v1 re-resolves (and re-counts a miss) on the next task.
func TestModelResolutionErrorIsNotCached(t *testing.T) {
	config.Set(&config.Config{Provider: "nope", Model: "nope"})
	ResetRuntime()
	t.Cleanup(ResetRuntime)

	if _, err := resolveRuntime(); err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	before, _ := monitor.ModelCacheSnapshot()
	if _, err := resolveRuntime(); err == nil {
		t.Fatal("expected the error again")
	}
	after, _ := monitor.ModelCacheSnapshot()
	if after-before != 1 {
		t.Fatalf("an error must be re-resolved, queries delta=%d", after-before)
	}
}

func TestBuildRuntimeChainAlwaysWrapsFallbackAndSkipsBadBackups(t *testing.T) {
	var warnings []string
	origWarn := logWarn
	logWarn = func(s string) { warnings = append(warnings, s) }
	t.Cleanup(func() { logWarn = origWarn })

	// Single-provider setup still returns *agentrt.FallbackRuntime (chain layer always present).
	withProvider(t)
	rt, err := resolveRuntime()
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := rt.(*agentrt.FallbackRuntime)
	if !ok {
		t.Fatalf("single-provider runtime must still be *agentrt.FallbackRuntime, got %T", rt)
	}
	if len(fb.Chain) != 1 || fb.Describe() != "custom/test-model" {
		t.Fatalf("unexpected chain len=%d describe=%q", len(fb.Chain), fb.Describe())
	}

	// Multi-provider chain: broken backup model is warned & skipped while valid backup joins.
	p1 := 1
	p2 := 2
	config.Set(&config.Config{
		Provider:        "custom",
		Model:           "test-model",
		MaxRetries:      2,
		FallbackEnabled: true,
		CustomProviders: []config.CustomProvider{
			{
				ID: "custom", API: config.APIOpenAICompletions,
				BaseURL: "http://127.0.0.1:1/v1", APIKey: "k1",
				Models: []config.ModelMeta{{ID: "test-model"}},
			},
			{
				ID: "broken-backup", API: config.APIOpenAICompletions,
				BaseURL: "http://127.0.0.1:1/v1", APIKey: "k2",
				Priority: &p1, Model: "nonexistent-model",
				Models: []config.ModelMeta{{ID: "other-model"}},
			},
			{
				ID: "ark", API: config.APIAnthropicMessages,
				BaseURL: "http://127.0.0.1:1", APIKey: "k3",
				Priority: &p2, Model: "ark-code-latest",
				Models: []config.ModelMeta{{ID: "ark-code-latest"}},
			},
		},
	})
	ResetRuntime()
	rt2, err := resolveRuntime()
	if err != nil {
		t.Fatalf("broken backup must not fail primary resolution: %v", err)
	}
	fb2, ok := rt2.(*agentrt.FallbackRuntime)
	if !ok || len(fb2.Chain) != 2 {
		t.Fatalf("expected 2-member FallbackRuntime, got %T (len %d)", rt2, len(fb2.Chain))
	}
	if got := fb2.Describe(); got != "chain(custom/test-model>ark/ark-code-latest)" {
		t.Fatalf("Describe() = %q", got)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], `fallback provider "broken-backup" unavailable`) {
		t.Fatalf("expected broken-backup warning, got %v", warnings)
	}

	// Broken primary provider MUST fail rather than silently degrading to ark.
	config.Set(&config.Config{
		Provider:        "custom",
		Model:           "wrong-primary-model",
		FallbackEnabled: true,
		CustomProviders: []config.CustomProvider{
			{
				ID: "custom", API: config.APIOpenAICompletions,
				BaseURL: "http://127.0.0.1:1/v1", APIKey: "k1",
				Models: []config.ModelMeta{{ID: "test-model"}},
			},
			{
				ID: "ark", API: config.APIAnthropicMessages,
				BaseURL: "http://127.0.0.1:1", APIKey: "k3",
				Priority: &p2, Model: "ark-code-latest",
				Models: []config.ModelMeta{{ID: "ark-code-latest"}},
			},
		},
	})
	ResetRuntime()
	if _, err := resolveRuntime(); err == nil || !strings.Contains(err.Error(), "Model not found: custom/wrong-primary-model") {
		t.Fatalf("broken primary must return Model not found error, got %v", err)
	}

	// Exercise logRetryEvent -> stderr + monitor counters.
	warnings = nil
	logRetryEvent(agentrt.RetryEvent{Kind: "retry", From: "custom/test-model", Attempt: 1, MaxAttempts: 2, DelayMs: 500, Err: errors.New("eof")})
	logRetryEvent(agentrt.RetryEvent{Kind: "fallback", From: "custom/test-model", To: "ark/ark-code-latest", Err: errors.New("503")})
	logRetryEvent(agentrt.RetryEvent{Kind: "give-up", From: "ark/ark-code-latest", Err: errors.New("down")})
	logRetryEvent(agentrt.RetryEvent{Kind: "budget-exhausted", From: "chain(custom/test-model>ark/ark-code-latest)", DelayMs: 2000, Err: errors.New("timeout")})
	if len(warnings) != 4 {
		t.Fatalf("expected 4 diagnostic log lines, got %v", warnings)
	}
}
