//! Session registry keyed by `(source, conversation_id, project)` (C1).
//!
//! The dispatcher's global queue is the ordering contract; this registry only
//! maps a key to a hot handle and enforces the §4.6 freshness fingerprint.
//! A fingerprint drift other than a pure memory-digest change rotates the
//! session through the §4.4 WAL path. Digest-only changes also rotate here
//! because pi's system-priority append channel (S-7) is unproven; the
//! conservative rule is the one that ships (§4.6).

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::Result;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tokio::sync::Mutex as AsyncMutex;

use crate::fsutil::{archived_name, ensure_dir, now_epoch_ms};
use crate::identity::{sha256_hex, SessionKey};
use crate::rotation::SessionRotation;
use crate::runtime::{AgentRuntime, AgentSession, SessionSpec};
use crate::session_index::SessionIndex;

pub const TOOL_POLICY_VERSION: &str = "botler-v2-tools-1";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SessionMeta {
    pub project: String,
    pub prompt_fingerprint: String,
    pub provider: String,
    pub model: String,
    pub tool_policy_version: String,
}

impl SessionMeta {
    pub fn from_spec(spec: &SessionSpec, fingerprint: impl Into<String>) -> Self {
        Self {
            project: spec.key.project.clone(),
            prompt_fingerprint: fingerprint.into(),
            provider: spec.provider.clone(),
            model: spec.model.clone(),
            tool_policy_version: TOOL_POLICY_VERSION.to_string(),
        }
    }
}

/// Hash the static inputs only. Dynamic placeholders (`__TODAY__`, sender,
/// project rendering) must never enter this value (§4.6).
pub fn fingerprint(parts: &[&str]) -> String {
    let mut hasher = Sha256::new();
    for part in parts {
        let len = part.len();
        hasher.update(len.to_string().as_bytes());
        hasher.update(b":");
        hasher.update(part.as_bytes());
        hasher.update(b"|");
    }
    hex::encode(hasher.finalize())
}

pub fn file_fingerprint(path: &Path) -> Option<String> {
    // Never hash through a symlink: it could point outside the config dir, so
    // the fingerprint would silently describe an external file. Callers treat a
    // missing/None fingerprint as "no valid session file", so a symlink here
    // is refused rather than resumed.
    if std::fs::symlink_metadata(path)
        .map(|m| m.file_type().is_symlink())
        .unwrap_or(false)
    {
        return None;
    }
    let bytes = std::fs::read(path).ok()?;
    Some(sha256_hex(&bytes))
}

// True when the path exists and is a symlink. A symlink at a session location
// is never trusted: it can point outside the config dir, or be dangling so a
// later create/open would follow it.
fn is_session_file_symlink(file: &Path) -> bool {
    std::fs::symlink_metadata(file)
        .map(|m| m.file_type().is_symlink())
        .unwrap_or(false)
}

struct Pooled {
    handle: Arc<AsyncMutex<Box<dyn AgentSession>>>,
    meta: SessionMeta,
    last_used: u64,
}

pub struct Acquired {
    pub handle: Arc<AsyncMutex<Box<dyn AgentSession>>>,
    pub meta: SessionMeta,
    /// True when an incompatible hot/cold session was archived and replaced.
    pub rotated: bool,
    /// User-visible notice for corrupt-session recovery (§4.7).
    pub notice: Option<String>,
    /// This key had no resumable file and was created new.
    pub fresh: bool,
}

pub struct SessionRegistry {
    runtime: Arc<dyn AgentRuntime>,
    sessions_dir: PathBuf,
    index: SessionIndex,
    rotation: SessionRotation,
    hot_limit: usize,
    tick: u64,
    hot: HashMap<SessionKey, Pooled>,
}

impl SessionRegistry {
    pub fn new(
        runtime: Arc<dyn AgentRuntime>,
        sessions_dir: impl Into<PathBuf>,
        index_file: impl Into<PathBuf>,
        hot_limit: usize,
    ) -> Result<Self> {
        let sessions_dir = sessions_dir.into();
        ensure_dir(&sessions_dir)?;
        let index = SessionIndex::open(index_file.into());
        let rotation = SessionRotation::new(&sessions_dir);
        let mut registry = Self {
            runtime,
            sessions_dir: sessions_dir.clone(),
            index,
            rotation,
            hot_limit: hot_limit.max(1),
            tick: 0,
            hot: HashMap::new(),
        };
        // Crash-window reconciliation before any resume (§4.7/§4.8).
        registry.rotation.recover(&mut registry.index)?;
        Ok(registry)
    }

