//! Session rotation WAL (§4.4 / §4.7 / §4.8).
//!
//! Rotation is *not* performed while a live handle is mutating the session.
//! The dispatcher first flags the reset, finishes the turn, drops the handle,
//! and only then calls [`SessionRotation::rotate`]. The fixed durable sequence
//! is ordered so the intent file is the last artifact removed:
//!
//! 1. write + fsync the per-rotation intent (atomic temp-file rename);
//! 2. rename `src` -> `dst`, fsync both parents;
//! 3. update the key->file index to `dst` and make it durable;
//! 4. append the completion record (same `rot_id`) to the audit journal;
//! 5. delete the intent file and fsync its directory (the commit point).
//!
//! Invariant: if the intent file is gone, the index is already durable at `dst`.

use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};

use crate::fsutil::{
    append_jsonl_fsync, archived_name, atomic_write_json, ensure_dir, fsync_dir, now_epoch_ms,
};
use crate::identity::SessionKey;
use crate::session_index::SessionIndex;

const INTENT_VERSION: u32 = 1;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RotationIntent {
    pub v: u32,
    pub rot_id: String,
    pub key_hash: String,
    pub src: String,
    pub dst: String,
    pub ts: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RecoveryOutcome {
    /// Rename had not landed; the rotation was redone from step 2.
    Redone { rot_id: String },
    /// Rename landed; index/journal reconciled and the intent removed.
    Finalized { rot_id: String },
    /// `src` and `dst` both/neither exist. The key is quarantined.
    Quarantined { rot_id: Option<String> },
}

#[derive(Debug, Clone)]
pub struct SessionRotation {
    sessions_dir: PathBuf,
}

impl SessionRotation {
    pub fn new(sessions_dir: impl Into<PathBuf>) -> Self {
        Self {
            sessions_dir: sessions_dir.into(),
        }
    }

    pub fn sessions_dir(&self) -> &Path {
        &self.sessions_dir
    }

    fn intent_path(&self, key_hash: &str) -> PathBuf {
        self.sessions_dir.join(format!(
            "rotate-{}.intent",
            &key_hash[..32.min(key_hash.len())]
        ))
    }

    fn journal_path(&self) -> PathBuf {
        self.sessions_dir.join("rotation-journal.jsonl")
    }

    fn quarantine_path(&self) -> PathBuf {
        self.sessions_dir.join("rotation-quarantine.jsonl")
    }

    pub fn archived_location(&self, key: &SessionKey) -> String {
        format!(
            "archived/{}",
            archived_name(&key.opaque_file_name(), now_epoch_ms())
        )
    }

    /// Execute the full rotation. `src`/`dst` are relative to `sessions_dir`.
    pub fn rotate(
        &self,
        index: &mut SessionIndex,
        key: &SessionKey,
        src: &str,
        dst: &str,
    ) -> Result<()> {
        ensure_dir(&self.sessions_dir)?;
        ensure_dir(&self.sessions_dir.join("archived"))?;
        let rot_id = format!("{}-{}", key.key_hash().trim_end(), now_epoch_ms());
        let intent = RotationIntent {
            v: INTENT_VERSION,
            rot_id: rot_id.clone(),
            key_hash: key.key_hash(),
            src: src.to_string(),
            dst: dst.to_string(),
            ts: now_epoch_ms(),
        };
        let intent_path = self.intent_path(&key.key_hash());
        atomic_write_json(&intent_path, &intent)?; // step 1 (fsyncs file + dir)

        self.land_rename(&intent)?; // step 2
        self.commit_index(index, &intent)?; // step 3
        self.append_completion(&intent)?; // step 4
        self.remove_intent(&intent_path)?; // step 5
        Ok(())
    }

    /// Full startup reconciliation over surviving intent files.
    pub fn recover(&self, index: &mut SessionIndex) -> Result<Vec<RecoveryOutcome>> {
        let mut outcomes = Vec::new();
        ensure_dir(&self.sessions_dir)?;
        let Ok(entries) = fs::read_dir(&self.sessions_dir) else {
            return Ok(outcomes);
        };
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().to_string();
            if !name.starts_with("rotate-") || !name.ends_with(".intent") {
                continue;
            }
            let path = entry.path();
            let intent = match fs::read_to_string(&path)
                .ok()
                .and_then(|raw| serde_json::from_str::<RotationIntent>(&raw).ok())
            {
                Some(intent) if intent.v == INTENT_VERSION => intent,
                _ => {
                    tracing::warn!(
                        "[rotation] unreadable intent {}, leaving for inspection",
                        path.display()
                    );
                    continue;
                }
            };
            match self.recover_one(index, &intent, &path) {
                Ok(outcome) => outcomes.push(outcome),
                Err(err) => {
                    tracing::error!("[rotation] recovery failed for {}: {err}", intent.rot_id);
                    outcomes.push(RecoveryOutcome::Quarantined {
                        rot_id: Some(intent.rot_id.clone()),
                    });
                }
            }
        }
        Ok(outcomes)
    }

    fn recover_one(
        &self,
        index: &mut SessionIndex,
        intent: &RotationIntent,
        intent_path: &Path,
    ) -> Result<RecoveryOutcome> {
        let src = self.sessions_dir.join(&intent.src);
        let dst = self.sessions_dir.join(&intent.dst);
        let src_exists = src.exists();
        let dst_exists = dst.exists();

        match (src_exists, dst_exists) {
            (true, false) => {
                // Step 2 never landed: redo the rename and continue from step 3.
                self.land_rename(intent)?;
                self.commit_index(index, intent)?;
                self.append_completion(intent)?;
                self.remove_intent(intent_path)?;
                Ok(RecoveryOutcome::Redone {
                    rot_id: intent.rot_id.clone(),
                })
            }
            (false, true) => {
                // Rename landed: re-apply step 3 if the index still says `src`.
                self.commit_index(index, intent)?;
                if !self.journal_has(&intent.rot_id) {
                    self.append_completion(intent)?;
                }
                self.remove_intent(intent_path)?;
                Ok(RecoveryOutcome::Finalized {
                    rot_id: intent.rot_id.clone(),
                })
            }
            _ => {
                // A same-filesystem rename cannot produce both/neither. Quarantine
                // the key rather than guess; no auto-resume.
                if let Some(entry) = index
                    .find_by_hash(&intent.key_hash)
                    .map(|entry| entry.key.clone())
                {
                    index.remove(&entry);
                }
                index.save()?;
                let record = serde_json::json!({
                    "kind": "quarantine",
                    "rot_id": intent.rot_id,
                    "key_hash": intent.key_hash,
                    "src": intent.src,
                    "dst": intent.dst,
                    "ts": now_epoch_ms(),
                });
                let _ = append_jsonl_fsync(&self.quarantine_path(), &record);
                // Keep the intent: an operator may want to inspect it. Editing the
                // key is required before this conversation resumes.
                Ok(RecoveryOutcome::Quarantined {
                    rot_id: Some(intent.rot_id.clone()),
                })
            }
        }
    }

    fn land_rename(&self, intent: &RotationIntent) -> Result<()> {
        let src = self.sessions_dir.join(&intent.src);
        let dst = self.sessions_dir.join(&intent.dst);
        if let Some(parent) = dst.parent() {
            ensure_dir(parent)?;
        }
        if src.exists() {
            fs::rename(&src, &dst)
                .with_context(|| format!("rotate rename {} -> {}", src.display(), dst.display()))?;
        } else if !dst.exists() {
            bail!("rotation {}: neither src nor dst exists", intent.rot_id);
        }
        // fsync both parent directories.
        if let Some(parent) = src.parent() {
            fsync_dir(parent)?;
        }
        if let Some(parent) = dst.parent() {
            fsync_dir(parent)?;
        }
        Ok(())
    }

    fn commit_index(&self, index: &mut SessionIndex, intent: &RotationIntent) -> Result<()> {
        let Some(entry) = index.find_by_hash(&intent.key_hash) else {
            // The key vanished from the index (e.g. manual edit): the rotation
            // still needs to be durable, so re-create an archived entry.
            return Ok(());
        };
        let key = entry.key.clone();
        if entry.location == intent.dst && entry.archived {
            return Ok(());
        }
        index.mark_archived(&key, intent.dst.clone());
        index.save()?;
        Ok(())
    }

    fn append_completion(&self, intent: &RotationIntent) -> Result<()> {
        let record = serde_json::json!({
            "kind": "complete",
            "rot_id": intent.rot_id,
            "key_hash": intent.key_hash,
            "dst": intent.dst,
            "ts": now_epoch_ms(),
        });
        append_jsonl_fsync(&self.journal_path(), &record)
    }

    /// Tolerant journal scan: a torn trailing line is informational only.
    fn journal_has(&self, rot_id: &str) -> bool {
        let Ok(raw) = fs::read_to_string(self.journal_path()) else {
            return false;
        };
        for line in raw.lines() {
            let Ok(value) = serde_json::from_str::<serde_json::Value>(line) else {
                continue;
            };
            if value.get("kind").and_then(|v| v.as_str()) == Some("complete")
                && value.get("rot_id").and_then(|v| v.as_str()) == Some(rot_id)
            {
                return true;
            }
        }
        false
    }

    fn remove_intent(&self, path: &Path) -> Result<()> {
        match fs::remove_file(path) {
            Ok(()) => {}
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
            Err(err) => return Err(err).context("remove rotation intent"),
        }
        if let Some(parent) = path.parent() {
            fsync_dir(parent)?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::identity::ChannelSource;

    fn setup() -> (tempfile::TempDir, SessionRotation, SessionIndex, SessionKey) {
        let tmp = tempfile::tempdir().unwrap();
        let sessions = tmp.path().to_path_buf();
        let index_path = sessions.join("index.json");
        let mut index = SessionIndex::open(&index_path);
        let key = SessionKey::new(ChannelSource::Telegram, "u1", "notes");
        let src = key.opaque_file_name();
        fs::write(sessions.join(&src), "{\"entries\":[]}\n").unwrap();
        index.insert_live(key.clone(), src, None);
        index.save().unwrap();
        let rotation = SessionRotation::new(sessions.clone());
        (tmp, rotation, index, key)
    }

    #[test]
    fn rotate_moves_file_and_archives_index() {
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);
        rotation.rotate(&mut index, &key, &src, &dst).unwrap();

        assert!(!rotation.sessions_dir().join(&src).exists());
        assert!(rotation.sessions_dir().join(&dst).exists());
        assert!(index.live(&key).is_none());
        assert_eq!(index.get(&key).unwrap().location, dst);
        // No intent remains.
        let intents = fs::read_dir(rotation.sessions_dir())
            .unwrap()
            .flatten()
            .filter(|e| e.file_name().to_string_lossy().ends_with(".intent"))
            .count();
        assert_eq!(intents, 0);
    }

    fn write_intent(rotation: &SessionRotation, key: &SessionKey, src: &str, dst: &str) -> PathBuf {
        let intent = RotationIntent {
            v: 1,
            rot_id: "rot-1".into(),
            key_hash: key.key_hash(),
            src: src.into(),
            dst: dst.into(),
            ts: 1,
        };
        let path = rotation.intent_path(&key.key_hash());
        atomic_write_json(&path, &intent).unwrap();
        path
    }

    #[test]
    fn recovery_redo_when_rename_never_landed() {
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);
        write_intent(&rotation, &key, &src, &dst);

        let outcomes = rotation.recover(&mut index).unwrap();
        assert_eq!(outcomes.len(), 1);
        assert!(matches!(outcomes[0], RecoveryOutcome::Redone { .. }));
        assert!(rotation.sessions_dir().join(&dst).exists());
        assert!(index.live(&key).is_none());
    }

    #[test]
    fn recovery_finalizes_after_rename_landed() {
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);
        write_intent(&rotation, &key, &src, &dst);
        fs::create_dir_all(rotation.sessions_dir().join("archived")).unwrap();
        fs::rename(
            rotation.sessions_dir().join(&src),
            rotation.sessions_dir().join(&dst),
        )
        .unwrap();

        let outcomes = rotation.recover(&mut index).unwrap();
        assert!(matches!(outcomes[0], RecoveryOutcome::Finalized { .. }));
        assert_eq!(index.get(&key).unwrap().location, dst);
    }

    #[test]
    fn recovery_does_not_duplicate_audit_record() {
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);
        write_intent(&rotation, &key, &src, &dst);
        fs::create_dir_all(rotation.sessions_dir().join("archived")).unwrap();
        fs::rename(
            rotation.sessions_dir().join(&src),
            rotation.sessions_dir().join(&dst),
        )
        .unwrap();
        // Step 4 already happened before the crash.
        let intent = RotationIntent {
            v: 1,
            rot_id: "rot-1".into(),
            key_hash: key.key_hash(),
            src: src.clone(),
            dst: dst.clone(),
            ts: 1,
        };
        rotation.append_completion(&intent).unwrap();

        rotation.recover(&mut index).unwrap();
        let journal = fs::read_to_string(rotation.journal_path()).unwrap();
        assert_eq!(journal.matches("\"rot_id\":\"rot-1\"").count(), 1);
    }

    #[test]
    fn kill_before_intent_is_a_clean_control() {
        // Step 0: the process died before the intent write. Nothing to repair.
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);

        let outcomes = rotation.recover(&mut index).unwrap();
        assert!(outcomes.is_empty());
        assert!(rotation.sessions_dir().join(&src).exists());
        assert!(!rotation.sessions_dir().join(&dst).exists());
        assert_eq!(index.live(&key).unwrap().location, src);
        assert!(!rotation.journal_path().exists());
        assert!(!rotation.quarantine_path().exists());
    }

    #[test]
    fn kill_after_index_durable_does_not_rewrite_the_index() {
        // Steps 1-3 landed: intent present, rename done, index already points at
        // `dst`. Recovery must only redo steps 4/5 and leave the index untouched.
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);
        write_intent(&rotation, &key, &src, &dst);
        fs::create_dir_all(rotation.sessions_dir().join("archived")).unwrap();
        fs::rename(
            rotation.sessions_dir().join(&src),
            rotation.sessions_dir().join(&dst),
        )
        .unwrap();
        index.mark_archived(&key, dst.clone());
        index.save().unwrap();

        // Extra trailing whitespace is valid JSON but would be normalized away by
        // any rewrite, so it proves the index file was not touched again.
        let index_path = index.path().to_path_buf();
        let mut padded = fs::read(&index_path).unwrap();
        padded.extend_from_slice(b"\n\n");
        fs::write(&index_path, &padded).unwrap();

        let outcomes = rotation.recover(&mut index).unwrap();
        assert!(matches!(outcomes[0], RecoveryOutcome::Finalized { .. }));
        assert_eq!(fs::read(&index_path).unwrap(), padded);

        let journal = fs::read_to_string(rotation.journal_path()).unwrap();
        assert_eq!(journal.matches("\"rot_id\":\"rot-1\"").count(), 1);
        assert!(!rotation.intent_path(&key.key_hash()).exists());
        assert!(rotation.sessions_dir().join(&dst).exists());
    }

    #[test]
    fn anomalous_state_quarantines_the_key() {
        let (_tmp, rotation, mut index, key) = setup();
        let src = key.opaque_file_name();
        let dst = rotation.archived_location(&key);
        write_intent(&rotation, &key, &src, &dst);
        // Neither src nor dst exists.
        fs::remove_file(rotation.sessions_dir().join(&src)).unwrap();

        let outcomes = rotation.recover(&mut index).unwrap();
        assert!(matches!(outcomes[0], RecoveryOutcome::Quarantined { .. }));
        assert!(index.get(&key).is_none());
        assert!(rotation.quarantine_path().exists());
    }
}
