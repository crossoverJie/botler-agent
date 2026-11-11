// Package agentrt is the agent runtime seam: a Request/Reply interface, the tool loop and the
// per-provider wire clients. Business code (runner / dispatch) only sees this interface, so the
// loop can be swapped without touching routing, logging or the scheduler.
//
// Design decision (docs/design/go-rewrite-adk-go.md §3.3 / Q-1, recorded in
// docs/design/go-rewrite-status.md): the tool loop is self-implemented (option A) while the wire
// protocol comes from adk-go — `openaimodel` with APIChatCompletions and `anthropicmodel`.
// adk-go always JSON-encodes a tool response ({"result": "..."}), so the model does not see v1's
// plain-text tool message; that is deviation D-1, not an implementation detail.
package agentrt

import (
	"context"
	"encoding/json"
)

// Role is a conversation message role.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType identifies a content block.
type PartType string

// Content block types.
const (
	PartText     PartType = "text"
	PartThinking PartType = "thinking"
	PartImage    PartType = "image"
)

// Part is one content block of a message.
type Part struct {
	Type PartType
	Text string // text / thinking
	// Signature is the provider's opaque signature of a thinking block (Anthropic returns one and
	// requires it back when the conversation is replayed). Empty when the provider does not use them.
	Signature []byte
	MimeType  string // image
	Data      []byte // image bytes (inline)
}

// ToolCall is one tool invocation requested by the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Usage is the token accounting of a single LLM call. CostUSD is always 0: v1 hard-codes zero
// unit prices for providers.json models, so cost is a deterministic constant.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	Reasoning  int
	// ReasoningKnown reports whether the provider returned a reasoning-token count at all, so a
	// genuine 0 can be distinguished from "not reported" (v1 omits the field in the latter case).
	ReasoningKnown bool
	Total          int
	CostUSD        float64
}

// Stop reasons, matching pi-ai's vocabulary (logs record these strings verbatim).
const (
	StopStop     = "stop"
	StopLength   = "length"
	StopToolUse  = "toolUse"
	StopError    = "error"
	StopAborted  = "aborted"
	StopDeferred = "deferred"
)

// Message is one conversation entry. Tool results are RoleTool messages carrying ToolCallID /
// ToolName / IsError plus their text and details.
type Message struct {
	Role         Role
	Timestamp    int64
	Parts        []Part
	ToolCalls    []ToolCall
	ToolCallID   string
	ToolName     string
	IsError      bool
	Details      json.RawMessage
	StopReason   string
	ErrorMessage string
	Usage        *Usage
}

// Text returns the concatenated text parts of a message.
func (m *Message) Text() string {
	out := ""
	for _, p := range m.Parts {
		if p.Type == PartText {
			out += p.Text
		}
	}
	return out
}

// Thinking returns the concatenated thinking parts of a message.
func (m *Message) Thinking() string {
	out := ""
	for _, p := range m.Parts {
		if p.Type == PartThinking {
			out += p.Text
		}
	}
	return out
}

// ToolResult is what a Tool returns to the loop.
type ToolResult struct {
	Text    string
	Details any
}

// Tool is a tool the agent may call. Schema is a JSON Schema object describing the parameters.
type Tool interface {
	Name() string
	Label() string
	Description() string
	Schema() json.RawMessage
	Execute(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ToolDef is one tool declaration sent to the model.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// InboundImage is an image received from a channel, fed to the model as vision input.
type InboundImage struct {
	Data     []byte
	MimeType string
}

// Request is one LLM call.
type Request struct {
	System   string
	Messages []Message
	Tools    []ToolDef
	// MaxTokens overrides the provider default (0 = default).
	MaxTokens int
}

// Runtime is a model provider client.
type Runtime interface {
	Complete(ctx context.Context, req Request) (*Message, error)
}
