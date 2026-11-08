//! axum routers and handlers for the local task-log WebUI.
//!
//! Parity notes (v1 `src/webui/server.ts`):
//! - All state-changing writes require the `X-Botler-UI: 1` header. No CORS
//!   headers are emitted, so a cross-origin fetch with a custom header is
//!   blocked by the browser's preflight.
//! - Bodies are capped at 1 MB; invalid JSON is a 400.
//! - Handlers are read-only against the task-log store and `DispatchStats`;
//!   they never hold an `Arc<Dispatcher>` and never touch `DATA_ROOT`.

use std::sync::Arc;

use axum::body::{to_bytes, Body};
use axum::extract::{Path, Query, State};
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::monitor::{build_metrics, build_snapshot, HISTORY_CAPACITY, SAMPLE_INTERVAL_SECS};
use crate::scheduler::cron::next_fire_epoch;
use crate::scheduler::store::{load_schedules, normalize_schedules, save_schedules};
use crate::task_log::store::{CleanupOptions, LogQuery, TokenTimeQuery};

use super::config_store::{
    list_backups, read_config_file, restore_backup, write_config_file, CONFIG_FILE_NAMES,
};
use super::schedule_history::{
    schedule_history_index, schedule_overview, schedule_run_stats, schedule_runs,
};
use super::AppState;

/// S-4 is unresolved: the `weixin-agent` transport is not linked, so pairing
/// cannot work. Return an explicit unavailable payload instead of pretending.
pub const WECHAT_UNAVAILABLE: &str =
    "WeChat pairing requires the weixin-agent transport (spike S-4); not linked in this build";

const BODY_LIMIT: usize = 1_000_000;
const SYSTEM_PROMPT_LIMIT: usize = 100_000;

pub struct ApiError(StatusCode, String);

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (self.0, self.1).into_response()
    }
}

type ApiResult<T> = Result<T, ApiError>;

fn json_response<T: Serialize>(value: T) -> Response {
    Json(value).into_response()
}

fn json_error(status: StatusCode, message: impl Into<String>) -> Response {
    (status, Json(json!({ "error": message.into() }))).into_response()
}

fn require_ui_header(headers: &HeaderMap) -> ApiResult<()> {
    if headers.get("x-botler-ui").and_then(|v| v.to_str().ok()) == Some("1") {
        Ok(())
    } else {
        Err(ApiError(
            StatusCode::FORBIDDEN,
            "Missing X-Botler-UI header".into(),
        ))
    }
}

async fn parse_body<T: serde::de::DeserializeOwned>(body: Body) -> ApiResult<T> {
    let bytes = to_bytes(body, BODY_LIMIT)
        .await
        .map_err(|_| ApiError(StatusCode::PAYLOAD_TOO_LARGE, "body too large".into()))?;
    let slice: &[u8] = if bytes.is_empty() { b"{}" } else { &bytes };
    serde_json::from_slice(slice)
        .map_err(|err| ApiError(StatusCode::BAD_REQUEST, format!("invalid JSON body: {err}")))
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct RawQuery {
    from: Option<i64>,
    to: Option<i64>,
    project: Option<String>,
    source: Option<String>,
    q: Option<String>,
    limit: Option<usize>,
    offset: Option<usize>,
    granularity: Option<String>,
    #[serde(rename = "groupBy")]
    group_by: Option<String>,
}

fn to_log_query(raw: &RawQuery) -> LogQuery {
    LogQuery {
        from: raw.from,
        to: raw.to,
        project: raw.project.clone(),
        source: raw.source.clone(),
        q: raw.q.clone(),
        limit: raw.limit,
        offset: raw.offset,
        ..LogQuery::default()
    }
}

// ---------------------------------------------------------------------------
// Health / metrics (shared by the WebUI port and the standalone 8899 port)
// ---------------------------------------------------------------------------

pub fn health_router() -> Router<Arc<AppState>> {
    Router::new()
        .route("/healthz", get(healthz))
        .route("/healthz/history", get(health_history))
        .route("/metrics", get(metrics))
}

pub fn monitor_router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/", get(healthz))
        .merge(health_router())
        .with_state(state)
}

