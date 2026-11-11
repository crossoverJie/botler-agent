// Command parity drives v1 (TypeScript + pi-ai) and v3 (Go) with identical scripted model output
// and compares everything user-visible: task logs, conversation history and the data tree.
//
// How the model is made deterministic: v1 binds pi-ai's streamSimple at module load, so instead of
// patching v1 this harness points providers.json at a local OpenAI-compatible stub
// (scripts/parity/stub) that replays a fixed assistant/tool-call sequence. The stub speaks both
// SSE (pi-ai always streams) and plain JSON (adk-go's chat-completions client), so both sides see
// the same model output.
//
// Usage:
//
//	go run ./scripts/parity                  # run the default scenario
//	go run ./scripts/parity -scenario scripts/parity/scenarios/basic.json
//	go run ./scripts/parity -skip-v1         # only exercise the Go side
//	go run ./scripts/parity -keep            # keep the work directory for inspection
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

func main() {
	scenario := flag.String("scenario", "scripts/parity/scenarios/basic.json", "scripted model steps (JSON)")
	port := flag.Int("port", 18099, "stub listen port")
	message := flag.String("message", "记一下：今天午饭 30 元", "user message to replay")
	skipV1 := flag.Bool("skip-v1", false, "only run the Go implementation")
	keep := flag.Bool("keep", false, "keep the work directory")
	repo, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	flag.Parse()

	work, err := os.MkdirTemp("", "botler-parity-")
	if err != nil {
		fail(err)
	}
	// Canonicalize the work dir: macOS puts temp dirs under /var (a symlink to /private/var), and
	// v1's safePath rejects that as a symlink escape, which would mask the real comparison.
	if real, err := filepath.EvalSymlinks(work); err == nil {
		work = real
	}
	if !*keep {
		defer os.RemoveAll(work)
	} else {
		fmt.Printf("work dir: %s\n", work)
	}
	if err := run(repo, *scenario, *port, *message, *skipV1, work); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "parity: %v\n", err)
	os.Exit(1)
}

