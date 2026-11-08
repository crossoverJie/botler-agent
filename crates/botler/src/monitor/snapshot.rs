//! `/healthz` snapshot and `/metrics` exposition, with field names identical
//! to v1's `src/monitor/health.ts` so the SPA needs no changes.
//!
//! Real values come from `DispatchStats` and `schedules.json`. Fields that only
//! exist in Node/V8 (heap, event-loop lag, model cache, channel liveness) are
//! honest zeros: the Rust core has no equivalent and must not fake data.

use std::collections::BTreeMap;
use std::sync::atomic::Ordering;

use serde::Serialize;

use crate::scheduler::cron::next_fire_epoch;
use crate::scheduler::store::load_schedules;

use super::procinfo::rss_bytes;
use super::state::MonitorState;

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct MemorySnapshot {
    pub rss: u64,
    pub heap_used: u64,
    pub heap_total: u64,
    pub external: u64,
    pub array_buffers: u64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LagSnapshot {
    pub p50: f64,
    pub p90: f64,
    pub p99: f64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ModelCacheSnapshot {
    pub queries: u64,
    pub hits: u64,
    pub hit_rate: f64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct HealthSnapshot {
    pub ok: bool,
    pub pid: u32,
    pub uptime_sec: u64,
    pub started_at: i64,
    pub memory: MemorySnapshot,
    pub event_loop_lag_ms: LagSnapshot,
    pub queue_depth: u64,
    pub active_tasks: u64,
    pub active_agents: u64,
    pub total_dispatched: u64,
    pub duplicates_total: u64,
    pub total_failed: u64,
    pub status_counts: BTreeMap<String, u64>,
    pub model_cache: ModelCacheSnapshot,
    pub schedules_total: usize,
    pub schedules_enabled: usize,
    pub next_fire_at_sec: i64,
    pub last_fire_at_sec: i64,
    pub last_fire_id: String,
    pub channels: BTreeMap<String, serde_json::Value>,
}

#[derive(Debug, Clone)]
struct Derived {
    uptime_sec: u64,
    started_at: i64,
    rss: u64,
    queue_depth: u64,
    active_tasks: u64,
    total_dispatched: u64,
    duplicates: u64,
    failures: u64,
    status_counts: BTreeMap<String, u64>,
    schedules_total: usize,
    schedules_enabled: usize,
    next_fire_sec: i64,
}

/// `queueDepth` and `activeTasks` are derived from the monotonic counters:
/// enqueued-started and started-finished respectively (v1's mark* posture).
fn derive(state: &MonitorState, now_ms: i64) -> Derived {
    let stats = &state.stats;
    let enqueued = stats.enqueued.load(Ordering::SeqCst);
    let started = stats.started.load(Ordering::SeqCst);
    let finished = stats.finished.load(Ordering::SeqCst);
    let entries = load_schedules(&state.config);
    let enabled: Vec<_> = entries.iter().filter(|e| e.enabled).collect();
    let next_fire = enabled
        .iter()
        .filter_map(|e| next_fire_epoch(e, now_ms, &state.holidays).ok().flatten())
        .min();
    Derived {
        uptime_sec: (now_ms - state.started_at).max(0) as u64 / 1000,
        started_at: state.started_at,
        rss: rss_bytes(),
        queue_depth: enqueued.saturating_sub(started),
        active_tasks: started.saturating_sub(finished),
        total_dispatched: started,
        duplicates: stats.duplicates.load(Ordering::SeqCst),
        failures: stats.failures.load(Ordering::SeqCst),
        status_counts: stats.status_counts(),
        schedules_total: entries.len(),
        schedules_enabled: enabled.len(),
        next_fire_sec: next_fire.map(|ms| ms / 1000).unwrap_or(0),
    }
}

pub fn build_snapshot(state: &MonitorState, now_ms: i64) -> HealthSnapshot {
    let d = derive(state, now_ms);
    HealthSnapshot {
        ok: true,
        pid: std::process::id(),
        uptime_sec: d.uptime_sec,
        started_at: d.started_at,
        memory: MemorySnapshot {
            rss: d.rss,
            ..MemorySnapshot::default()
        },
        event_loop_lag_ms: LagSnapshot::default(),
        queue_depth: d.queue_depth,
        active_tasks: d.active_tasks,
        // One global worker: at most one prompt in flight. Honest proxy.
        active_agents: d.active_tasks,
        total_dispatched: d.total_dispatched,
        duplicates_total: d.duplicates,
        total_failed: d.failures,
        status_counts: d.status_counts,
        model_cache: ModelCacheSnapshot::default(),
        schedules_total: d.schedules_total,
        schedules_enabled: d.schedules_enabled,
        next_fire_at_sec: d.next_fire_sec,
        last_fire_at_sec: 0,
        last_fire_id: String::new(),
        channels: BTreeMap::new(),
    }
}

fn emit(
    lines: &mut Vec<String>,
    name: &str,
    kind: &str,
    help: &str,
    value: impl std::fmt::Display,
) {
    lines.push(format!("# HELP {name} {help}"));
    lines.push(format!("# TYPE {name} {kind}"));
    lines.push(format!("{name} {value}"));
}

fn emit_labeled(lines: &mut Vec<String>, name: &str, help: &str, value: u64, label: &str) {
    lines.push(format!("# HELP {name} {help}"));
    lines.push(format!("# TYPE {name} counter"));
    lines.push(format!("{name}{{status=\"{label}\"}} {value}"));
}

pub fn build_metrics(state: &MonitorState, now_ms: i64) -> String {
    let d = derive(state, now_ms);
    let mut lines = Vec::new();
    emit(
        &mut lines,
        "botler_uptime_seconds",
        "gauge",
        "Process uptime in seconds",
        d.uptime_sec,
    );
    emit(
        &mut lines,
        "botler_memory_rss_bytes",
        "gauge",
        "Resident set size in bytes",
        d.rss,
    );
    for (name, help) in [
        ("botler_memory_heap_used_bytes", "V8 heap used in bytes"),
        ("botler_memory_heap_total_bytes", "V8 heap total in bytes"),
        ("botler_memory_external_bytes", "External memory in bytes"),
        (
            "botler_memory_array_buffers_bytes",
            "ArrayBuffer memory in bytes",
        ),
    ] {
        emit(&mut lines, name, "gauge", help, 0);
    }
    for (name, help) in [
        ("botler_event_loop_lag_p50_ms", "Event loop lag p50 in ms"),
        ("botler_event_loop_lag_p90_ms", "Event loop lag p90 in ms"),
        ("botler_event_loop_lag_p99_ms", "Event loop lag p99 in ms"),
    ] {
        emit(&mut lines, name, "gauge", help, 0);
    }
    emit(
        &mut lines,
        "botler_queue_depth",
        "gauge",
        "Tasks queued awaiting execution",
        d.queue_depth,
    );
    emit(
        &mut lines,
        "botler_active_tasks",
        "gauge",
        "Tasks currently executing",
        d.active_tasks,
    );
    emit(
        &mut lines,
        "botler_active_agents",
        "gauge",
        "Agent.prompt() instances in flight",
        d.active_tasks,
    );
    emit(
        &mut lines,
        "botler_dispatches_total",
        "counter",
        "Non-duplicate dispatches",
        d.total_dispatched,
    );
    emit(
        &mut lines,
        "botler_duplicates_total",
        "counter",
        "Messages dropped by dedup",
        d.duplicates,
    );
    emit(
        &mut lines,
        "botler_failures_total",
        "counter",
        "Failed tasks (error + validation-failed)",
        d.failures,
    );
    emit(
        &mut lines,
        "botler_model_cache_queries_total",
        "counter",
        "Model resolution cache queries",
        0,
    );
    emit(
        &mut lines,
        "botler_model_cache_hits_total",
        "counter",
        "Model resolution cache hits",
        0,
    );
    for (status, count) in &d.status_counts {
        emit_labeled(
            &mut lines,
            "botler_tasks_total",
            "Tasks by final status",
            *count,
            status,
        );
    }
    emit(
        &mut lines,
        "botler_schedules_total",
        "gauge",
        "Total schedule entries",
        d.schedules_total,
    );
    emit(
        &mut lines,
        "botler_schedules_enabled_total",
        "gauge",
        "Enabled schedule entries",
        d.schedules_enabled,
    );
    emit(
        &mut lines,
        "botler_scheduler_next_fire_epoch_seconds",
        "gauge",
        "Epoch seconds of next scheduled fire (0 = none)",
        d.next_fire_sec,
    );
    emit(
        &mut lines,
        "botler_scheduler_last_fire_epoch_seconds",
        "gauge",
        "Epoch seconds of last scheduled fire (0 = none)",
        0,
    );
    lines.join("\n") + "\n"
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::dispatcher::DispatchStats;
    use crate::monitor::history::History;
    use crate::monitor::state::{FixedClock, MonitorState};
    use crate::scheduler::holidays::HolidayCalendar;
    use crate::task_log::store::TaskLogStore;

    fn state(tmp: &tempfile::TempDir) -> MonitorState {
        let cfg = Arc::new(crate::config::Config::with_paths(
            tmp.path().join("data"),
            tmp.path().join("cfg"),
        ));
        let calendar = HolidayCalendar::load(tmp.path().join("holidays.json"), String::new());
        MonitorState {
            config: cfg,
            stats: Arc::new(DispatchStats::default()),
            store: TaskLogStore::new(tmp.path().join("logs")),
            history: Arc::new(std::sync::Mutex::new(History::default())),
            started_at: 1_000_000,
            holidays: calendar,
            clock: Arc::new(FixedClock(1_006_000)),
        }
    }

    #[test]
    fn snapshot_has_v1_shape_and_derived_counters() {
        let tmp = tempfile::tempdir().unwrap();
        let state = state(&tmp);
        state.stats.enqueued.store(5, Ordering::SeqCst);
        state.stats.started.store(3, Ordering::SeqCst);
        state.stats.finished.store(1, Ordering::SeqCst);
        state.stats.duplicates.store(2, Ordering::SeqCst);
        state.stats.failures.store(1, Ordering::SeqCst);
        state.stats.bump(crate::dispatcher::TaskStatus::Success);

        let snapshot = build_snapshot(&state, 1_006_000);
        assert!(snapshot.ok);
        assert_eq!(snapshot.uptime_sec, 6);
        assert_eq!(snapshot.queue_depth, 2);
        assert_eq!(snapshot.active_tasks, 2);
        assert_eq!(snapshot.active_agents, 2);
        assert_eq!(snapshot.total_dispatched, 3);
        assert_eq!(snapshot.duplicates_total, 2);
        assert_eq!(snapshot.total_failed, 1);
        assert_eq!(snapshot.status_counts.get("success"), Some(&1));
        assert_eq!(snapshot.memory.heap_used, 0);
        assert_eq!(snapshot.event_loop_lag_ms.p99, 0.0);
        assert_eq!(snapshot.last_fire_at_sec, 0);
        assert!(snapshot.channels.is_empty());

        let json = serde_json::to_value(&snapshot).unwrap();
        let obj = json.as_object().unwrap();
        for key in [
            "ok",
            "pid",
            "uptimeSec",
            "startedAt",
            "memory",
            "eventLoopLagMs",
            "queueDepth",
            "activeTasks",
            "activeAgents",
            "totalDispatched",
            "duplicatesTotal",
            "totalFailed",
            "statusCounts",
            "modelCache",
            "schedulesTotal",
            "schedulesEnabled",
            "nextFireAtSec",
            "lastFireAtSec",
            "lastFireId",
            "channels",
        ] {
            assert!(obj.contains_key(key), "missing {key}");
        }
        assert!(obj["memory"].get("rss").is_some());
        assert!(obj["eventLoopLagMs"].get("p99").is_some());
    }

    #[test]
    fn metrics_contains_v1_metric_names() {
        let tmp = tempfile::tempdir().unwrap();
        let state = state(&tmp);
        let text = build_metrics(&state, 1_006_000);
        for name in [
            "botler_uptime_seconds",
            "botler_memory_rss_bytes",
            "botler_queue_depth",
            "botler_dispatches_total",
            "botler_scheduler_next_fire_epoch_seconds",
        ] {
            assert!(text.contains(name), "missing {name}");
        }
        assert!(text.ends_with('\n'));
    }
}
