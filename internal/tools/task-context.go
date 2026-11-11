// Package tools implements the five data tools plus the two narrow framework-level exceptions
// (schedule, clear_conversation_context). All file access goes through paths.SafePath.
package tools

import (
	"sync"

	"github.com/crossoverJie/botler-agent/internal/push"
)

// TaskContext is the per-task context injected by the runner before an agent run.
// The dispatcher's sequential queue guarantees one task at a time, so a single slot is race-free;
// the mutex only protects against a stray concurrent read.
type TaskContext struct {
	Recipient *push.Recipient
	// ConversationSessionKey is set only for IM execute tasks; it decides whether
	// clear_conversation_context is available at all (part of the security boundary).
	ConversationSessionKey string
	// Projects is the task-local subset enforced on top of the global allowlist (nil = no subset).
	Projects []string
}

var (
	ctxMu   sync.RWMutex
	current *TaskContext
)

// SetTaskContext installs (or clears, with nil) the current task context.
func SetTaskContext(c *TaskContext) {
	ctxMu.Lock()
	current = c
	ctxMu.Unlock()
}

// GetTaskContext returns the current task context.
func GetTaskContext() *TaskContext {
	ctxMu.RLock()
	defer ctxMu.RUnlock()
	return current
}

// TaskProjects returns the task-local selected projects (nil when no subset is active).
func TaskProjects() []string {
	ctxMu.RLock()
	defer ctxMu.RUnlock()
	if current == nil {
		return nil
	}
	return current.Projects
}
