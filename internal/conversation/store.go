// Package conversation stores the shared recent-visible-turn window for the IM session
// (~/.botler-agent/conversations/im.json). Only user-visible text is kept: no thinking, no tool
// calls, no tool results.
package conversation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// IMSessionKey is the fixed conversation key shared by Telegram / Feishu / WeChat.
const IMSessionKey = "im"

// Turn is one user-visible exchange.
type Turn struct {
	TS        int64    `json:"ts"`
	Project   *string  `json:"project"`
	Projects  []string `json:"projects"`
	User      string   `json:"user"`
	Assistant string   `json:"assistant"`
}

var sessionKeyRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var mu sync.Mutex

func sessionFile(sessionKey string) (string, error) {
	if !sessionKeyRe.MatchString(sessionKey) {
		return "", errors.New("invalid conversation session key: " + sessionKey)
	}
	return filepath.Join(config.C().ConversationDir, sessionKey+".json"), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func normalizeTurn(t Turn) (Turn, bool) {
	user := truncate(t.User, config.C().ConversationTurnMaxChars)
	assistant := truncate(t.Assistant, config.C().ConversationTurnMaxChars)
	if user == "" && assistant == "" {
		return Turn{}, false
	}
	projects := t.Projects
	if len(projects) == 0 && t.Project != nil && *t.Project != "" {
		projects = []string{*t.Project}
	}
	if projects == nil {
		projects = []string{}
	}
	return Turn{TS: t.TS, Project: t.Project, Projects: projects, User: user, Assistant: assistant}, true
}

type fileShape struct {
	Turns []json.RawMessage `json:"turns"`
}

func readTurns(sessionKey string) []Turn {
	file, err := sessionFile(sessionKey)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var doc fileShape
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make([]Turn, 0, len(doc.Turns))
	for _, r := range doc.Turns {
		var t Turn
		if err := json.Unmarshal(r, &t); err != nil {
			continue
		}
		if t.TS == 0 {
			continue
		}
		if n, ok := normalizeTurn(t); ok {
			out = append(out, n)
		}
	}
	return out
}

// LoadRecentTurns returns the newest maxTurns turns, oldest first.
func LoadRecentTurns(sessionKey string, maxTurns int) []Turn {
	if maxTurns <= 0 {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	turns := readTurns(sessionKey)
	if len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	return turns
}

// AppendTurn appends a normalized turn and keeps only the newest maxTurns entries.
// Failures never break the dispatch flow (they are logged only).
func AppendTurn(sessionKey string, turn Turn, maxTurns int) error {
	if maxTurns <= 0 {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	normalized, ok := normalizeTurn(turn)
	if !ok {
		return nil
	}
	turns := append(readTurns(sessionKey), normalized)
	if len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	file, err := sessionFile(sessionKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.C().ConversationDir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(map[string]any{"turns": turns}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// v1 does not fsync the directory; we do (D-8 in the design doc), which is strictly safer.
	if dir, err := os.Open(config.C().ConversationDir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ClearSession removes a conversation session file (best effort).
func ClearSession(sessionKey string) {
	mu.Lock()
	defer mu.Unlock()
	file, err := sessionFile(sessionKey)
	if err != nil {
		return
	}
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		warnf("[conversation] clearSession failed: %v", err)
	}
}

// FormatOptions tunes FormatRecentTurns.
type FormatOptions struct {
	// IncludeProject renders the routed project(s) before each turn (routing phase only).
	IncludeProject bool
	// MaxChars overrides the whole-window cap (0 = config value).
	MaxChars int
}

// FormatRecentTurns renders recent turns for prompt injection. The newest turns are kept within
// the whole-window cap; when the newest turn alone exceeds it, that turn's head is kept instead
// of dropping everything.
func FormatRecentTurns(turns []Turn, opts FormatOptions) string {
	if len(turns) == 0 {
		return ""
	}
	maxChars := opts.MaxChars
	if maxChars <= 0 {
		maxChars = config.C().ConversationContextMax
	}
	if maxChars <= 0 {
		return ""
	}
	const separator = "\n\n---\n\n"
	rendered := make([]string, len(turns))
	for i, turn := range turns {
		label := strings.Join(turn.Projects, ", ")
		if label == "" && turn.Project != nil {
			label = *turn.Project
		}
		project := ""
		if opts.IncludeProject && label != "" {
			project = "（项目：" + label + "）"
		}
		rendered[i] = project + "用户：" + turn.User + "\nBot：" + turn.Assistant
	}
	var kept []string
	used := 0
	for i := len(rendered) - 1; i >= 0; i-- {
		piece := rendered[i]
		add := piece
		if len(kept) > 0 {
			add = separator + piece
		}
		if used+len([]rune(add)) > maxChars {
			if len(kept) == 0 {
				kept = append(kept, truncate(piece, maxChars))
			}
			break
		}
		kept = append([]string{piece}, kept...)
		used += len([]rune(add))
	}
	return strings.Join(kept, separator)
}

// RecordableStatuses are the statuses that qualify for being appended to IM history.
func recordable(status string) bool {
	return status == "success" || status == "auto-fixed" || status == "unknown-project"
}

// ShouldRecordTurn reports whether a completed execution should be appended to IM history.
func ShouldRecordTurn(phase, status, sessionKey string, recordableTurn bool) bool {
	if !recordableTurn || phase != "execute" || sessionKey == "" {
		return false
	}
	return recordable(status)
}

// RecentTurnLoadContext mirrors v1's shouldLoadRecentTurns inputs.
type RecentTurnLoadContext struct {
	Enabled    bool
	Phase      string
	Source     string
	SessionKey string
}

// ShouldLoadRecentTurns reports whether the runner should load the shared IM conversation.
func ShouldLoadRecentTurns(ctx RecentTurnLoadContext) bool {
	return ctx.Enabled && ctx.Phase == "execute" && ctx.Source != "scheduler" && ctx.Source != "cli" && ctx.SessionKey != ""
}

var warnf = func(format string, args ...any) {}
