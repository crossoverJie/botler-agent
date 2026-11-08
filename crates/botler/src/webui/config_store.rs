//! Config-file read / atomic write / backup / restore for the three files the
//! WebUI may edit. All writes snapshot the previous version into `.backups/`
//! (mode 0700 on Unix) before an atomic replace. Restore is strictly gated:
//! allowlisted file name, strict backup name, canonical parent check, and
//! symlink rejection. No user-supplied path is used as-is.

use std::fs;
use std::path::PathBuf;

use anyhow::{bail, Context, Result};
use chrono::{DateTime, Local, NaiveDateTime, TimeZone, Utc};
use serde::Serialize;

use crate::config::Config;
use crate::fsutil::{atomic_write, ensure_dir};
use crate::scheduler::store::save_schedules;

pub const CONFIG_FILE_NAMES: [&str; 3] = [".env", "system-prompt.md", "schedules.json"];
const MAX_BACKUPS: usize = 10;
const BACKUP_PREFIXES: [&str; 3] = [".env.", "system-prompt.md.", "schedules.json."];

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct BackupEntry {
    pub name: String,
    pub file: String,
    pub ts: i64,
    pub bytes: u64,
}

fn config_file_path(config: &Config, name: &str) -> PathBuf {
    if name == "schedules.json" {
        config.schedules_file.clone()
    } else {
        config.user_config_dir.join(name)
    }
}

fn backup_dir(config: &Config) -> PathBuf {
    config.user_config_dir.join(".backups")
}

/// Strict `<name>.YYYYMMDDTHHMMSS` backup name.
pub fn is_config_backup(file: &str) -> bool {
    for prefix in BACKUP_PREFIXES {
        if let Some(rest) = file.strip_prefix(prefix) {
            let b = rest.as_bytes();
            if b.len() == 15
                && b[8] == b'T'
                && b[..8].iter().all(u8::is_ascii_digit)
                && b[9..].iter().all(u8::is_ascii_digit)
            {
                return true;
            }
        }
    }
    false
}

/// `schedules.json.<epoch_ms>` backups produced by the scheduler store.
fn scheduler_backup_epoch(file: &str) -> Option<i64> {
    let rest = file.strip_prefix("schedules.json.")?;
    if rest.is_empty() || !rest.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    rest.parse().ok()
}

fn backup_name_of(file: &str) -> Option<&'static str> {
    if file.starts_with("system-prompt.md") {
        Some("system-prompt.md")
    } else if file.starts_with("schedules.json") {
        Some("schedules.json")
    } else if file.starts_with(".env") {
        Some(".env")
    } else {
        None
    }
}

pub fn read_config_file(config: &Config, name: &str) -> String {
    fs::read_to_string(config_file_path(config, name)).unwrap_or_default()
}

fn utc_dt(ts_ms: i64) -> DateTime<Utc> {
    DateTime::<Utc>::from_timestamp_millis(ts_ms)
        .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).expect("epoch"))
}

fn backup_timestamp(now_ms: i64) -> String {
    utc_dt(now_ms)
        .with_timezone(&Local)
        .format("%Y%m%dT%H%M%S")
        .to_string()
}

fn parse_backup_ts(stamp: &str) -> i64 {
    use chrono::offset::MappedLocalTime;
    let Ok(naive) = NaiveDateTime::parse_from_str(stamp, "%Y%m%dT%H%M%S") else {
        return 0;
    };
    match Local.from_local_datetime(&naive) {
        MappedLocalTime::Single(dt) => dt.timestamp_millis(),
        MappedLocalTime::Ambiguous(earlier, _) => earlier.timestamp_millis(),
        MappedLocalTime::None => naive.and_utc().timestamp_millis(),
    }
}

fn prune_backups(config: &Config, name: &str) -> Result<()> {
    let dir = backup_dir(config);
    let Ok(entries) = fs::read_dir(&dir) else {
        return Ok(());
    };
    let prefix = format!("{name}.");
    let mut names: Vec<String> = entries
        .flatten()
        .map(|e| e.file_name().to_string_lossy().to_string())
        .filter(|f| f.starts_with(&prefix) && is_config_backup(f))
        .collect();
    names.sort();
    while names.len() > MAX_BACKUPS {
        let old = names.remove(0);
        let _ = fs::remove_file(dir.join(old));
    }
    Ok(())
}

pub fn backup_config_file(config: &Config, name: &str, now_ms: i64) -> Result<()> {
    let src = config_file_path(config, name);
    if !src.exists() {
        return Ok(());
    }
    let dir = backup_dir(config);
    ensure_dir(&dir)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(&dir, fs::Permissions::from_mode(0o700));
    }
    let file = format!("{name}.{}", backup_timestamp(now_ms));
    fs::copy(&src, dir.join(&file)).with_context(|| format!("backup {name}"))?;
    prune_backups(config, name)
}

pub fn write_config_file(config: &Config, name: &str, content: &str, now_ms: i64) -> Result<()> {
    backup_config_file(config, name, now_ms)?;
    atomic_write(&config_file_path(config, name), content.as_bytes())
}

