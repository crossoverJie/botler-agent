// Package adk adapts adk-go's model layer to the agentrt.Runtime seam.
//
// Design note (docs/design/go-rewrite-adk-go.md §3.3 / Q-1): the tool loop itself stays in
// agentrt.Loop, while the wire protocol comes from adk-go (`openaimodel` with
// APIChatCompletions, and `anthropicmodel`). Because adk-go's model layer always serializes a
// tool result as JSON (`{"result": "..."}`), the model no longer sees v1's plain-text tool
// message — a documented, deliberate deviation of this option.
package adk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/anthropicmodel"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/genai"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

// Runtime implements agentrt.Runtime on top of an adk-go model.LLM.
type Runtime struct {
	llm       model.LLM
	modelName string
	maxTokens int
	// vision declares whether the model accepts image input. When false, images are replaced by
	// the same placeholder pi-ai used, instead of being sent (v1 parity).
	vision bool
}

// nonVisionImagePlaceholder mirrors pi-ai's NON_VISION_USER_IMAGE_PLACEHOLDER.
const nonVisionImagePlaceholder = "(image omitted: model does not support images)"

// OpenAIConfig configures an OpenAI-compatible gateway.
type OpenAIConfig struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	// Vision reports whether the model accepts image input (default true).
	Vision bool
}

// NewOpenAI builds a runtime against an OpenAI-compatible /chat/completions endpoint.
// APIChatCompletions is set explicitly: adk-go's zero value is the Responses API, which most
// self-hosted gateways do not implement.
func NewOpenAI(ctx context.Context, cfg OpenAIConfig) (*Runtime, error) {
	llm, err := openaimodel.NewModel(ctx, cfg.Model, &openaimodel.ClientConfig{
		APIKey:  cfg.APIKey,
		BaseURL: cfg.BaseURL,
		API:     openaimodel.APIChatCompletions,
	})
	if err != nil {
		return nil, err
	}
	return &Runtime{llm: llm, modelName: cfg.Model, maxTokens: cfg.MaxTokens, vision: cfg.Vision}, nil
}

// AnthropicConfig configures an Anthropic Messages endpoint (a custom base URL is supported).
type AnthropicConfig struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	// Vision reports whether the model accepts image input (default true).
	Vision bool
}

// NewAnthropic builds a runtime against an Anthropic Messages endpoint.
func NewAnthropic(ctx context.Context, cfg AnthropicConfig) (*Runtime, error) {
	llm, err := anthropicmodel.NewModel(ctx, cfg.Model, anthropicmodel.ClientConfig{
		APIKey:    cfg.APIKey,
		BaseURL:   cfg.BaseURL,
		MaxTokens: cfg.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	return &Runtime{llm: llm, modelName: cfg.Model, maxTokens: cfg.MaxTokens, vision: cfg.Vision}, nil
}

// ModelName reports the model id this runtime talks to.
func (r *Runtime) ModelName() string { return r.modelName }

// BuildContents converts agentrt messages into genai contents, replacing images with a text
// placeholder for models that do not accept image input.
func BuildContents(messages []agentrt.Message, vision bool) []*genai.Content {
	out := make([]*genai.Content, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case agentrt.RoleUser:
			parts := make([]*genai.Part, 0, len(m.Parts))
			lastWasPlaceholder := false
			for _, p := range m.Parts {
				switch p.Type {
				case agentrt.PartText:
					parts = append(parts, genai.NewPartFromText(p.Text))
					lastWasPlaceholder = p.Text == nonVisionImagePlaceholder
				case agentrt.PartImage:
					if vision {
						parts = append(parts, genai.NewPartFromBytes(p.Data, p.MimeType))
						lastWasPlaceholder = false
						continue
					}
					// v1 collapses runs of placeholders into one, so a multi-image message mentions
					// the omission once.
					if !lastWasPlaceholder {
						parts = append(parts, genai.NewPartFromText(nonVisionImagePlaceholder))
					}
					lastWasPlaceholder = true
				}
			}
			out = append(out, &genai.Content{Role: string(genai.RoleUser), Parts: parts})
		case agentrt.RoleAssistant:
			parts := make([]*genai.Part, 0, len(m.Parts)+len(m.ToolCalls))
			for _, p := range m.Parts {
				switch p.Type {
				case agentrt.PartText:
					if p.Text != "" {
						parts = append(parts, genai.NewPartFromText(p.Text))
					}
				case agentrt.PartThinking:
					// Replayed reasoning: the text and the provider signature travel together
					// (Anthropic rejects an unsigned thinking block; OpenAI drops it).
					parts = append(parts, &genai.Part{
						Thought:          true,
						Text:             p.Text,
						ThoughtSignature: p.Signature,
					})
				}
			}
			for _, call := range m.ToolCalls {
				args := map[string]any{}
				if len(call.Arguments) > 0 {
					_ = json.Unmarshal(call.Arguments, &args)
				}
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
					ID:   call.ID,
					Name: call.Name,
					Args: args,
				}})
			}
			out = append(out, &genai.Content{Role: string(genai.RoleModel), Parts: parts})
		case agentrt.RoleTool:
			// A function response travels in a user-role content, as adk-go's adapters expect.
			out = append(out, &genai.Content{
				Role: string(genai.RoleUser),
				Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
					ID:       m.ToolCallID,
					Name:     m.ToolName,
					Response: map[string]any{"result": m.Text()},
				}}},
			})
		}
	}
	return out
}

