package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
	"unicode/utf16"

	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/logs"
	"github.com/crossoverJie/botler-agent/internal/monitor"
)

//go:embed index.html
var indexFS embed.FS

// maxBodyBytes caps request bodies (v1: 1MB).
const maxBodyBytes = 1_000_000

// maxSystemPromptRunes is the accepted system-prompt length (v1: 100KB UTF-16 units).
const maxSystemPromptRunes = 100_000

func indexHTML() ([]byte, error) { return indexFS.ReadFile("index.html") }

func nowMillis() int64 { return time.Now().UnixMilli() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"encode failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeText(w http.ResponseWriter, status int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(s))
}

// requireUIHeader is the zero-dependency CSRF mitigation: every mutating endpoint requires the
// custom header `X-Botler-UI: 1`, which a cross-origin fetch cannot send without a CORS preflight
// this server never approves. It writes 403 and returns false when the header is missing.
func requireUIHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Botler-UI") == "1" {
		return true
	}
	writeText(w, http.StatusForbidden, "Missing X-Botler-UI header")
	return false
}

func readJSONBody(r *http.Request) (json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBodyBytes {
		return nil, errors.New("body too large")
	}
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON body")
	}
	return json.RawMessage(raw), nil
}

// parseQuery mirrors v1's parseQuery: an empty parameter is "absent", not 0.
func parseQuery(r *http.Request) logs.LogQuery {
	sp := r.URL.Query()
	num := func(key string) (int64, bool) {
		v := sp.Get(key)
		if v == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		return int64(n), true
	}
	q := logs.LogQuery{Project: sp.Get("project"), Source: sp.Get("source"), Q: sp.Get("q")}
	if v, ok := num("from"); ok {
		q.From = &v
	}
	if v, ok := num("to"); ok {
		q.To = &v
	}
	if v, ok := num("limit"); ok {
		q.Limit = int(v)
	}
	if v, ok := num("offset"); ok {
		q.Offset = int(v)
	}
	return q
}

