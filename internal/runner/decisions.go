// Package runner executes one task in two phases: route the message to a data subproject, then
// run a fresh agent with only that project's conventions.
package runner

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Route is the parsed routing decision.
type Route struct {
	// Projects is the ordered, deduplicated selection; empty = unknown (no project).
	Projects []string
	// AttachmentProject is the single project where an inbound image is saved (image messages only).
	AttachmentProject string
	ResetContext      bool
}

// EmptyRoute returns an undecided route.
func EmptyRoute() Route { return Route{} }

var (
	resetContextRe = regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(resetContextDecision) + `\b`)
	unknownRe      = regexp.MustCompile(`(?i)unknown|无法确定|不确定|无法判断|不明确`)
	schedulerAlias = regexp.MustCompile(`定时|提醒|日程`)
	codeFenceHead  = regexp.MustCompile("^```[a-zA-Z]*\\s*\\n?")
	codeFenceTail  = regexp.MustCompile("\\n?```\\s*$")
	leadingBullet  = regexp.MustCompile(`^[-*\s]+`)
)

// resetContextDecision mirrors prompt.ResetContextDecision (kept local to avoid an import cycle
// with the prompt package's heavier dependencies).
const resetContextDecision = "RESET_CONTEXT"

// schedulerVirtualProject mirrors prompt.SchedulerVirtualProject.
const schedulerVirtualProject = "__scheduler__"

func stripCodeFences(s string) string {
	s = codeFenceHead.ReplaceAllString(s, "")
	s = codeFenceTail.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func sliceFromFirstBrace(s string) string {
	if i := strings.Index(s, "{"); i >= 0 {
		return s[i:]
	}
	return s
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func validateRoute(raw any, candidates []string) Route {
	obj, ok := raw.(map[string]any)
	if !ok {
		return EmptyRoute()
	}
	projectsRaw, ok := obj["projects"].([]any)
	if !ok {
		return EmptyRoute()
	}
	var projects []string
	for _, p := range projectsRaw {
		s, ok := p.(string)
		if !ok || !contains(candidates, s) {
			return EmptyRoute()
		}
		if !contains(projects, s) {
			projects = append(projects, s)
		}
	}
	if len(projects) == 0 {
		return EmptyRoute()
	}
	// The scheduler virtual project is exclusive: it must be the sole entry.
	if contains(projects, schedulerVirtualProject) && len(projects) > 1 {
		return EmptyRoute()
	}
	attachment := ""
	if rawAtt, ok := obj["attachmentProject"]; ok && rawAtt != nil {
		if s, ok := rawAtt.(string); ok && s != "null" && s != "" {
			if s == schedulerVirtualProject || !contains(projects, s) {
				return EmptyRoute()
			}
			attachment = s
		} else if !ok {
			if _, isNil := rawAtt.(string); !isNil {
				return EmptyRoute()
			}
		}
	}
	return Route{Projects: projects, AttachmentProject: attachment}
}

var tokenSplitRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func exactNameMatches(text string, candidates []string) []string {
	tokens := tokenSplitRe.Split(text, -1)
	var out []string
	for _, c := range candidates {
		if contains(tokens, c) {
			out = append(out, c)
		}
	}
	return out
}

// ParseRoute parses the routing model output (structured JSON, with tolerant fallbacks).
func ParseRoute(output string, candidates []string) Route {
	t := stripCodeFences(strings.TrimSpace(leadingBullet.ReplaceAllString(output, "")))
	if t == "" {
		return EmptyRoute()
	}
	if resetContextRe.MatchString(t) {
		return Route{ResetContext: true}
	}
	if unknownRe.MatchString(t) {
		return EmptyRoute()
	}
	var parsed any
	if err := json.Unmarshal([]byte(sliceFromFirstBrace(t)), &parsed); err == nil && parsed != nil {
		return validateRoute(parsed, candidates)
	}
	// Tolerant fallback for a model that replies with a bare project name.
	matches := exactNameMatches(t, candidates)
	if len(matches) == 1 {
		return Route{Projects: matches}
	}
	if len(matches) == 0 && contains(candidates, schedulerVirtualProject) &&
		(t == "scheduler" || schedulerAlias.MatchString(t)) {
		return Route{Projects: []string{schedulerVirtualProject}}
	}
	return EmptyRoute()
}

// ShouldRecordConversationTurn reports whether an execution should append a visible turn.
// A request that only called clear_conversation_context (no data tool, no saved image) is not
// written back, otherwise it would immediately repopulate the session it just cleared.
func ShouldRecordConversationTurn(toolNames []string, savedImageCount int) bool {
	clearCalled := contains(toolNames, "clear_conversation_context")
	nonClearCalled := false
	for _, n := range toolNames {
		if n != "clear_conversation_context" {
			nonClearCalled = true
			break
		}
	}
	return !(clearCalled && !nonClearCalled && savedImageCount == 0)
}

// ShouldClearConversationForRoute reports whether a route decision clears the IM session.
func ShouldClearConversationForRoute(resetContext, isIMSession bool, sessionKey string) bool {
	return resetContext && isIMSession && sessionKey != ""
}
