//! Rolling trend samples for the WebUI monitor view.

use std::collections::VecDeque;

use serde::Serialize;

pub const SAMPLE_INTERVAL_SECS: u64 = 5;
pub const HISTORY_CAPACITY: usize = 720;

#[derive(Debug, Clone, Default, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Sample {
    pub ts: i64,
    pub rss: u64,
    pub heap_used: u64,
    pub queue_depth: u64,
    pub active_tasks: u64,
    pub lag_p50: f64,
    pub lag_p99: f64,
}

#[derive(Debug, Default)]
pub struct History {
    samples: VecDeque<Sample>,
}

impl History {
    pub fn push(&mut self, sample: Sample) {
        if self.samples.len() >= HISTORY_CAPACITY {
            self.samples.pop_front();
        }
        self.samples.push_back(sample);
    }

    pub fn snapshot(&self) -> Vec<Sample> {
        self.samples.iter().cloned().collect()
    }

    pub fn len(&self) -> usize {
        self.samples.len()
    }

    pub fn is_empty(&self) -> bool {
        self.samples.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn evicts_oldest_at_capacity() {
        let mut history = History::default();
        for i in 0..(HISTORY_CAPACITY + 5) {
            history.push(Sample {
                ts: i as i64,
                ..Sample::default()
            });
        }
        let samples = history.snapshot();
        assert_eq!(samples.len(), HISTORY_CAPACITY);
        assert_eq!(samples.first().unwrap().ts, 5);
        assert_eq!(samples.last().unwrap().ts, (HISTORY_CAPACITY + 4) as i64);
        assert!(!history.is_empty());
    }
}