// Handler builds the WebUI HTTP handler (exported for tests).
func Handler() http.Handler {
	mux := http.NewServeMux()

	// Process health/metrics, shared with the standalone monitor port.
	monitor.HandleHealth(mux)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			writeText(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeText(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		html, err := indexHTML()
		if err != nil {
			writeText(w, http.StatusInternalServerError, "Cannot load index.html: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	})

	// ---- Task logs ----
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, logs.QueryLogs(parseQuery(r)))
	})
	// A missing log returns 200 + literal null (never 404): the UI distinguishes "no such id".
	mux.HandleFunc("GET /api/logs/{id}", func(w http.ResponseWriter, r *http.Request) {
		entry, ok := logs.GetLog(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusOK, nil)
			return
		}
		writeJSON(w, http.StatusOK, entry)
	})
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, logs.Summary())
	})
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, logs.DiskUsage())
	})
	mux.HandleFunc("GET /api/token-stats", func(w http.ResponseWriter, r *http.Request) {
		q := parseQuery(r)
		sp := r.URL.Query()
		series := logs.TokenTimeSeries(logs.TokenTimeQuery{
			From:        q.From,
			To:          q.To,
			Granularity: sp.Get("granularity"),
			GroupBy:     sp.Get("groupBy"),
		})
		writeJSON(w, http.StatusOK, series)
	})

	mux.HandleFunc("POST /api/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if !requireUIHeader(w, r) {
			return
		}
		raw, err := readJSONBody(r)
		if err != nil {
			writeText(w, http.StatusBadRequest, err.Error())
			return
		}
		var body struct {
			Before *string  `json:"before"`
			Days   []string `json:"days"`
			DryRun bool     `json:"dryRun"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeText(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		writeJSON(w, http.StatusOK, logs.CleanupLogs(logs.CleanupOptions{
			Before: body.Before,
			Days:   body.Days,
			DryRun: body.DryRun,
		}))
	})

	// ---- Scheduler views (read-only; backed by the injected scheduler) ----
	mux.HandleFunc("GET /api/schedules/overview", func(w http.ResponseWriter, r *http.Request) {
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		writeJSON(w, http.StatusOK, b.Overview())
	})
	mux.HandleFunc("GET /api/schedules/history-index", func(w http.ResponseWriter, r *http.Request) {
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": b.HistoryIndex()})
	})
	mux.HandleFunc("GET /api/schedules/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		q := parseQuery(r)
		writeJSON(w, http.StatusOK, b.Runs(r.PathValue("id"), q.Limit, q.Offset))
	})
	mux.HandleFunc("GET /api/schedules/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		writeJSON(w, http.StatusOK, b.Stats(r.PathValue("id")))
	})

	// ---- Config ----
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, configView())
	})

	mux.HandleFunc("GET /api/config/system-prompt", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"content": ReadConfigFile("system-prompt.md")})
	})
	mux.HandleFunc("PUT /api/config/system-prompt", func(w http.ResponseWriter, r *http.Request) {
		if !requireUIHeader(w, r) {
			return
		}
		raw, err := readJSONBody(r)
		if err != nil {
			writeText(w, http.StatusBadRequest, err.Error())
			return
		}
		var body struct {
			Content *string `json:"content"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeText(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		content := ""
		if body.Content != nil {
			content = *body.Content
		}
		if len(trimSpace(content)) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "system prompt must not be empty"})
			return
		}
		if len(utf16.Encode([]rune(content))) > maxSystemPromptRunes {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "system prompt too long (limit 100KB)"})
			return
		}
		if err := WriteConfigFile("system-prompt.md", content); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "live": true})
	})

	mux.HandleFunc("GET /api/config/schedules", func(w http.ResponseWriter, r *http.Request) {
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		items, err := b.Load()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if items == nil {
			items = []any{}
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("PUT /api/config/schedules", func(w http.ResponseWriter, r *http.Request) {
		if !requireUIHeader(w, r) {
			return
		}
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		raw, err := readJSONBody(r)
		if err != nil {
			writeText(w, http.StatusBadRequest, err.Error())
			return
		}
		var body struct {
			Schedules json.RawMessage `json:"schedules"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeText(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		var items []any
		if len(body.Schedules) == 0 || json.Unmarshal(body.Schedules, &items) != nil || items == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "schedules must be an array"})
			return
		}
		if err := b.Save(items); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		b.Reload()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "live": true})
	})

	// Read-only preview: computes the next fire time of a candidate spec without persisting
	// anything, so it deliberately does NOT require the UI header.
	mux.HandleFunc("POST /api/config/schedules/preview", func(w http.ResponseWriter, r *http.Request) {
		b, ok := schedulesBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scheduler backend not wired"})
			return
		}
		raw, err := readJSONBody(r)
		if err != nil {
			writeText(w, http.StatusBadRequest, err.Error())
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			writeText(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		spec := map[string]any{"id": "preview", "enabled": true, "message": ""}
		if tz, ok := body["timezone"].(string); ok && tz != "" {
			spec["timezone"] = tz
		} else {
			spec["timezone"] = "Asia/Shanghai"
		}
		if hm, ok := body["holidayMode"].(string); ok && hm != "" {
			spec["holidayMode"] = hm
		}
		for _, key := range []string{"cron", "interval", "at", "once"} {
			if v, ok := body[key].(string); ok && v != "" {
				spec[key] = v
			}
		}
		next, hasNext, err := b.NextFire(spec, time.Now().UnixMilli())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if !hasNext {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "next": nil})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "next": next})
	})

	mux.HandleFunc("GET /api/config/backups", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ListBackups())
	})
	mux.HandleFunc("POST /api/config/backups/restore", func(w http.ResponseWriter, r *http.Request) {
		if !requireUIHeader(w, r) {
			return
		}
		raw, err := readJSONBody(r)
		if err != nil {
			writeText(w, http.StatusBadRequest, err.Error())
			return
		}
		var body struct {
			Name string `json:"name"`
			File string `json:"file"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeText(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if !IsConfigFileName(body.Name) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid file name"})
			return
		}
		if err := RestoreBackup(body.Name, body.File); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if body.Name == "schedules.json" {
			if b, ok := schedulesBackend(); ok {
				b.Reload()
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "live": body.Name != ".env"})
	})

	// ---- WeChat pairing (start / observe / cancel) ----
	mux.HandleFunc("POST /api/wechat/pair", func(w http.ResponseWriter, r *http.Request) {
		if !requireUIHeader(w, r) {
			return
		}
		backend, ok := wechatBackend()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "state": "error", "error": "wechat backend not wired"})
			return
		}
		res := backend.StartPairing(r.Context())
		if res.State == "error" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "state": "error", "error": res.Error})
			return
		}
		if res.QRImage == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "state": "error", "error": "qr render failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "qrImage": res.QRImage, "state": res.State})
	})
	mux.HandleFunc("GET /api/wechat/pair/status", func(w http.ResponseWriter, r *http.Request) {
		backend, ok := wechatBackend()
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"state": "idle"})
			return
		}
		writeJSON(w, http.StatusOK, backend.PairStatus())
	})
	mux.HandleFunc("POST /api/wechat/pair/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !requireUIHeader(w, r) {
			return
		}
		if backend, ok := wechatBackend(); ok {
			backend.CancelPairing()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "idle"})
	})

	return mux
}

// configView is the full GET /api/config payload.
func configView() map[string]any {
	items := []any{}
	if b, ok := schedulesBackend(); ok {
		if loaded, err := b.Load(); err == nil && loaded != nil {
			items = loaded
		}
	}
	var wechatStatus any = BuildWeChatStatus(nowMillis(), WeChatAccount{}, config.C().WeChatReminderHours, WeChatReminder{}, nil)
	var pairing any = map[string]any{"state": "idle"}
	if b, ok := wechatBackend(); ok {
		if b.Status != nil {
			wechatStatus = b.Status()
		}
		if b.PairStatus != nil {
			pairing = b.PairStatus()
		}
	}
	return map[string]any{
		"systemPrompt": ReadConfigFile("system-prompt.md"),
		"schedules":    items,
		"backups":      ListBackups(),
		"wechat":       wechatStatus,
		"pairing":      pairing,
	}
}

// Start starts the WebUI HTTP server and shuts it down when ctx is cancelled. Listen errors are
// logged only (the WebUI must never be able to kill the agent).
func Start(ctx context.Context) {
	// Install the production scheduler / WeChat backends (idempotent).
	WireRuntime()

	// Drive the history sampler so the monitor view has real data even when the standalone 8899
	// port is disabled (MONITOR_ENABLED=0).
	monitor.EnsureSampling()

	cfg := config.C()
	addr := fmt.Sprintf("%s:%d", cfg.WebUIHost, cfg.WebUIPort)
	srv := &http.Server{Addr: addr, Handler: Handler()}
	go func() {
		fmt.Printf("[webui] task log UI on http://%s\n", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[webui] server error: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