    pub fn sessions_dir(&self) -> &Path {
        &self.sessions_dir
    }

    pub fn index(&self) -> &SessionIndex {
        &self.index
    }

    pub fn hot_len(&self) -> usize {
        self.hot.len()
    }

    pub fn session_file(&self, key: &SessionKey) -> PathBuf {
        self.sessions_dir.join(key.opaque_file_name())
    }

    pub fn live_location(&self, key: &SessionKey) -> Option<String> {
        self.index.live(key).map(|entry| entry.location.clone())
    }

    /// Create or resume the keyed session. `meta` carries the freshly computed
    /// fingerprint; a hot handle with a different fingerprint is archived
    /// before a new one is opened.
    pub async fn acquire(&mut self, spec: SessionSpec, meta: SessionMeta) -> Result<Acquired> {
        self.tick += 1;
        let tick = self.tick;

        if spec.ephemeral {
            let handle = self.runtime.open_session(spec).await?;
            return Ok(Acquired {
                handle: Arc::new(AsyncMutex::new(handle)),
                meta,
                rotated: false,
                notice: None,
                fresh: true,
            });
        }

        if let Some(existing) = self.hot.get_mut(&spec.key) {
            if existing.meta == meta {
                existing.last_used = tick;
                return Ok(Acquired {
                    handle: Arc::clone(&existing.handle),
                    meta: existing.meta.clone(),
                    rotated: false,
                    notice: None,
                    fresh: false,
                });
            }
            // C3: never serve a stale prompt/provider/tool policy.
            let stale = self.hot.remove(&spec.key).expect("just checked");
            drop(stale.handle);
            self.archive_existing(&spec.key).await?;
            let acquired = self.open_fresh(spec, meta.clone()).await?;
            return Ok(Acquired {
                rotated: true,
                fresh: acquired.fresh,
                notice: acquired.notice,
                handle: acquired.handle,
                meta,
            });
        }

        let mut notice = None;
        let mut fresh = true;
        let live = self.index.live(&spec.key).cloned();
        let file = self.session_file(&spec.key);
        if let Some(entry) = live {
            if entry.meta.as_ref() != Some(&meta) {
                // A cold resume still proves freshness: a missing or changed
                // fingerprint cannot vouch for the prompt/provider/tool policy
                // (§4.6), so archive the old file and start over.
                let existed = file.exists();
                self.archive_existing(&spec.key).await?;
                let acquired = self.open_fresh(spec, meta).await?;
                return Ok(Acquired {
                    rotated: existed,
                    fresh: true,
                    notice: acquired.notice,
                    handle: acquired.handle,
                    meta: acquired.meta,
                });
            }
            if is_session_file_symlink(&file) {
                // A symlink at an indexed session location is untrusted whether
                // or not its target exists: resume would read through it (escape)
                // and creating through a dangling link would land outside the
                // config dir. Quarantine the link itself (rename does not follow
                // it) and start a fresh session at this location.
                let archived = self.quarantine(&spec.key, &file, "symlink")?;
                tracing::warn!(
                    "[session] session file for {} was a symlink; archived as {archived} and starting fresh",
                    spec.key.key_hash()
                );
                notice = Some(format!(
                    "本次会话的上下文文件被替换为链接，已归档为 {} 并重新开始该项目的会话。",
                    archived
                ));
                fresh = true;
            } else if file.exists() {
                match self.runtime.open_session(spec.clone()).await {
                    Ok(handle) => {
                        let arc = Arc::new(AsyncMutex::new(handle));
                        self.hot.insert(
                            spec.key.clone(),
                            Pooled {
                                handle: Arc::clone(&arc),
                                meta: meta.clone(),
                                last_used: tick,
                            },
                        );
                        self.evict(tick);
                        return Ok(Acquired {
                            handle: arc,
                            meta,
                            rotated: false,
                            notice: None,
                            fresh: false,
                        });
                    }
                    Err(err) => {
                        // Corrupt / format-upgrade-incompatible: archive, start
                        // fresh, and tell the user (never crash-loop) (§4.7).
                        tracing::warn!(
                            "[session] resume failed for {}: {err}; archiving and starting fresh",
                            spec.key.key_hash()
                        );
                        let corrupt = self.quarantine(&spec.key, &file, "corrupt")?;
                        self.index.remove(&spec.key);
                        self.index.save()?;
                        notice = Some(format!(
                            "本次会话的上下文文件无法读取，已归档为 {} 并重新开始该项目的会话。",
                            corrupt
                        ));
                        fresh = true;
                    }
                }
            } else {
                // The index points at a file that no longer exists.
                self.index.remove(&spec.key);
                self.index.save()?;
            }
        }

        let acquired = self.open_fresh(spec, meta).await?;
        Ok(Acquired {
            rotated: false,
            fresh,
            notice: acquired.notice.or(notice),
            handle: acquired.handle,
            meta: acquired.meta,
        })
    }

