//! In-process scheduler loop.
//!
//! One loop re-reads `schedules.json` each round, computes the soonest fire
//! from a per-entry watermark (never from `now`; `next_fire_epoch` is strictly
//! after its argument), dispatches the due entries through the same global
//! queue as chat, and sleeps until the next instant or a saved-listener wake.
//! Retries are handed to a timer and return immediately, so one failing entry
//! never starves the rest of the queue (v1 commit 1745c42).

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use tokio::sync::Notify;

use crate::config::Config;
use crate::dispatcher::{
    DispatchOptions, DispatchResult, Dispatcher, ScriptTaskOptions, TaskStatus,
};
use crate::fsutil::now_epoch_ms;
use crate::identity::ChannelSource;
use crate::push::PushService;
use crate::scheduler::cron::next_fire_epoch;
use crate::scheduler::holidays::HolidayCalendar;
use crate::scheduler::store::{load_schedules, set_schedules_saved_listener};
use crate::scheduler::types::ScheduleEntry;

pub const IDLE_POLL_MS: u64 = 60_000;
pub const MIN_SLEEP_MS: u64 = 1_000;
/// Keep just below the 32-bit timeout clamp on JS; tokio handles larger
/// durations, but chunking is correctness-neutral and keeps the loop simple.
pub const MAX_SLEEP_MS: u64 = 2_000_000_000;

pub fn poll_delay(soonest: Option<i64>, now: i64) -> u64 {
    let raw = match soonest {
        None => IDLE_POLL_MS as i64,
        Some(next) => next - now,
    };
    raw.clamp(MIN_SLEEP_MS as i64, MAX_SLEEP_MS as i64) as u64
}

#[async_trait]
pub trait SchedulerDispatch: Send + Sync {
    async fn dispatch_message(&self, message: &str, options: DispatchOptions) -> DispatchResult;
    async fn dispatch_script(&self, options: ScriptTaskOptions) -> DispatchResult;
}

#[async_trait]
impl SchedulerDispatch for Dispatcher {
    async fn dispatch_message(&self, message: &str, options: DispatchOptions) -> DispatchResult {
        self.dispatch(message, options).await
    }

    async fn dispatch_script(&self, options: ScriptTaskOptions) -> DispatchResult {
        self.dispatch_scripted(options).await
    }
}

/// Execution-relevant config; a pending retry is cancelled if the entry's
/// action / trigger / project changes during the backoff.
pub fn retry_signature(entry: &ScheduleEntry) -> String {
    let action = match &entry.script {
        Some(script) => format!(
            "script:{script}|args:{}",
            serde_json::to_string(&entry.script_args.clone().unwrap_or_default())
                .unwrap_or_default()
        ),
        None => format!("message:{}", entry.message.clone().unwrap_or_default()),
    };
    let trigger = entry
        .cron
        .clone()
        .or_else(|| entry.interval.clone())
        .or_else(|| entry.at.clone())
        .or_else(|| entry.once.clone())
        .unwrap_or_default();
    format!(
        "{action}|trigger:{trigger}|project:{}",
        entry.project.clone().unwrap_or_default()
    )
}

fn needs_retry(entry: &ScheduleEntry, result: &DispatchResult) -> bool {
    result.status == TaskStatus::Error
        || (result.status == TaskStatus::ValidationFailed && entry.script.is_none())
}

fn retry_max(entry: &ScheduleEntry) -> u32 {
    entry.retry.as_ref().map(|r| r.max).unwrap_or(0)
}

pub struct SchedulerEngine {
    config: Arc<Config>,
    dispatch: Arc<dyn SchedulerDispatch>,
    push: Arc<PushService>,
    holidays: HolidayCalendar,
    watermarks: Mutex<HashMap<String, i64>>,
    wake: Arc<Notify>,
    started: AtomicBool,
}

impl SchedulerEngine {
    pub fn new(
        config: Arc<Config>,
        dispatch: Arc<dyn SchedulerDispatch>,
        push: Arc<PushService>,
        holidays: HolidayCalendar,
    ) -> Arc<Self> {
        Arc::new(Self {
            config,
            dispatch,
            push,
            holidays,
            watermarks: Mutex::new(HashMap::new()),
            wake: Arc::new(Notify::new()),
            started: AtomicBool::new(false),
        })
    }

    pub fn wake_handle(&self) -> Arc<Notify> {
        Arc::clone(&self.wake)
    }