// BuildTools converts agentrt tool declarations into genai tools.
func BuildTools(tools []agentrt.ToolDef) []*genai.Tool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		var schema any
		if len(t.Schema) > 0 {
			var parsed any
			if err := json.Unmarshal(t.Schema, &parsed); err == nil {
				schema = parsed
			}
		}
		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJsonSchema: schema,
		})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

func systemInstruction(text string) *genai.Content {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return &genai.Content{Role: string(genai.RoleUser), Parts: []*genai.Part{genai.NewPartFromText(text)}}
}

// Complete performs one model call through adk-go.
//
// It always streams: the Anthropic SDK rejects a non-streaming request whose max_tokens could
// take longer than ten minutes, so streaming is the only shape that works for both providers.
// The stream is aggregated back into one assistant message (text / thinking / tool calls).
func (r *Runtime) Complete(ctx context.Context, req agentrt.Request) (*agentrt.Message, error) {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: systemInstruction(req.System),
		Tools:             BuildTools(req.Tools),
	}
	maxTokens := r.maxTokens
	if maxTokens == 0 {
		maxTokens = req.MaxTokens
	}
	if maxTokens > 0 {
		cfg.MaxOutputTokens = int32(maxTokens)
	}
	llmReq := &model.LLMRequest{Model: r.modelName, Contents: BuildContents(req.Messages, r.vision), Config: cfg}

	var (
		text        strings.Builder
		thinking    strings.Builder
		thinkingSig []byte
		calls       []agentrt.ToolCall
		callIndex   = map[string]int{}
		usage       *genai.GenerateContentResponseUsageMetadata
		finish      genai.FinishReason
		errMessage  string
		sawResponse bool
	)
	for resp, err := range r.llm.GenerateContent(ctx, llmReq, true) {
		if err != nil {
			return nil, err
		}
		if resp == nil {
			continue
		}
		sawResponse = true
		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}
		if resp.FinishReason != "" {
			finish = resp.FinishReason
		}
		if resp.ErrorMessage != "" || resp.ErrorCode != "" {
			errMessage = resp.ErrorMessage
			if errMessage == "" {
				errMessage = resp.ErrorCode
			}
		}
		if resp.Content == nil {
			continue
		}
		// adk-go marks streamed chunks Partial; the final non-partial response is the
		// authoritative aggregate and already contains the same text/calls, so restart the
		// accumulators when it arrives instead of appending twice.
		if !resp.Partial {
			text.Reset()
			thinking.Reset()
			thinkingSig = nil
			calls = calls[:0]
			callIndex = map[string]int{}
		}
		for _, p := range resp.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				calls = mergeCall(calls, callIndex, p.FunctionCall)
				continue
			}
			if p.Thought {
				thinking.WriteString(p.Text)
				if len(p.ThoughtSignature) > 0 {
					thinkingSig = p.ThoughtSignature
				}
				continue
			}
			if p.Text == "" {
				continue
			}
			text.WriteString(p.Text)
		}
	}
	if !sawResponse {
		return nil, fmt.Errorf("adk: %s returned no response", r.modelName)
	}
	if errMessage != "" {
		return nil, fmt.Errorf("%s", errMessage)
	}

	out := &agentrt.Message{Role: agentrt.RoleAssistant}
	if thinking.Len() > 0 || len(thinkingSig) > 0 {
		out.Parts = append(out.Parts, agentrt.Part{
			Type:      agentrt.PartThinking,
			Text:      thinking.String(),
			Signature: thinkingSig,
		})
	}
	if text.Len() > 0 {
		out.Parts = append(out.Parts, agentrt.Part{Type: agentrt.PartText, Text: text.String()})
	}
	out.ToolCalls = calls
	out.StopReason = stopReason(finish, len(out.ToolCalls))
	out.Usage = convertUsage(usage)
	return out, nil
}