func run(repo, scenario string, port int, message string, skipV1 bool, work string) error {
	dataDir := filepath.Join(work, "data")
	if err := seedData(dataDir); err != nil {
		return err
	}
	cfgV1 := filepath.Join(work, "cfg-v1")
	cfgGo := filepath.Join(work, "cfg-go")
	for _, dir := range []string{cfgV1, cfgGo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := writeProviders(filepath.Join(dir, "providers.json"), port); err != nil {
			return err
		}
		env := fmt.Sprintf("DATA_ROOT=%s\nPI_PROVIDER=stub\nPI_MODEL=stub-model\n"+
			"WEBUI_ENABLED=0\nSCHEDULER_ENABLED=0\nMONITOR_ENABLED=0\nGIT_PUSH=0\n"+
			"CONVERSATION_CONTEXT_ENABLED=1\n", dataDir)
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
			return err
		}
	}

	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}

	fmt.Println("==> building stub + botler")
	if err := goBuild(repo, filepath.Join(bin, "stub"), "./scripts/parity/stub"); err != nil {
		return err
	}
	if err := goBuild(repo, filepath.Join(bin, "botler"), "./cmd/botler"); err != nil {
		return err
	}

	fmt.Println("==> starting model stub")
	stub := exec.Command(filepath.Join(bin, "stub"), "-addr", fmt.Sprintf("127.0.0.1:%d", port), "-script", scenario)
	stub.Dir = repo
	stub.Stdout = os.Stdout
	stub.Stderr = os.Stderr
	if err := stub.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stub.Process.Kill()
		_, _ = stub.Process.Wait()
	}()
	if err := waitHealthy(port, 10*time.Second); err != nil {
		return err
	}

	if !skipV1 {
		fmt.Println("==> running v1 (TypeScript)")
		if err := resetStub(port); err != nil {
			return err
		}
		v1Env := append(os.Environ(),
			"BOTLER_CONFIG_DIR="+cfgV1, "PI_PROVIDER=stub", "PI_MODEL=stub-model")
		if out, err := runCmd(repo, v1Env, "npx", "tsx", "src/index.ts", message); err != nil {
			fmt.Printf("v1 run failed: %v\n%s\n", err, out)
			return err
		}
		if err := copyTree(dataDir, filepath.Join(work, "data-v1")); err != nil {
			return err
		}
		// Reset the shared data tree so the Go run starts from the same seed.
		if err := os.RemoveAll(dataDir); err != nil {
			return err
		}
		if err := seedData(dataDir); err != nil {
			return err
		}
	}

	fmt.Println("==> running v3 (Go)")
	if err := resetStub(port); err != nil {
		return err
	}
	goEnv := append(os.Environ(),
		"BOTLER_CONFIG_DIR="+cfgGo, "PI_PROVIDER=stub", "PI_MODEL=stub-model")
	if out, err := runCmd(repo, goEnv, filepath.Join(bin, "botler"), message); err != nil {
		fmt.Printf("go run failed: %v\n%s\n", err, out)
		return err
	}

	if skipV1 {
		fmt.Println("SKIP_V1: Go run completed")
		return nil
	}

	fmt.Println("==> comparing")
	failures := 0

	v1Log, err := lastTaskLog(filepath.Join(cfgV1, "task-logs"))
	if err != nil {
		return err
	}
	goLog, err := lastTaskLog(filepath.Join(cfgGo, "task-logs"))
	if err != nil {
		return err
	}
	n1, n3 := normalizeLog(v1Log), normalizeLog(goLog)
	for _, key := range unionKeys(n1, n3) {
		if !reflect.DeepEqual(n1[key], n3[key]) {
			fmt.Printf("FAIL task-log.%s\n  v1: %v\n  go: %v\n", key, n1[key], n3[key])
			failures++
		}
	}
	if usage, ok := goLog["usage"].(map[string]any); ok {
		if cost, _ := usage["costUsd"].(float64); cost != 0 {
			fmt.Println("FAIL usage.costUsd must be 0")
			failures++
		}
	}

	turnsV1, _ := readTurns(filepath.Join(cfgV1, "conversations", "im.json"))
	turnsGo, _ := readTurns(filepath.Join(cfgGo, "conversations", "im.json"))
	if !reflect.DeepEqual(turnsV1, turnsGo) {
		fmt.Printf("FAIL conversations/im.json\n  v1: %v\n  go: %v\n", turnsV1, turnsGo)
		failures++
	}

	diffData, err := diffTrees(filepath.Join(work, "data-v1"), dataDir)
	if err != nil {
		return err
	}
	for _, line := range diffData {
		fmt.Printf("FAIL data: %s\n", line)
		failures++
	}

	if failures > 0 {
		fmt.Printf("\n%d unexplained difference(s)\n", failures)
		return fmt.Errorf("%d parity difference(s)", failures)
	}
	fmt.Println("OK: task log, cost fields, conversation history and data tree match")
	return nil
}

func seedData(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "cook"), 0o755); err != nil {
		return err
	}
	agents := "# cook\n\n记账数据在 cook/data.json。\n\n路由提示：「记账」\n"
	if err := os.WriteFile(filepath.Join(dir, "cook", "AGENTS.md"), []byte(agents), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cook", "data.json"), []byte("{\"items\":[]}\n"), 0o644)
}

