// Package logs implements the append-only task-log store used by the WebUI.
//
// One TaskLog is written per dispatch execution (phase "execute") and, when a self-heal retry
// happens, one extra TaskLog with phase "self-heal". The logs are JSONL, one file per day
// (<BOTLER_LOG_DIR>/YYYY-MM-DD.jsonl), outside DATA_ROOT so they never enter git or the app/data
// separation boundary. All reads tolerate corrupt lines; the only mutating operation is
// CleanupLogs, which is locked to day-file names inside the log directory.
//
// JSON field names are camelCase and match v1 (src/logging/types.ts) byte-for-byte so the same
// WebUI works against both implementations.
package logs

import "encoding/json"

// TaskStatus is the final status of a dispatch.
type TaskStatus string

// Task statuses.
const (
	StatusSuccess          TaskStatus = "success"
	StatusAutoFixed        TaskStatus = "auto-fixed"
	StatusValidationFailed TaskStatus = "validation-failed"
	StatusError            TaskStatus = "error"
	StatusDuplicate        TaskStatus = "duplicate"
	StatusUnknownProject   TaskStatus = "unknown-project"
)

// TaskLog phases.
const (
	PhaseExecute  = "execute"
	PhaseSelfHeal = "self-heal"
)

// ToolCallLog is a single tool call paired with its result.
type ToolCallLog struct {
	// ID is the toolCallId, matching the corresponding tool result.
	ID string `json:"id"`
	// Name is the tool name.
	Name string `json:"name"`
	// Arguments are the raw tool arguments; kept as raw JSON so key order is never rewritten.
	Arguments json.RawMessage `json:"arguments"`
	// ResultText is the tool return text, truncated to TEXT_MAX characters.
	ResultText string `json:"resultText"`
	// ResultTruncated reports whether the result was longer than TEXT_MAX.
	ResultTruncated bool `json:"resultTruncated"`
	// IsError reports whether the tool returned an error to the model.
	IsError bool `json:"isError"`
	// StartedAt is the timestamp of the assistant message that issued this call.
	StartedAt int64 `json:"startedAt"`
	// DurationMs is the result timestamp minus StartedAt.
	DurationMs int64 `json:"durationMs"`
	// ResultChars is the full length of the tool return before truncation.
	ResultChars *int `json:"resultChars,omitempty"`
	// Details are the raw tool details (path / bytes / script / cwd ...).
	Details json.RawMessage `json:"details,omitempty"`
}

// ConversationEntry is one entry in the conversation timeline.
type ConversationEntry struct {
	Role      string `json:"role"`
	Timestamp int64  `json:"timestamp"`
	// Text is the user text / assistant reply text / tool result text. It is a pointer so an
	// empty-but-present text (e.g. an image-only user turn) still serializes as "" while a
	// thinking-only entry omits the key entirely, exactly like v1.
	Text *string `json:"text,omitempty"`
	// Thinking is the assistant thinking block.
	Thinking string `json:"thinking,omitempty"`
	// Chars is the full length of text/thinking before truncation.
	Chars *int `json:"chars,omitempty"`
	// StopReason is the assistant stop reason.
	StopReason string `json:"stopReason,omitempty"`
	// ToolName is the tool result's tool name.
	ToolName string `json:"toolName,omitempty"`
	// IsError is the tool result's error flag.
	IsError *bool `json:"isError,omitempty"`
}

// TokenUsageLog is the token usage of one LLM call or of an aggregate.
type TokenUsageLog struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// Reasoning is a subset of Output (reasoning tokens); never re-add it to Output.
	Reasoning int `json:"reasoning"`
	Total     int `json:"total"`
	// CostUSD is usage.cost.total. It is always 0: v1 hard-codes zero unit prices for
	// providers.json models, so this is a deterministic constant, not a measurement.
	CostUSD float64 `json:"costUsd"`
}

// CallUsage is the per-LLM-call usage detail, in conversation order.
type CallUsage struct {
	Timestamp  int64   `json:"timestamp"`
	StopReason string  `json:"stopReason,omitempty"`
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	CacheRead  int     `json:"cacheRead"`
	CacheWrite int     `json:"cacheWrite"`
	Reasoning  *int    `json:"reasoning,omitempty"`
	Total      int     `json:"total"`
	CostUSD    float64 `json:"costUsd"`
}

// ModelCacheStats is the process-lifetime model-resolution cache statistics.
type ModelCacheStats struct {
	Queries int     `json:"queries"`
	Hits    int     `json:"hits"`
	HitRate float64 `json:"hitRate"`
}

// RoutingInfo is present only on the main execute log when routing ran.
type RoutingInfo struct {
	Candidates []string      `json:"candidates"`
	Decision   *string       `json:"decision"`
	Usage      TokenUsageLog `json:"usage"`
	Prompt     string        `json:"prompt,omitempty"`
}

// TaskLog is one dispatch execution record.
type TaskLog struct {
	// ID is the per-log id.
	ID string `json:"id"`
	// TaskID is the dispatch-level id shared by the main log and its self-heal retry.
	TaskID string `json:"taskId"`
	Phase  string `json:"phase"`
	Source string `json:"source"`
	// Provider and Model are the configured provider/model ids.
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Project is the legacy singular display field: non-null only when exactly one real data
	// project was selected.
	Project *string `json:"project"`
	// Projects are the selected data subprojects (ordered, deduplicated); empty = not routed.
	Projects []string   `json:"projects"`
	Status   TaskStatus `json:"status"`
	// StartedAt / EndedAt are epoch milliseconds.
	StartedAt int64 `json:"startedAt"`
	EndedAt   int64 `json:"endedAt"`
	// DurationMs is EndedAt - StartedAt.
	DurationMs int64 `json:"durationMs"`
	// UserMessage and ReplyText are never truncated.
	UserMessage string   `json:"userMessage"`
	ReplyText   string   `json:"replyText"`
	Images      []string `json:"images"`
	// InboundImageCount is the number of inbound images that accompanied the message.
	InboundImageCount *int                `json:"inboundImageCount,omitempty"`
	Mutated           bool                `json:"mutated"`
	Tools             []ToolCallLog       `json:"tools"`
	Conversation      []ConversationEntry `json:"conversation"`
	// Usage aggregates the execute + routing calls.
	Usage TokenUsageLog `json:"usage"`
	// Calls holds the per-LLM-call usage in the execute phase, in conversation order.
	Calls []CallUsage `json:"calls"`
	// SystemPrompt is the rendered execution system prompt (absent for early-return paths).
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// ModelCache is the process-level snapshot at log time.
	ModelCache ModelCacheStats `json:"modelCache"`
	// Routing is present only on the main execute log when routing ran.
	Routing *RoutingInfo `json:"routing,omitempty"`
}

// LegacyProject derives the singular display field from the selected set: non-null only when
// exactly one real data project was selected. The virtual __scheduler__ project is never a data
// project, so it yields null too.
func LegacyProject(projects []string) *string {
	if len(projects) == 1 && projects[0] != "__scheduler__" {
		p := projects[0]
		return &p
	}
	return nil
}
