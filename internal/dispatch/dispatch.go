package dispatch

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/logs"
	"github.com/crossoverJie/botler-agent/internal/monitor"
	"github.com/crossoverJie/botler-agent/internal/paths"
	"github.com/crossoverJie/botler-agent/internal/runner"
	"github.com/crossoverJie/botler-agent/internal/safety"
	"github.com/crossoverJie/botler-agent/internal/tools"
	"github.com/crossoverJie/botler-agent/internal/version"
)

// dedupWindow is v1's 5-minute duplicate window.
const dedupWindow = 5 * time.Minute

var (
	mu   sync.Mutex
	seen = map[string]time.Time{}
	// chain is the sequential queue: only one task writes to the data directory at a time.
	chain = make(chan struct{}, 1)
)

func init() { chain <- struct{}{} }

func acquire() { <-chain }
func release() { chain <- struct{}{} }

func isDuplicate(id string) bool {
	if id == "" {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	// The map is never pruned (v1 behaviour): "5 minutes" only applies at decision time.
	if prev, ok := seen[id]; ok && now.Sub(prev) < dedupWindow {
		return true
	}
	seen[id] = now
	return false
}

var zeroUsage = logs.TokenUsageLog{}

func minimalLog(taskID, source string, startedAt int64, status logs.TaskStatus, userMessage, replyText string) logs.TaskLog {
	c := config.C()
	return logs.TaskLog{
		ID:           version.NewID(),
		TaskID:       taskID,
		Phase:        logs.PhaseExecute,
		Source:       source,
		Provider:     c.Provider,
		Model:        c.Model,
		Project:      nil,
		Projects:     []string{},
		Status:       status,
		StartedAt:    startedAt,
		EndedAt:      time.Now().UnixMilli(),
		DurationMs:   time.Now().UnixMilli() - startedAt,
		UserMessage:  userMessage,
		ReplyText:    replyText,
		Images:       []string{},
		Tools:        []logs.ToolCallLog{},
		Conversation: []logs.ConversationEntry{},
		Usage:        zeroUsage,
		Calls:        []logs.CallUsage{},
		ModelCache:   logs.ModelCacheStats{},
	}
}

// appendVisibleTurn appends the user-visible turn when the execute phase completed with a
// recordable status.
func appendVisibleTurn(sessionKey, message string, result *runner.Result, status logs.TaskStatus) {
	if !config.C().ConversationContextOn {
		return
	}
	recordable := true
	if result != nil {
		recordable = result.RecordConversationTurn
	}
	if !conversation.ShouldRecordTurn(logs.PhaseExecute, string(status), sessionKey, recordable) {
		return
	}
	if result == nil || result.Log == nil {
		return
	}
	assistant := result.Text
	if assistant == "" && result.Log != nil {
		assistant = result.Log.ReplyText
	}
	assistant = trimSpace(assistant)
	if assistant == "" {
		return
	}
	_ = conversation.AppendTurn(sessionKey, conversation.Turn{
		TS:        result.Log.StartedAt,
		Project:   result.Log.Project,
		Projects:  result.Log.Projects,
		User:      message,
		Assistant: assistant,
	}, config.C().ConversationContextTurns)
}

// Dispatch processes a single user message: dedup, serial execution, validation with one
// self-heal retry, then commit. It never rejects.
func Dispatch(ctx context.Context, message string, opts Options) Result {
	dispatchStartedAt := time.Now().UnixMilli()
	taskID := opts.ID
	if taskID == "" {
		taskID = version.NewID()
	}
	source := opts.Source
	if source == "" {
		source = "cli"
	}

	if isDuplicate(opts.ID) {
		fmt.Fprintf(os.Stderr, "[dispatch] Ignoring duplicate message %s\n", opts.ID)
		monitor.MarkDuplicate()
		logs.AppendTaskLog(minimalLog(taskID, source, dispatchStartedAt, logs.StatusDuplicate, message, DuplicateSentinel))
		return Result{Text: DuplicateSentinel, Images: []string{}, Status: logs.StatusDuplicate}
	}

	monitor.MarkEnqueued()
	monitor.MarkDispatched()
	acquire()

	monitor.MarkStarted()
	var result *runner.Result
	finalStatus := logs.StatusSuccess
	out := func(text string, images []string, status logs.TaskStatus) Result {
		if images == nil {
			images = []string{}
		}
		return Result{Text: text, Images: images, Status: status}
	}
	defer func() {
		appendVisibleTurn(opts.SessionKey, message, result, finalStatus)
		monitor.MarkFinished()
		monitor.RecordStatus(string(finalStatus))
		monitor.SetLastDispatchDuration(time.Now().UnixMilli() - dispatchStartedAt)
		if finalStatus == logs.StatusError || finalStatus == logs.StatusValidationFailed {
			monitor.MarkFailed()
		}
		release()
	}()

	res, err := runner.RunTask(ctx, message, &runner.LogContext{
		TaskID:        taskID,
		Source:        source,
		Phase:         logs.PhaseExecute,
		ProjectHint:   opts.ProjectHint,
		ProjectsHint:  opts.ProjectsHint,
		Recipient:     opts.Recipient,
		InboundImages: opts.InboundImages,
		SessionKey:    opts.SessionKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[dispatch] Task error: %v\n", err)
		errText := "⚠️ Task execution error: " + err.Error()
		log := minimalLog(taskID, source, dispatchStartedAt, logs.StatusError, message, errText)
		logs.AppendTaskLog(log)
		finalStatus = logs.StatusError
		return out(errText, []string{}, logs.StatusError)
	}
	result = res
	log := res.Log
	if log == nil {
		finalStatus = logs.StatusSuccess
		return out(res.Text, res.Images, logs.StatusSuccess)
	}

	if !res.Mutated {
		// Read-only task, or unrouted / no-project (empty selected set).
		if len(log.Projects) == 0 {
			log.Status = logs.StatusUnknownProject
		} else {
			log.Status = logs.StatusSuccess
		}
		log.ReplyText = res.Text
		logs.AppendTaskLog(*log)
		finalStatus = log.Status
		return out(res.Text, res.Images, log.Status)
	}

	validation := safety.ValidateState(log.Projects)
	if !validation.OK {
		fmt.Fprintf(os.Stderr, "[dispatch] Validation failed; self-heal retry: %s\n", validation.Fix)
		heal, healErr := runner.RunTask(ctx, validation.Fix, &runner.LogContext{
			TaskID:       taskID,
			Source:       source,
			Phase:        logs.PhaseSelfHeal,
			ProjectsHint: log.Projects,
		})
		if healErr != nil {
			fmt.Fprintf(os.Stderr, "[dispatch] self-heal error: %v\n", healErr)
		}
		validation = safety.ValidateState(log.Projects)
		if !validation.OK {
			errText := "⚠️ Could not save safely; please check the data: " + validation.Fix
			log.Status = logs.StatusValidationFailed
			log.ReplyText = errText
			logs.AppendTaskLog(*log)
			if heal != nil && heal.Log != nil {
				heal.Log.Status = logs.StatusValidationFailed
				heal.Log.ReplyText = errText
				logs.AppendTaskLog(*heal.Log)
			}
			finalStatus = logs.StatusValidationFailed
			return out(errText, []string{}, logs.StatusValidationFailed)
		}
		safety.CommitIfChanged("agent: "+truncateCommit(message), log.Projects)
		finalText := res.Text + "\n\n(auto-fixed and saved)"
		log.Status = logs.StatusAutoFixed
		log.ReplyText = finalText
		logs.AppendTaskLog(*log)
		if heal != nil && heal.Log != nil {
			heal.Log.Status = logs.StatusSuccess
			logs.AppendTaskLog(*heal.Log)
		}
		finalStatus = logs.StatusAutoFixed
		return out(finalText, res.Images, logs.StatusAutoFixed)
	}

	safety.CommitIfChanged("agent: "+truncateCommit(message), log.Projects)
	log.Status = logs.StatusSuccess
	log.ReplyText = res.Text
	logs.AppendTaskLog(*log)
	finalStatus = logs.StatusSuccess
	return out(res.Text, res.Images, logs.StatusSuccess)
}

// scriptLogInput describes one direct script run for its history log.
type scriptLogInput struct {
	taskID    string
	source    string
	startedAt int64
	status    logs.TaskStatus
	script    string
	replyText string
	project   *string
	ran       bool
}

// scriptTaskLog builds a history-compatible TaskLog for a direct script run.
func scriptTaskLog(o scriptLogInput) logs.TaskLog {
	c := config.C()
	projects := []string{}
	if o.project != nil {
		projects = []string{*o.project}
	}
	now := time.Now().UnixMilli()
	return logs.TaskLog{
		ID:           version.NewID(),
		TaskID:       o.taskID,
		Phase:        logs.PhaseExecute,
		Source:       o.source,
		Provider:     c.Provider,
		Model:        c.Model,
		Project:      o.project,
		Projects:     projects,
		Status:       o.status,
		StartedAt:    o.startedAt,
		EndedAt:      now,
		DurationMs:   now - o.startedAt,
		UserMessage:  "run " + o.script,
		ReplyText:    o.replyText,
		Images:       []string{},
		Mutated:      o.ran,
		Tools:        []logs.ToolCallLog{},
		Conversation: []logs.ConversationEntry{},
		Usage:        zeroUsage,
		Calls:        []logs.CallUsage{},
		ModelCache:   logs.ModelCacheStats{},
	}
}

// DispatchScriptedTask runs a scheduled script directly (bypassing the LLM) on the same
// sequential chain as Dispatch, so it never races a chat task's writes.
func DispatchScriptedTask(ctx context.Context, opts ScriptTaskOptions) Result {
	startedAt := time.Now().UnixMilli()
	source := opts.Source
	if source == "" {
		source = "scheduler"
	}
	monitor.MarkEnqueued()
	monitor.MarkDispatched()
	acquire()
	monitor.MarkStarted()

	finalStatus := logs.StatusSuccess
	text := ""
	var project *string
	ran := false
	out := func(t string, status logs.TaskStatus) Result {
		return Result{Text: t, Images: []string{}, Status: status}
	}
	defer func() {
		logs.AppendTaskLog(scriptTaskLog(scriptLogInput{
			taskID:    opts.ID,
			source:    source,
			startedAt: startedAt,
			status:    finalStatus,
			script:    opts.Script,
			replyText: text,
			project:   project,
			ran:       ran,
		}))
		monitor.MarkFinished()
		monitor.RecordStatus(string(finalStatus))
		monitor.SetLastDispatchDuration(time.Now().UnixMilli() - startedAt)
		if finalStatus == logs.StatusError || finalStatus == logs.StatusValidationFailed {
			monitor.MarkFailed()
		}
		release()
	}()

	res := tools.RunScript(opts.Script, opts.ScriptArgs)
	text = res.Text
	if res.Cwd != "" {
		if name, ok := paths.ProjectOf(res.Cwd); ok {
			project = &name
		}
	}
	ran = res.OK || res.ErrorKind == tools.ErrRuntime
	if !res.OK && res.ErrorKind == tools.ErrPrecondition {
		// The script never ran, so no data was touched: no validation needed.
		finalStatus = logs.StatusError
		return out(text, logs.StatusError)
	}
	// Any script that actually started may have written data, so always validate its project.
	var scope []string
	if project != nil {
		scope = []string{*project}
	}
	v := safety.ValidateState(scope)
	if !v.OK {
		finalStatus = logs.StatusValidationFailed
		scriptErr := ""
		if !res.OK {
			scriptErr = "Script failed: " + text + "\n"
		}
		text = "⚠️ " + scriptErr + "Script produced invalid data (left as-is, not committed): " + v.Fix
		return out(text, logs.StatusValidationFailed)
	}
	if !res.OK {
		finalStatus = logs.StatusError
		return out(text, logs.StatusError)
	}
	commit := true
	if opts.Commit != nil {
		commit = *opts.Commit
	}
	if commit {
		safety.CommitIfChanged("agent: scheduled script "+opts.ID, scope)
	}
	return out(text, logs.StatusSuccess)
}

// InboundImages is re-exported so channels can build dispatch options without importing agentrt.
type InboundImages = []agentrt.InboundImage

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
