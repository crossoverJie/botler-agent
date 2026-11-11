package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crossoverJie/botler-agent/internal/config"
)

// testEnv points the config dir and log dir at temporary directories and installs the process
// config, so no test touches the real ~/.botler-agent.
func testEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BOTLER_CONFIG_DIR", dir)
	t.Setenv("BOTLER_LOG_DIR", filepath.Join(dir, "task-logs"))
	t.Setenv("BOTLER_SCHEDULES_FILE", filepath.Join(dir, "schedules.json"))
	config.Set(config.Load())
	return dir
}

func resetBackends() {
	backendMu.Lock()
	backend = nil
	backendMu.Unlock()
	wechatMu.Lock()
	wechatBk = nil
	wechatMu.Unlock()
}

func do(t *testing.T, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

var uiHeader = map[string]string{"X-Botler-UI": "1"}

func TestIndexHTMLIsServedUnchanged(t *testing.T) {
	testEnv(t)
	resetBackends()

	want, err := os.ReadFile(filepath.Join("..", "..", "src", "webui", "index.html"))
	if err != nil {
		t.Fatalf("read v1 index.html: %v", err)
	}
	rec := do(t, http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	if rec.Body.String() != string(want) {
		t.Fatalf("served HTML differs from src/webui/index.html (len %d vs %d)", rec.Body.Len(), len(want))
	}
}

func TestUnknownPathIs404(t *testing.T) {
	testEnv(t)
	rec := do(t, http.MethodGet, "/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope status = %d, want 404", rec.Code)
	}
}

func TestUnknownLogIDReturns200Null(t *testing.T) {
	testEnv(t)
	rec := do(t, http.MethodGet, "/api/logs/does-not-exist", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "null" {
		t.Fatalf("body = %q, want literal null", got)
	}
}

func TestMutatingEndpointsRequireUIHeader(t *testing.T) {
	testEnv(t)
	resetBackends()
	SetSchedulesBackend(fakeBackend())
	SetWeChatBackend(fakeWeChatBackend())

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/cleanup", `{}`},
		{http.MethodPut, "/api/config/system-prompt", `{"content":"x"}`},
		{http.MethodPut, "/api/config/schedules", `{"schedules":[]}`},
		{http.MethodPost, "/api/config/backups/restore", `{"name":"system-prompt.md","file":"system-prompt.md.20260101T000000"}`},
		{http.MethodPost, "/api/wechat/pair", `{}`},
		{http.MethodPost, "/api/wechat/pair/cancel", `{}`},
	}
	for _, c := range cases {
		rec := do(t, c.method, c.path, c.body, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without header = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

func TestSchedulesPreviewDoesNotRequireUIHeader(t *testing.T) {
	testEnv(t)
	resetBackends()
	SetSchedulesBackend(fakeBackend())

	rec := do(t, http.MethodPost, "/api/config/schedules/preview", `{"cron":"0 8 * * *"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview without header = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK   bool    `json:"ok"`
		Next *int64  `json:"next"`
		Err  *string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode preview body: %v", err)
	}
	if !body.OK || body.Next == nil || *body.Next != 1234567890 {
		t.Fatalf("preview body = %s, want ok+next=1234567890", rec.Body.String())
	}
}

func TestBackupNameRegex(t *testing.T) {
	valid := []string{
		".env.20260101T120000",
		"system-prompt.md.20260101T120000",
		"schedules.json.20260101T120000",
	}
	invalid := []string{
		"holidays.json.1700000000000", // scheduler-owned holiday backup: never pruned by this layer
		"holidays.json.20260101T120000",
		"system-prompt.md.2026-01-01",
		"system-prompt.md.20260101T120000.bak",
		"../.env.20260101T120000",
		".env",
		"other.md.20260101T120000",
	}
	for _, name := range valid {
		if !backupRe.MatchString(name) {
			t.Errorf("expected %q to be a valid backup name", name)
		}
	}
	for _, name := range invalid {
		if backupRe.MatchString(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

func TestPruneBackupsKeepsTenAndSparesHolidays(t *testing.T) {
	dir := testEnv(t)
	resetBackends()
	backups := filepath.Join(dir, ".backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	// 12 config backups for system-prompt.md (fixed-width names sort chronologically).
	for i := 1; i <= 12; i++ {
		name := "system-prompt.md.202601" + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "T000000"
		if err := os.WriteFile(filepath.Join(backups, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	holiday := "holidays.json.1700000000000"
	if err := os.WriteFile(filepath.Join(backups, holiday), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	pruneBackups("system-prompt.md")

	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	var configBackups int
	var holidaySeen bool
	for _, e := range entries {
		if backupRe.MatchString(e.Name()) && strings.HasPrefix(e.Name(), "system-prompt.md.") {
			configBackups++
		}
		if e.Name() == holiday {
			holidaySeen = true
		}
	}
	if configBackups != maxBackups {
		t.Fatalf("kept %d config backups, want %d", configBackups, maxBackups)
	}
	if !holidaySeen {
		t.Fatal("prune removed the scheduler-owned holiday backup")
	}
	// The oldest two must be gone, the newest must survive.
	if _, err := os.Stat(filepath.Join(backups, "system-prompt.md.20260101T000000")); !os.IsNotExist(err) {
		t.Fatal("oldest backup was not pruned")
	}
	if _, err := os.Stat(filepath.Join(backups, "system-prompt.md.20260112T000000")); err != nil {
		t.Fatal("newest backup was pruned")
	}

	// ListBackups must never surface holiday backups, and must sort newest first.
	list := ListBackups()
	if len(list) != maxBackups {
		t.Fatalf("ListBackups returned %d entries, want %d", len(list), maxBackups)
	}
	for _, b := range list {
		if strings.HasPrefix(b.File, "holidays.json") {
			t.Fatalf("ListBackups leaked a holiday backup: %s", b.File)
		}
	}
	if list[0].TS < list[len(list)-1].TS {
		t.Fatal("ListBackups is not sorted newest first")
	}
}

func TestRestoreBackupRejectsTraversalAndUnknownFiles(t *testing.T) {
	dir := testEnv(t)
	resetBackends()
	backups := filepath.Join(dir, ".backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backups, "system-prompt.md.20260101T000000"), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RestoreBackup("system-prompt.md", "../../etc/passwd"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if err := RestoreBackup("system-prompt.md", "system-prompt.md.20260101T000000.bak"); err == nil {
		t.Fatal("invalid backup name was accepted")
	}
	if err := RestoreBackup("system-prompt.md", "schedules.json.20260101T000000"); err == nil {
		t.Fatal("backup of a different config file was accepted")
	}
	if err := RestoreBackup("../../etc/passwd", "system-prompt.md.20260101T000000"); err == nil {
		t.Fatal("invalid config file name was accepted")
	}
	if err := RestoreBackup("system-prompt.md", "system-prompt.md.20250101T000000"); err == nil {
		t.Fatal("missing backup was accepted")
	}

	// A valid restore works and is itself reversible (the live file is snapshotted first).
	if err := os.WriteFile(filepath.Join(dir, "system-prompt.md"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup("system-prompt.md", "system-prompt.md.20260101T000000"); err != nil {
		t.Fatalf("valid restore failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "system-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "backup" {
		t.Fatalf("restored content = %q, want %q", got, "backup")
	}
}

func TestWriteConfigFileBacksUpPreviousVersion(t *testing.T) {
	dir := testEnv(t)
	resetBackends()
	if err := os.WriteFile(filepath.Join(dir, "system-prompt.md"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := do(t, http.MethodPut, "/api/config/system-prompt", `{"content":"v2"}`, uiHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT system-prompt = %d (%s)", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "system-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Fatalf("live content = %q, want v2", got)
	}
	backups := ListBackups()
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}
	body, err := os.ReadFile(filepath.Join(dir, ".backups", backups[0].File))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1" {
		t.Fatalf("backup content = %q, want v1", body)
	}
}

func TestSystemPromptValidation(t *testing.T) {
	testEnv(t)
	resetBackends()
	rec := do(t, http.MethodPut, "/api/config/system-prompt", `{"content":"   "}`, uiHeader)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt = %d, want 400", rec.Code)
	}
	big := strings.Repeat("a", maxSystemPromptRunes+1)
	rec = do(t, http.MethodPut, "/api/config/system-prompt", `{"content":"`+big+`"}`, uiHeader)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized prompt = %d, want 400", rec.Code)
	}
}

func TestConfigViewShape(t *testing.T) {
	testEnv(t)
	resetBackends()
	SetSchedulesBackend(fakeBackend())
	SetWeChatBackend(fakeWeChatBackend())

	rec := do(t, http.MethodGet, "/api/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"systemPrompt", "schedules", "backups", "wechat", "pairing"} {
		if _, ok := body[key]; !ok {
			t.Errorf("GET /api/config missing key %q", key)
		}
	}
	var schedules []any
	if err := json.Unmarshal(body["schedules"], &schedules); err != nil {
		t.Errorf("schedules is not an array: %v", err)
	}
	var wechat map[string]any
	if err := json.Unmarshal(body["wechat"], &wechat); err != nil {
		t.Fatalf("wechat is not an object: %v", err)
	}
	for _, key := range []string{"configured", "owner", "thresholdHours", "reminderEnabled", "ownerHasContext"} {
		if _, ok := wechat[key]; !ok {
			t.Errorf("wechat panel missing key %q", key)
		}
	}
}

func TestSchedulesEndpointsUseBackend(t *testing.T) {
	testEnv(t)
	resetBackends()
	SetSchedulesBackend(fakeBackend())

	rec := do(t, http.MethodGet, "/api/schedules/overview", "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, http.MethodGet, "/api/schedules/history-index", "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Fatalf("history-index = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, http.MethodGet, "/api/schedules/demo/runs?limit=1", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"logs"`) {
		t.Fatalf("runs = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, http.MethodGet, "/api/schedules/demo/stats", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scheduleId":"demo"`) {
		t.Fatalf("stats = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, http.MethodGet, "/api/config/schedules", "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("config/schedules = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, http.MethodPut, "/api/config/schedules", `{"schedules":{}}`, uiHeader)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-array schedules = %d, want 400", rec.Code)
	}
	rec = do(t, http.MethodPut, "/api/config/schedules", `{"schedules":[{"id":"a"}]}`, uiHeader)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"live":true`) {
		t.Fatalf("save schedules = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadOnlyEndpointsRespond(t *testing.T) {
	testEnv(t)
	resetBackends()
	for _, path := range []string{"/api/logs", "/api/summary", "/api/usage", "/api/token-stats?granularity=day", "/healthz", "/healthz/history", "/api/config/backups"} {
		rec := do(t, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (%s)", path, rec.Code, rec.Body.String())
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("GET %s Content-Type = %q", path, rec.Header().Get("Content-Type"))
		}
	}
	rec := do(t, http.MethodGet, "/metrics", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "botler_uptime_seconds") {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	if rec := do(t, http.MethodGet, "/api/logs", "", nil); !strings.Contains(rec.Body.String(), `"logs"`) {
		t.Fatalf("logs body = %s", rec.Body.String())
	}
}

func TestCleanupRequiresHeaderAndRuns(t *testing.T) {
	testEnv(t)
	resetBackends()
	rec := do(t, http.MethodPost, "/api/cleanup", `{"days":["2026-01-01"]}`, uiHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode cleanup: %v", err)
	}
	if body.Deleted == nil {
		t.Fatal("cleanup response must contain a deleted array")
	}
}

func TestBuildWeChatStatus(t *testing.T) {
	reminded := int64(2_000)
	loginAt := int64(1_000)
	status := BuildWeChatStatus(10_000, WeChatAccount{Configured: true, Owner: "owner@im.wechat", LoginAtMs: &loginAt}, 23,
		WeChatReminder{Started: true, TickMs: 600_000},
		&WeChatContext{LastMsgAt: 1_000, LastRemindedAt: &reminded})

	if status["owner"] != "owner@im.wechat" || status["configured"] != true {
		t.Fatalf("bad basic fields: %v", status)
	}
	if status["ownerHasContext"] != true {
		t.Fatal("ownerHasContext should be true")
	}
	if status["quietMs"] != int64(9_000) {
		t.Fatalf("quietMs = %v, want 9000", status["quietMs"])
	}
	if status["remindedThisStretch"] != true {
		t.Fatal("remindedThisStretch should be true when lastRemindedAt >= lastMsgAt")
	}
	if status["nextReminderAt"] != int64(1_000)+23*3600e3 {
		t.Fatalf("nextReminderAt = %v", status["nextReminderAt"])
	}
	if status["reminderEnabled"] != true {
		t.Fatal("reminderEnabled should be true for a 23h threshold")
	}

	idle := BuildWeChatStatus(10_000, WeChatAccount{}, 0, WeChatReminder{}, nil)
	if idle["owner"] != nil || idle["ownerHasContext"] != false || idle["reminderEnabled"] != false {
		t.Fatalf("idle status = %v", idle)
	}
	if idle["nextReminderAt"] != nil || idle["quietMs"] != nil {
		t.Fatalf("idle status must use nulls: %v", idle)
	}
}

// fakeBackend is a minimal scheduler seam used by the handler tests.
func fakeBackend() SchedulesBackend {
	return SchedulesBackend{
		Load:   func() ([]any, error) { return []any{}, nil },
		Save:   func(items []any) error { return nil },
		Reload: func() {},
		NextFire: func(spec map[string]any, afterMs int64) (int64, bool, error) {
			return 1234567890, true, nil
		},
		Overview:     func() any { return []any{} },
		HistoryIndex: func() any { return []any{} },
		Runs: func(id string, limit, offset int) any {
			return map[string]any{"logs": []any{}, "total": 0}
		},
		Stats: func(id string) any {
			return map[string]any{"scheduleId": id, "totalRuns": 0}
		},
	}
}

// fakeWeChatBackend is a minimal WeChat seam used by the handler tests.
func fakeWeChatBackend() WeChatBackend {
	return WeChatBackend{
		Status: func() any {
			return BuildWeChatStatus(nowMillis(), WeChatAccount{Configured: true, Owner: "owner"}, 23, WeChatReminder{}, nil)
		},
		StartPairing: func(ctx context.Context) PairStartResult {
			return PairStartResult{State: "pending", QRImage: "data:image/png;base64,AA=="}
		},
		PairStatus:    func() any { return map[string]any{"state": "idle"} },
		CancelPairing: func() {},
	}
}

func TestWireRuntimeServesRealScheduler(t *testing.T) {
	dir := testEnv(t)
	resetBackends()
	// Reset the one-shot wiring so this test installs the production backends.
	wireOnce = sync.Once{}
	WireRuntime()

	entry := `{"schedules":[{"id":"wire-demo","enabled":true,"cron":"0 8 * * *","timezone":"Asia/Shanghai","message":"hi"}]}`
	if err := os.WriteFile(filepath.Join(dir, "schedules.json"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := do(t, http.MethodGet, "/api/config/schedules", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config/schedules = %d (%s)", rec.Code, rec.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode schedules: %v", err)
	}
	if len(items) != 1 || items[0]["id"] != "wire-demo" {
		t.Fatalf("schedules = %s", rec.Body.String())
	}

	rec = do(t, http.MethodPost, "/api/config/schedules/preview", `{"cron":"0 8 * * *","timezone":"Asia/Shanghai"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", rec.Code, rec.Body.String())
	}
	var preview struct {
		OK   bool   `json:"ok"`
		Next *int64 `json:"next"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if !preview.OK || preview.Next == nil || *preview.Next <= time.Now().UnixMilli() {
		t.Fatalf("preview next = %v", preview.Next)
	}

	// A spent `once` must render as "no upcoming fire" (null), not as an error.
	rec = do(t, http.MethodPost, "/api/config/schedules/preview", `{"once":"2020-01-01T00:00:00+08:00"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next":null`) {
		t.Fatalf("past once preview = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, http.MethodGet, "/api/schedules/overview", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wire-demo") {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}

	// PUT without the header is still rejected with the production backend installed.
	rec = do(t, http.MethodPut, "/api/config/schedules", `{"schedules":[]}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT schedules without header = %d, want 403", rec.Code)
	}
	rec = do(t, http.MethodPut, "/api/config/schedules", `{"schedules":[]}`, uiHeader)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT schedules with header = %d (%s)", rec.Code, rec.Body.String())
	}
}
