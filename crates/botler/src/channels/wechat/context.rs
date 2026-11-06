//! Per-sender WeChat context persistence.
//!
//! `context_token` is tied to a rolling 24h window refreshed by any inbound
//! message, so it must survive restarts. The store also keeps the last inbound
//! timestamp and the last renewal-reminder timestamp so a quiet stretch is
//! nudged at most once. Addresses are recorded into `contacts.json` separately
//! (the push fallback contract).

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};

use anyhow::Result;
use serde::{Deserialize, Serialize};

use crate::fsutil::{atomic_write_json, ensure_dir};
use crate::identity::ChannelSource;
use crate::push::ContactsStore;

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct WechatContact {
    #[serde(default)]
    pub context_token: Option<String>,
    #[serde(default)]
    pub last_msg_at: i64,
    #[serde(default)]
    pub last_reminded_at: Option<i64>,
}

#[derive(Debug, Clone)]
pub struct WechatContextStore {
    file: PathBuf,
    contacts: ContactsStore,
}

type ContextMap = HashMap<String, WechatContact>;

impl WechatContextStore {
    pub fn new(file: impl Into<PathBuf>, contacts: ContactsStore) -> Self {
        Self {
            file: file.into(),
            contacts,
        }
    }

    pub fn path(&self) -> &Path {
        &self.file
    }

    pub fn contacts(&self) -> &ContactsStore {
        &self.contacts
    }

    fn load(&self) -> ContextMap {
        fs::read_to_string(&self.file)
            .ok()
            .and_then(|raw| serde_json::from_str(&raw).ok())
            .unwrap_or_default()
    }

    fn save(&self, map: &ContextMap) -> Result<()> {
        if let Some(parent) = self.file.parent() {
            ensure_dir(parent)?;
        }
        atomic_write_json(&self.file, map)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(&self.file, fs::Permissions::from_mode(0o600));
        }
        Ok(())
    }

    /// Record an inbound message: refresh the token, the timestamp, and the
    /// known-address list used by push fallback.
    pub fn record_message(&self, sender: &str, context_token: Option<&str>, now_ms: i64) {
        let mut map = self.load();
        let entry = map.entry(sender.to_string()).or_default();
        if let Some(token) = context_token.filter(|t| !t.is_empty()) {
            entry.context_token = Some(token.to_string());
        }
        entry.last_msg_at = now_ms;
        if let Err(err) = self.save(&map) {
            tracing::warn!("[wechat] save context failed: {err}");
        }
        self.contacts.record(ChannelSource::Wechat, sender);
    }

    pub fn get(&self, sender: &str) -> Option<WechatContact> {
        self.load().get(sender).cloned()
    }

    pub fn mark_reminded(&self, sender: &str, now_ms: i64) {
        let mut map = self.load();
        let entry = map.entry(sender.to_string()).or_default();
        entry.last_reminded_at = Some(now_ms);
        if let Err(err) = self.save(&map) {
            tracing::warn!("[wechat] save context failed: {err}");
        }
    }

    pub fn remove(&self, sender: &str) {
        let mut map = self.load();
        if map.remove(sender).is_some() {
            if let Err(err) = self.save(&map) {
                tracing::warn!("[wechat] save context failed: {err}");
            }
        }
        self.contacts.remove(ChannelSource::Wechat);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn records_token_and_refreshes_the_timestamp() {
        let dir = tempfile::tempdir().unwrap();
        let contacts = ContactsStore::new(dir.path().join("contacts.json"));
        let store =
            WechatContextStore::new(dir.path().join("wechat-context.json"), contacts.clone());
        store.record_message("u1", Some("t1"), 100);
        let first = store.get("u1").unwrap();
        assert_eq!(first.context_token.as_deref(), Some("t1"));
        assert_eq!(first.last_msg_at, 100);

        store.record_message("u1", None, 200);
        let second = store.get("u1").unwrap();
        // A text-only message must not clear the token that replies need.
        assert_eq!(second.context_token.as_deref(), Some("t1"));
        assert_eq!(second.last_msg_at, 200);
        assert_eq!(contacts.get(ChannelSource::Wechat), vec!["u1"]);
    }

    #[test]
    fn mark_reminded_is_per_sender() {
        let dir = tempfile::tempdir().unwrap();
        let store = WechatContextStore::new(
            dir.path().join("c.json"),
            ContactsStore::new(dir.path().join("contacts.json")),
        );
        store.record_message("u1", Some("t"), 100);
        store.mark_reminded("u1", 150);
        assert_eq!(store.get("u1").unwrap().last_reminded_at, Some(150));
    }
}
