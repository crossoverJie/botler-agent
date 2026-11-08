//! Read-only schedule views derived from task logs, ported from v1's
//! `src/scheduler/history.ts`. Scheduler dispatches carry
//! `taskId = "schedule:<id>:<epoch>"`, so no separate history file is needed.

use std::collections::{BTreeMap, HashMap, HashSet};

use serde::Serialize;

use crate::scheduler::cron::next_fire_epoch;
use crate::scheduler::store::load_schedules;
use crate::scheduler::types::ScheduleEntry;
use crate::task_log::record::TaskLog;
use crate::task_log::store::{schedule_id_from_task_id, LogPage, LogQuery};

use super::AppState;

const MAX_HISTORY_IDS: usize = 200;
const HISTORY_MESSAGE_CHARS: usize = 200;

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleLastRun {
    pub started_at: i64,
    pub status: String,
    pub duration_ms: i64,
    pub project: Option<String>,
    pub token_total: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct ScheduleOverviewItem {
    #[serde(flatten)]
    pub entry: ScheduleEntry,
    #[serde(rename = "nextFireAt")]
    pub next_fire_at: Option<i64>,
    pub state: String,
    #[serde(rename = "lastRun")]
    pub last_run: Option<ScheduleLastRun>,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RunToken {
    pub input: u64,
    pub output: u64,
    pub total: u64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleRunStats {
    pub schedule_id: String,
    pub total_runs: usize,
    pub by_status: BTreeMap<String, usize>,
    pub first_run_at: Option<i64>,
    pub last_run_at: Option<i64>,
    pub avg_duration_ms: i64,
    pub token: RunToken,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleHistoryEntry {
    pub schedule_id: String,
    pub run_count: usize,
    pub first_run_at: i64,
    pub last_run_at: i64,
    pub last_status: String,
    pub last_project: Option<String>,
    pub last_message: String,
}

fn scheduler_query() -> LogQuery {
    LogQuery {
        source: Some("scheduler".into()),
        phase: Some("execute".into()),
        ..LogQuery::default()
    }
}

fn project_of(log: &TaskLog) -> Option<String> {
    log.project.clone().or_else(|| {
        if log.projects.is_empty() {
            None
        } else {
            Some(log.projects.join(", "))
        }
    })
}

pub fn schedule_runs(state: &AppState, id: &str, q: &LogQuery) -> LogPage {
    let mut query = q.clone();
    query.source = Some("scheduler".into());
    query.schedule_id = Some(id.to_string());
    state.store.query(&query)
}

pub fn schedule_run_stats(state: &AppState, id: &str) -> ScheduleRunStats {
    let mut query = scheduler_query();
    query.schedule_id = Some(id.to_string());
    let mut by_status = BTreeMap::new();
    let mut token = RunToken::default();
    let mut duration_sum: i64 = 0;
    let mut first: Option<i64> = None;
    let mut last: Option<i64> = None;
    let mut total_runs = 0usize;
    state.store.scan_logs(&query, |log| {
        total_runs += 1;
        *by_status.entry(log.status.clone()).or_insert(0) += 1;
        token.input += log.usage.input;
        token.output += log.usage.output;
        token.total += log.usage.total;
        duration_sum += log.duration_ms;
        first = Some(first.map_or(log.started_at, |v| v.min(log.started_at)));
        last = Some(last.map_or(log.started_at, |v| v.max(log.started_at)));
    });
    ScheduleRunStats {
        schedule_id: id.to_string(),
        total_runs,
        by_status,
        first_run_at: first,
        last_run_at: last,
        avg_duration_ms: if total_runs == 0 {
            0
        } else {
            duration_sum / total_runs as i64
        },
        token,
    }
}

pub fn schedule_overview(state: &AppState, now_ms: i64) -> Vec<ScheduleOverviewItem> {
    let entries = load_schedules(&state.config);
    let mut latest: HashMap<String, TaskLog> = HashMap::new();
    state.store.scan_logs(&scheduler_query(), |log| {
        let Some(id) = schedule_id_from_task_id(&log.task_id) else {
            return;
        };
        let replace = latest
            .get(&id)
            .is_none_or(|prev| log.started_at > prev.started_at);
        if replace {
            latest.insert(id, log.clone());
        }
    });
    entries
        .into_iter()
        .map(|entry| {
            let next = if entry.enabled {
                next_fire_epoch(&entry, now_ms, &state.holidays)
                    .ok()
                    .flatten()
            } else {
                None
            };
            let state_str = if !entry.enabled {
                "paused"
            } else if next.is_none() {
                "past"
            } else {
                "active"
            };
            let last_run = latest.get(&entry.id).map(|log| ScheduleLastRun {
                started_at: log.started_at,
                status: log.status.clone(),
                duration_ms: log.duration_ms,
                project: project_of(log),
                token_total: log.usage.total,
            });
            ScheduleOverviewItem {
                entry,
                next_fire_at: next,
                state: state_str.to_string(),
                last_run,
            }
        })
        .collect()
}

struct HistoryAcc {
    count: usize,
    first: i64,
    last: i64,
    log: TaskLog,
}

pub fn schedule_history_index(state: &AppState) -> Vec<ScheduleHistoryEntry> {
    let mut acc: HashMap<String, HistoryAcc> = HashMap::new();
    state.store.scan_logs(&scheduler_query(), |log| {
        let Some(id) = schedule_id_from_task_id(&log.task_id) else {
            return;
        };
        match acc.get_mut(&id) {
            None => {
                acc.insert(
                    id,
                    HistoryAcc {
                        count: 1,
                        first: log.started_at,
                        last: log.started_at,
                        log: log.clone(),
                    },
                );
            }
            Some(a) => {
                a.count += 1;
                a.first = a.first.min(log.started_at);
                if log.started_at > a.last {
                    a.last = log.started_at;
                    a.log = log.clone();
                }
            }
        }
    });
    let exclude: HashSet<String> = load_schedules(&state.config)
        .into_iter()
        .map(|e| e.id)
        .collect();
    let mut out: Vec<ScheduleHistoryEntry> = acc
        .into_iter()
        .filter(|(id, _)| !exclude.contains(id))
        .map(|(schedule_id, a)| ScheduleHistoryEntry {
            schedule_id,
            run_count: a.count,
            first_run_at: a.first,
            last_run_at: a.last,
            last_status: a.log.status.clone(),
            last_project: project_of(&a.log),
            last_message: a
                .log
                .user_message
                .chars()
                .take(HISTORY_MESSAGE_CHARS)
                .collect(),
        })
        .collect();
    out.sort_by_key(|entry| std::cmp::Reverse(entry.last_run_at));
    out.truncate(MAX_HISTORY_IDS);
    out
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
    use crate::task_log::record::RecordInput;
    use crate::task_log::store::TaskLogStore;

    fn state(tmp: &tempfile::TempDir) -> AppState {
        let config = Arc::new(Config::with_paths(
            tmp.path().join("data"),
            tmp.path().join("cfg"),
        ));
        MonitorState {
            config,
            stats: Arc::new(DispatchStats::default()),
            store: TaskLogStore::new(tmp.path().join("logs")),
            history: Arc::new(std::sync::Mutex::new(History::default())),
            started_at: 0,
            holidays: HolidayCalendar::load(tmp.path().join("holidays.json"), String::new()),
            clock: Arc::new(FixedClock(1_700_000_000_000)),
        }
    }

    fn log(id: &str, started: i64, status: &str) -> TaskLog {
        TaskLog::new(RecordInput {
            id: id.into(),
            task_id: format!("schedule:morning:{started}"),
            phase: "execute".into(),
            source: "scheduler".into(),
            provider: "stub".into(),
            model: "stub".into(),
            projects: vec!["cook".into()],
            status: status.into(),
            started_ms: started,
            ended_ms: started + 20,
            user_message: "run daily.sh".into(),
            reply_text: "done".into(),
            images: vec![],
            inbound_image_count: None,
            mutated: true,
        })
    }

    #[test]
    fn derived_views_read_schedule_logs() {
        let tmp = tempfile::tempdir().unwrap();
        let state = state(&tmp);
        state
            .store
            .append(&log("a", 1_700_000_000_000, "success"))
            .unwrap();
        state
            .store
            .append(&log("b", 1_700_000_100_000, "error"))
            .unwrap();

        let runs = schedule_runs(&state, "morning", &LogQuery::default());
        assert_eq!(runs.total, 2);
        let stats = schedule_run_stats(&state, "morning");
        assert_eq!(stats.total_runs, 2);
        assert_eq!(stats.by_status.get("success"), Some(&1));
        assert_eq!(stats.avg_duration_ms, 20);
        let history = schedule_history_index(&state);
        assert_eq!(history.len(), 1);
        assert_eq!(history[0].schedule_id, "morning");
        assert_eq!(history[0].run_count, 2);
        assert_eq!(history[0].last_status, "error");
        let overview = schedule_overview(&state, 1_700_000_000_000);
        assert!(overview.is_empty()); // no schedules.json entries
    }
}
