//! Per-channel known contact addresses for push fallback.
//!
//! Only addresses that have actually contacted the bot are eligible, so a
//! fallback never fires at an unknown address or logs a misleading "channel
//! not configured" line for a channel the user never used.

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};

use crate::fsutil::{atomic_write_json, ensure_dir};
use crate::identity::ChannelSource;

#[derive(Debug, Clone)]
pub struct ContactsStore {
    file: PathBuf,
}

type ContactsMap = HashMap<String, Vec<String>>;

impl ContactsStore {
    pub fn new(file: impl Into<PathBuf>) -> Self {
        Self { file: file.into() }
    }

    pub fn path(&self) -> &Path {
        &self.file
    }

    fn load(&self) -> ContactsMap {
        fs::read_to_string(&self.file)
            .ok()
            .and_then(|raw| serde_json::from_str::<ContactsMap>(&raw).ok())
            .unwrap_or_default()
    }

    fn save(&self, map: &ContactsMap) {
        if let Some(parent) = self.file.parent() {
            let _ = ensure_dir(parent);
        }
        if let Err(err) = atomic_write_json(&self.file, map) {
            tracing::warn!("[push] save contacts failed: {err}");
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(&self.file, fs::Permissions::from_mode(0o600));
        }
    }

    /// Record that `user_id` contacted the bot on `source` (deduped).
    pub fn record(&self, source: ChannelSource, user_id: &str) {
        if user_id.is_empty() {
            return;
        }
        let mut map = self.load();
        let list = map.entry(source.as_str().to_string()).or_default();
        if !list.iter().any(|known| known == user_id) {
            list.push(user_id.to_string());
            self.save(&map);
        }
    }

    pub fn get(&self, source: ChannelSource) -> Vec<String> {
        self.load()
            .get(source.as_str())
            .cloned()
            .unwrap_or_default()
    }

    pub fn remove(&self, source: ChannelSource) {
        let mut map = self.load();
        if map.remove(source.as_str()).is_some() {
            self.save(&map);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn records_dedupes_and_removes() {
        let dir = tempfile::tempdir().unwrap();
        let store = ContactsStore::new(dir.path().join("contacts.json"));
        assert!(store.get(ChannelSource::Wechat).is_empty());
        store.record(ChannelSource::Wechat, "u1");
        store.record(ChannelSource::Wechat, "u1");
        store.record(ChannelSource::Telegram, "42");
        assert_eq!(store.get(ChannelSource::Wechat), vec!["u1"]);
        assert_eq!(store.get(ChannelSource::Telegram), vec!["42"]);
        store.remove(ChannelSource::Wechat);
        assert!(store.get(ChannelSource::Wechat).is_empty());
        assert_eq!(store.get(ChannelSource::Telegram), vec!["42"]);
    }
}
