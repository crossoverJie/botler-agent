//! Key -> opaque session file index (§4.8).
//!
//! File names are derived from a digest of the canonical key tuple, never from
//! raw channel identifiers. The index stores the key tuple *as data* beside the
//! opaque name, and validates every entry on load: a recomputed digest that
//! disagrees with the stored name quarantines the entry instead of following it.

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

use crate::fsutil::{atomic_write_json, ensure_dir, now_epoch_ms};
use crate::identity::SessionKey;
use crate::session_registry::SessionMeta;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SessionIndexEntry {
    pub key: SessionKey,
    /// Path relative to the sessions dir: `s-<hash>.jsonl` or
    /// `archived/s-<hash>-<epoch>.jsonl`.
    pub location: String,
    #[serde(default)]
    pub archived: bool,
    /// Fingerprint of the session's static inputs at creation/resume. `None`
    /// only for entries written before this field existed; such an entry can
    /// never be proven fresh, so the registry rotates it.
    #[serde(default)]
    pub meta: Option<SessionMeta>,
    pub created_at: i64,
    pub updated_at: i64,
}

#[derive(Debug, Serialize, Deserialize)]
struct IndexFile {
    version: u32,
    sessions: Vec<SessionIndexEntry>,
}

#[derive(Debug)]
pub struct SessionIndex {
    path: PathBuf,
    entries: HashMap<SessionKey, SessionIndexEntry>,
}

impl SessionIndex {
    pub fn open(path: impl AsRef<Path>) -> Self {
        let path = path.as_ref().to_path_buf();
        let mut entries = HashMap::new();
        let mut quarantined = Vec::new();
        if let Ok(raw) = fs::read_to_string(&path) {
            match serde_json::from_str::<IndexFile>(&raw) {
                Ok(file) => {
                    for entry in file.sessions {
                        if validate_entry(&entry) {
                            entries.insert(entry.key.clone(), entry);
                        } else {
                            quarantined.push(entry);
                        }
                    }
                }
                Err(err) => {
                    tracing::warn!("[session-index] unreadable index {}: {err}", path.display());
                }
            }
        }
        if !quarantined.is_empty() {
            tracing::warn!(
                "[session-index] quarantined {} entr{} with an invalid opaque name",
                quarantined.len(),
                if quarantined.len() == 1 { "y" } else { "ies" }
            );
            let audit = path.with_file_name("sessions-index-quarantine.jsonl");
            for entry in quarantined {
                let value = serde_json::to_value(&entry).unwrap_or(serde_json::Value::Null);
                let _ = crate::fsutil::append_jsonl_fsync(&audit, &value);
            }
        }
        Self { path, entries }
    }

    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Live (non-archived) session for a key, if any.
    pub fn live(&self, key: &SessionKey) -> Option<&SessionIndexEntry> {
        self.entries.get(key).filter(|entry| !entry.archived)
    }

    pub fn get(&self, key: &SessionKey) -> Option<&SessionIndexEntry> {
        self.entries.get(key)
    }

    pub fn find_by_hash(&self, key_hash: &str) -> Option<&SessionIndexEntry> {
        self.entries
            .values()
            .find(|entry| entry.key.key_hash() == key_hash)
    }

    pub fn entries(&self) -> impl Iterator<Item = &SessionIndexEntry> {
        self.entries.values()
    }

    /// Register a freshly created live session file and its freshness metadata.
    pub fn insert_live(
        &mut self,
        key: SessionKey,
        file_name: impl Into<String>,
        meta: Option<SessionMeta>,
    ) -> SessionIndexEntry {
        let now = now_epoch_ms();
        let created_at = self.entries.get(&key).map(|e| e.created_at).unwrap_or(now);
        let entry = SessionIndexEntry {
            key: key.clone(),
            location: file_name.into(),
            archived: false,
            meta,
            created_at,
            updated_at: now,
        };
        self.entries.insert(key, entry.clone());
        entry
    }

    /// Record the metadata a cold session was just proven to match.
    pub fn set_meta(&mut self, key: &SessionKey, meta: SessionMeta) {
        let now = now_epoch_ms();
        if let Some(entry) = self.entries.get_mut(key) {
            entry.meta = Some(meta);
            entry.updated_at = now;
        }
    }

    /// Point a key at an archived file. The key is no longer resumable.
    pub fn mark_archived(&mut self, key: &SessionKey, location: impl Into<String>) {
        let now = now_epoch_ms();
        if let Some(entry) = self.entries.get_mut(key) {
            entry.location = location.into();
            entry.archived = true;
            entry.updated_at = now;
        }
    }

    pub fn remove(&mut self, key: &SessionKey) -> Option<SessionIndexEntry> {
        self.entries.remove(key)
    }

    pub fn save(&self) -> Result<()> {
        if let Some(parent) = self.path.parent() {
            ensure_dir(parent)?;
        }
        let file = IndexFile {
            version: 1,
            sessions: self.entries.values().cloned().collect(),
        };
        atomic_write_json(&self.path, &file)
            .with_context(|| format!("save session index {}", self.path.display()))
    }
}

fn validate_entry(entry: &SessionIndexEntry) -> bool {
    let expected = entry.key.opaque_file_name();
    let name = Path::new(&entry.location)
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_default();
    if !entry.archived {
        // A live entry must be exactly the digest-derived name in the sessions root.
        return name == expected && !entry.location.contains('/');
    }
    // Archived names are `<stem>-<epoch>.jsonl`; the stem must still match.
    let stem = expected.trim_end_matches(".jsonl");
    name.starts_with(stem) && name.ends_with(".jsonl") && !entry.location.contains("..")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::identity::ChannelSource;

    fn key() -> SessionKey {
        SessionKey::new(ChannelSource::Telegram, "42", "notes")
    }

    fn meta() -> SessionMeta {
        SessionMeta {
            project: "notes".into(),
            prompt_fingerprint: "fp".into(),
            provider: "p".into(),
            model: "m".into(),
            tool_policy_version: "v".into(),
        }
    }

    #[test]
    fn round_trips_and_resumes_live_entry() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("index.json");
        let mut index = SessionIndex::open(&path);
        let name = key().opaque_file_name();
        index.insert_live(key(), name.clone(), Some(meta()));
        index.save().unwrap();

        let reopened = SessionIndex::open(&path);
        let live = reopened.live(&key()).unwrap();
        assert_eq!(live.location, name);
        assert_eq!(live.meta.as_ref(), Some(&meta()));
    }

    #[test]
    fn quarantines_name_that_does_not_match_digest() {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("index.json");
        let bad = SessionIndexEntry {
            key: key(),
            location: "s-deadbeef.jsonl".into(),
            archived: false,
            meta: None,
            created_at: 0,
            updated_at: 0,
        };
        let file = IndexFile {
            version: 1,
            sessions: vec![bad],
        };
        fs::write(&path, serde_json::to_vec(&file).unwrap()).unwrap();
        let index = SessionIndex::open(&path);
        assert!(index.live(&key()).is_none());
        assert!(tmp.path().join("sessions-index-quarantine.jsonl").exists());
    }

    #[test]
    fn archived_entry_is_not_resumable() {
        let tmp = tempfile::tempdir().unwrap();
        let mut index = SessionIndex::open(tmp.path().join("index.json"));
        index.insert_live(key(), key().opaque_file_name(), Some(meta()));
        let archived = format!(
            "archived/{}",
            key().opaque_file_name().replace(".jsonl", "-1.jsonl")
        );
        index.mark_archived(&key(), archived);
        assert!(index.live(&key()).is_none());
        assert!(index.get(&key()).unwrap().archived);
    }
}