    async fn open_fresh(&mut self, spec: SessionSpec, meta: SessionMeta) -> Result<Acquired> {
        let tick = self.tick;
        let file_name = spec.key.opaque_file_name();
        let file = self.sessions_dir.join(&file_name);
        if file.exists() || is_session_file_symlink(&file) {
            // A file nobody indexed (index lost / planted) or a symlink (valid or
            // dangling) cannot prove its prompt/provider/tool fingerprint, so it
            // must never be resumed. exists() follows symlinks, so a dangling
            // link reads as absent and would slip through to open_session, which
            // could then create through the link outside the config dir; checking
            // the path itself with symlink_metadata catches it. Quarantine
            // renames the path itself -- it does not follow a symlink -- so the
            // open_session below creates a fresh regular file in sessions_dir and
            // never escapes the config dir.
            let reason = if is_session_file_symlink(&file) {
                "symlink"
            } else {
                "unindexed"
            };
            let archived = self.quarantine(&spec.key, &file, reason)?;
            tracing::warn!(
                "[session] archived unindexed session file for {} as {archived}",
                spec.key.key_hash()
            );
        }
        self.index
            .insert_live(spec.key.clone(), file_name, Some(meta.clone()));
        self.index.save()?;
        let handle = self.runtime.open_session(spec.clone()).await?;
        let arc = Arc::new(AsyncMutex::new(handle));
        self.hot.insert(
            spec.key.clone(),
            Pooled {
                handle: Arc::clone(&arc),
                meta: meta.clone(),
                last_used: tick,
            },
        );
        self.evict(tick);
        Ok(Acquired {
            handle: arc,
            meta,
            rotated: false,
            notice: None,
            fresh: true,
        })
    }

    fn evict(&mut self, keep: u64) {
        while self.hot.len() > self.hot_limit {
            let Some(victim) = self
                .hot
                .iter()
                .filter(|(_, pooled)| pooled.last_used != keep)
                .min_by_key(|(_, pooled)| pooled.last_used)
                .map(|(key, _)| key.clone())
            else {
                break;
            };
            self.hot.remove(&victim);
        }
    }

    async fn archive_existing(&mut self, key: &SessionKey) -> Result<()> {
        let Some(location) = self.live_location(key) else {
            return Ok(());
        };
        let src = self.sessions_dir.join(&location);
        if !src.exists() {
            self.index.remove(key);
            self.index.save()?;
            return Ok(());
        }
        let dst = self.rotation.archived_location(key);
        self.rotation
            .rotate(&mut self.index, key, &location, &dst)?;
        Ok(())
    }

    fn quarantine(&self, key: &SessionKey, file: &Path, reason: &str) -> Result<String> {
        let name = format!(
            "{}.{reason}-{}.jsonl",
            key.key_hash().get(..32).unwrap_or(&key.key_hash()),
            now_epoch_ms()
        );
        let dst = self.sessions_dir.join("archived").join(&name);
        if let Some(parent) = dst.parent() {
            ensure_dir(parent)?;
        }
        std::fs::rename(file, &dst)?;
        Ok(format!("archived/{name}"))
    }

