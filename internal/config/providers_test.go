package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

func TestProviderChainOrderingAndFiltering(t *testing.T) {
	c := &Config{
		Provider:        "primary",
		Model:           "primary-m",
		FallbackEnabled: true,
		CustomProviders: []CustomProvider{
			// Primary itself has priority+model, must stay at index 0 with PI_MODEL and not repeat.
			{ID: "primary", Priority: intPtr(5), Model: "ignored-model"},
			// Unset priority: must never enter the fallback chain.
			{ID: "unranked", Model: "u-m"},
			// Priority 0 is a valid opt-in, NOT "unset".
			{ID: "zero", Priority: intPtr(0), Model: "zero-m"},
			// Same priority tie-breaks lexicographically by ID ("alpha" before "beta").
			{ID: "beta", Priority: intPtr(2), Model: "beta-m"},
			{ID: "alpha", Priority: intPtr(2), Model: "alpha-m"},
			// Priority set, but missing model -> skipped.
			{ID: "nomodel", Priority: intPtr(1)},
			// Explicit fallback:false -> excluded.
			{ID: "disabled", Priority: intPtr(1), Model: "dis-m", Fallback: boolPtr(false)},
			// Explicit fallback:true -> included.
			{ID: "first", Priority: intPtr(1), Model: "first-m", Fallback: boolPtr(true)},
		},
	}

	got := c.ProviderChain()
	want := []ProviderTarget{
		{Provider: "primary", Model: "primary-m"},
		{Provider: "zero", Model: "zero-m"},
		{Provider: "first", Model: "first-m"},
		{Provider: "alpha", Model: "alpha-m"},
		{Provider: "beta", Model: "beta-m"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderChain() =\n  %+v\nwant\n  %+v", got, want)
	}

	// LLM_FALLBACK_ENABLED=0 truncates the chain to just the primary choice.
	c.FallbackEnabled = false
	gotDisabled := c.ProviderChain()
	wantDisabled := []ProviderTarget{{Provider: "primary", Model: "primary-m"}}
	if !reflect.DeepEqual(gotDisabled, wantDisabled) {
		t.Fatalf("ProviderChain() with FallbackEnabled=false = %+v, want %+v", gotDisabled, wantDisabled)
	}
}

func TestLoadProvidersFileFallbackFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOTLER_CONFIG_DIR", dir)

	body := `{
  "providers": {
    "main": {
      "api": "openai-completions",
      "baseUrl": "https://main.example/v1",
      "apiKey": "sk-1",
      "models": [{"id": "m1"}]
    },
    "ark": {
      "api": "anthropic-messages",
      "baseUrl": "https://ark.example",
      "apiKey": "sk-2",
      "priority": 0,
      "model": "ark-code-latest",
      "fallback": true,
      "models": [{"id": "ark-code-latest"}]
    },
    "badfields": {
      "baseUrl": "https://bad.example/v1",
      "apiKey": "sk-3",
      "priority": "high",
      "fallback": "nope",
      "models": [{"id": "b1"}]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	provs := LoadProvidersFile()
	if len(provs) != 3 {
		t.Fatalf("expected 3 providers, got %d: %+v", len(provs), provs)
	}
	cfg := &Config{Provider: "main", Model: "m1", FallbackEnabled: true, CustomProviders: provs}

	ark, ok := cfg.FindProvider("ark")
	if !ok || ark.Priority == nil || *ark.Priority != 0 || ark.Model != "ark-code-latest" || ark.Fallback == nil || !*ark.Fallback {
		t.Fatalf("unexpected ark provider: %+v", ark)
	}
	bad, ok := cfg.FindProvider("badfields")
	if !ok || bad.Priority != nil || bad.Fallback != nil {
		t.Fatalf("malformed optional fields must be ignored without dropping the provider: %+v", bad)
	}

	wantChain := []ProviderTarget{
		{Provider: "main", Model: "m1"},
		{Provider: "ark", Model: "ark-code-latest"},
	}
	if got := cfg.ProviderChain(); !reflect.DeepEqual(got, wantChain) {
		t.Fatalf("ProviderChain() = %+v, want %+v", got, wantChain)
	}
}
