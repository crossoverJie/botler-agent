package runner

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/greeting"
	"github.com/crossoverJie/botler-agent/internal/images"
	"github.com/crossoverJie/botler-agent/internal/logs"
	"github.com/crossoverJie/botler-agent/internal/monitor"
	"github.com/crossoverJie/botler-agent/internal/paths"
	"github.com/crossoverJie/botler-agent/internal/prompt"
	"github.com/crossoverJie/botler-agent/internal/push"
	"github.com/crossoverJie/botler-agent/internal/tools"
	"github.com/crossoverJie/botler-agent/internal/version"
)

// LogContext describes where a run comes from; the dispatcher finalizes the task log status.
type LogContext struct {
	TaskID        string
	Source        string
	Phase         string
	ProjectHint   string
	ProjectsHint  []string
	Recipient     *push.Recipient
	InboundImages []agentrt.InboundImage
	SessionKey    string
}

// Result is the outcome of one task run.
type Result struct {
	Text                   string
	Images                 []string
	Mutated                bool
	Log                    *logs.TaskLog
	RecordConversationTurn bool
}

var (
	imageRefRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)]*)\)`)
	imageExtRe = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp|bmp)$`)
)

// extractImages splits the reply into text plus image references (markdown ![alt](path)).
// Local paths go through paths.SafePath (allowlist + symlink check + task subset) and must be an
// existing image file; https URLs are passed through untouched. Image markdown is always stripped
// from the text so channels that cannot render it do not show raw markdown.
func extractImages(text string, projects []string) (string, []string) {
	var out []string
	for _, m := range imageRefRe.FindAllStringSubmatch(text, -1) {
		ref := strings.TrimSpace(m[1])
		if ref == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(ref), "https://") {
			if !contains(out, ref) {
				out = append(out, ref)
			}
			continue
		}
		if !imageExtRe.MatchString(ref) {
			continue
		}
		abs, err := paths.SafePath(ref, paths.Options{Projects: projects})
		if err != nil {
			logWarn("[runner] ignoring unusable image reference: " + ref)
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			logWarn("[runner] ignoring unusable image reference: " + ref)
			continue
		}
		if !contains(out, abs) {
			out = append(out, abs)
		}
	}
	cleaned := strings.TrimSpace(imageRefRe.ReplaceAllString(text, ""))
	return cleaned, out
}

// lastAssistantText takes the body of the last assistant message (empty on failure).
func lastAssistantText(messages []agentrt.Message) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Role != agentrt.RoleAssistant {
		return ""
	}
	if last.StopReason == agentrt.StopError || last.StopReason == agentrt.StopAborted {
		return ""
	}
	return strings.TrimSpace(last.Text())
}

type routeOutcome struct {
	projects          []string
	attachmentProject string
	resetContext      bool
	usage             *agentrt.Usage
	prompt            string
	candidates        []string
}

// routeProject decides which data subproject the message targets. A single project short-circuits;
// otherwise one lightweight LLM call without tools decides.
func routeProject(ctx context.Context, rt agentrt.Runtime, userMessage string, projects []string, includeScheduler bool, imagesContent []agentrt.InboundImage, recentTurns []conversation.Turn, allowReset bool) (routeOutcome, error) {
	if len(projects) == 1 {
		return routeOutcome{projects: []string{projects[0]}, candidates: []string{projects[0]}}, nil
	}
	candidates := projects
	if includeScheduler {
		candidates = append(append([]string{}, projects...), prompt.SchedulerVirtualProject)
	}
	historyBlock := ""
	if len(recentTurns) > 0 {
		historyBlock = conversation.FormatRecentTurns(recentTurns, conversation.FormatOptions{IncludeProject: true, MaxChars: config.C().ConversationContextMax})
	}
	routePrompt := prompt.BuildRoutePrompt(userMessage, prompt.RouteOptions{
		IncludeScheduler:  includeScheduler,
		HasImages:         len(imagesContent) > 0,
		RecentTurns:       historyBlock,
		AllowResetContext: allowReset,
	})
	reply, err := rt.Complete(ctx, agentrt.Request{
		System: routePrompt,
		Messages: []agentrt.Message{{
			Role:      agentrt.RoleUser,
			Timestamp: time.Now().UnixMilli(),
			Parts:     userParts(userMessage, imagesContent),
		}},
	})
	if err != nil {
		return routeOutcome{}, err
	}
	decision := ParseRoute(strings.TrimSpace(reply.Text()), candidates)
	return routeOutcome{
		projects:          decision.Projects,
		attachmentProject: decision.AttachmentProject,
		resetContext:      decision.ResetContext,
		usage:             reply.Usage,
		prompt:            routePrompt,
		candidates:        candidates,
	}, nil
}

