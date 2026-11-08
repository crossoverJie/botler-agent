//! Append-only JSONL task-log store, one file per local day.
//!
//! The directory lives outside `DATA_ROOT` (default `~/.botler-agent/task-logs`),
//! so logs never enter the data/git boundary. Reads tolerate corrupt lines; the
//! only mutating operation is `cleanup`, locked to day-file names inside the
//! directory. All writes come from the single dispatcher worker.

use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::{Path, PathBuf};

use anyhow::Result;
use chrono::{DateTime, Datelike, Duration as ChronoDuration, Local, TimeZone, Timelike, Utc};
use serde::{Deserialize, Serialize};

use crate::fsutil::append_jsonl_fsync;

use super::record::{ModelCacheStats, TaskLog};

/// Strict `YYYY-MM-DD.jsonl` name; also the allowlist for cleanup deletions.
pub fn is_day_file(name: &str) -> bool {
    let b = name.as_bytes();
    if b.len() != 16 || &b[10..] != b".jsonl" || b[4] != b'-' || b[7] != b'-' {
        return false;
    }
    b[..10]
        .iter()
        .enumerate()
        .all(|(i, c)| i == 4 || i == 7 || c.is_ascii_digit())
}

pub fn validate_day(day: &str) -> bool {
    is_day_file(&format!("{day}.jsonl"))
}

/// v1 names files by local calendar day (`new Date(ts).getDate()`).
pub fn day_file_name(ts_ms: i64) -> String {
    utc_dt(ts_ms)
        .with_timezone(&Local)
        .format("%Y-%m-%d.jsonl")
        .to_string()
}

fn utc_dt(ts_ms: i64) -> DateTime<Utc> {
    DateTime::<Utc>::from_timestamp_millis(ts_ms)
        .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).expect("epoch"))
}

/// Scheduler dispatches use `schedule:<id>:<epoch>`; capture `<id>` greedily so
/// ids containing `:` still resolve.
pub fn schedule_id_from_task_id(task_id: &str) -> Option<String> {
    let rest = task_id.strip_prefix("schedule:")?;
    let (id, epoch) = rest.rsplit_once(':')?;
    if id.is_empty() || epoch.is_empty() || !epoch.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    Some(id.to_string())
}