    /// Register the saved-listener (a schedule write wakes the loop) and start
    /// the holiday refresh plus the firing loop. No-op unless enabled.
    pub async fn start(self: &Arc<Self>) {
        if !self.config.scheduler_enabled || self.started.swap(true, Ordering::SeqCst) {
            return;
        }
        let wake = self.wake_handle();
        set_schedules_saved_listener(Some(Arc::new(move || wake.notify_one())));
        let holidays = self.holidays.clone();
        let wake = self.wake_handle();
        tokio::spawn(async move {
            loop {
                holidays.ensure().await;
                wake.notify_one();
                tokio::time::sleep(Duration::from_secs(24 * 3600)).await;
            }
        });
        tracing::info!(
            "[scheduler] started (config: {})",
            self.config.schedules_file.display()
        );
        Arc::clone(self).run().await;
    }

    async fn run(self: Arc<Self>) {
        loop {
            let delay = self.tick().await;
            tokio::select! {
                _ = tokio::time::sleep(Duration::from_millis(delay)) => {}
                _ = self.wake.notified() => {}
            }
        }
    }

    /// One round: fire everything due, then return the sleep delay. Public so
    /// tests can drive the loop deterministically.
    pub async fn tick(self: &Arc<Self>) -> u64 {
        let entries = load_schedules(&self.config);
        let enabled: Vec<ScheduleEntry> = entries.into_iter().filter(|e| e.enabled).collect();
        self.prune(&enabled);
        let mut soonest: Option<i64> = None;
        let now = now_epoch_ms();
        for entry in &enabled {
            let watermark = self.watermark_of(&entry.id, now);
            match next_fire_epoch(entry, watermark, &self.holidays) {
                Ok(Some(next)) => {
                    if next <= now {
                        // Advance before firing: dispatch is awaited and can
                        // take a while; the next fire measures from here.
                        self.set_watermark(&entry.id, now);
                        self.fire(entry.clone(), 0).await;
                        if let Ok(Some(next)) =
                            next_fire_epoch(entry, now_epoch_ms(), &self.holidays)
                        {
                            soonest = Some(soonest.map_or(next, |s| s.min(next)));
                        }
                    } else {
                        soonest = Some(soonest.map_or(next, |s| s.min(next)));
                    }
                }
                Ok(None) => {}
                Err(err) => {
                    tracing::error!("[scheduler] entry \"{}\" failed: {err}", entry.id)
                }
            }
        }
        poll_delay(soonest, now_epoch_ms())
    }

    async fn fire(self: &Arc<Self>, entry: ScheduleEntry, attempt: u32) {
        let result = self.execute(&entry).await;
        if needs_retry(&entry, &result) && attempt < retry_max(&entry) {
            // Hand the backoff to a timer so the firing loop keeps serving the
            // rest of the queue (v1 1745c42).
            let backoff = entry.retry.as_ref().map(|r| r.backoff_ms).unwrap_or(0);
            let signature = retry_signature(&entry);
            let engine = Arc::clone(self);
            tokio::spawn(async move {
                tokio::time::sleep(Duration::from_millis(backoff)).await;
                let Some(current) = load_schedules(&engine.config)
                    .into_iter()
                    .find(|candidate| candidate.id == entry.id)
                else {
                    return;
                };
                if !current.enabled {
                    return;
                }
                if retry_signature(&current) != signature {
                    tracing::info!(
                        "[scheduler] {} retry cancelled: entry config changed during backoff",
                        current.id
                    );
                    return;
                }
                engine.retry_task(current, attempt + 1).await;
            });
            return;
        }
        self.finish(&entry, result).await;
    }

    /// Retry path: a plain loop with no re-entry into `fire`, so the block
    /// spawned from `fire` never awaits itself.
    async fn retry_task(self: Arc<Self>, mut entry: ScheduleEntry, mut attempt: u32) {
        loop {
            let result = self.execute(&entry).await;
            if needs_retry(&entry, &result) && attempt < retry_max(&entry) {
                let backoff = entry.retry.as_ref().map(|r| r.backoff_ms).unwrap_or(0);
                let signature = retry_signature(&entry);
                tokio::time::sleep(Duration::from_millis(backoff)).await;
                let Some(current) = load_schedules(&self.config)
                    .into_iter()
                    .find(|candidate| candidate.id == entry.id)
                else {
                    return;
                };
                if !current.enabled || retry_signature(&current) != signature {
                    tracing::info!(
                        "[scheduler] {} retry cancelled: entry config changed during backoff",
                        current.id
                    );
                    return;
                }
                entry = current;
                attempt += 1;
                continue;
            }
            self.finish(&entry, result).await;
            return;
        }
    }

