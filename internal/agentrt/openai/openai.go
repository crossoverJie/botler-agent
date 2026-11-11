// Package openai implements the OpenAI Chat Completions wire protocol (the default for
// providers.json entries without an explicit `api`, and for legacy CUSTOM_* gateways).
//
// Tool results are sent as v1's pi-ai adapter did: a plain-text `role:"tool"` message, never a
// JSON envelope (design doc §3.3).
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

// Client speaks the OpenAI chat completions protocol against a (possibly custom) base URL.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	// Vision declares whether the model accepts image input; false replaces images with a note.
	Vision    bool
	MaxTokens int
	HTTP      *http.Client
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Tools     []chatTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   *string        `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Endpoint returns the chat completions URL for the configured base URL.
func (c *Client) Endpoint() string {
	return strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Build converts an agentrt request into the wire request body.
func (c *Client) Build(req agentrt.Request) chatRequest {
	out := chatRequest{Model: c.Model, Stream: false}
	if c.MaxTokens > 0 {
		out.MaxTokens = c.MaxTokens
	} else if req.MaxTokens > 0 {
		out.MaxTokens = req.MaxTokens
	}
	if req.System != "" {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case agentrt.RoleUser:
			parts := make([]chatPart, 0, len(m.Parts))
			for _, p := range m.Parts {
				switch p.Type {
				case agentrt.PartText:
					parts = append(parts, chatPart{Type: "text", Text: p.Text})
				case agentrt.PartImage:
					if !c.Vision {
						continue
					}
					parts = append(parts, chatPart{Type: "image_url", ImageURL: &chatImageURL{
						URL: "data:" + p.MimeType + ";base64," + base64.StdEncoding.EncodeToString(p.Data),
					}})
				}
			}
			hasImage := false
			for _, p := range parts {
				if p.Type == "image_url" {
					hasImage = true
				}
			}
			if hasImage || len(parts) > 1 {
				out.Messages = append(out.Messages, chatMessage{Role: "user", Content: parts})
			} else {
				text := ""
				if len(parts) == 1 {
					text = parts[0].Text
				}
				out.Messages = append(out.Messages, chatMessage{Role: "user", Content: text})
			}
		case agentrt.RoleAssistant:
			msg := chatMessage{Role: "assistant"}
			if text := m.Text(); text != "" {
				msg.Content = text
			}
			for _, call := range m.ToolCalls {
				tc := chatToolCall{ID: call.ID, Type: "function"}
				tc.Function.Name = call.Name
				tc.Function.Arguments = string(call.Arguments)
				if tc.Function.Arguments == "" {
					tc.Function.Arguments = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, tc)
			}
			out.Messages = append(out.Messages, msg)
		case agentrt.RoleTool:
			out.Messages = append(out.Messages, chatMessage{
				Role:       "tool",
				Content:    m.Text(),
				ToolCallID: m.ToolCallID,
			})
		}
	}
	for _, t := range req.Tools {
		tool := chatTool{Type: "function"}
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = t.Schema
		out.Tools = append(out.Tools, tool)
	}
	return out
}

// Complete performs one chat completion call.
func (c *Client) Complete(ctx context.Context, req agentrt.Request) (*agentrt.Message, error) {
	body, err := json.Marshal(c.Build(req))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai-completions %s: HTTP %d: %s", c.Model, resp.StatusCode, truncate(string(raw), 400))
	}
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("openai-completions %s: decode response: %v", c.Model, err)
	}
	if parsed.Error != nil {
		return nil, errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("openai-completions: response has no choices")
	}
	choice := parsed.Choices[0]
	msg := &agentrt.Message{Role: agentrt.RoleAssistant}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		msg.Parts = append(msg.Parts, agentrt.Part{Type: agentrt.PartText, Text: *choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, agentrt.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(defaultArgs(tc.Function.Arguments)),
		})
	}
	switch {
	case len(msg.ToolCalls) > 0 || choice.FinishReason == "tool_calls":
		msg.StopReason = agentrt.StopToolUse
	case choice.FinishReason == "length":
		msg.StopReason = agentrt.StopLength
	default:
		msg.StopReason = agentrt.StopStop
	}
	msg.Usage = &agentrt.Usage{
		Input:      parsed.Usage.PromptTokens,
		Output:     parsed.Usage.CompletionTokens,
		CacheRead:  parsed.Usage.PromptTokensDetails.CachedTokens,
		Reasoning:  parsed.Usage.CompletionTokensDetails.ReasoningTokens,
		Total:      parsed.Usage.TotalTokens,
		CacheWrite: 0,
		CostUSD:    0,
	}
	return msg, nil
}

func defaultArgs(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