#[derive(Debug, Clone, Default)]
pub struct LogQuery {
    pub from: Option<i64>,
    pub to: Option<i64>,
    pub project: Option<String>,
    pub source: Option<String>,
    pub schedule_id: Option<String>,
    pub phase: Option<String>,
    pub q: Option<String>,
    pub limit: Option<usize>,
    pub offset: Option<usize>,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LogPage {
    pub logs: Vec<TaskLog>,
    pub total: usize,
}

/// Pure predicate, kept separate so parity fixtures can exercise it.
pub fn matches_log_query(log: &TaskLog, q: &LogQuery) -> bool {
    if let Some(project) = q.project.as_deref() {
        let matches = if log.projects.is_empty() {
            log.project.as_deref() == Some(project)
        } else {
            log.projects.iter().any(|p| p == project)
        };
        if !matches {
            return false;
        }
    }
    if let Some(source) = q.source.as_deref() {
        if log.source != source {
            return false;
        }
    }
    if let Some(phase) = q.phase.as_deref() {
        if log.phase != phase {
            return false;
        }
    }
    if let Some(schedule_id) = q.schedule_id.as_deref() {
        if schedule_id_from_task_id(&log.task_id).as_deref() != Some(schedule_id) {
            return false;
        }
    }
    if let Some(q) = q.q.as_deref() {
        let needle = q.to_lowercase();
        let mut project_text = log.project.clone().unwrap_or_default();
        for p in &log.projects {
            project_text.push(' ');
            project_text.push_str(p);
        }
        if !log.user_message.to_lowercase().contains(&needle)
            && !log.reply_text.to_lowercase().contains(&needle)
            && !project_text.to_lowercase().contains(&needle)
        {
            return false;
        }
    }
    true
}

/// Pure filter/sort/page helper (v1 `filterAndPageLogs`).
pub fn filter_and_page_logs(mut all: Vec<TaskLog>, q: &LogQuery) -> LogPage {
    all.retain(|l| matches_log_query(l, q));
    all.sort_by_key(|l| std::cmp::Reverse(l.started_at));
    let total = all.len();
    let offset = q.offset.unwrap_or(0);
    let limit = q.limit.unwrap_or(50);
    let logs = all.into_iter().skip(offset).take(limit).collect();
    LogPage { logs, total }
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenSummary {
    pub input: u64,
    pub output: u64,
    pub cache_read: u64,
    pub total: u64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TaskSummary {
    pub total_tasks: usize,
    pub by_status: BTreeMap<String, usize>,
    pub by_project: BTreeMap<String, usize>,
    pub by_source: BTreeMap<String, usize>,
    pub token: TokenSummary,
    pub model_cache: ModelCacheStats,
    pub avg_duration_ms: i64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LogFileStat {
    pub day: String,
    pub file: String,
    pub bytes: u64,
    pub entries: usize,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DiskUsage {
    pub dir: String,
    pub total_bytes: u64,
    pub file_count: usize,
    pub entry_count: usize,
    pub oldest_day: Option<String>,
    pub newest_day: Option<String>,
    pub files: Vec<LogFileStat>,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CleanupOptions {
    pub before: Option<String>,
    pub days: Option<Vec<String>>,
    pub dry_run: Option<bool>,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CleanupResult {
    pub deleted: Vec<String>,
    pub freed_bytes: u64,
    pub remaining_bytes: u64,
}

#[derive(Debug, Clone, Default)]
pub struct TokenTimeQuery {
    pub from: Option<i64>,
    pub to: Option<i64>,
    pub granularity: Option<String>,
    pub group_by: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenBucket {
    pub start: i64,
    pub end: i64,
    pub tasks: u64,
    pub input: u64,
    pub output: u64,
    pub cache_read: u64,
    pub cache_write: u64,
    pub total: u64,
    pub cost_usd: f64,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenGroup {
    pub key: String,
    pub buckets: Vec<TokenBucket>,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TokenTimeSeries {
    pub granularity: String,
    /// Server-local timezone name (display only; buckets use local time).
    pub tz: String,
    pub from: i64,
    pub to: i64,
    pub groups: Vec<TokenGroup>,
}

#[derive(Debug, Clone)]
pub struct TaskLogStore {
    dir: PathBuf,
}

impl TaskLogStore {
    pub fn new(dir: impl Into<PathBuf>) -> Self {
        Self { dir: dir.into() }
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    /// Append one record. Best-effort: callers log and continue on error.
    pub fn append(&self, log: &TaskLog) -> Result<()> {
        let path = self.dir.join(day_file_name(log.started_at));
        append_jsonl_fsync(&path, &serde_json::to_value(log)?)
    }

    fn day_files(&self, from: Option<i64>, to: Option<i64>) -> Vec<String> {
        let Ok(entries) = fs::read_dir(&self.dir) else {
            return Vec::new();
        };
        let mut files: Vec<String> = entries
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().to_string())
            .filter(|name| is_day_file(name))
            .collect();
        if let Some(from) = from {
            let lo = day_file_name(from);
            files.retain(|f| f.as_str() >= lo.as_str());
        }
        if let Some(to) = to {
            let hi = day_file_name(to);
            files.retain(|f| f.as_str() <= hi.as_str());
        }
        files.sort();
        files.reverse();
        files
    }

    fn load_day_file(&self, name: &str) -> Vec<TaskLog> {
        let Ok(raw) = fs::read_to_string(self.dir.join(name)) else {
            return Vec::new();
        };
        raw.lines()
            .map(str::trim)
            .filter(|line| !line.is_empty())
            .filter_map(|line| serde_json::from_str::<TaskLog>(line).ok())
            .collect()
    }

    fn all_logs(&self, q: &LogQuery) -> Vec<TaskLog> {
        let mut out = Vec::new();
        for name in self.day_files(q.from, q.to) {
            out.extend(self.load_day_file(&name));
        }
        out
    }

    pub fn query(&self, q: &LogQuery) -> LogPage {
        filter_and_page_logs(self.all_logs(q), q)
    }

    /// Single-pass visitor for aggregations; does not retain all records.
    pub fn scan_logs<F: FnMut(&TaskLog)>(&self, q: &LogQuery, mut visit: F) {
        for name in self.day_files(q.from, q.to) {
            for log in self.load_day_file(&name) {
                if matches_log_query(&log, q) {
                    visit(&log);
                }
            }
        }
    }

    pub fn get(&self, id: &str) -> Option<TaskLog> {
        for name in self.day_files(None, None) {
            if let Some(log) = self.load_day_file(&name).into_iter().find(|l| l.id == id) {
                return Some(log);
            }
        }
        None
    }

    pub fn summary(&self) -> TaskSummary {
        let all = self.all_logs(&LogQuery::default());
        let mut by_status = BTreeMap::new();
        let mut by_project = BTreeMap::new();
        let mut by_source = BTreeMap::new();
        let mut token = TokenSummary::default();
        let mut duration_sum: i64 = 0;
        for log in &all {
            *by_status.entry(log.status.clone()).or_insert(0) += 1;
            let names: Vec<String> = if log.projects.is_empty() {
                vec![log.project.clone().unwrap_or_else(|| "(none)".into())]
            } else {
                log.projects.clone()
            };
            for name in names {
                *by_project.entry(name).or_insert(0) += 1;
            }
            *by_source.entry(log.source.clone()).or_insert(0) += 1;
            token.input += log.usage.input;
            token.output += log.usage.output;
            token.cache_read += log.usage.cache_read;
            token.total += log.usage.total;
            duration_sum += log.duration_ms;
        }
        let model_cache = all
            .last()
            .map(|l| l.model_cache.clone())
            .unwrap_or_default();
        TaskSummary {
            total_tasks: all.len(),
            by_status,
            by_project,
            by_source,
            token,
            model_cache,
            avg_duration_ms: if all.is_empty() {
                0
            } else {
                duration_sum / all.len() as i64
            },
        }
    }

    pub fn disk_usage(&self) -> DiskUsage {
        let mut names: Vec<String> = fs::read_dir(&self.dir)
            .map(|entries| {
                entries
                    .filter_map(|e| e.ok())
                    .map(|e| e.file_name().to_string_lossy().to_string())
                    .filter(|n| is_day_file(n))
                    .collect()
            })
            .unwrap_or_default();
        names.sort();
        let mut total_bytes = 0;
        let mut entry_count = 0;
        let mut files = Vec::new();
        for name in names {
            let path = self.dir.join(&name);
            let Ok(meta) = fs::metadata(&path) else {
                continue;
            };
            let bytes = meta.len();
            let entries = fs::read_to_string(&path)
                .map(|raw| raw.lines().filter(|l| !l.trim().is_empty()).count())
                .unwrap_or(0);
            total_bytes += bytes;
            entry_count += entries;
            files.push(LogFileStat {
                day: name.trim_end_matches(".jsonl").to_string(),
                file: name,
                bytes,
                entries,
            });
        }
        DiskUsage {
            dir: self.dir.display().to_string(),
            total_bytes,
            file_count: files.len(),
            entry_count,
            oldest_day: files.first().map(|f| f.day.clone()),
            newest_day: files.last().map(|f| f.day.clone()),
            files,
        }
    }

    /// Delete day-files inside `dir` only. Names are strictly validated and
    /// taken from `disk_usage`, so no traversal or off-directory deletion.
    pub fn cleanup(&self, opts: &CleanupOptions) -> CleanupResult {
        let usage = self.disk_usage();
        let mut candidates = BTreeSet::new();
        if let Some(before) = opts.before.as_deref() {
            if !validate_day(before) {
                return CleanupResult {
                    remaining_bytes: usage.total_bytes,
                    ..CleanupResult::default()
                };
            }
            for file in &usage.files {
                if file.day.as_str() < before {
                    candidates.insert(file.file.clone());
                }
            }
        }
        if let Some(days) = opts.days.as_deref() {
            for day in days {
                if !validate_day(day) {
                    continue;
                }
                let name = format!("{day}.jsonl");
                if usage.files.iter().any(|f| f.file == name) {
                    candidates.insert(name);
                }
            }
        }
        let dry_run = opts.dry_run.unwrap_or(false);
        let mut deleted = Vec::new();
        let mut freed_bytes = 0;
        for name in candidates {
            let Some(stat) = usage.files.iter().find(|f| f.file == name) else {
                continue;
            };
            if !dry_run && fs::remove_file(self.dir.join(&name)).is_err() {
                tracing::warn!("[task-log] cleanup failed for {name}");
                continue;
            }
            deleted.push(name);
            freed_bytes += stat.bytes;
        }
        CleanupResult {
            deleted,
            freed_bytes,
            remaining_bytes: if dry_run {
                usage.total_bytes
            } else {
                usage.total_bytes.saturating_sub(freed_bytes)
            },
        }
    }

    /// Aggregate usage into continuous local-time buckets. `now_ms` is injected
    /// so the no-data case is deterministic in tests.
    pub fn token_time_series(&self, q: &TokenTimeQuery, now_ms: i64) -> TokenTimeSeries {
        let granularity = q.granularity.as_deref().unwrap_or("day");
        let granularity = match granularity {
            "hour" | "week" => granularity,
            _ => "day",
        };
        let scan = LogQuery {
            from: q.from,
            to: q.to,
            ..LogQuery::default()
        };
        let logs = self.all_logs(&scan);
        let lo = q
            .from
            .unwrap_or_else(|| logs.iter().map(|l| l.started_at).min().unwrap_or(now_ms));
        let hi =
            q.to.unwrap_or_else(|| logs.iter().map(|l| l.started_at).max().unwrap_or(now_ms));
        let step = step_ms(granularity);
        let first = bucket_start(lo, granularity);
        let mut starts = Vec::new();
        let mut t = first;
        while t <= hi {
            starts.push(t);
            t += step;
        }
        let buckets: Vec<TokenBucket> = starts
            .iter()
            .enumerate()
            .map(|(i, start)| TokenBucket {
                start: *start,
                end: starts.get(i + 1).copied().unwrap_or(*start + step),
                ..TokenBucket::default()
            })
            .collect();

        let mut groups: BTreeMap<String, Vec<TokenBucket>> = BTreeMap::new();
        if q.group_by.is_none() {
            groups
                .entry("(all)".to_string())
                .or_insert_with(|| buckets.clone());
        }
        for log in &logs {
            if log.started_at < lo || log.started_at > hi {
                continue;
            }
            let idx = (log.started_at - first) / step;
            if idx < 0 || idx as usize >= buckets.len() {
                continue;
            }
            let keys: Vec<String> = match q.group_by.as_deref() {
                Some("project") => {
                    if log.projects.is_empty() {
                        vec![log.project.clone().unwrap_or_else(|| "(none)".into())]
                    } else {
                        log.projects.clone()
                    }
                }
                Some("source") => vec![log.source.clone()],
                _ => vec!["(all)".into()],
            };
            for key in keys {
                let bucket =
                    &mut groups.entry(key).or_insert_with(|| buckets.clone())[idx as usize];
                bucket.tasks += 1;
                bucket.input += log.usage.input;
                bucket.output += log.usage.output;
                bucket.cache_read += log.usage.cache_read;
                bucket.cache_write += log.usage.cache_write;
                bucket.total += log.usage.total;
                bucket.cost_usd += log.usage.cost_usd;
            }
        }
        TokenTimeSeries {
            granularity: granularity.to_string(),
            tz: local_tz_name(now_ms),
            from: lo,
            to: hi,
            groups: groups
                .into_iter()
                .map(|(key, buckets)| TokenGroup { key, buckets })
                .collect(),
        }
    }
}

fn step_ms(granularity: &str) -> i64 {
    match granularity {
        "hour" => 3_600_000,
        "week" => 7 * 86_400_000,
        _ => 86_400_000,
    }
}

/// Bucket start truncated to local time; weeks start Monday 00:00.
fn bucket_start(ts_ms: i64, granularity: &str) -> i64 {
    let local = utc_dt(ts_ms).with_timezone(&Local).naive_local();
    let naive = if granularity == "hour" {
        local.with_minute(0).and_then(|v| v.with_second(0))
    } else if granularity == "day" {
        local.with_hour(0)
    } else {
        let dow = local.weekday().num_days_from_monday() as i64;
        (local.date() - ChronoDuration::days(dow)).and_hms_opt(0, 0, 0)
    }
    .and_then(|v| v.with_nanosecond(0))
    .unwrap_or(local);
    epoch_ms_from_local(naive)
}

fn epoch_ms_from_local(naive: chrono::NaiveDateTime) -> i64 {
    use chrono::offset::MappedLocalTime;
    match Local.from_local_datetime(&naive) {
        MappedLocalTime::Single(dt) => dt.timestamp_millis(),
        MappedLocalTime::Ambiguous(earlier, _) => earlier.timestamp_millis(),
        MappedLocalTime::None => naive.and_utc().timestamp_millis(),
    }
}

fn local_tz_name(now_ms: i64) -> String {
    // chrono's `Local` has no stable IANA name accessor; the offset derived from
    // the injected timestamp is enough for the SPA's display-only field.
    utc_dt(now_ms)
        .with_timezone(&Local)
        .format("%z")
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::task_log::record::RecordInput;

    fn log(id: &str, started: i64, status: &str, project: &str) -> TaskLog {
        TaskLog::new(RecordInput {
            id: id.into(),
            task_id: format!("task-{id}"),
            phase: "execute".into(),
            source: "cli".into(),
            provider: "stub".into(),
            model: "stub".into(),
            projects: if project.is_empty() {
                vec![]
            } else {
                vec![project.into()]
            },
            status: status.into(),
            started_ms: started,
            ended_ms: started + 10,
            user_message: format!("message {id}"),
            reply_text: format!("reply {id}"),
            images: vec![],
            inbound_image_count: Some(0),
            mutated: false,
        })
    }

    #[test]
    fn is_day_file_is_strict() {
        assert!(is_day_file("2026-11-06.jsonl"));
        assert!(!is_day_file("2026-11-6.jsonl"));
        assert!(!is_day_file("2026-11-06.json"));
        assert!(!is_day_file("../../.env"));
        assert!(!is_day_file("202a-11-06.jsonl"));
        assert!(!validate_day("../../.env"));
        assert!(validate_day("2026-11-06"));
    }

    #[test]
    fn schedule_id_is_greedy() {
        assert_eq!(
            schedule_id_from_task_id("schedule:morning:1700000000000"),
            Some("morning".into())
        );
        assert_eq!(
            schedule_id_from_task_id("schedule:a:b:1700000000000"),
            Some("a:b".into())
        );
        assert_eq!(schedule_id_from_task_id("task-1"), None);
        assert_eq!(schedule_id_from_task_id("schedule:x:abc"), None);
    }

    #[test]
    fn matches_query_filters_and_pages() {
        let a = log("a", 100, "success", "cook");
        let b = log("b", 200, "error", "ledger");
        let q = LogQuery {
            q: Some("REPLY".into()),
            ..LogQuery::default()
        };
        let page = filter_and_page_logs(vec![a.clone(), b.clone()], &q);
        assert_eq!(page.total, 2);
        assert_eq!(page.logs[0].id, "b");
        let page = filter_and_page_logs(
            vec![a.clone(), b.clone()],
            &LogQuery {
                project: Some("cook".into()),
                ..LogQuery::default()
            },
        );
        assert_eq!(page.total, 1);
        assert_eq!(page.logs[0].id, "a");
        let page = filter_and_page_logs(
            vec![a, b],
            &LogQuery {
                limit: Some(1),
                offset: Some(1),
                ..LogQuery::default()
            },
        );
        assert_eq!(page.total, 2);
        assert_eq!(page.logs.len(), 1);
    }

    #[test]
    fn append_query_summary_and_corrupt_lines() {
        let tmp = tempfile::tempdir().unwrap();
        let store = TaskLogStore::new(tmp.path().join("task-logs"));
        let a = log("a", 1_700_000_000_000, "success", "cook");
        let b = log("b", 1_700_000_000_000, "error", "");
        store.append(&a).unwrap();
        store.append(&b).unwrap();
        let day = day_file_name(1_700_000_000_000);
        assert!(store.dir().join(&day).exists());
        fs::write(store.dir().join(&day), "{not json}\n").unwrap();
        store.append(&a).unwrap();

        let page = store.query(&LogQuery::default());
        assert_eq!(page.total, 1);
        assert_eq!(page.logs[0].id, "a");
        assert_eq!(store.get("a").unwrap().status, "success");
        assert!(store.get("missing").is_none());
        let summary = store.summary();
        assert_eq!(summary.total_tasks, 1);
        assert_eq!(summary.by_status.get("success"), Some(&1));
        assert_eq!(summary.by_project.get("cook"), Some(&1));
        assert_eq!(summary.avg_duration_ms, 10);
    }

    #[test]
    fn cleanup_is_scoped_and_dry_run_safe() {
        let tmp = tempfile::tempdir().unwrap();
        let store = TaskLogStore::new(tmp.path().join("task-logs"));
        let day1 = day_file_name(1_700_000_000_000);
        let day2 = day_file_name(1_700_000_000_000 + 86_400_000);
        fs::create_dir_all(store.dir()).unwrap();
        fs::write(store.dir().join(&day1), "{}\n").unwrap();
        fs::write(store.dir().join(&day2), "{}\n").unwrap();
        fs::write(store.dir().join(".env"), "SECRET=x\n").unwrap();
        let before = day2.trim_end_matches(".jsonl").to_string();

        let dry = store.cleanup(&CleanupOptions {
            before: Some(before.clone()),
            days: None,
            dry_run: Some(true),
        });
        assert_eq!(dry.deleted.len(), 1);
        assert!(store.dir().join(&day1).exists());

        let done = store.cleanup(&CleanupOptions {
            before: Some(before),
            days: None,
            dry_run: Some(false),
        });
        assert_eq!(done.deleted, vec![day1.clone()]);
        assert!(done.freed_bytes > 0);
        assert!(!store.dir().join(&day1).exists());
        assert!(store.dir().join(".env").exists());

        let evil = store.cleanup(&CleanupOptions {
            days: Some(vec![
                "../../.env".into(),
                day2.trim_end_matches(".jsonl").into(),
            ]),
            ..CleanupOptions::default()
        });
        assert_eq!(evil.deleted.len(), 1);
        assert!(store.dir().join(".env").exists());
    }

    #[test]
    fn token_series_has_continuous_buckets_and_groups() {
        let tmp = tempfile::tempdir().unwrap();
        let store = TaskLogStore::new(tmp.path().join("task-logs"));
        let ts = 1_700_000_000_000;
        store.append(&log("a", ts, "success", "cook")).unwrap();
        store
            .append(&log("b", ts + 86_400_000, "success", "ledger"))
            .unwrap();
        let series = store.token_time_series(
            &TokenTimeQuery {
                from: Some(ts),
                to: Some(ts + 86_400_000),
                granularity: Some("day".into()),
                group_by: Some("project".into()),
            },
            ts,
        );
        assert!(series.groups.len() >= 2);
        assert!(series.groups.iter().all(|g| g.buckets.len() >= 2));
        assert!(series.groups.iter().any(|g| g.key == "cook"));
        let empty = store.token_time_series(&TokenTimeQuery::default(), ts);
        assert_eq!(empty.groups.len(), 1);
        assert_eq!(empty.groups[0].key, "(all)");
    }
}
