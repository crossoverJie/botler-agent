//! Shared, read-mostly state backing both HTTP servers.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::config::Config;
use crate::dispatcher::DispatchStats;
use crate::fsutil::now_epoch_ms;
use crate::scheduler::holidays::HolidayCalendar;
use crate::task_log::store::TaskLogStore;

use super::history::{History, Sample, SAMPLE_INTERVAL_SECS};
use super::procinfo::rss_bytes;

pub trait Clock: Send + Sync {
    fn now_ms(&self) -> i64;
}

pub struct SystemClock;

impl Clock for SystemClock {
    fn now_ms(&self) -> i64 {
        now_epoch_ms()
    }
}

pub struct FixedClock(pub i64);

impl Clock for FixedClock {
    fn now_ms(&self) -> i64 {
        self.0
    }
}

pub struct MonitorState {
    pub config: Arc<Config>,
    pub stats: Arc<DispatchStats>,
    pub store: TaskLogStore,
    pub history: Arc<Mutex<History>>,
    pub started_at: i64,
    pub holidays: HolidayCalendar,
    pub clock: Arc<dyn Clock>,
}

static SAMPLER_STARTED: AtomicBool = AtomicBool::new(false);

/// Start the 5-second sampler once, regardless of which servers are enabled
/// (v1 `ensureMonitorSampling`). The WebUI monitor view and the standalone
/// 8899 endpoint share this single sampler.
pub fn spawn_sampler(state: Arc<MonitorState>) {
    if SAMPLER_STARTED.swap(true, Ordering::SeqCst) {
        return;
    }
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(Duration::from_secs(SAMPLE_INTERVAL_SECS));
        loop {
            ticker.tick().await;
            let enqueued = state.stats.enqueued.load(Ordering::SeqCst);
            let started = state.stats.started.load(Ordering::SeqCst);
            let finished = state.stats.finished.load(Ordering::SeqCst);
            let sample = Sample {
                ts: state.clock.now_ms(),
                rss: rss_bytes(),
                heap_used: 0,
                queue_depth: enqueued.saturating_sub(started),
                active_tasks: started.saturating_sub(finished),
                lag_p50: 0.0,
                lag_p99: 0.0,
            };
            if let Ok(mut history) = state.history.lock() {
                history.push(sample);
            }
        }
    });
}
