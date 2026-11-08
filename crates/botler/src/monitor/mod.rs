//! Process health snapshot, metrics, and the shared 5-second sampler.
//!
//! Ported from v1's `src/monitor/`. Gated behind `webui` because nothing in
//! the deterministic core needs it.

pub mod history;
pub mod procinfo;
pub mod snapshot;
pub mod state;

pub use history::{History, Sample, HISTORY_CAPACITY, SAMPLE_INTERVAL_SECS};
pub use snapshot::{build_metrics, build_snapshot, HealthSnapshot};
pub use state::{spawn_sampler, Clock, FixedClock, MonitorState, SystemClock};