func writeProviders(path string, port int) error {
	payload := map[string]any{
		"providers": map[string]any{
			"stub": map[string]any{
				"api":     "openai-completions",
				"baseUrl": fmt.Sprintf("http://127.0.0.1:%d/v1", port),
				"apiKey":  "stub-key",
				"models": []map[string]any{{
					"id": "stub-model", "name": "Stub", "contextWindow": 100000, "maxTokens": 4096,
				}},
			},
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

// resetStub rewinds the scripted replies so each implementation starts from step one.
func resetStub(port int) error {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/reset", port))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func waitHealthy(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("stub did not become healthy on port %d", port)
}

func goBuild(dir, out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCmd(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	fmt.Print(string(out))
	return string(out), err
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// lastTaskLog returns the newest task-log record of a directory.
func lastTaskLog(dir string) (map[string]any, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no task logs in %s", dir)
	}
	sort.Strings(files)
	raw, err := os.ReadFile(filepath.Join(dir, files[len(files)-1]))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Normalization follows the design doc §8.2 field-handling table.
var ignoredTop = map[string]bool{
	"id": true, "taskId": true, "startedAt": true, "endedAt": true,
	"durationMs": true, "modelCache": true, "systemPrompt": true,
}

func normalizeLog(log map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range log {
		if ignoredTop[k] {
			continue
		}
		switch k {
		case "usage":
			out[k] = map[string]any{"costUsd": fieldOf(v, "costUsd")}
		case "calls":
			list, _ := v.([]any)
			calls := make([]any, 0, len(list))
			for _, c := range list {
				calls = append(calls, map[string]any{
					"stopReason": fieldOf(c, "stopReason"),
					"costUsd":    fieldOf(c, "costUsd"),
				})
			}
			out[k] = calls
		case "routing":
			out[k] = map[string]any{
				"decision": fieldOf(v, "decision"),
				"usage":    map[string]any{"costUsd": fieldOf(fieldOf(v, "usage"), "costUsd")},
			}
		case "tools":
			list, _ := v.([]any)
			tools := make([]any, 0, len(list))
			for _, t := range list {
				tools = append(tools, map[string]any{
					"name":       fieldOf(t, "name"),
					"arguments":  fieldOf(t, "arguments"),
					"resultText": fieldOf(t, "resultText"),
					"isError":    fieldOf(t, "isError"),
					"details":    fieldOf(t, "details"),
				})
			}
			out[k] = tools
		case "conversation":
			list, _ := v.([]any)
			conv := make([]any, 0, len(list))
			for _, c := range list {
				conv = append(conv, map[string]any{
					"role":       fieldOf(c, "role"),
					"text":       fieldOf(c, "text"),
					"thinking":   fieldOf(c, "thinking"),
					"stopReason": fieldOf(c, "stopReason"),
					"toolName":   fieldOf(c, "toolName"),
					"isError":    fieldOf(c, "isError"),
				})
			}
			out[k] = conv
		case "images":
			list, _ := v.([]any)
			var https []string
			for _, item := range list {
				s, _ := item.(string)
				if strings.HasPrefix(s, "https://") {
					https = append(https, s)
				}
			}
			sort.Strings(https)
			out[k] = https
		default:
			out[k] = v
		}
	}
	return out
}

func fieldOf(v any, key string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m[key]
}

func unionKeys(a, b map[string]any) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func readTurns(path string) ([]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(doc.Turns))
	for _, t := range doc.Turns {
		out = append(out, map[string]any{
			"user":      t["user"],
			"assistant": t["assistant"],
			"projects":  t["projects"],
		})
	}
	return out, nil
}

// diffTrees reports files whose presence or content differs between two trees.
func diffTrees(a, b string) ([]string, error) {
	explained = nil
	fa, err := snapshot(a)
	if err != nil {
		return nil, err
	}
	fb, err := snapshot(b)
	if err != nil {
		return nil, err
	}
	var out []string
	for rel, content := range fa {
		other, ok := fb[rel]
		if !ok {
			out = append(out, rel+" only in v1")
			continue
		}
		if content == other {
			continue
		}
		// Known, explained deviation: adk-go hands tool arguments to the loop as a
		// map[string]any, so JSON written by the `write` tool has sorted keys instead of the
		// model's key order. Two files that parse to the same value are therefore equal.
		if strings.HasSuffix(rel, ".json") {
			var a, b any
			if json.Unmarshal([]byte(content), &a) == nil && json.Unmarshal([]byte(other), &b) == nil && reflect.DeepEqual(a, b) {
				explained = append(explained, rel)
				continue
			}
		}
		out = append(out, rel+" content differs")
	}
	for rel := range fb {
		if _, ok := fa[rel]; !ok {
			out = append(out, rel+" only in v3")
		}
	}
	sort.Strings(out)
	if len(explained) > 0 {
		sort.Strings(explained)
		fmt.Printf("explained (JSON key order only): %s\n", strings.Join(explained, ", "))
	}
	return out, nil
}

// explained records data files that differ only in JSON key order (adk-go map round trip).
var explained []string

func snapshot(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(raw)
		return nil
	})
	return out, err
}
