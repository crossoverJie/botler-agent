package runner

import (
	"testing"

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
