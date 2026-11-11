// Package fake is a deterministic agentrt.Runtime for tests: no network, scripted replies.
package fake

import (
	"context"
	"sync"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

// Step is one scripted reply.
type Step struct {
	// Text is the assistant text of this step.
	Text string
	// ToolCalls are executed after this reply (the loop feeds their results back).
	ToolCalls []agentrt.ToolCall
	// StopReason overrides the default (toolUse when ToolCalls is non-empty, else stop).
	StopReason string
	// ErrorMessage is set together with StopReason "error".
	ErrorMessage string
	// Usage overrides the default zero usage.
	Usage *agentrt.Usage
}

// Runtime replays Steps in order; the last step repeats when the loop calls again.
type Runtime struct {
	mu      sync.Mutex
	Steps   []Step
	Calls   []agentrt.Request
	callIdx int
}

// Complete implements agentrt.Runtime.
func (r *Runtime) Complete(_ context.Context, req agentrt.Request) (*agentrt.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, req)
	idx := r.callIdx
	if idx >= len(r.Steps) {
		idx = len(r.Steps) - 1
	}
	r.callIdx++
	if len(r.Steps) == 0 {
		return &agentrt.Message{Role: agentrt.RoleAssistant, StopReason: agentrt.StopStop}, nil
	}
	step := r.Steps[idx]
	msg := &agentrt.Message{Role: agentrt.RoleAssistant}
	if step.Text != "" {
		msg.Parts = append(msg.Parts, agentrt.Part{Type: agentrt.PartText, Text: step.Text})
	}
	msg.ToolCalls = step.ToolCalls
	msg.StopReason = step.StopReason
	if msg.StopReason == "" {
		if len(step.ToolCalls) > 0 {
			msg.StopReason = agentrt.StopToolUse
		} else {
			msg.StopReason = agentrt.StopStop
		}
	}
	msg.ErrorMessage = step.ErrorMessage
	if step.Usage != nil {
		msg.Usage = step.Usage
	} else {
		msg.Usage = &agentrt.Usage{}
	}
	return msg, nil
}

// CallCount returns how many LLM calls were made.
func (r *Runtime) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Calls)
}