func userParts(text string, images []agentrt.InboundImage) []agentrt.Part {
	parts := []agentrt.Part{{Type: agentrt.PartText, Text: text}}
	for _, img := range images {
		parts = append(parts, agentrt.Part{Type: agentrt.PartImage, Data: img.Data, MimeType: img.MimeType})
	}
	return parts
}

func modelCacheSnapshot() logs.ModelCacheStats {
	queries, hits := monitor.ModelCacheSnapshot()
	rate := 0.0
	if queries > 0 {
		rate = float64(hits) / float64(queries)
	}
	return logs.ModelCacheStats{Queries: int(queries), Hits: int(hits), HitRate: rate}
}

type minimalLogOpts struct {
	ctx          LogContext
	startedAt    int64
	endedAt      int64
	userMessage  string
	replyText    string
	projects     []string
	routingUsage *agentrt.Usage
	routing      bool
	candidates   []string
	decision     *string
	routePrompt  string
}

func buildMinimalLog(o minimalLogOpts) *logs.TaskLog {
	count := len(o.ctx.InboundImages)
	in := logs.CollectInput{
		ID:                version.NewID(),
		TaskID:            o.ctx.TaskID,
		Phase:             o.ctx.Phase,
		Source:            o.ctx.Source,
		Provider:          config.C().Provider,
		Model:             config.C().Model,
		Projects:          o.projects,
		StartedAt:         o.startedAt,
		EndedAt:           o.endedAt,
		UserMessage:       o.userMessage,
		ReplyText:         o.replyText,
		Images:            []string{},
		InboundImageCount: &count,
		Messages:          []agentrt.Message{},
		RoutingUsage:      o.routingUsage,
		ModelCache:        modelCacheSnapshot(),
	}
	if o.routing {
		in.RoutingCandidates = o.candidates
		in.RoutingDecision = o.decision
		in.RoutingPrompt = o.routePrompt
	}
	log := logs.CollectTaskLog(in)
	return &log
}

