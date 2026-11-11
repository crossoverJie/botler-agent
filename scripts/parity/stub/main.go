// Command stub is a deterministic OpenAI-compatible chat-completions server used by the parity
// harness (scripts/parity.sh).
//
// It replays a scripted sequence of assistant replies, so both the v1 TypeScript implementation
// (through pi-ai's streaming adapter) and the Go implementation (non-streaming) can be driven
// with identical, reproducible model output. Streaming (SSE) is supported because pi-ai always
// uses the streaming endpoint (streamSimple).
//
// Usage: stub -addr 127.0.0.1:18099 -script path/to/steps.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
)

type toolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type step struct {
	Text      string     `json:"text"`
	ToolCalls []toolCall `json:"toolCalls"`
	Finish    string     `json:"finish,omitempty"`
}

type request struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

var (
	mu      sync.Mutex
	counter int
	steps   []step
)

func pickStep() step {
	mu.Lock()
	defer mu.Unlock()
	if len(steps) == 0 {
		return step{Text: "stub has no steps"}
	}
	idx := counter
	if idx >= len(steps) {
		idx = len(steps) - 1
	} else {
		counter++
	}
	return steps[idx]
}

func completion(step step) map[string]any {
	message := map[string]any{"role": "assistant", "content": step.Text}
	if len(step.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(step.ToolCalls))
		for i, tc := range step.ToolCalls {
			args := string(tc.Arguments)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   fmt.Sprintf("call_%d", i+1),
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
	}
	finish := step.Finish
	if finish == "" {
		if len(step.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	return map[string]any{
		"id":      "chatcmpl-stub",
		"object":  "chat.completion",
		"model":   "stub-model",
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110,
		},
	}
}

func writeStream(w http.ResponseWriter, step step) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(payload map[string]any) {
		body, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", body)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Text delta.
	if step.Text != "" {
		send(map[string]any{
			"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "stub-model",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": step.Text}}},
		})
	}
	// Tool-call deltas (arguments are emitted whole; pi-ai accumulates them).
	for i, tc := range step.ToolCalls {
		args := string(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		send(map[string]any{
			"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "stub-model",
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": i, "id": fmt.Sprintf("call_%d", i+1), "type": "function",
						"function": map[string]any{"name": tc.Name, "arguments": args},
					}},
				},
			}},
		})
	}
	finish := step.Finish
	if finish == "" {
		if len(step.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	send(map[string]any{
		"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "stub-model",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18099", "listen address")
	scriptPath := flag.String("script", "", "JSON file with the scripted steps")
	flag.Parse()
	if *scriptPath != "" {
		raw, err := os.ReadFile(*scriptPath)
		if err != nil {
			log.Fatalf("read script: %v", err)
		}
		if err := json.Unmarshal(raw, &steps); err != nil {
			log.Fatalf("parse script: %v", err)
		}
	}
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req request
		_ = json.NewDecoder(r.Body).Decode(&req)
		st := pickStep()
		if req.Stream {
			writeStream(w, st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(completion(st))
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	log.Printf("stub listening on %s (%d scripted steps)", *addr, len(steps))
	log.Fatal(http.ListenAndServe(*addr, nil))
}
