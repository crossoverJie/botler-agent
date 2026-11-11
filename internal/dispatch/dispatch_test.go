package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/agentrt/fake"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/logs"
	"github.com/crossoverJie/botler-agent/internal/paths"
	"github.com/crossoverJie/botler-agent/internal/runner"
)

func setup(t *testing.T, maxTurns int) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cook"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cook", "AGENTS.md"), []byte("# cook\n记账项目\n路由提示：「记账」\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Set(&config.Config{
		DataRoot:                 root,
		Provider:                 "custom",
		Model:                    "test-model",
		LogDir:                   filepath.Join(t.TempDir(), "task-logs"),
		ConversationDir:          filepath.Join(t.TempDir(), "conversations"),
		ConversationContextOn:    true,
		ConversationContextTurns: 5,
		ConversationTurnMaxChars: 4000,
		ConversationContextMax:   12000,
		MaxToolTurns:             maxTurns,
	})
	paths.ResetCache()
	t.Cleanup(paths.ResetCache)
	return root
}

func TestDispatchWriteTaskSucceeds(t *testing.T) {
	root := setup(t, 20)
	rt := &fake.Runtime{Steps: []fake.Step{
		{ToolCalls: []agentrt.ToolCall{{ID: "c1", Name: "write", Arguments: json.RawMessage(`{"path":"cook/a.json","content":{"x":1}}`)}}},
		{Text: "done ✅"},
	}}
	runner.SetRuntimeForTest(rt)

	res := Dispatch(context.Background(), "记一下", Options{Source: "telegram", SessionKey: conversation.IMSessionKey, ID: "tg:1"})
	if res.Status != logs.StatusSuccess {
		t.Fatalf("unexpected status %q (text %q)", res.Status, res.Text)
	}
	if res.Text != "done ✅" {
		t.Fatalf("unexpected text %q", res.Text)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cook", "a.json"))
	if err != nil {
		t.Fatalf("expected the data file to be written: %v", err)
	}
	if !strings.Contains(string(raw), `"x": 1`) {
		t.Fatalf("unexpected file content: %s", raw)
	}

	page := logs.QueryLogs(logs.LogQuery{})
	if page.Total != 1 {
		t.Fatalf("expected one task log, got %d", page.Total)
	}
	if page.Logs[0].Status != logs.StatusSuccess || page.Logs[0].Source != "telegram" {
		t.Fatalf("unexpected log: %+v", page.Logs[0])
	}
	if len(page.Logs[0].Tools) != 1 || page.Logs[0].Tools[0].Name != "write" {
		t.Fatalf("expected one tool call in the log: %+v", page.Logs[0].Tools)
	}
	if !page.Logs[0].Mutated {
		t.Fatal("a write task must be marked mutated")
	}
	if page.Logs[0].Usage.CostUSD != 0 {
		t.Fatalf("costUsd must stay 0, got %v", page.Logs[0].Usage.CostUSD)
	}

	turns := conversation.LoadRecentTurns(conversation.IMSessionKey, 5)
	if len(turns) != 1 || turns[0].User != "记一下" || turns[0].Assistant != "done ✅" {
		t.Fatalf("unexpected conversation history: %+v", turns)
	}
}

func TestDispatchDeduplicatesWithinWindow(t *testing.T) {
	setup(t, 20)
	runner.SetRuntimeForTest(&fake.Runtime{Steps: []fake.Step{{Text: "ok"}}})
	first := Dispatch(context.Background(), "记一下", Options{Source: "telegram", ID: "tg:dup"})
	if first.Status != logs.StatusSuccess {
		t.Fatalf("unexpected first status %q", first.Status)
	}
	second := Dispatch(context.Background(), "hi", Options{Source: "telegram", ID: "tg:dup"})
	if second.Status != logs.StatusDuplicate || second.Text != DuplicateSentinel {
		t.Fatalf("expected duplicate, got %+v", second)
	}
}

func TestDispatchGreetingIsRecordedAsUnknownProject(t *testing.T) {
	setup(t, 20)
	runner.SetRuntimeForTest(&fake.Runtime{Steps: []fake.Step{{Text: "unused"}}})
	res := Dispatch(context.Background(), "hi", Options{Source: "telegram", ID: "tg:greet"})
	// v1 seeds the greeting log with an empty project set, so the dispatcher buckets it as
	// unknown-project (it must not be mistaken for a routed task).
	if res.Status != logs.StatusUnknownProject {
		t.Fatalf("unexpected status %q (text %q)", res.Status, res.Text)
	}
}

func TestDispatchEmptyReplyIsNotRecorded(t *testing.T) {
	setup(t, 20)
	runner.SetRuntimeForTest(&fake.Runtime{Steps: []fake.Step{{Text: ""}}})
	res := Dispatch(context.Background(), "查询", Options{Source: "telegram", SessionKey: conversation.IMSessionKey})
	if res.Status != logs.StatusSuccess {
		t.Fatalf("unexpected status %q", res.Status)
	}
	turns := conversation.LoadRecentTurns(conversation.IMSessionKey, 5)
	if len(turns) != 0 {
		t.Fatalf("a run with no reply must not be recorded as a turn: %+v", turns)
	}
}

func TestDispatchToolTurnCapHardStop(t *testing.T) {
	setup(t, 1)
	rt := &fake.Runtime{Steps: []fake.Step{
		{ToolCalls: []agentrt.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"cook/AGENTS.md"}`)}}},
		{Text: "should never be reached"},
	}}
	runner.SetRuntimeForTest(rt)
	res := Dispatch(context.Background(), "读一下", Options{Source: "telegram"})
	if !strings.Contains(res.Text, "⚠️ Reached the tool-call limit (1 turns)") {
		t.Fatalf("unexpected text %q", res.Text)
	}
	if !strings.Contains(res.Text, "Last step: read") {
		t.Fatalf("hard-stop copy must name the last tool: %q", res.Text)
	}
	if rt.CallCount() != 1 {
		t.Fatalf("the loop must hard-stop after the cap, got %d calls", rt.CallCount())
	}
}

func TestDispatchGreetingShortCircuit(t *testing.T) {
	setup(t, 20)
	rt := &fake.Runtime{Steps: []fake.Step{{Text: "should not be called"}}}
	runner.SetRuntimeForTest(rt)
	res := Dispatch(context.Background(), "你好", Options{Source: "telegram", SessionKey: conversation.IMSessionKey})
	if rt.CallCount() != 0 {
		t.Fatalf("greeting must not call the model, got %d calls", rt.CallCount())
	}
	if !strings.Contains(res.Text, "我是你的个人数据助手") {
		t.Fatalf("unexpected greeting reply %q", res.Text)
	}
	if turns := conversation.LoadRecentTurns(conversation.IMSessionKey, 5); len(turns) != 0 {
		t.Fatalf("greeting must not consume a conversation slot: %+v", turns)
	}
}

func TestDispatchProviderError(t *testing.T) {
	setup(t, 20)
	runner.SetRuntimeForTest(&fake.Runtime{Steps: []fake.Step{
		{StopReason: agentrt.StopError, ErrorMessage: "boom: connection refused"},
	}})
	res := Dispatch(context.Background(), "记一下", Options{Source: "telegram"})
	if !strings.Contains(res.Text, "⚠️ Task did not complete successfully: boom: connection refused") {
		t.Fatalf("unexpected text %q", res.Text)
	}
}

func TestDispatchScriptedTaskPreconditionError(t *testing.T) {
	setup(t, 20)
	res := DispatchScriptedTask(context.Background(), ScriptTaskOptions{ID: "schedule:x:1", Script: "cook/missing.py"})
	if res.Status != logs.StatusError {
		t.Fatalf("unexpected status %q", res.Status)
	}
	page := logs.QueryLogs(logs.LogQuery{})
	if page.Total != 1 {
		t.Fatalf("expected a script task log, got %d", page.Total)
	}
	if page.Logs[0].UserMessage != "run cook/missing.py" {
		t.Fatalf("unexpected userMessage %q", page.Logs[0].UserMessage)
	}
	if page.Logs[0].Mutated {
		t.Fatal("a precondition failure must not be marked mutated")
	}
}

// v1 records an unrouted (unknown-project) reply in the IM window: the status is in the recordable
// set and the runner does not opt out. Regression guard for the bool-zero-value bug.
func TestDispatchUnknownProjectTurnIsRecorded(t *testing.T) {
	root := setup(t, 20)
	// A second project makes routing run, so the stub's UNKNOWN answer is what decides.
	if err := os.MkdirAll(filepath.Join(root, "vocab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vocab", "AGENTS.md"), []byte("# vocab\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.SetRuntimeForTest(&fake.Runtime{Steps: []fake.Step{{Text: "UNKNOWN"}}})
	res := Dispatch(context.Background(), "随便说点什么", Options{Source: "telegram", SessionKey: conversation.IMSessionKey})
	if res.Status != logs.StatusUnknownProject {
		t.Fatalf("unexpected status %q", res.Status)
	}
	turns := conversation.LoadRecentTurns(conversation.IMSessionKey, 5)
	if len(turns) != 1 {
		t.Fatalf("the unrouted reply must consume a conversation slot, got %+v", turns)
	}
	if turns[0].Assistant != res.Text {
		t.Fatalf("unexpected recorded turn: %+v", turns[0])
	}
}

// A DATA_ROOT that is a symlink (macOS /var → /private/var) or a relative path must still attribute
// a script run to its project. Regression guard: the cwd used to be joined from the raw root, which
// never matched the canonicalized allowlist, so validation/commit widened to every project.
func TestScriptedTaskAttributesProjectUnderSymlinkedRoot(t *testing.T) {
	raw := t.TempDir() // deliberately NOT EvalSymlinks'd
	projectDir := filepath.Join(raw, "cook")
	scriptsDir := filepath.Join(projectDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scriptsDir, "ok.py")
	if err := os.WriteFile(script, []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "data.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	config.Set(&config.Config{
		DataRoot:     raw,
		Provider:     "custom",
		Model:        "test-model",
		LogDir:       filepath.Join(t.TempDir(), "task-logs"),
		MaxToolTurns: 20,
	})
	paths.ResetCache()
	t.Cleanup(paths.ResetCache)

	res := DispatchScriptedTask(context.Background(), ScriptTaskOptions{ID: "schedule:x:1", Script: "cook/scripts/ok.py"})
	if res.Status != logs.StatusSuccess {
		t.Fatalf("unexpected status %q (%s)", res.Status, res.Text)
	}
	page := logs.QueryLogs(logs.LogQuery{})
	if page.Total != 1 {
		t.Fatalf("expected one log, got %d", page.Total)
	}
	log := page.Logs[0]
	if log.Project == nil || *log.Project != "cook" {
		t.Fatalf("script run lost its project attribution: %+v", log.Project)
	}
	if len(log.Projects) != 1 || log.Projects[0] != "cook" {
		t.Fatalf("unexpected projects: %v", log.Projects)
	}
}

// panickingRuntime explodes inside the agent loop to exercise the dispatcher's panic guard.
type panickingRuntime struct{}

func (panickingRuntime) Complete(context.Context, agentrt.Request) (*agentrt.Message, error) {
	panic("boom from the model layer")
}

// v1 wraps the whole queued section in try/catch; a panic must become a task error plus a log line,
// never a dead process (CLI mode has no channel-level guard).
func TestDispatchRecoversFromPanic(t *testing.T) {
	setup(t, 20)
	runner.SetRuntimeForTest(panickingRuntime{})

	res := Dispatch(context.Background(), "记一下", Options{Source: "cli"})
	if res.Status != logs.StatusError {
		t.Fatalf("unexpected status %q", res.Status)
	}
	if !strings.Contains(res.Text, "⚠️ Task execution error: boom from the model layer") {
		t.Fatalf("unexpected text %q", res.Text)
	}
	page := logs.QueryLogs(logs.LogQuery{})
	if page.Total == 0 {
		t.Fatal("a recovered panic must still write a task log")
	}
	if page.Logs[len(page.Logs)-1].Status != logs.StatusError {
		t.Fatalf("unexpected log status %q", page.Logs[len(page.Logs)-1].Status)
	}
}

// errorRuntime fails every model call.
type errorRuntime struct{}

func (errorRuntime) Complete(context.Context, agentrt.Request) (*agentrt.Message, error) {
	return nil, errors.New("model exploded")
}

// A failing *routing* call is an unrouted message (v1 parses the empty text and replies with the
// Chinese fallback), not a task error.
func TestRoutingFailureFallsBackToUnroutedReply(t *testing.T) {
	root := setup(t, 20)
	if err := os.MkdirAll(filepath.Join(root, "vocab"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.SetRuntimeForTest(errorRuntime{})

	res := Dispatch(context.Background(), "随便说点什么", Options{Source: "telegram", SessionKey: conversation.IMSessionKey})
	if res.Status != logs.StatusUnknownProject {
		t.Fatalf("unexpected status %q (%s)", res.Status, res.Text)
	}
	if !strings.Contains(res.Text, "没太确定你想操作哪个数据子项目") {
		t.Fatalf("expected the Chinese unrouted fallback, got %q", res.Text)
	}
	if turns := conversation.LoadRecentTurns(conversation.IMSessionKey, 5); len(turns) != 1 {
		t.Fatalf("the fallback turn must be recorded, got %+v", turns)
	}
}
