package logs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
)

func TestLegacyProject(t *testing.T) {
	if p := LegacyProject([]string{"cook"}); p == nil || *p != "cook" {
		t.Fatalf("single project should be promoted, got %v", p)
	}
	if p := LegacyProject([]string{"cook", "notes"}); p != nil {
		t.Fatalf("multi project must stay null, got %v", *p)
	}
	if p := LegacyProject([]string{"__scheduler__"}); p != nil {
		t.Fatalf("virtual scheduler project must stay null, got %v", *p)
	}
	if p := LegacyProject(nil); p != nil {
		t.Fatal("empty selection must stay null")
	}
}

func TestCollectTaskLog(t *testing.T) {
	longText := strings.Repeat("x", 3000)
	args := json.RawMessage(`{"path":"cook/data/intake.json"}`)
	details := json.RawMessage(`{"path":"cook/data/intake.json","bytes":42}`)

	messages := []agentrt.Message{
		{Role: agentrt.RoleUser, Timestamp: 1000, Parts: []agentrt.Part{{Type: agentrt.PartText, Text: "统计一下"}}},
		{
			Role:       agentrt.RoleAssistant,
			Timestamp:  1100,
			StopReason: agentrt.StopStop,
			Parts: []agentrt.Part{
				{Type: agentrt.PartText, Text: "我先查一下数据"},
				{Type: agentrt.PartThinking, Text: "thinking..."},
			},
			Usage:     &agentrt.Usage{Input: 100, Output: 20, CacheRead: 5, Total: 125},
			ToolCalls: []agentrt.ToolCall{{ID: "call-1", Name: "read", Arguments: args}},
		},
		{
			Role:       agentrt.RoleTool,
			Timestamp:  1200,
			ToolCallID: "call-1",
			ToolName:   "read",
			Parts:      []agentrt.Part{{Type: agentrt.PartText, Text: longText}},
			Details:    details,
		},
		{
			Role:       agentrt.RoleAssistant,
			Timestamp:  1300,
			StopReason: agentrt.StopStop,
			Parts:      []agentrt.Part{{Type: agentrt.PartText, Text: "共 1500 千卡"}},
			Usage:      &agentrt.Usage{Input: 200, Output: 30, Total: 230},
		},
	}

	count := 1
	log := CollectTaskLog(CollectInput{
		ID:                "log-1",
		TaskID:            "task-1",
		Phase:             PhaseExecute,
		Source:            "wechat",
		Provider:          "custom",
		Model:             "m",
		Projects:          []string{"cook"},
		StartedAt:         1000,
		EndedAt:           2000,
		UserMessage:       "统计一下",
		ReplyText:         "共 1500 千卡",
		Images:            []string{},
		InboundImageCount: &count,
		Mutated:           false,
		Messages:          messages,
		RoutingUsage:      &agentrt.Usage{Input: 7, Output: 3, Total: 10},
		RoutingCandidates: []string{"cook", "__scheduler__"},
		RoutingPrompt:     "route prompt",
		SystemPrompt:      "system prompt",
		ModelCache:        ModelCacheStats{Queries: 2, Hits: 1, HitRate: 0.5},
	})

	if log.Status != StatusSuccess {
		t.Fatalf("status should be seeded success, got %s", log.Status)
	}
	if log.DurationMs != 1000 {
		t.Fatalf("durationMs = %d", log.DurationMs)
	}
	if log.Project == nil || *log.Project != "cook" {
		t.Fatalf("project = %v", log.Project)
	}
	if log.InboundImageCount == nil || *log.InboundImageCount != 1 {
		t.Fatalf("inboundImageCount = %v", log.InboundImageCount)
	}

	if len(log.Tools) != 1 {
		t.Fatalf("tools = %+v", log.Tools)
	}
	tool := log.Tools[0]
	if tool.Name != "read" || tool.ID != "call-1" {
		t.Fatalf("tool identity = %+v", tool)
	}
	if string(tool.Arguments) != string(args) {
		t.Fatalf("arguments must be preserved verbatim, got %s", string(tool.Arguments))
	}
	if len([]rune(tool.ResultText)) != TextMax || !tool.ResultTruncated {
		t.Fatalf("tool result must be truncated to %d chars, got %d", TextMax, len([]rune(tool.ResultText)))
	}
	if tool.ResultChars == nil || *tool.ResultChars != 3000 {
		t.Fatalf("resultChars = %v", tool.ResultChars)
	}
	if tool.DurationMs != 100 {
		t.Fatalf("tool durationMs = %d", tool.DurationMs)
	}
	if string(tool.Details) != string(details) {
		t.Fatalf("details must be preserved verbatim, got %s", string(tool.Details))
	}

	roles := make([]string, 0, len(log.Conversation))
	for _, c := range log.Conversation {
		roles = append(roles, c.Role)
	}
	wantRoles := []string{"user", "assistant", "assistant", "toolResult", "assistant"}
	if strings.Join(roles, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("conversation roles = %v, want %v", roles, wantRoles)
	}
	if log.Conversation[2].Thinking != "thinking..." || log.Conversation[2].Chars == nil {
		t.Fatalf("thinking entry = %+v", log.Conversation[2])
	}
	if log.Conversation[3].ToolName != "read" || log.Conversation[3].IsError == nil || *log.Conversation[3].IsError {
		t.Fatalf("tool result entry = %+v", log.Conversation[3])
	}
	if log.Conversation[3].Text == nil || len([]rune(*log.Conversation[3].Text)) != TextMax {
		t.Fatalf("tool result conversation text must be truncated, got %v", log.Conversation[3].Text)
	}
	if log.Conversation[2].Text != nil {
		t.Fatalf("thinking entry must omit text, got %v", *log.Conversation[2].Text)
	}
	if log.Conversation[0].Text == nil || *log.Conversation[0].Text != "统计一下" {
		t.Fatalf("user entry text = %v", log.Conversation[0].Text)
	}

	if len(log.Calls) != 2 {
		t.Fatalf("calls = %+v", log.Calls)
	}
	if log.Calls[0].Input != 100 || log.Calls[0].StopReason != agentrt.StopStop {
		t.Fatalf("call[0] = %+v", log.Calls[0])
	}

	// Aggregate: execute calls (125 + 230) plus the routing call (10).
	if log.Usage.Input != 307 || log.Usage.Output != 53 || log.Usage.Total != 365 {
		t.Fatalf("usage aggregate = %+v", log.Usage)
	}
	if log.Usage.CostUSD != 0 {
		t.Fatalf("costUsd must be 0, got %v", log.Usage.CostUSD)
	}
	if log.Routing == nil {
		t.Fatal("routing info must be present")
	}
	if log.Routing.Decision != nil {
		t.Fatalf("routing decision = %v, want null", log.Routing.Decision)
	}
	if log.Routing.Usage.Total != 10 {
		t.Fatalf("routing usage = %+v", log.Routing.Usage)
	}
	if log.SystemPrompt != "system prompt" {
		t.Fatalf("systemPrompt = %q", log.SystemPrompt)
	}
	if log.ModelCache != (ModelCacheStats{Queries: 2, Hits: 1, HitRate: 0.5}) {
		t.Fatalf("modelCache = %+v", log.ModelCache)
	}
}

func TestCollectTaskLogJSONShape(t *testing.T) {
	log := CollectTaskLog(CollectInput{
		ID:        "id",
		TaskID:    "tid",
		Phase:     PhaseExecute,
		Source:    "cli",
		StartedAt: 1,
		EndedAt:   2,
		Messages:  []agentrt.Message{},
	})
	raw, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, key := range []string{`"taskId"`, `"startedAt"`, `"durationMs"`, `"userMessage"`, `"replyText"`, `"mutated"`, `"modelCache"`, `"usage"`, `"costUsd"`, `"projects":[]`} {
		if !strings.Contains(text, key) {
			t.Fatalf("serialized log missing %s: %s", key, text)
		}
	}
	for _, key := range []string{`"inboundImageCount"`, `"systemPrompt"`, `"routing"`} {
		if strings.Contains(text, key) {
			t.Fatalf("optional field %s must be omitted when unset: %s", key, text)
		}
	}
	if !strings.Contains(text, `"project":null`) {
		t.Fatalf("project must serialize as null when unrouted: %s", text)
	}
}
