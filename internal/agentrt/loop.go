package agentrt

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ToolTurnCapHint is injected as a user message as the tool-turn cap approaches, asking the model
// to stop calling tools and summarize (v1: TOOL_TURN_CAP_HINT in src/runner.ts).
const ToolTurnCapHint = "Note: this task's tool-call count is approaching the limit. Please stop calling any tools immediately (do not use read / write / edit / run / schedule again).\n" +
	"Based on the reads/writes you have already completed, give your final summary reply directly.\n" +
	"If the task is not finished, state honestly what is done, where it is stuck, and why it could not be completed."

// Loop runs one task: prompt the model, execute requested tools sequentially, feed the results
// back as plain text, and stop at the tool-turn cap.
type Loop struct {
	Runtime Runtime
	// MaxToolTurns counts assistant messages that contain at least one tool call (a message with
	// several calls still counts as one turn). Default 20 (MAX_TOOL_TURNS).
	MaxToolTurns int
}

// RunInput is one task execution.
type RunInput struct {
	SystemPrompt string
	UserText     string
	Images       []InboundImage
	Tools        []Tool
}

// RunResult is the outcome of a task execution.
type RunResult struct {
	// Messages is the full conversation (user / assistant / tool), used for logging and for
	// extracting the final reply.
	Messages []Message
	// ToolNames lists every executed tool name, in order.
	ToolNames []string
	// ToolTurnCapHit reports that the loop hard-stopped at the cap with no final reply.
	ToolTurnCapHit bool
}

func (l *Loop) maxTurns() int {
	if l.MaxToolTurns > 0 {
		return l.MaxToolTurns
	}
	return 20
}

// CountToolTurns counts assistant messages containing at least one tool call.
func CountToolTurns(messages []Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			n++
		}
	}
	return n
}

// Run executes the task. It never returns an error for model-provider failures: those become an
// assistant message with StopReason "error" (v1 behaviour), so the runner can render the two
// fixed English fallbacks. Only context cancellation aborts early.
func (l *Loop) Run(ctx context.Context, in RunInput) (*RunResult, error) {
	maxTurns := l.maxTurns()
	userParts := []Part{{Type: PartText, Text: in.UserText}}
	for _, img := range in.Images {
		userParts = append(userParts, Part{Type: PartImage, Data: img.Data, MimeType: img.MimeType})
	}
	messages := []Message{{Role: RoleUser, Timestamp: time.Now().UnixMilli(), Parts: userParts}}

	toolDefs := make([]ToolDef, 0, len(in.Tools))
	byName := make(map[string]Tool, len(in.Tools))
	for _, t := range in.Tools {
		toolDefs = append(toolDefs, ToolDef{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
		byName[t.Name()] = t
	}

	res := &RunResult{}
	executed := make([]string, 0, 8)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reply, err := l.Runtime.Complete(ctx, Request{System: in.SystemPrompt, Messages: messages, Tools: toolDefs})
		if err != nil {
			// Provider-level failure: v1 turns this into an error assistant message.
			reply = &Message{
				Role:         RoleAssistant,
				Timestamp:    time.Now().UnixMilli(),
				StopReason:   StopError,
				ErrorMessage: err.Error(),
				Usage:        &Usage{},
			}
		}
		if reply.Timestamp == 0 {
			reply.Timestamp = time.Now().UnixMilli()
		}
		messages = append(messages, *reply)

		if reply.StopReason == StopError || reply.StopReason == StopAborted || len(reply.ToolCalls) == 0 {
			break
		}

		for _, call := range reply.ToolCalls {
			executed = append(executed, call.Name)
			messages = append(messages, l.execTool(ctx, call, byName[call.Name]))
		}

		if CountToolTurns(messages) >= maxTurns {
			res.ToolTurnCapHit = true
			break
		}
		if maxTurns >= 2 && CountToolTurns(messages) == maxTurns-1 {
			messages = append(messages, Message{
				Role:      RoleUser,
				Timestamp: time.Now().UnixMilli(),
				Parts:     []Part{{Type: PartText, Text: ToolTurnCapHint}},
			})
		}
	}

	res.Messages = messages
	res.ToolNames = executed
	return res, nil
}

func (l *Loop) execTool(ctx context.Context, call ToolCall, tool Tool) Message {
	msg := Message{
		Role:       RoleTool,
		Timestamp:  time.Now().UnixMilli(),
		ToolCallID: call.ID,
		ToolName:   call.Name,
	}
	if tool == nil {
		msg.IsError = true
		msg.Parts = []Part{{Type: PartText, Text: fmt.Sprintf("Unknown tool: %s", call.Name)}}
		return msg
	}
	res, err := tool.Execute(ctx, call.Arguments)
	if err != nil {
		msg.IsError = true
		msg.Parts = []Part{{Type: PartText, Text: err.Error()}}
		return msg
	}
	msg.Parts = []Part{{Type: PartText, Text: res.Text}}
	if res.Details != nil {
		if raw, err := json.Marshal(res.Details); err == nil {
			msg.Details = raw
		}
	}
	return msg
}
