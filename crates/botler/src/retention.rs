//! Session-data retention (P5).
//!
//! v2 persists raw conversation text under `BOTLER_CONFIG_DIR/sessions` — a
//! long-lived local copy v1 never had. This pass bounds it with a TTL and a
//! total-size cap across live, `archived/`, and `.corrupt` files. It never
//! touches the index, journals, pending rotation intents, or memory stores.

use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::Serialize;

use crate::fsutil::now_epoch_ms;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum SessionFileKind {
    /// A live `s-<hash>.jsonl` at the sessions root.
    Live,
    /// An archived session after rotation or reset.
    Archived,
    /// A corrupt / format-incompatible session moved aside on resume.
    Corrupt,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SessionFileInfo {
    /// Path relative to the sessions directory.
    pub path: String,
    pub kind: SessionFileKind,
    pub bytes: u64,
    pub modified_ms: i64,
}

#[derive(Debug, Clone, Copy)]
pub struct RetentionPolicy {
    /// `0` disables the age rule.
    pub ttl_days: u64,
    /// `0` disables the size rule.
    pub max_bytes: u64,
}

impl RetentionPolicy {
    pub fn new(ttl_days: u64, max_bytes: u64) -> Self {
        Self {
            ttl_days,
            max_bytes,
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct RetentionReport {
    pub removed: Vec<String>,
    pub bytes_freed: u64,
    pub bytes_kept: u64,
}

impl RetentionReport {
    pub fn removed_count(&self) -> usize {
        self.removed.len()
    }
}

const DAY_MS: i64 = 24 * 60 * 60 * 1000;

fn is_session_file(name: &str) -> bool {
    name.ends_with(".jsonl") && (name.starts_with("s-") || name.contains(".corrupt-"))
}

fn modified_ms(metadata: &fs::Metadata) -> i64 {
    metadata
        .modified()
        .ok()
        .and_then(|time| time.duration_since(std::time::UNIX_EPOCH).ok())
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

fn classify(relative: &str) -> Option<SessionFileKind> {
    let path = Path::new(relative);
    let name = path.file_name()?.to_string_lossy();
    if !is_session_file(&name) {
        return None;
    }
    if name.contains(".corrupt-") {
        return Some(SessionFileKind::Corrupt);
    }
    if path.components().count() > 1 {
        Some(SessionFileKind::Archived)
    } else {
        Some(SessionFileKind::Live)
    }
}

fn push_file(root: &Path, path: &Path, kind: SessionFileKind, out: &mut Vec<SessionFileInfo>) {
    let Ok(metadata) = path.metadata() else {
        return;
    };
    if !metadata.is_file() {
        return;
    }
    let relative = path
        .strip_prefix(root)
        .unwrap_or(path)
        .to_string_lossy()
        .replace('\\', "/");
    out.push(SessionFileInfo {
        path: relative,
        kind,
        bytes: metadata.len(),
        modified_ms: modified_ms(&metadata),
    });
}

/// All session files covered by the policy, newest first.
pub fn discover(sessions_dir: &Path) -> Result<Vec<SessionFileInfo>> {
    let mut out = Vec::new();
    let Ok(entries) = fs::read_dir(sessions_dir) else {
        return Ok(out);
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let name = entry.file_name().to_string_lossy().to_string();
        if name == "archived" {
            continue;
        }
        let relative = name.clone();
        if let Some(kind) = classify(&relative) {
            push_file(sessions_dir, &path, kind, &mut out);
        }
    }

    let archived = sessions_dir.join("archived");
    if let Ok(entries) = fs::read_dir(&archived) {
        for entry in entries.flatten() {
            let path = entry.path();
            let name = entry.file_name().to_string_lossy().to_string();
            let relative = format!("archived/{name}");
            if let Some(kind) = classify(&relative) {
                push_file(sessions_dir, &path, kind, &mut out);
            }
        }
    }

    out.sort_by(|a, b| b.modified_ms.cmp(&a.modified_ms));
    Ok(out)
}

fn remove(sessions_dir: &Path, info: &SessionFileInfo, report: &mut RetentionReport) {
    let path = sessions_dir.join(&info.path);
    match fs::remove_file(&path) {
        Ok(()) => {
            report.bytes_freed = report.bytes_freed.saturating_add(info.bytes);
            report.removed.push(info.path.clone());
        }
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
        Err(err) => {
            tracing::warn!("[retention] could not remove {}: {err}", path.display());
        }
    }
}

/// Apply `policy` at `now_ms`. Expired files go first, then the oldest
/// survivors until the total fits the cap. A cap smaller than a single file
/// still removes that file rather than overshooting.
pub fn sweep_at(
    sessions_dir: &Path,
    policy: RetentionPolicy,
    now_ms: i64,
) -> Result<RetentionReport> {
    let mut files = discover(sessions_dir)?;
    let mut report = RetentionReport::default();

    if policy.ttl_days > 0 {
        let cutoff = now_ms.saturating_sub(policy.ttl_days as i64 * DAY_MS);
        let (expired, survivors): (Vec<_>, Vec<_>) = files
            .into_iter()
            .partition(|file| file.modified_ms < cutoff);
        for file in &expired {
            remove(sessions_dir, file, &mut report);
        }
        files = survivors;
    }

    let mut total: u64 = files.iter().map(|file| file.bytes).sum();
    if policy.max_bytes > 0 && total > policy.max_bytes {
        // `files` is newest first; drop from the back.
        while total > policy.max_bytes {
            let Some(oldest) = files.pop() else {
                break;
            };
            total = total.saturating_sub(oldest.bytes);
            remove(sessions_dir, &oldest, &mut report);
        }
    }

    report.bytes_kept = total;
    if report.removed_count() > 0 {
        tracing::info!(
            "[retention] removed {} session file(s), freed {} bytes (kept {} bytes)",
            report.removed_count(),
            report.bytes_freed,
            report.bytes_kept
        );
    }
    Ok(report)
}

pub fn sweep(sessions_dir: &Path, policy: RetentionPolicy) -> Result<RetentionReport> {
    sweep_at(sessions_dir, policy, now_epoch_ms())
}

/// Delete every session file. Used by an explicit `sessions purge --all`.
pub fn purge_all(sessions_dir: &Path) -> Result<RetentionReport> {
    let files = discover(sessions_dir)?;
    let mut report = RetentionReport::default();
    for file in &files {
        remove(sessions_dir, file, &mut report);
    }
    report.bytes_kept = 0;
    Ok(report)
}

/// Resolve a user-supplied path or file name to one discovered session file.
pub fn find<'a>(files: &'a [SessionFileInfo], needle: &str) -> Option<&'a SessionFileInfo> {
    let needle = needle.trim();
    files.iter().find(|file| file.path == needle).or_else(|| {
        files.iter().find(|file| {
            Path::new(&file.path)
                .file_name()
                .map(|name| name.to_string_lossy() == needle)
                .unwrap_or(false)
        })
    })
}

/// Read at most `max_chars` of a session file for inspection.
pub fn inspect(sessions_dir: &Path, file: &SessionFileInfo, max_chars: usize) -> Result<String> {
    let raw = fs::read_to_string(sessions_dir.join(&file.path))
        .with_context(|| format!("read session {}", file.path))?;
    if raw.chars().count() <= max_chars {
        return Ok(raw);
    }
    let prefix: String = raw.chars().take(max_chars).collect();
    Ok(format!("{prefix}\n…（已截断，共 {} 字节）", file.bytes))
}

pub fn describe_kind(kind: SessionFileKind) -> &'static str {
    match kind {
        SessionFileKind::Live => "live",
        SessionFileKind::Archived => "archived",
        SessionFileKind::Corrupt => "corrupt",
    }
}

pub fn sessions_dir_of(config: &crate::config::Config) -> PathBuf {
    config.sessions_dir.clone()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{Duration, SystemTime};

    fn write(root: &Path, relative: &str, bytes: usize, age_days: u64) -> PathBuf {
        let path = root.join(relative);
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(&path, "x".repeat(bytes)).unwrap();
        let when = SystemTime::now() - Duration::from_secs(age_days * 24 * 60 * 60);
        let file = fs::File::options().write(true).open(&path).unwrap();
        file.set_modified(when).unwrap();
        path
    }

    #[test]
    fn discovers_live_archived_and_corrupt_only() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "s-abc.jsonl", 10, 0);
        write(tmp.path(), "archived/s-abc-1.jsonl", 10, 0);
        write(tmp.path(), "archived/deadbeef.corrupt-1.jsonl", 10, 0);
        fs::write(tmp.path().join("index.json"), "{}").unwrap();
        fs::write(tmp.path().join("rotation-journal.jsonl"), "").unwrap();
        fs::write(tmp.path().join("rotate-abc.intent"), "{}").unwrap();

        let files = discover(tmp.path()).unwrap();
        assert_eq!(files.len(), 3);
        assert_eq!(
            files
                .iter()
                .filter(|f| f.kind == SessionFileKind::Live)
                .count(),
            1
        );
        assert_eq!(
            files
                .iter()
                .filter(|f| f.kind == SessionFileKind::Archived)
                .count(),
            1
        );
        assert_eq!(
            files
                .iter()
                .filter(|f| f.kind == SessionFileKind::Corrupt)
                .count(),
            1
        );
    }

    #[test]
    fn ttl_removes_old_files_across_all_kinds() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "s-old.jsonl", 10, 100);
        write(tmp.path(), "archived/s-old-1.jsonl", 10, 100);
        write(tmp.path(), "archived/deadbeef.corrupt-1.jsonl", 10, 100);
        write(tmp.path(), "s-new.jsonl", 10, 1);
        fs::write(tmp.path().join("index.json"), "{}").unwrap();

        let report = sweep(tmp.path(), RetentionPolicy::new(90, 0)).unwrap();
        assert_eq!(report.removed_count(), 3);
        assert!(tmp.path().join("s-new.jsonl").exists());
        assert!(tmp.path().join("index.json").exists());
    }

    #[test]
    fn size_cap_drops_oldest_first() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "s-newest.jsonl", 40, 1);
        write(tmp.path(), "archived/s-mid-1.jsonl", 40, 10);
        write(tmp.path(), "archived/s-old-1.jsonl", 40, 20);

        let report = sweep(tmp.path(), RetentionPolicy::new(0, 80)).unwrap();
        assert_eq!(report.removed, vec!["archived/s-old-1.jsonl"]);
        assert_eq!(report.bytes_kept, 80);
        assert!(tmp.path().join("s-newest.jsonl").exists());
        assert!(tmp.path().join("archived/s-mid-1.jsonl").exists());
    }

    #[test]
    fn zero_policy_keeps_everything() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "s-old.jsonl", 10, 1000);
        let report = sweep(tmp.path(), RetentionPolicy::new(0, 0)).unwrap();
        assert_eq!(report.removed_count(), 0);
        assert_eq!(report.bytes_kept, 10);
    }

    #[test]
    fn purge_all_removes_every_session_file() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "s-a.jsonl", 10, 0);
        write(tmp.path(), "archived/s-b-1.jsonl", 10, 0);
        fs::write(tmp.path().join("index.json"), "{}").unwrap();

        let report = purge_all(tmp.path()).unwrap();
        assert_eq!(report.removed_count(), 2);
        assert!(tmp.path().join("index.json").exists());
    }

    #[test]
    fn find_matches_relative_path_or_file_name() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "archived/s-abc-1.jsonl", 10, 0);
        let files = discover(tmp.path()).unwrap();
        assert!(find(&files, "archived/s-abc-1.jsonl").is_some());
        assert!(find(&files, "s-abc-1.jsonl").is_some());
        assert!(find(&files, "nope.jsonl").is_none());
    }

    #[test]
    fn inspect_truncates() {
        let tmp = tempfile::tempdir().unwrap();
        write(tmp.path(), "s-big.jsonl", 100, 0);
        let files = discover(tmp.path()).unwrap();
        let text = inspect(tmp.path(), &files[0], 10).unwrap();
        assert!(text.starts_with("xxxxxxxxxx"));
        assert!(text.contains("已截断"));
    }
}
