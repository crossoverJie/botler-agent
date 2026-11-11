// Package anthropic implements the Anthropic Messages wire protocol (`/v1/messages`) against a
// configurable base URL, matching pi-ai's anthropic provider path convention (the SDK appends
// /v1/messages to the configured base).
package anthropic

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

// Client speaks the Anthropic Messages protocol.
type Client struct {
	BaseURL   string
	APIKey    string
	Model     string
	Vision    bool
	MaxTokens int
	HTTP      *http.Client
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// image
	Source *imageSource `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	MaxTokens int                `json:"max_tokens"`
}

type messagesResponse struct {
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Endpoint returns the messages URL for the configured base URL.
func (c *Client) Endpoint() string {
	return strings.TrimRight(c.BaseURL, "/") + "/v1/messages"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) maxTokens(req agentrt.Request) int {
	if c.MaxTokens > 0 {
		return c.MaxTokens
	}
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return 8192
}

// Build converts an agentrt request into the wire request body.
func (c *Client) Build(req agentrt.Request) messagesRequest {
	out := messagesRequest{Model: c.Model, System: req.System, MaxTokens: c.maxTokens(req)}
	for _, m := range req.Messages {
		switch m.Role {
		case agentrt.RoleUser:
			var blocks []contentBlock
			for _, p := range m.Parts {
				switch p.Type {
				case agentrt.PartText:
					blocks = append(blocks, contentBlock{Type: "text", Text: p.Text})
				case agentrt.PartImage:
					if !c.Vision {
						continue
					}
					blocks = append(blocks, contentBlock{Type: "image", Source: &imageSource{
						Type:      "base64",
						MediaType: p.MimeType,
						Data:      base64.StdEncoding.EncodeToString(p.Data),
					}})
				}
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: blocks})
		case agentrt.RoleAssistant:
			var blocks []contentBlock
			if text := m.Text(); text != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: text})
			}
			for _, call := range m.ToolCalls {
				blocks = append(blocks, contentBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: call.Arguments})
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "assistant", Content: blocks})
		case agentrt.RoleTool:
			// Tool results travel in a user-role message, as an array of tool_result blocks.
			text, _ := json.Marshal(m.Text())
			block := contentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: text, IsError: m.IsError}
			if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == "user" {
				out.Messages[n-1].Content = append(out.Messages[n-1].Content, block)
			} else {
				out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: []contentBlock{block}})
			}
		}
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	return out
}

// Complete performs one messages call.
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
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if c.APIKey != "" {
		httpReq.Header.Set("x-api-key", c.APIKey)
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
		return nil, fmt.Errorf("anthropic-messages %s: HTTP %d: %s", c.Model, resp.StatusCode, truncate(string(raw), 400))
	}
	var parsed messagesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("anthropic-messages %s: decode response: %v", c.Model, err)
	}
	if parsed.Error != nil {
		return nil, errors.New(parsed.Error.Message)
	}
	msg := &agentrt.Message{Role: agentrt.RoleAssistant}
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			msg.Parts = append(msg.Parts, agentrt.Part{Type: agentrt.PartText, Text: block.Text})
		case "thinking":
			msg.Parts = append(msg.Parts, agentrt.Part{Type: agentrt.PartThinking, Text: block.Thinking})
		case "tool_use":
			args := block.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			msg.ToolCalls = append(msg.ToolCalls, agentrt.ToolCall{ID: block.ID, Name: block.Name, Arguments: args})
		}
	}
	switch {
	case len(msg.ToolCalls) > 0 || parsed.StopReason == "tool_use":
		msg.StopReason = agentrt.StopToolUse
	case parsed.StopReason == "max_tokens":
		msg.StopReason = agentrt.StopLength
	default:
		msg.StopReason = agentrt.StopStop
	}
	msg.Usage = &agentrt.Usage{
		Input:      parsed.Usage.InputTokens,
		Output:     parsed.Usage.OutputTokens,
		CacheRead:  parsed.Usage.CacheReadInputTokens,
		CacheWrite: parsed.Usage.CacheCreationInputTokens,
		Total:      parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
		CostUSD:    0,
	}
	return msg, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
