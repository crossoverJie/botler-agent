package monitor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpointsShape(t *testing.T) {
	mux := http.NewServeMux()
	HandleHealth(mux)

	for _, path := range []string{"/healthz", "/healthz/history", "/metrics"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty body", path)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var snap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"ok", "pid", "uptimeSec", "startedAt", "memory", "eventLoopLagMs", "queueDepth",
		"activeTasks", "activeAgents", "totalDispatched", "duplicatesTotal", "totalFailed",
		"statusCounts", "modelCache", "schedulesTotal", "schedulesEnabled",
		"nextFireAtSec", "lastFireAtSec", "lastFireId", "channels",
	} {
		if _, ok := snap[key]; !ok {
			t.Fatalf("snapshot is missing %q", key)
		}
	}
}

func TestMetricNamesMatchV1(t *testing.T) {
	body := BuildMetrics()
	for _, name := range []string{
		"botler_uptime_seconds",
		"botler_memory_rss_bytes",
		"botler_event_loop_lag_p50_ms",
		"botler_model_cache_queries_total",
		"botler_model_cache_hits_total",
		"botler_schedules_total",
		"botler_schedules_enabled_total",
		"botler_scheduler_next_fire_epoch_seconds",
		"botler_scheduler_last_fire_epoch_seconds",
	} {
		if !containsLine(body, "# TYPE "+name+" ") {
			t.Fatalf("metric %q is missing (or misnamed)", name)
		}
	}
}

func containsLine(body, needle string) bool {
	for _, line := range splitLines(body) {
		if len(line) >= len(needle) && line[:len(needle)] == needle {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
