//! Three-tier memory (§4.3).
//!
//! - Tier 1 (personal conversation): keyed by `(source, principal|conversation, project)`
//!   — the default retain target.
//! - Tier 2 (project-shared): keyed by `(project)`; writing requires the explicit
//!   `tier = "project"` parameter.
//! - Tier 3 (user-global): keyed by `PrincipalKey = (source, authenticated_principal_id)`;
//!   writing requires the explicit `tier = "global"` parameter and is never
//!   visible to group conversations.
//!
//! Storage lives under `BOTLER_CONFIG_DIR/memory` with opaque file names. A
//! namespace's visibility is computed from the acting `MemoryContext`; recall
//! and the injected digest search exactly that union.

use std::cmp::Reverse;
use std::collections::HashSet;
use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{bail, Result};
use serde::{Deserialize, Serialize};

use crate::fsutil::{atomic_write, ensure_dir, now_epoch_ms};
use crate::identity::{sha256_hex, MemoryContext, MemoryNamespace};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum MemoryTier {
    Personal,
    Project,
    Global,
}

impl MemoryTier {
    pub fn parse(value: &str) -> Option<Self> {
        match value.trim().to_ascii_lowercase().as_str() {
            "personal" | "conversation" | "1" => Some(Self::Personal),
            "project" | "shared" | "2" => Some(Self::Project),
            "global" | "user" | "3" => Some(Self::Global),
            _ => None,
        }
    }

    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Personal => "personal",
            Self::Project => "project",
            Self::Global => "global",
        }
    }

    pub const fn tier(self) -> u8 {
        match self {
            Self::Personal => 1,
            Self::Project => 2,
            Self::Global => 3,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MemoryRecord {
    pub id: String,
    pub content: String,
    pub created_at: i64,
    pub updated_at: i64,
    #[serde(default)]
    pub source_project: Option<String>,
    /// Set when a newer memory supersedes this one; superseded records are
    /// excluded from recall/digest but remain auditable.
    #[serde(default)]
    pub superseded_by: Option<String>,
}

#[derive(Debug, Clone)]
pub struct ScoredMemory {
    pub tier: MemoryTier,
    pub namespace: MemoryNamespace,
    pub record: MemoryRecord,
    pub score: i64,
}

#[derive(Debug, Clone)]
pub struct MemoryStore {
    dir: PathBuf,
    content_max_bytes: usize,
    recall_char_budget: usize,
    digest_max_chars: usize,
}

impl MemoryStore {
    pub fn new(
        dir: impl Into<PathBuf>,
        content_max_bytes: usize,
        recall_token_budget: usize,
        digest_max_chars: usize,
    ) -> Self {
        Self {
            dir: dir.into(),
            content_max_bytes,
            // A conservative char-per-token estimate; recall never exceeds it.
            recall_char_budget: recall_token_budget.saturating_mul(4).max(256),
            digest_max_chars,
        }
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    fn namespace_path(&self, ns: &MemoryNamespace) -> PathBuf {
        self.dir.join(ns.opaque_file_name())
    }

    fn read_namespace(&self, ns: &MemoryNamespace) -> Vec<MemoryRecord> {
        let path = self.namespace_path(ns);
        let Ok(raw) = fs::read_to_string(&path) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        for line in raw.lines() {
            if line.trim().is_empty() {
                continue;
            }
            match serde_json::from_str::<MemoryRecord>(line) {
                Ok(record) => out.push(record),
                Err(err) => {
                    tracing::warn!("[memory] skipping torn record in {}: {err}", path.display());
                }
            }
        }
        out
    }

    fn write_namespace(&self, ns: &MemoryNamespace, records: &[MemoryRecord]) -> Result<()> {
        ensure_dir(&self.dir)?;
        let mut buf = Vec::new();
        for record in records {
            serde_json::to_writer(&mut buf, record)?;
            buf.push(b'\n');
        }
        atomic_write(&self.namespace_path(ns), &buf)
    }

    /// Retain a fact into an explicitly chosen tier. Tier 2/3 writes are never
    /// the default and tier 3 is refused in group conversations.
    pub fn retain(
        &self,
        ctx: &MemoryContext,
        tier: MemoryTier,
        content: &str,
    ) -> Result<MemoryRecord> {
        let content = content.trim();
        if content.is_empty() {
            bail!("memory content is empty");
        }
        if content.len() > self.content_max_bytes {
            bail!(
                "memory content exceeds {} bytes (got {})",
                self.content_max_bytes,
                content.len()
            );
        }
        let ns = match tier {
            MemoryTier::Personal => MemoryNamespace::personal(ctx),
            MemoryTier::Project => MemoryNamespace::project(ctx.project.clone()),
            MemoryTier::Global => {
                if !ctx.can_write_global() {
                    bail!("group conversations cannot write user-global memory");
                }
                MemoryNamespace::global(ctx)
            }
        };
        let (sanitized, redactions) = screen_secrets(content);
        if redactions > 0 {
            tracing::warn!("[memory] redacted {redactions} secret-shaped span(s) before retain");
        }
        let now = now_epoch_ms();
        let mut records = self.read_namespace(&ns);
        let id = format!(
            "m-{}",
            &sha256_hex(format!("{}|{}|{}", ns.canonical(), now, sanitized).as_bytes())[..16]
        );
        let record = MemoryRecord {
            id,
            content: sanitized,
            created_at: now,
            updated_at: now,
            source_project: Some(ctx.project.clone()),
            superseded_by: None,
        };
        records.push(record.clone());
        self.write_namespace(&ns, &records)?;
        Ok(record)
    }

    /// Search the visible union. Scoring is deliberately simple and local:
    /// case-insensitive substring + token overlap, newest first on ties.
    pub fn recall(&self, ctx: &MemoryContext, query: &str, limit: usize) -> Vec<ScoredMemory> {
        let query = query.trim().to_lowercase();
        let tokens: Vec<&str> = query.split_whitespace().filter(|t| !t.is_empty()).collect();
        let mut hits = Vec::new();
        for ns in MemoryNamespace::visible_set(ctx) {
            let tier = tier_of(&ns);
            for record in self.read_namespace(&ns) {
                if record.superseded_by.is_some() {
                    continue;
                }
                let lower = record.content.to_lowercase();
                let mut score = 0i64;
                if !query.is_empty() && lower.contains(&query) {
                    score += 100;
                }
                for token in &tokens {
                    if lower.contains(token) {
                        score += 10;
                    }
                }
                // Tier 3 facts are the sanctioned cross-project association
                // channel; break ties in their favor.
                if score > 0 {
                    score += i64::from(tier.tier());
                    hits.push(ScoredMemory {
                        tier,
                        namespace: ns.clone(),
                        record,
                        score,
                    });
                }
            }
        }
        hits.sort_by(|a, b| {
            b.score
                .cmp(&a.score)
                .then_with(|| b.record.updated_at.cmp(&a.record.updated_at))
        });

        let mut budget = self.recall_char_budget;
        let mut out = Vec::new();
        for hit in hits {
            let cost = hit.record.content.chars().count();
            if cost > budget && !out.is_empty() {
                break;
            }
            budget = budget.saturating_sub(cost);
            out.push(hit);
            if out.len() >= limit.max(1) {
                break;
            }
        }
        out
    }

    /// Bounded digest injected into the system prompt. Tier 3 first, then tier
    /// 2, then tier 1; newest first within each tier. Truncation is explicit.
    pub fn mental_model(&self, ctx: &MemoryContext) -> String {
        let mut buckets: Vec<(u8, Vec<MemoryRecord>)> = Vec::new();
        for ns in MemoryNamespace::visible_set(ctx) {
            let tier = tier_of(&ns).tier();
            let mut records: Vec<MemoryRecord> = self
                .read_namespace(&ns)
                .into_iter()
                .filter(|r| r.superseded_by.is_none())
                .collect();
            records.sort_by_key(|r| Reverse(r.updated_at));
            if !records.is_empty() {
                buckets.push((tier, records));
            }
        }
        buckets.sort_by_key(|b| Reverse(b.0)); // 3, 2, 1

        let mut lines = Vec::new();
        let mut used = 0usize;
        let mut seen = HashSet::new();
        for (tier, records) in buckets {
            for record in records {
                let content = record.content.replace('\n', " ");
                let line = format!("- [tier {tier}] {content}");
                let cost = line.chars().count() + 1;
                if used + cost > self.digest_max_chars {
                    lines.push("- …（更多记忆已省略）".to_string());
                    return lines.join("\n");
                }
                if seen.insert(content) {
                    used += cost;
                    lines.push(line);
                }
            }
        }
        lines.join("\n")
    }

    pub fn list(&self, ctx: &MemoryContext) -> Vec<ScoredMemory> {
        let mut out = Vec::new();
        for ns in MemoryNamespace::visible_set(ctx) {
            let tier = tier_of(&ns);
            for record in self.read_namespace(&ns) {
                out.push(ScoredMemory {
                    tier,
                    namespace: ns.clone(),
                    record,
                    score: 0,
                });
            }
        }
        out.sort_by_key(|s| Reverse(s.record.updated_at));
        out
    }

    /// Delete one memory, scoped to namespaces this task may write.
    pub fn delete(&self, ctx: &MemoryContext, id: &str) -> Result<bool> {
        let writable = MemoryNamespace::writable_set(ctx);
        for ns in writable {
            let mut records = self.read_namespace(&ns);
            let before = records.len();
            records.retain(|r| r.id != id);
            if records.len() != before {
                self.write_namespace(&ns, &records)?;
                return Ok(true);
            }
        }
        Ok(false)
    }

    /// Clear one tier for the acting context. Tier 2 clears the whole project
    /// namespace and therefore requires the explicit project-wide flag.
    pub fn clear(&self, ctx: &MemoryContext, tier: MemoryTier, project_wide: bool) -> Result<bool> {
        let ns = match tier {
            MemoryTier::Personal => MemoryNamespace::personal(ctx),
            MemoryTier::Project => {
                if !project_wide {
                    bail!("clearing tier 2 affects every conversation in the project; pass project_wide=true explicitly");
                }
                MemoryNamespace::project(ctx.project.clone())
            }
            MemoryTier::Global => {
                if !ctx.can_write_global() {
                    bail!("group conversations cannot clear user-global memory");
                }
                MemoryNamespace::global(ctx)
            }
        };
        let existed = self.namespace_path(&ns).exists();
        self.write_namespace(&ns, &[])?;
        Ok(existed)
    }

    pub fn replace(&self, ctx: &MemoryContext, id: &str, content: &str) -> Result<bool> {
        if content.len() > self.content_max_bytes {
            bail!("memory content exceeds {} bytes", self.content_max_bytes);
        }
        let (sanitized, _) = screen_secrets(content);
        let now = now_epoch_ms();
        for ns in MemoryNamespace::writable_set(ctx) {
            let mut records = self.read_namespace(&ns);
            if let Some(record) = records.iter_mut().find(|r| r.id == id) {
                record.content = sanitized;
                record.updated_at = now;
                self.write_namespace(&ns, &records)?;
                return Ok(true);
            }
        }
        Ok(false)
    }
}

fn tier_of(ns: &MemoryNamespace) -> MemoryTier {
    match ns {
        MemoryNamespace::Personal { .. } => MemoryTier::Personal,
        MemoryNamespace::Project { .. } => MemoryTier::Project,
        MemoryNamespace::Global { .. } => MemoryTier::Global,
    }
}

/// Redact obvious credential-shaped spans. This is hygiene, not the leakage
/// boundary — tier separation is (§4.3).
pub fn screen_secrets(input: &str) -> (String, usize) {
    let mut out = String::with_capacity(input.len());
    let mut redactions = 0usize;
    for token in input.split_inclusive(char::is_whitespace) {
        let trimmed = token.trim_end();
        let suffix = &token[trimmed.len()..];
        let lower = trimmed.to_lowercase();
        let secret = is_secret_shaped(trimmed)
            || lower.starts_with("password=")
            || lower.starts_with("passwd=")
            || lower.starts_with("api_key=")
            || lower.starts_with("apikey=")
            || lower.starts_with("token=")
            || lower.starts_with("secret=")
            || lower.starts_with("authorization:");
        if secret {
            out.push_str("[REDACTED]");
            out.push_str(suffix);
            redactions += 1;
        } else {
            out.push_str(token);
        }
    }
    (out, redactions)
}

fn is_secret_shaped(token: &str) -> bool {
    let t = token.trim_matches(|c: char| c.is_ascii_punctuation());
    if t.starts_with("sk-") && t.len() >= 16 {
        return true;
    }
    if (t.starts_with("ghp_") || t.starts_with("xoxb-") || t.starts_with("AKIA")) && t.len() >= 16 {
        return true;
    }
    // Long opaque hex/base32-ish strings.
    t.len() >= 40 && t.chars().all(|c| c.is_ascii_alphanumeric())
}

#[cfg(test)]
mod tests {
    // These tests intentionally hold the process-global task-context lock
    // across awaits so cases cannot interleave.
    #![allow(clippy::await_holding_lock)]

    use std::sync::atomic::AtomicBool;
    use std::sync::Arc;

    use super::*;
    use crate::identity::{ChannelSource, MemoryContext};
    use crate::tools::memory::{RecallTool, RetainTool};
    use crate::tools::task_context::{set_task_context, test_lock, TaskContext};
    use crate::tools::Tool;

    fn store() -> (tempfile::TempDir, MemoryStore) {
        let tmp = tempfile::tempdir().unwrap();
        let store = MemoryStore::new(tmp.path(), 2048, 300, 400);
        (tmp, store)
    }

    fn install(ctx: &MemoryContext) {
        set_task_context(Some(Arc::new(TaskContext {
            recipient: None,
            origin: None,
            projects: vec![ctx.project.clone()],
            memory: Some(ctx.clone()),
            session_key: None,
            reset_requested: AtomicBool::new(false),
        })));
    }

    #[test]
    fn personal_memory_does_not_leak_across_conversations() {
        let (_tmp, store) = store();
        let a = MemoryContext::direct(ChannelSource::Telegram, "chat-a", "alice", "notes");
        let b = MemoryContext::direct(ChannelSource::Telegram, "chat-b", "bob", "notes");
        store
            .retain(&a, MemoryTier::Personal, "alice prefers dark mode")
            .unwrap();
        assert!(store.recall(&b, "dark mode", 10).is_empty());
        assert_eq!(store.recall(&a, "dark mode", 10).len(), 1);
    }

    #[test]
    fn group_cannot_read_owner_global_memory() {
        let (_tmp, store) = store();
        let dm = MemoryContext::direct(ChannelSource::Telegram, "alice", "alice", "ledger");
        store
            .retain(&dm, MemoryTier::Global, "bought a banana for 2 yuan")
            .unwrap();

        let group = MemoryContext::group(ChannelSource::Telegram, "group-1", "alice", "cook");
        assert!(store.recall(&group, "banana", 10).is_empty());
        assert!(!store.mental_model(&group).contains("banana"));
    }

    #[test]
    fn tier3_links_projects_in_the_same_dm() {
        let (_tmp, store) = store();
        let ledger = MemoryContext::direct(ChannelSource::Telegram, "alice", "alice", "ledger");
        store
            .retain(&ledger, MemoryTier::Global, "买香蕉 2 元")
            .unwrap();

        let cook = MemoryContext::direct(ChannelSource::Telegram, "alice", "alice", "cook");
        let hits = store.recall(&cook, "香蕉", 10);
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].tier, MemoryTier::Global);
    }

    #[test]
    fn identity_collision_does_not_share_tier3() {
        let (_tmp, store) = store();
        let tg = MemoryContext::direct(ChannelSource::Telegram, "u1", "u1", "ledger");
        store
            .retain(&tg, MemoryTier::Global, "secret plan")
            .unwrap();
        let fs = MemoryContext::direct(ChannelSource::Feishu, "u1", "u1", "ledger");
        assert!(store.recall(&fs, "secret plan", 10).is_empty());
    }

    #[test]
    fn group_cannot_write_global() {
        let (_tmp, store) = store();
        let group = MemoryContext::group(ChannelSource::Telegram, "g", "alice", "notes");
        assert!(store
            .retain(&group, MemoryTier::Global, "something")
            .is_err());
    }

    #[test]
    fn content_cap_is_enforced() {
        let (_tmp, store) = store();
        let ctx = MemoryContext::direct(ChannelSource::Telegram, "u", "u", "p");
        let big = "x".repeat(2049);
        assert!(store.retain(&ctx, MemoryTier::Personal, &big).is_err());
    }

    #[test]
    fn secrets_are_redacted() {
        let (sanitized, n) = screen_secrets("token=abc123 and sk-abcdefghijklmnop leaked");
        assert!(n >= 2);
        assert!(!sanitized.contains("abc123"));
        assert!(!sanitized.contains("sk-abcdefghijklmnop"));
    }

    #[test]
    fn digest_is_bounded() {
        let (_tmp, store) = store();
        let ctx = MemoryContext::direct(ChannelSource::Telegram, "u", "u", "p");
        for i in 0..50 {
            store
                .retain(
                    &ctx,
                    MemoryTier::Personal,
                    &format!("fact number {i} {}", "y".repeat(40)),
                )
                .unwrap();
        }
        let digest = store.mental_model(&ctx);
        assert!(digest.chars().count() <= 420, "{}", digest.chars().count());
    }

    #[test]
    fn project_tier_is_shared_across_conversations() {
        let (_tmp, store) = store();
        let a = MemoryContext::direct(ChannelSource::Telegram, "a", "a", "notes");
        let b = MemoryContext::group(ChannelSource::Telegram, "g", "b", "notes");
        store
            .retain(&a, MemoryTier::Project, "project convention: use UTC")
            .unwrap();
        assert_eq!(store.recall(&b, "UTC", 10).len(), 1);
    }

    #[tokio::test]
    async fn retain_tool_defaults_to_personal_and_writes_only_tier_one() {
        let _guard = test_lock();
        let (_tmp, store) = store();
        let dm = MemoryContext::direct(ChannelSource::Telegram, "chat-a", "alice", "notes");
        install(&dm);
        let tool = RetainTool::new(store.clone());

        let out = tool
            .execute(serde_json::json!({ "content": "alice likes green tea" }))
            .await
            .unwrap();
        assert!(out.text.contains("tier personal"), "{}", out.text);

        // Tier 1 is the default: nothing is written to tier 2 or tier 3.
        let other_project =
            MemoryContext::direct(ChannelSource::Telegram, "chat-a", "alice", "ledger");
        let group_same_project = MemoryContext::group(ChannelSource::Telegram, "g", "bob", "notes");
        assert!(store.recall(&other_project, "green tea", 10).is_empty());
        assert!(store
            .recall(&group_same_project, "green tea", 10)
            .is_empty());
        assert_eq!(store.recall(&dm, "green tea", 10).len(), 1);
        set_task_context(None);
    }

    #[tokio::test]
    async fn retain_tool_honours_an_explicit_project_tier() {
        let _guard = test_lock();
        let (_tmp, store) = store();
        let dm = MemoryContext::direct(ChannelSource::Telegram, "chat-a", "alice", "notes");
        install(&dm);
        let tool = RetainTool::new(store.clone());

        tool.execute(serde_json::json!({
            "content": "project convention: use UTC",
            "tier": "project"
        }))
        .await
        .unwrap();

        let peer = MemoryContext::group(ChannelSource::Telegram, "g", "bob", "notes");
        let hits = store.recall(&peer, "UTC", 10);
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].tier, MemoryTier::Project);
        set_task_context(None);
    }

    #[tokio::test]
    async fn retain_tool_refuses_global_from_a_group_conversation() {
        let _guard = test_lock();
        let (_tmp, store) = store();
        let group = MemoryContext::group(ChannelSource::Telegram, "g", "alice", "notes");
        install(&group);
        let tool = RetainTool::new(store.clone());

        let err = tool
            .execute(serde_json::json!({ "content": "alice's home address", "tier": "global" }))
            .await
            .unwrap_err();
        assert!(err.to_string().contains("group"), "{err}");
        assert!(store.list(&group).is_empty());
        set_task_context(None);
    }

    #[tokio::test]
    async fn retain_tool_rejects_an_unknown_tier_without_writing() {
        let _guard = test_lock();
        let (_tmp, store) = store();
        let dm = MemoryContext::direct(ChannelSource::Telegram, "chat-a", "alice", "notes");
        install(&dm);
        let tool = RetainTool::new(store.clone());

        assert!(tool
            .execute(serde_json::json!({ "content": "x", "tier": "galaxy" }))
            .await
            .is_err());
        assert!(store.list(&dm).is_empty());
        set_task_context(None);
    }

    #[tokio::test]
    async fn recall_tool_reads_the_visible_union_only() {
        let _guard = test_lock();
        let (_tmp, store) = store();
        let dm = MemoryContext::direct(ChannelSource::Telegram, "chat-a", "alice", "notes");
        store
            .retain(&dm, MemoryTier::Global, "banana budget")
            .unwrap();
        let group = MemoryContext::group(ChannelSource::Telegram, "g", "bob", "notes");
        install(&group);
        let tool = RecallTool::new(store.clone());

        let out = tool
            .execute(serde_json::json!({ "query": "banana" }))
            .await
            .unwrap();
        assert_eq!(out.text, "No matching memories.");
        set_task_context(None);
    }
}