    async fn execute(&self, entry: &ScheduleEntry) -> DispatchResult {
        let task_id = format!("schedule:{}:{}", entry.id, now_epoch_ms());
        match &entry.script {
            Some(script) => {
                self.dispatch
                    .dispatch_script(ScriptTaskOptions {
                        id: task_id,
                        script: script.clone(),
                        script_args: entry.script_args.clone().unwrap_or_default(),
                        commit: entry.commit,
                        source: ChannelSource::Scheduler,
                    })
                    .await
            }
            None => {
                let message = entry.message.clone().unwrap_or_default();
                self.dispatch
                    .dispatch_message(
                        &message,
                        DispatchOptions {
                            id: Some(task_id),
                            source: ChannelSource::Scheduler,
                            project_hint: entry.project.clone(),
                            projects_hint: None,
                            recipient: entry.recipient.clone(),
                            inbound_images: Vec::new(),
                            conversation: None,
                            origin: entry.origin.clone(),
                        },
                    )
                    .await
            }
        }
    }

    async fn finish(&self, entry: &ScheduleEntry, result: DispatchResult) {
        let text = if result.status == TaskStatus::UnknownProject {
            entry.message.clone().unwrap_or(result.text)
        } else {
            result.text
        };
        if let Some(recipient) = &entry.recipient {
            let payload = crate::push::PushPayload {
                text,
                images: result.images,
            };
            let outcome = self.push.deliver(&payload, recipient).await;
            if !outcome.ok {
                tracing::error!(
                    "[scheduler] {} push failed: {}",
                    entry.id,
                    outcome.error.unwrap_or_else(|| "unknown".into())
                );
            }
        }
        tracing::info!("[scheduler] {} -> {}", entry.id, result.status.as_str());
    }

    fn watermark_of(&self, id: &str, now: i64) -> i64 {
        let mut marks = self.watermarks.lock().unwrap();
        *marks.entry(id.to_string()).or_insert(now)
    }

    fn set_watermark(&self, id: &str, value: i64) {
        if let Ok(mut marks) = self.watermarks.lock() {
            marks.insert(id.to_string(), value);
        }
    }

    fn prune(&self, entries: &[ScheduleEntry]) {
        let live: std::collections::HashSet<&str> =
            entries.iter().map(|entry| entry.id.as_str()).collect();
        if let Ok(mut marks) = self.watermarks.lock() {
            marks.retain(|id, _| live.contains(id.as_str()));
        }
    }

    /// Test hook: seed a watermark so a cron entry is immediately due.
    pub fn seed_watermark(&self, id: &str, epoch_ms: i64) {
        self.set_watermark(id, epoch_ms);
    }
}

/// Holiday refresh can be triggered independently of the firing loop.
pub async fn refresh_holidays(calendar: &HolidayCalendar) {
    calendar.ensure().await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::scheduler::types::{ScheduleRetry, SilentHours};

    #[test]
    fn poll_delay_is_bounded() {
        assert_eq!(poll_delay(None, 0), IDLE_POLL_MS);
        assert_eq!(poll_delay(Some(500), 0), MIN_SLEEP_MS);
        assert_eq!(poll_delay(Some(i64::MAX), 0), MAX_SLEEP_MS);
        assert_eq!(poll_delay(Some(5_000), 0), 5_000);
    }

    #[test]
    fn retry_signature_covers_action_trigger_project_only() {
        let base = ScheduleEntry {
            id: "a".into(),
            enabled: true,
            cron: Some("0 8 * * *".into()),
            interval: None,
            at: None,
            once: None,
            timezone: "Asia/Shanghai".into(),
            message: Some("hi".into()),
            script: None,
            script_args: None,
            commit: None,
            project: Some("notes".into()),
            retry: Some(ScheduleRetry {
                max: 2,
                backoff_ms: 10,
            }),
            silent_hours: Some(SilentHours {
                from: "22:00".into(),
                to: "08:00".into(),
            }),
            holiday_mode: None,
            recipient: None,
            origin: None,
        };
        let mut edited = base.clone();
        edited.retry = Some(ScheduleRetry {
            max: 9,
            backoff_ms: 999,
        });
        edited.silent_hours = None;
        assert_eq!(retry_signature(&base), retry_signature(&edited));
        edited.message = Some("different".into());
        assert_ne!(retry_signature(&base), retry_signature(&edited));
    }
}