pub fn build_router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/", get(index))
        .route("/index.html", get(index))
        .merge(health_router())
        .route("/api/logs", get(api_logs))
        .route("/api/logs/:id", get(api_log_detail))
        .route("/api/summary", get(api_summary))
        .route("/api/usage", get(api_usage))
        .route("/api/token-stats", get(api_token_stats))
        .route("/api/cleanup", post(api_cleanup))
        .route("/api/schedules/overview", get(api_schedules_overview))
        .route(
            "/api/schedules/history-index",
            get(api_schedules_history_index),
        )
        .route("/api/schedules/:id/runs", get(api_schedule_runs))
        .route("/api/schedules/:id/stats", get(api_schedule_stats))
        .route("/api/config", get(api_config))
        .route(
            "/api/config/system-prompt",
            get(api_system_prompt_get).put(api_system_prompt_put),
        )
        .route(
            "/api/config/schedules",
            get(api_schedules_get).put(api_schedules_put),
        )
        .route("/api/config/schedules/preview", post(api_schedules_preview))
        .route("/api/config/backups", get(api_backups))
        .route("/api/config/backups/restore", post(api_backups_restore))
        .route("/api/wechat/pair", post(api_wechat_pair))
        .route("/api/wechat/pair/status", get(api_wechat_pair_status))
        .route("/api/wechat/pair/cancel", post(api_wechat_pair_cancel))
        .with_state(state)
}

async fn index() -> Html<&'static str> {
    Html(include_str!("index.html"))
}

async fn healthz(State(state): State<Arc<AppState>>) -> Response {
    json_response(build_snapshot(&state, state.clock.now_ms()))
}

async fn health_history(State(state): State<Arc<AppState>>) -> Response {
    let samples = state
        .history
        .lock()
        .map(|h| h.snapshot())
        .unwrap_or_default();
    json_response(json!({
        "intervalSec": SAMPLE_INTERVAL_SECS,
        "capacity": HISTORY_CAPACITY,
        "samples": samples,
    }))
}

async fn metrics(State(state): State<Arc<AppState>>) -> Response {
    let text = build_metrics(&state, state.clock.now_ms());
    (
        [(
            header::CONTENT_TYPE,
            "text/plain; version=0.0.4; charset=utf-8",
        )],
        text,
    )
        .into_response()
}

// ---------------------------------------------------------------------------
// Task logs / stats
// ---------------------------------------------------------------------------

async fn api_logs(State(state): State<Arc<AppState>>, Query(raw): Query<RawQuery>) -> Response {
    json_response(state.store.query(&to_log_query(&raw)))
}

async fn api_log_detail(State(state): State<Arc<AppState>>, Path(id): Path<String>) -> Response {
    json_response(state.store.get(&id))
}

async fn api_summary(State(state): State<Arc<AppState>>) -> Response {
    json_response(state.store.summary())
}

async fn api_usage(State(state): State<Arc<AppState>>) -> Response {
    json_response(state.store.disk_usage())
}

async fn api_token_stats(
    State(state): State<Arc<AppState>>,
    Query(raw): Query<RawQuery>,
) -> Response {
    let query = TokenTimeQuery {
        from: raw.from,
        to: raw.to,
        granularity: raw.granularity.clone(),
        group_by: raw.group_by.clone(),
    };
    json_response(state.store.token_time_series(&query, state.clock.now_ms()))
}

#[derive(Debug, Default, Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct CleanupBody {
    before: Option<String>,
    days: Option<Vec<String>>,
    dry_run: Option<bool>,
}

async fn api_cleanup(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Body,
) -> ApiResult<Response> {
    require_ui_header(&headers)?;
    let body: CleanupBody = parse_body(body).await?;
    let opts = CleanupOptions {
        before: body.before,
        days: body.days,
        dry_run: body.dry_run,
    };
    Ok(json_response(state.store.cleanup(&opts)))
}

// ---------------------------------------------------------------------------
// Derived schedule views
// ---------------------------------------------------------------------------

async fn api_schedules_overview(State(state): State<Arc<AppState>>) -> Response {
    json_response(schedule_overview(&state, state.clock.now_ms()))
}

async fn api_schedules_history_index(State(state): State<Arc<AppState>>) -> Response {
    json_response(json!({ "items": schedule_history_index(&state) }))
}

async fn api_schedule_runs(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
    Query(raw): Query<RawQuery>,
) -> Response {
    json_response(schedule_runs(&state, &id, &to_log_query(&raw)))
}

