//! Small durability helpers shared by the index, rotation WAL, and stores.

use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};

pub fn ensure_dir(path: &Path) -> Result<()> {
    fs::create_dir_all(path).with_context(|| format!("create dir {}", path.display()))
}

pub fn now_epoch_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

/// fsync a directory so a rename/creation inside it is durable.
pub fn fsync_dir(path: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        let dir = File::open(path).with_context(|| format!("open dir {}", path.display()))?;
        dir.sync_all()
            .with_context(|| format!("fsync dir {}", path.display()))?;
    }
    Ok(())
}

/// Write `bytes` to `path` atomically: temp file in the same directory, fsync,
/// rename, then fsync the parent directory.
pub fn atomic_write(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("path has no parent: {}", path.display()))?;
    ensure_dir(parent)?;
    let tmp = unique_temp_path(path);
    {
        let mut file =
            File::create(&tmp).with_context(|| format!("create temp {}", tmp.display()))?;
        file.write_all(bytes)?;
        file.sync_all()?;
    }
    fs::rename(&tmp, path)
        .with_context(|| format!("rename {} -> {}", tmp.display(), path.display()))?;
    fsync_dir(parent)?;
    Ok(())
}

pub fn atomic_write_json<T: serde::Serialize>(path: &Path, value: &T) -> Result<()> {
    let mut bytes = serde_json::to_vec_pretty(value)?;
    bytes.push(b'\n');
    atomic_write(path, &bytes)
}

fn unique_temp_path(path: &Path) -> PathBuf {
    let pid = std::process::id();
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let name = path.file_name().unwrap_or_default().to_string_lossy();
    path.with_file_name(format!(".{name}.tmp.{pid}.{nanos}"))
}

/// Append one JSON line and fsync the file.
pub fn append_jsonl_fsync(path: &Path, value: &serde_json::Value) -> Result<()> {
    if let Some(parent) = path.parent() {
        ensure_dir(parent)?;
    }
    let mut file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .with_context(|| format!("open journal {}", path.display()))?;
    // One write so a concurrent reader never observes half a JSON line.
    let mut line = serde_json::to_string(value)?;
    line.push('\n');
    file.write_all(line.as_bytes())?;
    file.sync_all()?;
    Ok(())
}

/// Rotate a file name with an epoch suffix, keeping the original extension.
pub fn archived_name(file_name: &str, epoch_ms: i64) -> String {
    let path = Path::new(file_name);
    let stem = path
        .file_stem()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_else(|| file_name.to_string());
    let ext = path
        .extension()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_else(|| "jsonl".into());
    format!("{stem}-{epoch_ms}.{ext}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn atomic_write_replaces_content() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("x.json");
        atomic_write(&path, b"{\"a\":1}").unwrap();
        atomic_write(&path, b"{\"a\":2}").unwrap();
        assert_eq!(fs::read_to_string(&path).unwrap(), "{\"a\":2}");
    }

    #[test]
    fn archived_name_keeps_extension() {
        assert_eq!(archived_name("s-abc.jsonl", 42), "s-abc-42.jsonl");
    }
}