// RunTask runs one task through the two phases: route, then execute with a fresh agent.
func RunTask(ctx context.Context, userMessage string, lc *LogContext) (*Result, error) {
	lctx := LogContext{TaskID: version.NewID(), Source: "cli", Phase: logs.PhaseExecute}
	if lc != nil {
		lctx = *lc
	}
	if lctx.Phase == "" {
		lctx.Phase = logs.PhaseExecute
	}
	startedAt := time.Now().UnixMilli()
	inbound := lctx.InboundImages
	isIMSession := lctx.Phase == logs.PhaseExecute && lctx.Source != "scheduler" && lctx.Source != "cli" && lctx.SessionKey != ""

	var recentTurns []conversation.Turn
	if conversation.ShouldLoadRecentTurns(conversation.RecentTurnLoadContext{
		Enabled:    config.C().ConversationContextOn,
		Phase:      lctx.Phase,
		Source:     lctx.Source,
		SessionKey: lctx.SessionKey,
	}) {
		recentTurns = conversation.LoadRecentTurns(lctx.SessionKey, config.C().ConversationContextTurns)
	}

	rt, err := resolveRuntime()
	if err != nil {
		return nil, err
	}

	projects := prompt.ListProjectDirs()
	if len(projects) == 0 {
		replyText := "⚠️ No data subprojects under DATA_ROOT; please check your config."
		return &Result{
			Text: replyText,
			Log: buildMinimalLog(minimalLogOpts{
				ctx: lctx, startedAt: startedAt, endedAt: time.Now().UnixMilli(),
				userMessage: userMessage, replyText: replyText, projects: []string{},
			}),
			RecordConversationTurn: false,
		}, nil
	}

	// Greeting short-circuit: no projectHint, not a scheduler run, no inbound image.
	if lctx.ProjectHint == "" && lctx.Source != "scheduler" && len(inbound) == 0 && greeting.IsGreeting(userMessage) {
		replyText := greeting.Reply()
		return &Result{
			Text: replyText,
			Log: buildMinimalLog(minimalLogOpts{
				ctx: lctx, startedAt: startedAt, endedAt: time.Now().UnixMilli(),
				userMessage: userMessage, replyText: replyText, projects: []string{},
			}),
			RecordConversationTurn: false,
		}, nil
	}

	// Phase 1: routing. A valid projectHint / projectsHint skips the routing LLM call.
	var selected []string
	var attachment string
	var routingUsage *agentrt.Usage
	var routingPrompt string
	var candidates []string
	resetContext := false
	switch {
	case lctx.ProjectHint != "" && contains(projects, lctx.ProjectHint):
		selected = []string{lctx.ProjectHint}
		candidates = append(append([]string{}, projects...), prompt.SchedulerVirtualProject)
	case len(lctx.ProjectsHint) > 0:
		for _, n := range lctx.ProjectsHint {
			if contains(projects, n) {
				selected = append(selected, n)
			}
		}
		candidates = append(append([]string{}, projects...), prompt.SchedulerVirtualProject)
	default:
		routed, err := routeProject(ctx, rt, userMessage, projects, lctx.Source != "scheduler", inbound, recentTurns, isIMSession)
		if err != nil {
			return nil, err
		}
		resetContext = routed.resetContext
		if !resetContext {
			selected = routed.projects
			attachment = routed.attachmentProject
		}
		routingUsage = routed.usage
		routingPrompt = routed.prompt
		candidates = routed.candidates
	}

	if ShouldClearConversationForRoute(resetContext, isIMSession, lctx.SessionKey) {
		conversation.ClearSession(lctx.SessionKey)
		replyText := greeting.ContextResetReply()
		return &Result{
			Text: replyText,
			Log: buildMinimalLog(minimalLogOpts{
				ctx: lctx, startedAt: startedAt, endedAt: time.Now().UnixMilli(),
				userMessage: userMessage, replyText: replyText, projects: []string{},
				routingUsage: routingUsage, routing: true, candidates: candidates,
				decision: strPtr(prompt.ResetContextDecision), routePrompt: routingPrompt,
			}),
			RecordConversationTurn: false,
		}, nil
	}
	if len(selected) == 0 {
		replyText := greeting.FallbackUnknownReply(len(inbound) > 0)
		return &Result{
			Text: replyText,
			Log: buildMinimalLog(minimalLogOpts{
				ctx: lctx, startedAt: startedAt, endedAt: time.Now().UnixMilli(),
				userMessage: userMessage, replyText: replyText, projects: []string{},
				routingUsage: routingUsage, routing: true, candidates: candidates,
				decision: nil, routePrompt: routingPrompt,
			}),
			// v1 records unrouted replies: the unknown-project status is in the recordable set.
			RecordConversationTurn: true,
		}, nil
	}

	// Inbound images must persist to a single data subproject.
	// A non-nil empty slice matters: it disables the task-local subset entirely (a schedule-only
	// route must not gain access to every project), while nil would mean "no subset".
	realSelected := make([]string, 0, len(selected))
	for _, n := range selected {
		if n != prompt.SchedulerVirtualProject {
			realSelected = append(realSelected, n)
		}
	}
	if len(inbound) > 0 && attachment == "" {
		if len(realSelected) == 1 {
			attachment = realSelected[0]
		} else if len(realSelected) > 1 {
			replyText := greeting.FallbackUnknownReply(true)
			return &Result{
				Text: replyText,
				Log: buildMinimalLog(minimalLogOpts{
					ctx: lctx, startedAt: startedAt, endedAt: time.Now().UnixMilli(),
					userMessage: userMessage, replyText: replyText, projects: selected,
					routingUsage: routingUsage, routing: true, candidates: candidates,
					decision: nil, routePrompt: routingPrompt,
				}),
				RecordConversationTurn: true,
			}, nil
		}
	}

	var savedPaths []string
	if len(inbound) > 0 && attachment != "" {
		for _, img := range inbound {
			rel, err := images.PersistInboundImage(img, attachment)
			if err != nil {
				logWarn("[runner] persist image failed: " + err.Error())
				continue
			}
			savedPaths = append(savedPaths, rel)
		}
	}
	attachmentNote := ""
	if len(savedPaths) > 0 {
		attachmentNote = "\n[图片已保存到] " + strings.Join(savedPaths, ", ") + "（可在记录中引用这些相对路径）"
	}
	execText := strings.TrimSpace(userMessage + attachmentNote)

	// Phase 2: execution with the selected project's conventions.
	historyBlock := conversation.FormatRecentTurns(recentTurns, conversation.FormatOptions{MaxChars: config.C().ConversationContextMax})
	baseSystemPrompt := prompt.LoadSystemPrompt(selected)
	if historyBlock != "" {
		baseSystemPrompt = baseSystemPrompt + "\n\n# 最近对话\n" + historyBlock +
			"\n\n以上是最近若干轮用户可见对话。当前消息如果明显是新的独立任务，请忽略旧历史并按新任务处理；如果它是上一轮任务的确认、补充或纠正，请结合历史继续处理。无法确定时向用户确认。"
	}
	canControlConversation := isIMSession && !contains(selected, prompt.SchedulerVirtualProject)
	clearInstruction := ""
	if canControlConversation {
		clearInstruction = "\n\n# 会话上下文控制\n你可以使用 clear_conversation_context 工具。仅当用户明确要求清空 / 忽略 / 重置之前的上下文（例如「新任务」「忽略上文」「重置上下文」，或等价表达）时，在处理当前请求前调用该工具一次。调用后，如果用户还带有具体任务，继续完成该任务；如果没有具体任务，只需简短确认已清空。"
	}
	systemPrompt := baseSystemPrompt + clearInstruction

	execTools := tools.DataTools()
	if canControlConversation {
		execTools = tools.FileTools()
	}

	tools.SetTaskContext(&tools.TaskContext{
		Recipient:              lctx.Recipient,
		ConversationSessionKey: sessionKeyFor(canControlConversation, lctx.SessionKey),
		Projects:               realSelected,
	})
	var run *agentrt.RunResult
	monitor.MarkAgentStart()
	func() {
		defer monitor.MarkAgentEnd()
		defer tools.SetTaskContext(nil)
		loop := &agentrt.Loop{Runtime: rt, MaxToolTurns: config.C().MaxToolTurns}
		run, err = loop.Run(ctx, agentrt.RunInput{SystemPrompt: systemPrompt, UserText: execText, Images: inbound, Tools: execTools})
	}()
	if err != nil {
		return nil, err
	}

	// Reply extraction: three branches, exactly like v1.
	text := "⚠️ No reply generated"
	var outImages []string
	messages := run.Messages
	var last *agentrt.Message
	if len(messages) > 0 {
		last = &messages[len(messages)-1]
	}
	switch {
	case last != nil && last.Role == agentrt.RoleAssistant:
		if last.StopReason == agentrt.StopError || last.StopReason == agentrt.StopAborted {
			detail := last.ErrorMessage
			if detail == "" {
				detail = last.StopReason
			}
			text = "⚠️ Task did not complete successfully: " + detail
		} else {
			text, outImages = extractImages(last.Text(), realSelected)
		}
	case last != nil && last.Role == agentrt.RoleTool && run.ToolTurnCapHit:
		toolName := last.ToolName
		if toolName == "" {
			toolName = "unknown tool"
		}
		errSuffix := ""
		if last.IsError {
			errSuffix = " (errored)"
		}
		resultText := strings.TrimSpace(last.Text())
		if resultText == "" {
			resultText = "(no text result)"
		}
		text = "⚠️ Reached the tool-call limit (" + itoa(config.C().MaxToolTurns) + " turns); the task was aborted with no final reply.\n\n" +
			"Last step: " + toolName + errSuffix + "\n" +
			"Last result: " + resultText + "\n\n" +
			"To raise the limit, set MAX_TOOL_TURNS in .env (default 20)."
	}

	mutated := len(savedPaths) > 0
	for _, name := range run.ToolNames {
		if name == "write" || name == "edit" {
			mutated = true
		}
	}

	endedAt := time.Now().UnixMilli()
	inboundCount := len(inbound)
	in := logs.CollectInput{
		ID:                version.NewID(),
		TaskID:            lctx.TaskID,
		Phase:             lctx.Phase,
		Source:            lctx.Source,
		Provider:          config.C().Provider,
		Model:             config.C().Model,
		Projects:          selected,
		StartedAt:         startedAt,
		EndedAt:           endedAt,
		UserMessage:       userMessage,
		ReplyText:         text,
		Images:            outImages,
		InboundImageCount: &inboundCount,
		Mutated:           mutated,
		Messages:          messages,
		RoutingUsage:      routingUsage,
		SystemPrompt:      systemPrompt,
		ModelCache:        modelCacheSnapshot(),
		RoutingCandidates: candidates,
		RoutingDecision:   logs.LegacyProject(selected),
		RoutingPrompt:     routingPrompt,
	}
	log := logs.CollectTaskLog(in)
	if outImages == nil {
		log.Images = []string{}
	}
	return &Result{
		Text:                   text,
		Images:                 outImages,
		Mutated:                mutated,
		Log:                    &log,
		RecordConversationTurn: ShouldRecordConversationTurn(run.ToolNames, len(savedPaths)),
	}, nil
}

func sessionKeyFor(enabled bool, key string) string {
	if enabled {
		return key
	}
	return ""
}

func strPtr(s string) *string { return &s }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var logWarn = func(s string) { os.Stderr.WriteString(s + "\n") }

// ErrNoProjects is returned when DATA_ROOT has no subprojects (kept for callers that branch).
var ErrNoProjects = errors.New("no data subprojects under DATA_ROOT")
