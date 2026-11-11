// Package dispatch serializes tasks, deduplicates messages, validates and commits data changes,
// and appends task logs. It never rejects: every failure becomes readable text plus a log line.
package dispatch

import (
	"strings"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/logs"
	"github.com/crossoverJie/botler-agent/internal/push"
)

// DuplicateSentinel is returned as Text when a message is dropped as a duplicate; channels must
// not echo it.
const DuplicateSentinel = "(duplicate message ignored)"

// Options are the per-dispatch knobs.
type Options struct {
	// ID is the dedup key (e.g. "<chatId>:<messageId>"). Duplicates within 5 minutes are ignored.
	ID string
	// Source is the channel name, used for logging only.
	Source string
	// ProjectHint is a valid data subproject name; it skips the routing LLM call.
	ProjectHint string
	// ProjectsHint is a plural routing hint (valid subproject names), used by the self-heal retry.
	ProjectsHint []string
	// Recipient is the message sender; tools such as schedule inject it as the push target.
	Recipient *push.Recipient
	// InboundImages are decoded images to feed the model AND persist under the target subproject.
	InboundImages []agentrt.InboundImage
	// SessionKey is the fixed conversation session key for IM channels (empty for scheduler / CLI).
	SessionKey string
}

// Result is the outcome returned to the channel.
type Result struct {
	// Text is the reply text. It may be empty when the reply is images only.
	Text string
	// Images are absolute paths inside DATA_ROOT, or https URLs.
	Images []string
	// Status mirrors the appended TaskLog status (used by the scheduler to decide on retry).
	Status logs.TaskStatus
}

// ScriptTaskOptions configures a direct scheduled-script run (bypassing the LLM).
type ScriptTaskOptions struct {
	// ID is the namespaced task id (e.g. "schedule:<id>:<epoch>").
	ID string
	// Script is the script path relative to DATA_ROOT.
	Script string
	// ScriptArgs are passed straight to the interpreter.
	ScriptArgs []string
	// Commit auto-commits the script's data changes on success (default true).
	Commit *bool
	// Source is the logging label (default "scheduler").
	Source string
	// Recipient is echoed through for logging only.
	Recipient *push.Recipient
}

// truncateCommit flattens whitespace and truncates to 60 runes, appending "…" when longer.
func truncateCommit(s string) string {
	flat := strings.Join(strings.Fields(s), " ")
	r := []rune(flat)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return flat
}
