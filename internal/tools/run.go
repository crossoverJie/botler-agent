package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/crossoverJie/botler-agent/internal/agentrt"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/conversation"
	"github.com/crossoverJie/botler-agent/internal/paths"
)

// ErrorKind classifies a script failure.
type ErrorKind string

// Failure classes.
const (
	ErrPrecondition ErrorKind = "precondition"
	ErrRuntime      ErrorKind = "runtime"
)

// RunResult is the result of RunScript. It never throws, so failures can flow into retry / push /
// stats uniformly.
type RunResult struct {
	// OK reports whether the script ran and exited zero.
	OK bool
	// Text is the trimmed stdout on success, or a human-readable failure description.
	Text string
	// Cwd is the subproject root the script ran in (absent on precondition rejection outside a project).
	Cwd string
	// ErrorKind is `precondition` (never started) or `runtime` (started but non-zero exit).
	ErrorKind ErrorKind
}

// maxScriptOutput caps captured output (v1's maxBuffer = 1MB).
const maxScriptOutput = 1 << 20

const scriptTimeout = 60 * time.Second

func interpreterOf(script string) string {
	switch {
	case strings.HasSuffix(script, ".py"):
		return "python3"
	case strings.HasSuffix(script, ".js"), strings.HasSuffix(script, ".mjs"):
		return "node"
	}
	return ""
}

type cappedBuffer struct {
	buf       []byte
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := maxScriptOutput - len(c.buf)
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf = append(c.buf, p[:room]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return string(c.buf) }

// RunScript executes an existing script inside a data subproject (only python3 .py / node
// .js|.mjs) without throwing. No shell is involved (args are passed as an array), the working
// directory is locked to the script's subproject root, and the timeout is 60s. It is shared by
// the `run` tool and the scheduler's direct-script path, so there is exactly one execution core.
func RunScript(script string, args []string) RunResult {
	abs, err := paths.SafePath(script, paths.Options{Projects: TaskProjects()})
	if err != nil {
		return RunResult{Text: "Script path error: " + err.Error(), ErrorKind: ErrPrecondition}
	}
	project, ok := paths.ProjectOf(abs)
	cwd := ""
	if ok {
		cwd = filepath.Join(config.C().DataRoot, project)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.Mode().IsRegular() {
		return RunResult{Text: "Script does not exist or is not a file: " + script, Cwd: cwd, ErrorKind: ErrPrecondition}
	}
	if !ok {
		return RunResult{Text: "Script is not inside any data subproject: " + script, ErrorKind: ErrPrecondition}
	}
	interp := interpreterOf(script)
	if interp == "" {
		return RunResult{Text: "Unsupported script type (only .py / .js / .mjs): " + script, Cwd: cwd, ErrorKind: ErrPrecondition}
	}
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, interp, append([]string{abs}, args...)...)
	cmd.Dir = cwd
	stdout := &cappedBuffer{}
	stderr := &cappedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if runErr == nil {
		out := strings.TrimSpace(stdout.String())
		if out == "" {
			out = "(script ran successfully, no output)"
		}
		return RunResult{OK: true, Text: out, Cwd: cwd}
	}
	detail := strings.TrimSpace(stdout.String() + stderr.String())
	msg := runErr.Error()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg = "timed out after 60s"
	}
	if detail != "" {
		return RunResult{Text: fmt.Sprintf("Script failed (%s):\n%s", msg, detail), Cwd: cwd, ErrorKind: ErrRuntime}
	}
	return RunResult{Text: "Script failed: " + msg, Cwd: cwd, ErrorKind: ErrRuntime}
}

type runTool struct{}

func (runTool) Name() string  { return "run" }
func (runTool) Label() string { return "Run in-project script" }
func (runTool) Description() string {
	return "Run an existing script inside a data subproject (only python3 .py or node .js/.mjs), used to run build/refresh scripts defined by the project's conventions (e.g. build.py). No shell is used; args are passed directly; the working directory is locked to the script's subproject root with a 60-second timeout. Only run it when the project's conventions (AGENTS.md) require it; skip if the project has no such script."
}

func (runTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"script":{"type":"string","description":"Script path relative to DATA_ROOT, e.g. my-project/scripts/build.py"},"args":{"type":"array","items":{"type":"string"},"description":"Arguments passed to the script (optional), e.g. [\"--dry-run\"]"}},"required":["script"]}`)
}

func (runTool) Execute(_ context.Context, args json.RawMessage) (agentrt.ToolResult, error) {
	var in struct {
		Script string   `json:"script"`
		Args   []string `json:"args"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentrt.ToolResult{}, err
	}
	res := RunScript(in.Script, in.Args)
	// Preserve v1 behaviour: a pre-condition error throws (so the agent can self-heal), while a
	// runtime failure returns its output as text for the model to decide whether to fix and retry.
	if !res.OK && res.ErrorKind == ErrPrecondition {
		return agentrt.ToolResult{}, errors.New(res.Text)
	}
	return agentrt.ToolResult{
		Text:    res.Text,
		Details: map[string]any{"script": in.Script, "cwd": res.Cwd, "failed": !res.OK},
	}, nil
}

type clearConversationTool struct{}

func (clearConversationTool) Name() string  { return "clear_conversation_context" }
func (clearConversationTool) Label() string { return "Clear conversation context" }
func (clearConversationTool) Description() string {
	return "Clear the shared recent IM conversation history. Call this only when the user explicitly asks to start a new task and ignore/reset/clear the previous context. After clearing, continue with the current request if one was provided."
}

func (clearConversationTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (clearConversationTool) Execute(_ context.Context, _ json.RawMessage) (agentrt.ToolResult, error) {
	ctx := GetTaskContext()
	if ctx == nil || ctx.ConversationSessionKey == "" {
		return agentrt.ToolResult{}, errors.New("conversation context can only be cleared for IM user tasks")
	}
	conversation.ClearSession(ctx.ConversationSessionKey)
	return agentrt.ToolResult{Text: "Conversation context cleared.", Details: map[string]any{}}, nil
}