    /// Archive and remove a key after the turn completes (C4). Returns whether
    /// a rotation actually happened.
    pub async fn rotate_now(&mut self, key: &SessionKey) -> Result<bool> {
        if let Some(pooled) = self.hot.remove(key) {
            drop(pooled.handle);
        }
        let Some(location) = self.live_location(key) else {
            return Ok(false);
        };
        let src = self.sessions_dir.join(&location);
        if !src.exists() {
            self.index.remove(key);
            self.index.save()?;
            return Ok(false);
        }
        let dst = self.rotation.archived_location(key);
        self.rotation
            .rotate(&mut self.index, key, &location, &dst)?;
        Ok(true)
    }

    /// All live keys for one conversation (used by a routing-level reset).
    pub fn keys_for_conversation(
        &self,
        source: crate::identity::ChannelSource,
        conversation_id: &str,
    ) -> Vec<SessionKey> {
        self.index
            .entries()
            .filter(|entry| {
                !entry.archived
                    && entry.key.source == source
                    && entry.key.conversation_id == conversation_id
            })
            .map(|entry| entry.key.clone())
            .collect()
    }

    /// Register a live location and its freshness metadata after the runtime
    /// created or continued a file.
    pub fn touch(&mut self, key: &SessionKey, meta: SessionMeta) -> Result<()> {
        if self.index.live(key).is_none() {
            self.index
                .insert_live(key.clone(), key.opaque_file_name(), Some(meta));
        } else {
            self.index.set_meta(key, meta);
        }
        self.index.save()
    }
}

