package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/paths"
)

// jsLen mirrors JavaScript's String.length (UTF-16 code units), which v1 uses for byte counts in
// tool results. Keeping it identical avoids diffs in the tool-result text.
func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

type readTool struct{}

func (readTool) Name() string  { return "read" }
func (readTool) Label() string { return "Read file" }
func (readTool) Description() string {
	return "Read the content of a file inside the data directory. The path is relative to DATA_ROOT and limited to data subprojects under DATA_ROOT. Returns the raw file content (JSON files as JSON text) so you can understand the current data before deciding how to modify it."
}

func (readTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to DATA_ROOT (inside a data subproject under DATA_ROOT), e.g. project-name/data/xxx.json"}},"required":["path"]}`)
}

func (readTool) Execute(_ context.Context, args json.RawMessage) (agentrt.ToolResult, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentrt.ToolResult{}, err
	}
	abs, err := paths.SafePath(in.Path, paths.Options{Projects: TaskProjects()})
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	content := string(raw)
	return agentrt.ToolResult{
		Text:    content,
		Details: map[string]any{"path": in.Path, "bytes": jsLen(content)},
	}, nil
}

type writeTool struct{}

func (writeTool) Name() string  { return "write" }
func (writeTool) Label() string { return "Write JSON file" }
func (writeTool) Description() string {
	return "Write data (object or array) to a file, automatically serialized into formatted valid JSON. Used to create or modify JSON files inside a data subproject under DATA_ROOT. Note: pass the data structure itself (not JSON text) — this tool handles serialization. Before writing, read the existing content with read, modify it in memory, then write the whole thing back."
}

func (writeTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Target file path relative to DATA_ROOT (inside a data subproject under DATA_ROOT)"},"content":{"type":["object","array"],"description":"JSON value to write (object or array). Serialized with 2-space indentation. Pass the data structure itself, not a JSON text string."}},"required":["path","content"]}`)
}

func (writeTool) Execute(_ context.Context, args json.RawMessage) (agentrt.ToolResult, error) {
	var in struct {
		Path    string          `json:"path"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentrt.ToolResult{}, err
	}
	value := in.Content
	recovered := false
	// Auto-recovery: the model occasionally passes content as a JSON text string (which would
	// otherwise be double-encoded). If it is a string, parse it once; the result must be an
	// object/array, otherwise return an actionable error.
	trimmed := strings.TrimSpace(string(value))
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if err := json.Unmarshal(value, &inner); err != nil {
			return agentrt.ToolResult{}, fmt.Errorf("the write tool received a string for content, and it is not valid JSON text. Pass the data structure itself (object or array) — this tool handles serialization; do not pass a JSON text string")
		}
		value = json.RawMessage(inner)
		recovered = true
		trimmed = strings.TrimSpace(inner)
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return agentrt.ToolResult{}, fmt.Errorf("the write tool's content must be an object or array. Pass the data structure itself — this tool handles serialization")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, value, "", "  "); err != nil {
		return agentrt.ToolResult{}, fmt.Errorf("the write tool's content is not valid JSON: %v", err)
	}
	text := buf.String()
	abs, err := paths.SafePath(in.Path, paths.Options{Projects: TaskProjects()})
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return agentrt.ToolResult{}, err
	}
	if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
		return agentrt.ToolResult{}, err
	}
	n := jsLen(text)
	return agentrt.ToolResult{
		Text:    fmt.Sprintf("Wrote %s (%d bytes, valid JSON)", in.Path, n),
		Details: map[string]any{"path": in.Path, "bytes": n, "recovered": recovered},
	}, nil
}

type editTool struct{}

func (editTool) Name() string  { return "edit" }
func (editTool) Label() string { return "Text replacement" }
func (editTool) Description() string {
	return "Replace oldText with newText in a file. oldText must match the file's existing content character-for-character (including spaces and newlines), otherwise it fails. all=true replaces all matches, otherwise only the first. Used for local fixes inside JSON files."
}

func (editTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to DATA_ROOT"},"oldText":{"type":"string","description":"Original text to be replaced; must match the file content exactly"},"newText":{"type":"string","description":"Replacement text"},"all":{"type":"boolean","description":"Replace all matches; default false (first occurrence only)"}},"required":["path","oldText","newText"]}`)
}

func (editTool) Execute(_ context.Context, args json.RawMessage) (agentrt.ToolResult, error) {
	var in struct {
		Path    string `json:"path"`
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
		All     bool   `json:"all"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentrt.ToolResult{}, err
	}
	abs, err := paths.SafePath(in.Path, paths.Options{Projects: TaskProjects()})
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	current := string(raw)
	if !strings.Contains(current, in.OldText) {
		head := in.OldText
		if r := []rune(head); len(r) > 60 {
			head = string(r[:60])
		}
		return agentrt.ToolResult{}, fmt.Errorf("oldText not found in the original file (first 60 chars): %s", head)
	}
	next := strings.Replace(current, in.OldText, in.NewText, 1)
	if in.All {
		next = strings.ReplaceAll(current, in.OldText, in.NewText)
	}
	if err := os.WriteFile(abs, []byte(next), 0o644); err != nil {
		return agentrt.ToolResult{}, err
	}
	return agentrt.ToolResult{Text: "Updated " + in.Path, Details: map[string]any{"path": in.Path}}, nil
}
