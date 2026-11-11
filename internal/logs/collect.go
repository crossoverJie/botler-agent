package logs

import (
	"encoding/json"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

// TextMax is the truncation limit applied to tool result text, thinking and conversation text.
// User messages and the final reply are deliberately not truncated.
const TextMax = 2000

func truncate(s string) string {
	r := []rune(s)
	if len(r) > TextMax {
		return string(r[:TextMax])
	}
	return s
}

// contentText joins the text blocks of a message (pi-ai's contentText equivalent).
func contentText(m *agentrt.Message) string {
	out := ""
	for _, p := range m.Parts {
		if p.Type == agentrt.PartText {
			out += p.Text
		}
	}
	return out
}

func sumUsage(list []*agentrt.Usage) TokenUsageLog {
	acc := TokenUsageLog{}
	for _, u := range list {
		if u == nil {
			continue
		}
		acc.Input += u.Input
		acc.Output += u.Output
		acc.CacheRead += u.CacheRead
		acc.CacheWrite += u.CacheWrite
		acc.Reasoning += u.Reasoning
		acc.Total += u.Total
		acc.CostUSD += u.CostUSD
	}
	return acc
}

// CollectInput is everything the collector needs to build a TaskLog.
type CollectInput struct {
	ID       string
	TaskID   string
	Phase    string
	Source   string
	Provider string
	Model    string
	// Projects are the selected data subprojects (ordered, deduplicated); empty = not routed.
	Projects    []string
	StartedAt   int64
	EndedAt     int64
	UserMessage string
	ReplyText   string
	Images      []string
	// InboundImageCount is nil when the caller has nothing to report (the field is then omitted).
	InboundImageCount *int
	Mutated           bool
	// Messages is the execution-phase conversation (empty for early-return paths).
	Messages []agentrt.Message
	// RoutingUsage is the routing LLM call usage (nil when routing short-circuited).
	RoutingUsage      *agentrt.Usage
	RoutingCandidates []string
	RoutingDecision   *string
	RoutingPrompt     string
	SystemPrompt      string
	ModelCache        ModelCacheStats
}

// CollectTaskLog renders a TaskLog from an execution's messages. It is a pure function (no I/O);
// the dispatcher owns Source and the final Status and overrides them afterwards.
func CollectTaskLog(in CollectInput) TaskLog {
	tools := make([]ToolCallLog, 0, 4)
	toolIndex := make(map[string]int, 4)
	conversation := make([]ConversationEntry, 0, len(in.Messages))
	calls := make([]CallUsage, 0, 4)

	for i := range in.Messages {
		m := &in.Messages[i]
		switch m.Role {
		case agentrt.RoleUser:
			text := contentText(m)
			conversation = append(conversation, ConversationEntry{
				Role:      "user",
				Timestamp: m.Timestamp,
				Text:      &text,
			})
		case agentrt.RoleAssistant:
			if m.Usage != nil {
				call := CallUsage{
					Timestamp:  m.Timestamp,
					StopReason: m.StopReason,
					Input:      m.Usage.Input,
					Output:     m.Usage.Output,
					CacheRead:  m.Usage.CacheRead,
					CacheWrite: m.Usage.CacheWrite,
					Total:      m.Usage.Total,
					CostUSD:    m.Usage.CostUSD,
				}
				if m.Usage.ReasoningKnown || m.Usage.Reasoning != 0 {
					r := m.Usage.Reasoning
					call.Reasoning = &r
				}
				calls = append(calls, call)
			}
			if text := contentText(m); text != "" {
				conversation = append(conversation, ConversationEntry{
					Role:       "assistant",
					Timestamp:  m.Timestamp,
					Text:       &text,
					StopReason: m.StopReason,
				})
			}
			for _, p := range m.Parts {
				if p.Type == agentrt.PartThinking {
					chars := len([]rune(p.Text))
					conversation = append(conversation, ConversationEntry{
						Role:       "assistant",
						Timestamp:  m.Timestamp,
						Thinking:   truncate(p.Text),
						Chars:      &chars,
						StopReason: m.StopReason,
					})
				}
			}
			for _, call := range m.ToolCalls {
				args := call.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				toolIndex[call.ID] = len(tools)
				tools = append(tools, ToolCallLog{
					ID:         call.ID,
					Name:       call.Name,
					Arguments:  args,
					ResultText: "",
					StartedAt:  m.Timestamp,
				})
			}
		case agentrt.RoleTool:
			text := contentText(m)
			chars := len([]rune(text))
			isErr := m.IsError
			truncated := truncate(text)
			conversation = append(conversation, ConversationEntry{
				Role:      "toolResult",
				Timestamp: m.Timestamp,
				Text:      &truncated,
				Chars:     &chars,
				ToolName:  m.ToolName,
				IsError:   &isErr,
			})
			idx, ok := toolIndex[m.ToolCallID]
			if !ok {
				continue
			}
			t := &tools[idx]
			t.ResultText = truncate(text)
			t.ResultTruncated = len([]rune(text)) > TextMax
			t.ResultChars = &chars
			t.IsError = m.IsError
			t.Details = m.Details
			t.DurationMs = m.Timestamp - t.StartedAt
		}
	}

	usageList := make([]*agentrt.Usage, 0, len(calls)+1)
	for i := range in.Messages {
		if in.Messages[i].Role == agentrt.RoleAssistant && in.Messages[i].Usage != nil {
			usageList = append(usageList, in.Messages[i].Usage)
		}
	}
	if in.RoutingUsage != nil {
		usageList = append(usageList, in.RoutingUsage)
	}
	usage := sumUsage(usageList)

	projects := in.Projects
	if projects == nil {
		projects = []string{}
	}
	images := in.Images
	if images == nil {
		images = []string{}
	}
	if in.StartedAt == 0 {
		in.StartedAt = time.Now().UnixMilli()
	}
	if in.EndedAt == 0 {
		in.EndedAt = time.Now().UnixMilli()
	}

	log := TaskLog{
		ID:                in.ID,
		TaskID:            in.TaskID,
		Phase:             in.Phase,
		Source:            in.Source,
		Provider:          in.Provider,
		Model:             in.Model,
		Project:           LegacyProject(projects),
		Projects:          projects,
		Status:            StatusSuccess,
		StartedAt:         in.StartedAt,
		EndedAt:           in.EndedAt,
		DurationMs:        in.EndedAt - in.StartedAt,
		UserMessage:       in.UserMessage,
		ReplyText:         in.ReplyText,
		Images:            images,
		InboundImageCount: in.InboundImageCount,
		Mutated:           in.Mutated,
		Tools:             tools,
		Conversation:      conversation,
		Usage:             usage,
		Calls:             calls,
		SystemPrompt:      in.SystemPrompt,
		ModelCache:        in.ModelCache,
	}
	if in.RoutingCandidates != nil || in.RoutingDecision != nil || in.RoutingPrompt != "" {
		routingUsage := sumUsage([]*agentrt.Usage{in.RoutingUsage})
		log.Routing = &RoutingInfo{
			Candidates: in.RoutingCandidates,
			Decision:   in.RoutingDecision,
			Usage:      routingUsage,
			Prompt:     in.RoutingPrompt,
		}
		if log.Routing.Candidates == nil {
			log.Routing.Candidates = []string{}
		}
	}
	return log
}