/// Name helper shared with the retention pass (P5).
pub fn retained_archive_name(file_name: &str, epoch_ms: i64) -> String {
    archived_name(file_name, epoch_ms)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::identity::ChannelSource;
    use crate::runtime::StubRuntime;
    use crate::tools::Toolset;

    fn spec(key: SessionKey, sessions: &Path) -> SessionSpec {
        SessionSpec {
            session_file: sessions.join(key.opaque_file_name()),
            workspace: PathBuf::from("/tmp"),
            system_prompt: "p".into(),
            provider: "p".into(),
            model: "m".into(),
            tools: Toolset::default(),
            ephemeral: false,
            key,
        }
    }

    fn meta(p: &str, f: &str) -> SessionMeta {
        SessionMeta {
            project: p.into(),
            prompt_fingerprint: f.into(),
            provider: "p".into(),
            model: "m".into(),
            tool_policy_version: TOOL_POLICY_VERSION.into(),
        }
    }

    #[tokio::test]
    async fn reuses_a_hot_session_and_rotates_on_fingerprint_drift() {
        let dir = tempfile::tempdir().unwrap();
        let runtime = Arc::new(StubRuntime::new());
        let mut reg = SessionRegistry::new(
            runtime.clone(),
            dir.path().join("sessions"),
            dir.path().join("index.json"),
            4,
        )
        .unwrap();
        let key = SessionKey::new(ChannelSource::Telegram, "1", "notes");
        let sessions = dir.path().join("sessions");
        let first = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        drop(first);
        // Simulate the runtime writing the file so rotation has a source.
        std::fs::write(reg.session_file(&key), "{}").unwrap();
        reg.touch(&key, meta("notes", "v1")).unwrap();

        let second = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        assert!(!second.fresh && !second.rotated);
        drop(second);

        let third = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v2"))
            .await
            .unwrap();
        assert!(third.rotated);
        assert!(third.fresh);
        assert!(!reg.session_file(&key).exists());
        assert_eq!(runtime.opened().len(), 2);
    }

    #[tokio::test]
    async fn corrupt_resume_archives_and_starts_fresh() {
        let dir = tempfile::tempdir().unwrap();
        let runtime = Arc::new(StubRuntime::new());
        let mut reg = SessionRegistry::new(
            runtime.clone(),
            dir.path().join("sessions"),
            dir.path().join("index.json"),
            4,
        )
        .unwrap();
        let key = SessionKey::new(ChannelSource::Feishu, "oc_x", "p");
        let sessions = dir.path().join("sessions");
        reg.acquire(spec(key.clone(), &sessions), meta("p", "v1"))
            .await
            .unwrap();
        std::fs::write(reg.session_file(&key), "{}").unwrap();
        reg.touch(&key, meta("p", "v1")).unwrap();
        // Drop the hot handle by rotating the registry's map without archiving.
        reg.hot.clear();
        runtime.fail_resume_once();
        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("p", "v1"))
            .await
            .unwrap();
        assert!(acquired.fresh);
        assert!(acquired.notice.is_some());
    }

    async fn open_and_persist(
        runtime: &Arc<StubRuntime>,
        dir: &Path,
        key: &SessionKey,
        meta: SessionMeta,
    ) {
        let sessions = dir.join("sessions");
        let runtime_dyn: Arc<dyn AgentRuntime> = runtime.clone();
        let mut reg =
            SessionRegistry::new(runtime_dyn, &sessions, dir.join("index.json"), 4).unwrap();
        reg.acquire(spec(key.clone(), &sessions), meta.clone())
            .await
            .unwrap();
        std::fs::write(reg.session_file(key), "{}").unwrap();
        reg.touch(key, meta).unwrap();
    }

    #[tokio::test]
    async fn cold_resume_with_a_matching_fingerprint_is_reused() {
        let dir = tempfile::tempdir().unwrap();
        let runtime = Arc::new(StubRuntime::new());
        let key = SessionKey::new(ChannelSource::Telegram, "cold", "notes");
        open_and_persist(&runtime, dir.path(), &key, meta("notes", "v1")).await;

        let sessions = dir.path().join("sessions");
        let mut reg =
            SessionRegistry::new(runtime.clone(), &sessions, dir.path().join("index.json"), 4)
                .unwrap();
        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        assert!(!acquired.fresh && !acquired.rotated);
        assert!(reg.session_file(&key).exists());
        assert_eq!(runtime.opened().len(), 2);
        assert!(runtime.opened()[1].1, "second open must resume the file");
    }

    #[tokio::test]
    async fn cold_resume_with_a_changed_fingerprint_rotates() {
        let dir = tempfile::tempdir().unwrap();
        let runtime = Arc::new(StubRuntime::new());
        let key = SessionKey::new(ChannelSource::Telegram, "cold", "notes");
        open_and_persist(&runtime, dir.path(), &key, meta("notes", "v1")).await;

        let sessions = dir.path().join("sessions");
        let mut reg =
            SessionRegistry::new(runtime.clone(), &sessions, dir.path().join("index.json"), 4)
                .unwrap();
        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v2"))
            .await
            .unwrap();
        assert!(acquired.rotated && acquired.fresh);
        // The old file moved to archived/; the runtime creates the new file on
        // its first turn.
        assert!(!reg.session_file(&key).exists());
        let archived = std::fs::read_dir(sessions.join("archived"))
            .unwrap()
            .count();
        assert_eq!(archived, 1);
        assert_eq!(reg.live_location(&key).unwrap(), key.opaque_file_name());
    }

    #[tokio::test]
    async fn unindexed_session_file_is_never_resumed() {
        let dir = tempfile::tempdir().unwrap();
        let sessions = dir.path().join("sessions");
        std::fs::create_dir_all(&sessions).unwrap();
        let key = SessionKey::new(ChannelSource::Telegram, "cold", "notes");
        // A file exists (index lost, or planted) with no index entry vouching
        // for its fingerprint.
        std::fs::write(sessions.join(key.opaque_file_name()), "{}").unwrap();

        let runtime = Arc::new(StubRuntime::new());
        let mut reg =
            SessionRegistry::new(runtime.clone(), &sessions, dir.path().join("index.json"), 4)
                .unwrap();
        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        assert!(acquired.fresh);
        assert!(!runtime.opened()[0].1, "an unindexed file must not resume");
        assert!(!reg.session_file(&key).exists());
        let archived: Vec<_> = std::fs::read_dir(sessions.join("archived"))
            .unwrap()
            .map(|entry| entry.unwrap().file_name().to_string_lossy().to_string())
            .collect();
        assert_eq!(archived.len(), 1);
        assert!(archived[0].contains(".unindexed-"), "{archived:?}");
    }

    #[test]
    fn fingerprint_ignores_nothing_but_is_stable() {
        assert_eq!(fingerprint(&["a", "b"]), fingerprint(&["a", "b"]));
        assert_ne!(fingerprint(&["a", "b"]), fingerprint(&["a:b"]));
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn symlinked_session_file_is_quarantined_not_resumed() {
        use std::os::unix::fs::symlink;
        let dir = tempfile::tempdir().unwrap();
        let sessions = dir.path().join("sessions");
        std::fs::create_dir_all(&sessions).unwrap();
        let key = SessionKey::new(ChannelSource::Telegram, "1", "notes");

        // An external file the symlink would point at; resuming it would read
        // outside the config dir.
        let outside = dir.path().join("secret.jsonl");
        std::fs::write(&outside, "external content").unwrap();
        symlink(&outside, sessions.join(key.opaque_file_name())).unwrap();

        let runtime = Arc::new(StubRuntime::new());
        let mut reg =
            SessionRegistry::new(runtime.clone(), &sessions, dir.path().join("index.json"), 4)
                .unwrap();
        // Index vouches for the symlinked location with a matching fingerprint.
        reg.touch(&key, meta("notes", "v1")).unwrap();

        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        // The symlink must never be resumed; a fresh regular file takes its place.
        assert!(acquired.fresh);
        assert!(acquired.notice.is_some());
        // The symlink was quarantined, not resumed. StubRuntime does not
        // materialize the session file, so the path may not exist; what matters
        // is that it is no longer a symlink (the link was renamed away).
        let still_symlink = std::fs::symlink_metadata(reg.session_file(&key))
            .map(|m| m.file_type().is_symlink())
            .unwrap_or(false);
        assert!(!still_symlink, "session path must not be a symlink");
        // The external target is untouched by the resume.
        assert_eq!(
            std::fs::read_to_string(&outside).unwrap(),
            "external content"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn dangling_symlink_session_is_quarantined_not_followed() {
        use std::os::unix::fs::symlink;
        let dir = tempfile::tempdir().unwrap();
        let sessions = dir.path().join("sessions");
        std::fs::create_dir_all(&sessions).unwrap();
        let key = SessionKey::new(ChannelSource::Telegram, "1", "notes");
        // Dangling link: target does not exist. exists() would return false, so a
        // naive check would skip it and let open_session create through the link.
        symlink(
            dir.path().join("does-not-exist.jsonl"),
            sessions.join(key.opaque_file_name()),
        )
        .unwrap();

        let runtime = Arc::new(StubRuntime::new());
        let mut reg =
            SessionRegistry::new(runtime.clone(), &sessions, dir.path().join("index.json"), 4)
                .unwrap();
        reg.touch(&key, meta("notes", "v1")).unwrap();

        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        assert!(acquired.fresh);
        assert!(acquired.notice.is_some());
        // The dangling link was quarantined, not followed: the path is no longer
        // a symlink and no target file was created outside the config dir.
        let still_symlink = std::fs::symlink_metadata(reg.session_file(&key))
            .map(|m| m.file_type().is_symlink())
            .unwrap_or(false);
        assert!(!still_symlink, "dangling link must be quarantined");
        assert!(!dir.path().join("does-not-exist.jsonl").exists());
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn unindexed_dangling_symlink_is_quarantined() {
        use std::os::unix::fs::symlink;
        let dir = tempfile::tempdir().unwrap();
        let sessions = dir.path().join("sessions");
        std::fs::create_dir_all(&sessions).unwrap();
        let key = SessionKey::new(ChannelSource::Telegram, "x", "notes");
        symlink(
            dir.path().join("nope.jsonl"),
            sessions.join(key.opaque_file_name()),
        )
        .unwrap();

        let runtime = Arc::new(StubRuntime::new());
        let mut reg =
            SessionRegistry::new(runtime.clone(), &sessions, dir.path().join("index.json"), 4)
                .unwrap();
        let acquired = reg
            .acquire(spec(key.clone(), &sessions), meta("notes", "v1"))
            .await
            .unwrap();
        assert!(acquired.fresh);
        // The dangling link is gone (quarantined), not followed to create outside.
        let still_symlink = std::fs::symlink_metadata(reg.session_file(&key))
            .map(|m| m.file_type().is_symlink())
            .unwrap_or(false);
        assert!(!still_symlink, "dangling link must be quarantined");
        assert!(!dir.path().join("nope.jsonl").exists());
        let archived: Vec<_> = std::fs::read_dir(sessions.join("archived"))
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().to_string())
            .collect();
        assert_eq!(archived.len(), 1);
        assert!(archived[0].contains(".symlink-"), "{archived:?}");
    }
}