// mergeCall folds a streamed function call into the accumulated list. A provider either sends the
// whole call once or repeats the same id with growing arguments, so the newest non-empty payload wins.
func mergeCall(calls []agentrt.ToolCall, index map[string]int, fc *genai.FunctionCall) []agentrt.ToolCall {
	args, err := json.Marshal(fc.Args)
	if err != nil {
		args = json.RawMessage("{}")
	}
	key := fc.ID
	if key == "" {
		key = fc.Name
	}
	if i, ok := index[key]; ok {
		if len(args) > 2 {
			calls[i].Arguments = args
		}
		if fc.Name != "" {
			calls[i].Name = fc.Name
		}
		if fc.ID != "" {
			calls[i].ID = fc.ID
		}
		return calls
	}
	index[key] = len(calls)
	return append(calls, agentrt.ToolCall{ID: fc.ID, Name: fc.Name, Arguments: args})
}

// stopReason maps adk-go's finish reason onto pi-ai's vocabulary, which the task logs record.
func stopReason(reason genai.FinishReason, toolCalls int) string {
	if toolCalls > 0 {
		return agentrt.StopToolUse
	}
	switch reason {
	case genai.FinishReasonMaxTokens:
		return agentrt.StopLength
	case genai.FinishReasonStop, genai.FinishReasonUnspecified, "":
		return agentrt.StopStop
	default:
		// Every other reason (safety, malformed function call, ...) is an incomplete turn.
		return agentrt.StopLength
	}
}

// convertUsage maps adk-go's usage metadata onto the task-log fields. costUsd stays 0: v1
// hard-codes zero unit prices for providers.json models.
func convertUsage(u *genai.GenerateContentResponseUsageMetadata) *agentrt.Usage {
	if u == nil {
		return &agentrt.Usage{}
	}
	total := int(u.TotalTokenCount)
	if total == 0 {
		total = int(u.PromptTokenCount) + int(u.CandidatesTokenCount)
	}
	return &agentrt.Usage{
		Input:          int(u.PromptTokenCount),
		Output:         int(u.CandidatesTokenCount),
		CacheRead:      int(u.CachedContentTokenCount),
		CacheWrite:     0,
		Reasoning:      int(u.ThoughtsTokenCount),
		ReasoningKnown: true,
		Total:          total,
		CostUSD:        0,
	}
}