pub fn list_backups(config: &Config) -> Vec<BackupEntry> {
    let dir = backup_dir(config);
    let Ok(entries) = fs::read_dir(&dir) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for entry in entries.flatten() {
        let file = entry.file_name().to_string_lossy().to_string();
        let Some(name) = backup_name_of(&file) else {
            continue;
        };
        let (ts, valid) = if is_config_backup(&file) {
            let stamp = file.rsplit_once('.').map(|(_, s)| s).unwrap_or("");
            (parse_backup_ts(stamp), true)
        } else if let Some(epoch) = scheduler_backup_epoch(&file) {
            (epoch, true)
        } else {
            (0, false)
        };
        if !valid {
            continue;
        }
        let bytes = entry.metadata().map(|m| m.len()).unwrap_or(0);
        out.push(BackupEntry {
            name: name.to_string(),
            file,
            ts,
            bytes,
        });
    }
    out.sort_by_key(|entry| std::cmp::Reverse(entry.ts));
    out
}

/// Restore a backup over the live config file. `now_ms` is injected so the
/// pre-restore snapshot is deterministic in tests.
pub fn restore_backup(config: &Config, name: &str, file: &str, now_ms: i64) -> Result<()> {
    if !CONFIG_FILE_NAMES.contains(&name) {
        bail!("invalid config file name: {name}");
    }
    let config_backup = is_config_backup(file);
    let scheduler_backup = scheduler_backup_epoch(file).is_some();
    if !config_backup && !scheduler_backup {
        bail!("invalid backup file name: {file}");
    }
    if let Some(expected) = backup_name_of(file) {
        if expected != name {
            bail!("backup file does not match config file name");
        }
    }
    let dir = backup_dir(config);
    let src = dir.join(file);
    // Reject dangling or escaping symlinks before resolving anything.
    let meta = fs::symlink_metadata(&src).context("backup not found")?;
    if meta.file_type().is_symlink() {
        bail!("symlinked backup rejected");
    }
    let resolved = fs::canonicalize(&src).context("backup not found")?;
    let resolved_dir = fs::canonicalize(&dir).unwrap_or_else(|_| dir.clone());
    if resolved.parent() != Some(resolved_dir.as_path()) {
        bail!("backup path traversal rejected");
    }
    if !resolved.is_file() {
        bail!("backup not found");
    }

    // Snapshot the live file first, so the restore itself is reversible.
    backup_config_file(config, name, now_ms)?;
    if name == "schedules.json" {
        let raw = fs::read_to_string(&resolved).context("read backup")?;
        let parsed: serde_json::Value = serde_json::from_str(&raw).context("parse backup")?;
        let items = parsed
            .get("schedules")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        // Re-normalizes, backs up again, and wakes the firing loop.
        save_schedules(config, &items)?;
    } else {
        let bytes = fs::read(&resolved).context("read backup")?;
        atomic_write(&config_file_path(config, name), &bytes)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cfg(tmp: &std::path::Path) -> Config {
        Config::with_paths(tmp.join("data"), tmp.join("cfg"))
    }

    #[test]
    fn backup_names_are_strict() {
        assert!(is_config_backup("system-prompt.md.20261106T101530"));
        assert!(is_config_backup(".env.20261106T101530"));
        assert!(is_config_backup("schedules.json.20261106T101530"));
        assert!(!is_config_backup("../../.env"));
        assert!(!is_config_backup("system-prompt.md.20261106"));
        assert!(!is_config_backup("system-prompt.md.20261106T10153a"));
        assert!(scheduler_backup_epoch("schedules.json.1700000000000").is_some());
        assert!(scheduler_backup_epoch("schedules.json.abc").is_none());
    }

    #[test]
    fn write_creates_atomic_file_and_backup() {
        let tmp = tempfile::tempdir().unwrap();
        let config = cfg(tmp.path());
        write_config_file(&config, "system-prompt.md", "first", 1_700_000_000_000).unwrap();
        assert_eq!(read_config_file(&config, "system-prompt.md"), "first");
        assert!(list_backups(&config).is_empty()); // no prior file to back up

        write_config_file(&config, "system-prompt.md", "second", 1_700_000_060_000).unwrap();
        assert_eq!(read_config_file(&config, "system-prompt.md"), "second");
        let backups = list_backups(&config);
        assert_eq!(backups.len(), 1);
        assert_eq!(backups[0].name, "system-prompt.md");
        assert!(backups[0].bytes > 0);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(backup_dir(&config))
                .unwrap()
                .permissions()
                .mode();
            assert_eq!(mode & 0o777, 0o700);
        }
    }

    #[test]
    fn restore_rejects_bad_names_and_traversal() {
        let tmp = tempfile::tempdir().unwrap();
        let config = cfg(tmp.path());
        assert!(restore_backup(&config, "bad.txt", "x", 0).is_err());
        assert!(restore_backup(&config, ".env", "../../.env", 0).is_err());
        let file = "system-prompt.md.20261106T101530";
        fs::create_dir_all(backup_dir(&config)).unwrap();
        fs::write(backup_dir(&config).join(file), "restored").unwrap();
        restore_backup(&config, "system-prompt.md", file, 1_700_000_000_000).unwrap();
        assert_eq!(read_config_file(&config, "system-prompt.md"), "restored");
    }

    #[test]
    fn restore_rejects_symlinked_backup() {
        let tmp = tempfile::tempdir().unwrap();
        let config = cfg(tmp.path());
        let dir = backup_dir(&config);
        fs::create_dir_all(&dir).unwrap();
        let outside = tmp.path().join("outside.txt");
        fs::write(&outside, "secret").unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::symlink;
            let file = "system-prompt.md.20261106T101530";
            symlink(&outside, dir.join(file)).unwrap();
            assert!(restore_backup(&config, "system-prompt.md", file, 0).is_err());
            assert_eq!(read_config_file(&config, "system-prompt.md"), "");
        }
    }
}
