package adk

import (
	"encoding/json"
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
