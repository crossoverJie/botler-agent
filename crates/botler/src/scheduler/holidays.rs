//! China legal-holiday calendar for `holidayMode: "workday"` schedules.
//!
//! Cached at `holidays.json` under the config dir. Fetch failures never crash:
//! the cached calendar is kept and `is_legal_workday` degrades to plain
//! Mon-Fri when a year is absent.

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, RwLock};
use std::time::Duration;

use anyhow::Result;
use chrono::{Datelike, NaiveDate, Weekday};
use serde::{Deserialize, Serialize};

use crate::fsutil::{atomic_write_json, ensure_dir, now_epoch_ms};

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
pub struct YearData {
    #[serde(default)]
    pub holidays: Vec<String>,
    #[serde(default)]
    pub workdays: Vec<String>,
}

pub type HolidayData = HashMap<String, YearData>;

#[derive(Clone)]
pub struct HolidayCalendar {
    data: Arc<RwLock<HolidayData>>,
    file: PathBuf,
    api_url: String,
    refresh_lock: Arc<tokio::sync::Mutex<()>>,
}

impl std::fmt::Debug for HolidayCalendar {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HolidayCalendar")
            .field("file", &self.file)
            .field("years", &self.data.read().map(|d| d.len()).unwrap_or(0))
            .finish()
    }
}

impl HolidayCalendar {
    pub fn from_data(data: HolidayData) -> Self {
        Self {
            data: Arc::new(RwLock::new(data)),
            file: PathBuf::new(),
            api_url: String::new(),
            refresh_lock: Arc::new(tokio::sync::Mutex::new(())),
        }
    }

    pub fn load(file: impl Into<PathBuf>, api_url: impl Into<String>) -> Self {
        let file = file.into();
        let data = read_holidays(&file);
        Self {
            data: Arc::new(RwLock::new(data)),
            file,
            api_url: api_url.into(),
            refresh_lock: Arc::new(tokio::sync::Mutex::new(())),
        }
    }

    pub fn file(&self) -> &Path {
        &self.file
    }

    pub fn snapshot(&self) -> HolidayData {
        self.data.read().map(|d| d.clone()).unwrap_or_default()
    }

    /// Is `date` a China legal workday?
    /// - no data / missing year -> Mon-Fri;
    /// - in `holidays` -> false;
    /// - in `workdays` -> true (makeup workday);
    /// - otherwise Mon-Fri.
    pub fn is_legal_workday(&self, date: NaiveDate) -> bool {
        let Ok(data) = self.data.read() else {
            return is_mon_fri(date);
        };
        let key = date.format("%Y-%m-%d").to_string();
        match data.get(&date.year().to_string()) {
            Some(year) => {
                if year.holidays.iter().any(|d| d == &key) {
                    false
                } else if year.workdays.iter().any(|d| d == &key) {
                    true
                } else {
                    is_mon_fri(date)
                }
            }
            None => is_mon_fri(date),
        }
    }

    /// Fetch current + next year's calendars. Serialized; never panics.
    pub async fn ensure(&self) {
        if self.api_url.is_empty() {
            return;
        }
        let _guard = self.refresh_lock.lock().await;
        let year = chrono::Local::now().year();
        for y in [year, year + 1] {
            let _ = self.refresh_year(y).await;
        }
    }

    pub async fn refresh_year(&self, year: i32) -> bool {
        if self.api_url.is_empty() {
            return false;
        }
        let url = self.api_url.replace("{year}", &year.to_string());
        let client = match reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .build()
        {
            Ok(c) => c,
            Err(_) => return false,
        };
        let Ok(resp) = client.get(&url).send().await else {
            tracing::warn!("[scheduler] holiday refresh for {year} failed: request error");
            return false;
        };
        if !resp.status().is_success() {
            tracing::warn!(
                "[scheduler] holiday refresh for {year} failed: HTTP {}",
                resp.status()
            );
            return false;
        }
        let Ok(json) = resp.json::<HolidayApiResponse>().await else {
            tracing::warn!("[scheduler] holiday refresh for {year}: invalid JSON");
            return false;
        };
        let mut holidays = Vec::new();
        let mut workdays = Vec::new();
        for day in json.days.unwrap_or_default() {
            if day.date.is_empty() {
                continue;
            }
            if day.is_off_day {
                holidays.push(day.date);
            } else {
                workdays.push(day.date);
            }
        }
        let mut data = self.snapshot();
        data.insert(year.to_string(), YearData { holidays, workdays });
        if let Err(err) = write_holidays(&self.file, &data) {
            tracing::warn!("[scheduler] could not save holidays.json: {err}");
            return false;
        }
        if let Ok(mut guard) = self.data.write() {
            *guard = data;
        }
        true
    }
}

fn is_mon_fri(date: NaiveDate) -> bool {
    !matches!(date.weekday(), Weekday::Sat | Weekday::Sun)
}

#[derive(Deserialize)]
struct HolidayApiResponse {
    #[serde(default)]
    days: Option<Vec<HolidayApiDay>>,
}

#[derive(Deserialize)]
struct HolidayApiDay {
    #[serde(default)]
    date: String,
    #[serde(default, rename = "isOffDay")]
    is_off_day: bool,
}

fn read_holidays(file: &Path) -> HolidayData {
    let Ok(raw) = fs::read_to_string(file) else {
        return HolidayData::new();
    };
    let parsed: HolidayData = match serde_json::from_str(&raw) {
        Ok(v) => v,
        Err(err) => {
            tracing::warn!("[scheduler] holidays.json is malformed: {err}");
            return HolidayData::new();
        }
    };
    parsed
}

fn write_holidays(file: &Path, data: &HolidayData) -> Result<()> {
    if let Some(parent) = file.parent() {
        ensure_dir(parent)?;
        // Pre-write backup so a corrupt write is recoverable.
        if file.exists() {
            let backup_dir = parent.join(".backups");
            let _ = ensure_dir(&backup_dir);
            let name = format!(
                "{}.{}",
                file.file_name().unwrap_or_default().to_string_lossy(),
                now_epoch_ms()
            );
            let _ = fs::copy(file, backup_dir.join(name));
            prune_backups(&backup_dir, 10);
        }
    }
    atomic_write_json(file, data)
}

fn prune_backups(dir: &Path, keep: usize) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    let mut names: Vec<_> = entries
        .flatten()
        .filter_map(|e| {
            let name = e.file_name().to_string_lossy().to_string();
            name.starts_with("holidays.json.").then_some(name)
        })
        .collect();
    names.sort();
    while names.len() > keep {
        let old = names.remove(0);
        let _ = fs::remove_file(dir.join(old));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn falls_back_to_weekdays_without_data() {
        let cal = HolidayCalendar::from_data(HolidayData::new());
        assert!(cal.is_legal_workday(NaiveDate::from_ymd_opt(2026, 11, 3).unwrap())); // Tue
        assert!(!cal.is_legal_workday(NaiveDate::from_ymd_opt(2026, 11, 7).unwrap()));
        // Sat
    }

    #[test]
    fn honors_holidays_and_makeup_workdays() {
        let mut data = HolidayData::new();
        data.insert(
            "2026".into(),
            YearData {
                holidays: vec!["2026-11-03".into()],
                workdays: vec!["2026-11-07".into()],
            },
        );
        let cal = HolidayCalendar::from_data(data);
        assert!(!cal.is_legal_workday(NaiveDate::from_ymd_opt(2026, 11, 3).unwrap()));
        assert!(cal.is_legal_workday(NaiveDate::from_ymd_opt(2026, 11, 7).unwrap()));
    }
}