async fn api_schedule_stats(
    State(state): State<Arc<AppState>>,
    Path(id): Path<String>,
) -> Response {
    json_response(schedule_run_stats(&state, &id))
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

async fn api_config(State(state): State<Arc<AppState>>) -> Response {
    json_response(json!({
        "systemPrompt": read_config_file(&state.config, "system-prompt.md"),
        "schedules": load_schedules(&state.config),
        "backups": list_backups(&state.config),
        "wechat": { "configured": false },
        "pairing": { "state": "unavailable", "error": WECHAT_UNAVAILABLE },
    }))
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct ContentBody {
    content: Option<String>,
}

async fn api_system_prompt_get(State(state): State<Arc<AppState>>) -> Response {
    json_response(json!({
        "content": read_config_file(&state.config, "system-prompt.md"),
    }))
}

async fn api_system_prompt_put(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Body,
) -> ApiResult<Response> {
    require_ui_header(&headers)?;
    let body: ContentBody = parse_body(body).await?;
    let content = body.content.unwrap_or_default();
    if content.trim().is_empty() {
        return Ok(json_error(
            StatusCode::BAD_REQUEST,
            "system prompt must not be empty",
        ));
    }
    if content.chars().count() > SYSTEM_PROMPT_LIMIT {
        return Ok(json_error(
            StatusCode::BAD_REQUEST,
            "system prompt too long (limit 100KB)",
        ));
    }
    write_config_file(
        &state.config,
        "system-prompt.md",
        &content,
        state.clock.now_ms(),
    )
    .map_err(|err| ApiError(StatusCode::BAD_REQUEST, err.to_string()))?;
    Ok(json_response(json!({ "ok": true, "live": true })))
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct SchedulesBody {
    schedules: Option<Vec<Value>>,
}

async fn api_schedules_get(State(state): State<Arc<AppState>>) -> Response {
    json_response(load_schedules(&state.config))
}

async fn api_schedules_put(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Body,
) -> ApiResult<Response> {
    require_ui_header(&headers)?;
    let body: SchedulesBody = parse_body(body).await?;
    let Some(items) = body.schedules else {
        return Ok(json_error(
            StatusCode::BAD_REQUEST,
            "schedules must be an array",
        ));
    };
    save_schedules(&state.config, &items)
        .map_err(|err| ApiError(StatusCode::BAD_REQUEST, err.to_string()))?;
    Ok(json_response(json!({ "ok": true, "live": true })))
}

#[derive(Debug, Default, Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct PreviewBody {
    cron: Option<String>,
    interval: Option<String>,
    at: Option<String>,
    once: Option<String>,
    timezone: Option<String>,
    holiday_mode: Option<String>,
}

async fn api_schedules_preview(
    State(state): State<Arc<AppState>>,
    body: Body,
) -> ApiResult<Response> {
    let body: PreviewBody = parse_body(body).await?;
    let mut entry = json!({
        "id": "preview",
        "enabled": true,
        "timezone": body.timezone.filter(|v| !v.is_empty()).unwrap_or_else(|| "Asia/Shanghai".into()),
        // normalize_entry() rejects an empty message; v1 used "".
        "message": "preview",
    });
    let object = entry.as_object_mut().expect("object");
    for (key, value) in [
        ("cron", body.cron),
        ("interval", body.interval),
        ("at", body.at),
        ("once", body.once),
        ("holidayMode", body.holiday_mode),
    ] {
        if let Some(value) = value.filter(|v| !v.is_empty()) {
            object.insert(key.to_string(), Value::String(value));
        }
    }
    let entries = normalize_schedules(std::slice::from_ref(&entry))
        .map_err(|err| ApiError(StatusCode::BAD_REQUEST, err.to_string()))?;
    let next = entries.first().and_then(|e| {
        next_fire_epoch(e, state.clock.now_ms(), &state.holidays)
            .ok()
            .flatten()
    });
    Ok(json_response(json!({ "ok": true, "next": next })))
}

async fn api_backups(State(state): State<Arc<AppState>>) -> Response {
    json_response(list_backups(&state.config))
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct RestoreBody {
    name: Option<String>,
    file: Option<String>,
}

async fn api_backups_restore(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: Body,
) -> ApiResult<Response> {
    require_ui_header(&headers)?;
    let body: RestoreBody = parse_body(body).await?;
    let name = body.name.unwrap_or_default();
    let file = body.file.unwrap_or_default();
    if !CONFIG_FILE_NAMES.contains(&name.as_str()) {
        return Ok(json_error(StatusCode::BAD_REQUEST, "invalid file name"));
    }
    restore_backup(&state.config, &name, &file, state.clock.now_ms())
        .map_err(|err| ApiError(StatusCode::BAD_REQUEST, err.to_string()))?;
    Ok(json_response(json!({ "ok": true, "live": name != ".env" })))
}

// ---------------------------------------------------------------------------
// WeChat pairing — blocked by S-4, explicit unavailable payloads
// ---------------------------------------------------------------------------

async fn api_wechat_pair(headers: HeaderMap) -> ApiResult<Response> {
    require_ui_header(&headers)?;
    Ok(json_response(json!({
        "ok": false,
        "state": "error",
        "error": WECHAT_UNAVAILABLE,
    })))
}

async fn api_wechat_pair_status() -> Response {
    json_response(json!({
        "state": "unavailable",
        "error": WECHAT_UNAVAILABLE,
    }))
}

async fn api_wechat_pair_cancel(headers: HeaderMap) -> ApiResult<Response> {
    require_ui_header(&headers)?;
    Ok(json_response(json!({ "ok": true, "state": "idle" })))
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::config::Config;
    use crate::dispatcher::DispatchStats;
    use crate::monitor::history::History;
    use crate::monitor::state::{FixedClock, MonitorState};
    use crate::scheduler::holidays::HolidayCalendar;
    use crate::task_log::record::{RecordInput, TaskLog};
    use crate::task_log::store::TaskLogStore;

    struct Harness {
        base: String,
        state: Arc<AppState>,
        _tmp: tempfile::TempDir,
    }

    async fn harness() -> Harness {
        let tmp = tempfile::tempdir().unwrap();
        let config = Arc::new(Config::with_paths(
            tmp.path().join("data"),
            tmp.path().join("cfg"),
        ));
        let state: Arc<AppState> = Arc::new(MonitorState {
            config,
            stats: Arc::new(DispatchStats::default()),
            store: TaskLogStore::new(tmp.path().join("logs")),
            history: Arc::new(std::sync::Mutex::new(History::default())),
            started_at: 1_700_000_000_000,
            holidays: HolidayCalendar::load(tmp.path().join("holidays.json"), String::new()),
            clock: Arc::new(FixedClock(1_700_000_000_000)),
        });
        let router = build_router(Arc::clone(&state));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
        Harness {
            base: format!("http://{addr}"),
            state,
            _tmp: tmp,
        }
    }

    fn url(h: &Harness, path: &str) -> String {
        format!("{}{path}", h.base)
    }

    fn seed(state: &AppState) -> String {
        let log = TaskLog::new(RecordInput {
            id: "log-1".into(),
            task_id: "task-1".into(),
            phase: "execute".into(),
            source: "cli".into(),
            provider: "stub".into(),
            model: "stub".into(),
            projects: vec!["cook".into()],
            status: "success".into(),
            started_ms: 1_700_000_000_000,
            ended_ms: 1_700_000_000_010,
            user_message: "hello".into(),
            reply_text: "done".into(),
            images: vec![],
            inbound_image_count: Some(0),
            mutated: false,
        });
        state.store.append(&log).unwrap();
        log.id
    }

    #[tokio::test]
    async fn serves_index_and_health_shapes() {
        let h = harness().await;
        let client = reqwest::Client::new();

        let html = client
            .get(url(&h, "/"))
            .send()
            .await
            .unwrap()
            .text()
            .await
            .unwrap();
        assert!(html.contains("<html") || html.contains("<!DOCTYPE"));

        let health: Value = client
            .get(url(&h, "/healthz"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(health["ok"], true);
        assert!(health["memory"].get("rss").is_some());
        assert!(health["eventLoopLagMs"].get("p99").is_some());

        let history: Value = client
            .get(url(&h, "/healthz/history"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(history["capacity"], HISTORY_CAPACITY);

        let metrics = client
            .get(url(&h, "/metrics"))
            .send()
            .await
            .unwrap()
            .text()
            .await
            .unwrap();
        assert!(metrics.contains("botler_uptime_seconds"));
    }

    #[tokio::test]
    async fn reads_seeded_logs() {
        let h = harness().await;
        let id = seed(&h.state);
        let client = reqwest::Client::new();

        let logs: Value = client
            .get(url(&h, "/api/logs"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(logs["total"], 1);
        assert_eq!(logs["logs"][0]["id"], id);
        assert_eq!(logs["logs"][0]["project"], "cook");

        let detail: Value = client
            .get(url(&h, "/api/logs/log-1"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(detail["status"], "success");

        let summary: Value = client
            .get(url(&h, "/api/summary"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(summary["totalTasks"], 1);
    }

    #[tokio::test]
    async fn writes_require_ui_header_and_validate() {
        let h = harness().await;
        let client = reqwest::Client::new();

        let denied = client
            .post(url(&h, "/api/cleanup"))
            .json(&json!({ "dryRun": true }))
            .send()
            .await
            .unwrap();
        assert_eq!(denied.status(), StatusCode::FORBIDDEN);

        let ok = client
            .post(url(&h, "/api/cleanup"))
            .header("X-Botler-UI", "1")
            .json(&json!({ "dryRun": true }))
            .send()
            .await
            .unwrap();
        assert_eq!(ok.status(), StatusCode::OK);

        let bad = client
            .put(url(&h, "/api/config/system-prompt"))
            .header("X-Botler-UI", "1")
            .json(&json!({ "content": "  " }))
            .send()
            .await
            .unwrap();
        assert_eq!(bad.status(), StatusCode::BAD_REQUEST);

        let put = client
            .put(url(&h, "/api/config/system-prompt"))
            .header("X-Botler-UI", "1")
            .json(&json!({ "content": "You are helpful." }))
            .send()
            .await
            .unwrap();
        assert_eq!(put.status(), StatusCode::OK);
        assert_eq!(
            read_config_file(&h.state.config, "system-prompt.md"),
            "You are helpful."
        );
        // The first write has no prior file to back up; the second one does.
        let second = client
            .put(url(&h, "/api/config/system-prompt"))
            .header("X-Botler-UI", "1")
            .json(&json!({ "content": "You are very helpful." }))
            .send()
            .await
            .unwrap();
        assert_eq!(second.status(), StatusCode::OK);
        assert!(!list_backups(&h.state.config).is_empty());

        let malformed = client
            .post(url(&h, "/api/cleanup"))
            .header("X-Botler-UI", "1")
            .body("{not json")
            .send()
            .await
            .unwrap();
        assert_eq!(malformed.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn config_shape_and_blocked_pairing() {
        let h = harness().await;
        let client = reqwest::Client::new();

        let config: Value = client
            .get(url(&h, "/api/config"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(config["wechat"]["configured"], false);
        assert_eq!(config["pairing"]["state"], "unavailable");
        assert!(config["schedules"].is_array());
        assert!(config["backups"].is_array());

        let pair: Value = client
            .post(url(&h, "/api/wechat/pair"))
            .header("X-Botler-UI", "1")
            .json(&json!({}))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(pair["ok"], false);
        assert!(pair["error"].as_str().unwrap().contains("S-4"));

        let pair_denied = client
            .post(url(&h, "/api/wechat/pair"))
            .send()
            .await
            .unwrap();
        assert_eq!(pair_denied.status(), StatusCode::FORBIDDEN);

        let cancel: Value = client
            .post(url(&h, "/api/wechat/pair/cancel"))
            .header("X-Botler-UI", "1")
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(cancel["state"], "idle");
    }

    #[tokio::test]
    async fn schedules_preview_and_stats() {
        let h = harness().await;
        let client = reqwest::Client::new();

        let preview: Value = client
            .post(url(&h, "/api/config/schedules/preview"))
            .json(&json!({ "cron": "0 9 * * *", "timezone": "Asia/Shanghai" }))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(preview["ok"], true);
        assert!(!preview["next"].is_null());

        let overview: Value = client
            .get(url(&h, "/api/schedules/overview"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert!(overview.is_array());
        let runs: Value = client
            .get(url(&h, "/api/schedules/x/runs"))
            .send()
            .await
            .unwrap()
            .json()
            .await
            .unwrap();
        assert_eq!(runs["total"], 0);
    }
}
