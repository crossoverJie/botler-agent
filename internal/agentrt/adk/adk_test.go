package adk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

func TestBuildContentsRoles(t *testing.T) {
	msgs := []agentrt.Message{
		{Role: agentrt.RoleUser, Parts: []agentrt.Part{{Type: agentrt.PartText, Text: "hi"}}},
		{Role: agentrt.RoleAssistant, Parts: []agentrt.Part{{Type: agentrt.PartText, Text: "calling"}},
			ToolCalls: []agentrt.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.json"}`)}}},
		{Role: agentrt.RoleTool, ToolCallID: "c1", ToolName: "read", Parts: []agentrt.Part{{Type: agentrt.PartText, Text: "content"}}},
	}
	contents := BuildContents(msgs, true)
	if len(contents) != 3 {
		t.Fatalf("want 3 contents, got %d", len(contents))
	}
	if contents[0].Role != "user" || contents[1].Role != "model" || contents[2].Role != "user" {
		t.Fatalf("unexpected roles: %q %q %q", contents[0].Role, contents[1].Role, contents[2].Role)
	}
	// The assistant turn must carry the function call with its args so the model can be answered.
	parts := contents[1].Parts
	if len(parts) != 2 || parts[1].FunctionCall == nil {
		t.Fatalf("expected text + function call, got %+v", parts)
	}
	if parts[1].FunctionCall.ID != "c1" || parts[1].FunctionCall.Name != "read" {
		t.Fatalf("unexpected function call: %+v", parts[1].FunctionCall)
	}
	if got := parts[1].FunctionCall.Args["path"]; got != "a.json" {
		t.Fatalf("unexpected args: %v", parts[1].FunctionCall.Args)
	}
	// The tool result must travel as a function response in a user-role content.
	fr := contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.ID != "c1" || fr.Name != "read" {
		t.Fatalf("unexpected function response: %+v", fr)
	}
	if fr.Response["result"] != "content" {
		t.Fatalf("tool text must be wrapped as {result: ...}: %v", fr.Response)
	}
}

func TestBuildContentsReplacesImagesForTextOnlyModels(t *testing.T) {
	msgs := []agentrt.Message{{Role: agentrt.RoleUser, Parts: []agentrt.Part{
		{Type: agentrt.PartText, Text: "看看这个"},
		{Type: agentrt.PartImage, Data: []byte{1, 2, 3}, MimeType: "image/jpeg"},
		{Type: agentrt.PartImage, Data: []byte{4, 5, 6}, MimeType: "image/png"},
	}}}

	withVision := BuildContents(msgs, true)
	images := 0
	for _, p := range withVision[0].Parts {
		if p.InlineData != nil {
			images++
		}
	}
	if images != 2 {
		t.Fatalf("vision model must receive both images, got %d", images)
	}

	textOnly := BuildContents(msgs, false)
	var placeholders int
	for _, p := range textOnly[0].Parts {
		if p.Text == nonVisionImagePlaceholder {
			placeholders++
		}
		if p.InlineData != nil {
			t.Fatal("text-only model must not receive image bytes")
		}
	}
	if placeholders != 1 {
		t.Fatalf("consecutive image placeholders must collapse into one, got %d", placeholders)
	}
}

func TestBuildToolsCarriesJSONSchema(t *testing.T) {
	tools := BuildTools([]agentrt.ToolDef{{
		Name: "write", Description: "write a file",
		Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}})
	if len(tools) != 1 || len(tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	decl := tools[0].FunctionDeclarations[0]
	if decl.Name != "write" {
		t.Fatalf("unexpected declaration: %+v", decl)
	}
	schema, ok := decl.ParametersJsonSchema.(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("schema must be passed through as JSON: %#v", decl.ParametersJsonSchema)
	}
}

func TestStopReasonMapping(t *testing.T) {
	cases := []struct {
		reason    string
		toolCalls int
		want      string
	}{
		{"", 0, agentrt.StopStop},
		{"STOP", 0, agentrt.StopStop},
		{"MAX_TOKENS", 0, agentrt.StopLength},
		{"", 1, agentrt.StopToolUse},
		{"SAFETY", 0, agentrt.StopLength},
	}
	for _, c := range cases {
		if got := stopReason(finishReason(c.reason), c.toolCalls); got != c.want {
			t.Errorf("stopReason(%q,%d)=%q want %q", c.reason, c.toolCalls, got, c.want)
		}
	}
}

// finishReason converts a test string into genai's enum type.
func finishReason(s string) genai.FinishReason { return genai.FinishReason(s) }

func TestSystemInstruction(t *testing.T) {
	// An all-whitespace prompt must become nil, not an empty content block: sending an empty system
	// instruction makes some providers reject the request.
	if got := systemInstruction("   \n\t "); got != nil {
		t.Fatalf("whitespace-only prompt must map to nil, got %+v", got)
	}
	got := systemInstruction("you are botler")
	if got == nil || got.Role != "user" {
		t.Fatalf("unexpected content: %+v", got)
	}
	if len(got.Parts) != 1 || got.Parts[0].Text != "you are botler" {
		t.Fatalf("unexpected parts: %+v", got.Parts)
	}
}

func TestMergeCallAppendsThenMerges(t *testing.T) {
	index := map[string]int{}
	calls := mergeCall(nil, index, &genai.FunctionCall{ID: "c1", Name: "read", Args: map[string]any{"path": "a"}})
	if len(calls) != 1 || calls[0].ID != "c1" || calls[0].Name != "read" {
		t.Fatalf("unexpected first call: %+v", calls)
	}
	if !strings.Contains(string(calls[0].Arguments), `"path":"a"`) {
		t.Fatalf("unexpected args: %s", calls[0].Arguments)
	}

	// The same id streamed again with growing arguments grows the existing entry in place.
	calls = mergeCall(calls, index, &genai.FunctionCall{ID: "c1", Args: map[string]any{"path": "a", "n": 2}})
	if len(calls) != 1 {
		t.Fatalf("a repeated id must merge, got %d calls", len(calls))
	}
	if !strings.Contains(string(calls[0].Arguments), `"n":2`) {
		t.Fatalf("arguments were not merged: %s", calls[0].Arguments)
	}
	// An empty name/id on the update must not erase the accumulated values.
	if calls[0].Name != "read" || calls[0].ID != "c1" {
		t.Fatalf("identity was erased: %+v", calls[0])
	}

	// An empty payload ({} is 2 bytes) must not overwrite real arguments.
	calls = mergeCall(calls, index, &genai.FunctionCall{ID: "c1"})
	if !strings.Contains(string(calls[0].Arguments), `"n":2`) {
		t.Fatalf("empty args overwrote the payload: %s", calls[0].Arguments)
	}

	// A call without an id is keyed by name, so an id-less stream still merges in place.
	calls = mergeCall(calls, index, &genai.FunctionCall{Name: "write", Args: map[string]any{"x": 1}})
	calls = mergeCall(calls, index, &genai.FunctionCall{Name: "write", Args: map[string]any{"x": 1, "y": 2}})
	if len(calls) != 2 {
		t.Fatalf("name-keyed call must merge, got %+v", calls)
	}
	if !strings.Contains(string(calls[1].Arguments), `"y":2`) {
		t.Fatalf("name-keyed arguments were not merged: %s", calls[1].Arguments)
	}

	// Known limitation, documented here on purpose: the key prefers the id, so a chunk that only
	// later starts carrying an id is treated as a new call rather than folded into the name-keyed
	// one. Providers seen so far send the id in the first chunk, so this has not been an issue.
	before := len(calls)
	calls = mergeCall(calls, index, &genai.FunctionCall{ID: "c2", Name: "write", Args: map[string]any{"z": 3}})
	if len(calls) != before+1 || calls[before].ID != "c2" {
		t.Fatalf("a new id must start a new call: %+v", calls)
	}
}

func TestStopReason(t *testing.T) {
	cases := []struct {
		reason genai.FinishReason
		calls  int
		want   string
	}{
		{genai.FinishReasonStop, 0, agentrt.StopStop},
		{genai.FinishReasonUnspecified, 0, agentrt.StopStop},
		{"", 0, agentrt.StopStop},
		{genai.FinishReasonMaxTokens, 0, agentrt.StopLength},
		{genai.FinishReasonSafety, 0, agentrt.StopLength},
		// Tool calls win over the finish reason: the loop must keep going.
		{genai.FinishReasonStop, 2, agentrt.StopToolUse},
	}
	for _, c := range cases {
		if got := stopReason(c.reason, c.calls); got != c.want {
			t.Fatalf("stopReason(%q,%d) = %q, want %q", c.reason, c.calls, got, c.want)
		}
	}
}

func TestConvertUsage(t *testing.T) {
	// nil metadata still yields a usable (zeroed) Usage: the task log always has the fields.
	usage := convertUsage(nil)
	if usage == nil || usage.Total != 0 || usage.CostUSD != 0 {
		t.Fatalf("unexpected nil-metadata usage: %+v", usage)
	}

	usage = convertUsage(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        10,
		CandidatesTokenCount:    20,
		CachedContentTokenCount: 5,
		ThoughtsTokenCount:      3,
	})
	if usage.Input != 10 || usage.Output != 20 || usage.CacheRead != 5 || usage.Reasoning != 3 {
		t.Fatalf("unexpected mapping: %+v", usage)
	}
	// TotalTokenCount absent: it is derived from input + output.
	if usage.Total != 30 {
		t.Fatalf("total must fall back to input+output, got %d", usage.Total)
	}
	if !usage.ReasoningKnown {
		t.Fatal("ReasoningKnown must be true when the provider reports the field")
	}
	if usage.CostUSD != 0 {
		t.Fatal("cost is always zero by design")
	}

	usage = convertUsage(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 1, CandidatesTokenCount: 2, TotalTokenCount: 99,
	})
	if usage.Total != 99 {
		t.Fatalf("an explicit total must win, got %d", usage.Total)
	}
}

func TestConstructors(t *testing.T) {
	ctx := context.Background()

	openai, err := NewOpenAI(ctx, OpenAIConfig{BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", Model: "m", MaxTokens: 64, Vision: true})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if openai.modelName != "m" || openai.maxTokens != 64 || !openai.vision {
		t.Fatalf("openai runtime not populated: %+v", openai)
	}

	anthropic, err := NewAnthropic(ctx, AnthropicConfig{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m2", MaxTokens: 32})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	if anthropic.modelName != "m2" || anthropic.maxTokens != 32 {
		t.Fatalf("anthropic runtime not populated: %+v", anthropic)
	}
}
