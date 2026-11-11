package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/paths"
)

func withRoot(t *testing.T, projects ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Config{DataRoot: root, ConversationDir: t.TempDir(), ConversationTurnMaxChars: 4000})
	paths.ResetCache()
	t.Cleanup(paths.ResetCache)
	SetTaskContext(nil)
	return root
}

func TestReadToolReturnsFileContent(t *testing.T) {
	root := withRoot(t, "cook")
	path := filepath.Join(root, "cook", "a.json")
	if err := os.WriteFile(path, []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := readTool{}.Execute(context.Background(), json.RawMessage(`{"path":"cook/a.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != `{"x":1}` {
		t.Fatalf("unexpected content: %q", res.Text)
	}
}

func TestReadToolRejectsOutOfBounds(t *testing.T) {
	withRoot(t, "cook")
	if _, err := (readTool{}).Execute(context.Background(), json.RawMessage(`{"path":"../etc/passwd"}`)); err == nil {
		t.Fatal("expected out-of-bounds error")
	}
}

func TestWriteToolSerializesIndentedJSON(t *testing.T) {
	root := withRoot(t, "cook")
	_, err := writeTool{}.Execute(context.Background(), json.RawMessage(`{"path":"cook/new/data.json","content":{"b":2,"a":[1,2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cook", "new", "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"b\": 2,\n  \"a\": [\n    1,\n    2\n  ]\n}"
	if string(raw) != want {
		t.Fatalf("unexpected serialization:\n%s", raw)
	}
}

func TestWriteToolRecoversStringifiedJSON(t *testing.T) {
	root := withRoot(t, "cook")
	res, err := writeTool{}.Execute(context.Background(), json.RawMessage(`{"path":"cook/s.json","content":"{\"a\":1}"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "valid JSON") {
		t.Fatalf("unexpected text: %q", res.Text)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cook", "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), `"`) {
		t.Fatalf("content must not be double-encoded: %s", raw)
	}
}

func TestWriteToolRejectsScalarContent(t *testing.T) {
	withRoot(t, "cook")
	if _, err := (writeTool{}).Execute(context.Background(), json.RawMessage(`{"path":"cook/x.json","content":42}`)); err == nil {
		t.Fatal("expected an error for scalar content")
	}
}

func TestEditToolFirstOccurrenceAndAll(t *testing.T) {
	root := withRoot(t, "cook")
	path := filepath.Join(root, "cook", "e.json")
	if err := os.WriteFile(path, []byte("a\na\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (editTool{}).Execute(context.Background(), json.RawMessage(`{"path":"cook/e.json","oldText":"a","newText":"b"}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "b\na\n" {
		t.Fatalf("first occurrence only expected, got %q", raw)
	}
	if _, err := (editTool{}).Execute(context.Background(), json.RawMessage(`{"path":"cook/e.json","oldText":"a","newText":"c","all":true}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != "b\nc\n" {
		t.Fatalf("expected all occurrences replaced, got %q", raw)
	}
}

func TestEditToolMissingOldText(t *testing.T) {
	root := withRoot(t, "cook")
	path := filepath.Join(root, "cook", "e.json")
	if err := os.WriteFile(path, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := editTool{}.Execute(context.Background(), json.RawMessage(`{"path":"cook/e.json","oldText":"zzz","newText":"b"}`))
	if err == nil || !strings.HasPrefix(err.Error(), "oldText not found in the original file (first 60 chars): ") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunToolRejectsNonScriptTypes(t *testing.T) {
	root := withRoot(t, "cook")
	if err := os.WriteFile(filepath.Join(root, "cook", "x.sh"), []byte("echo hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTool{}.Execute(context.Background(), json.RawMessage(`{"script":"cook/x.sh"}`))
	if err == nil || !strings.Contains(err.Error(), "Unsupported script type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunScriptSuccessAndFailure(t *testing.T) {
	root := withRoot(t, "cook")
	ok := filepath.Join(root, "cook", "ok.py")
	if err := os.WriteFile(ok, []byte("print('hello')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := RunScript("cook/ok.py", nil)
	if !res.OK || strings.TrimSpace(res.Text) != "hello" {
		t.Fatalf("unexpected result: %+v", res)
	}
	bad := filepath.Join(root, "cook", "bad.py")
	if err := os.WriteFile(bad, []byte("import sys\nprint('boom')\nsys.exit(3)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = RunScript("cook/bad.py", nil)
	if res.OK || res.ErrorKind != ErrRuntime || !strings.Contains(res.Text, "boom") {
		t.Fatalf("unexpected failure result: %+v", res)
	}
}

func TestClearConversationRequiresIMTaskContext(t *testing.T) {
	withRoot(t, "cook")
	SetTaskContext(&TaskContext{})
	if _, err := (clearConversationTool{}).Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected an error outside an IM task")
	}
	SetTaskContext(&TaskContext{ConversationSessionKey: "im"})
	if _, err := (clearConversationTool{}).Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("expected success for an IM task: %v", err)
	}
	SetTaskContext(nil)
}
