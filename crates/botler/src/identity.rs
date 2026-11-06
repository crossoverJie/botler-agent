//! Channel-safe identity types.
//!
//! Channel identifiers live in disjoint namespaces and can collide (a numeric
//! Telegram chat id, a Feishu `oc_…` id, and a WeChat `ilink_user_id` are three
//! different identity spaces). Every key therefore carries its source
//! explicitly; raw-id string equality never merges identities.

use std::fmt;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ChannelSource {
    Telegram,
    Feishu,
    Wechat,
    Scheduler,
    Cli,
}

impl ChannelSource {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Telegram => "telegram",
            Self::Feishu => "feishu",
            Self::Wechat => "wechat",
            Self::Scheduler => "scheduler",
            Self::Cli => "cli",
        }
    }

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "telegram" => Some(Self::Telegram),
            "feishu" => Some(Self::Feishu),
            "wechat" => Some(Self::Wechat),
            "scheduler" => Some(Self::Scheduler),
            "cli" => Some(Self::Cli),
            _ => None,
        }
    }

    pub const fn is_im(self) -> bool {
        matches!(self, Self::Telegram | Self::Feishu | Self::Wechat)
    }
}

impl fmt::Display for ChannelSource {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum ConversationKind {
    /// A 1:1 chat; the sender principal owns the memory domain.
    Direct,
    /// A group chat; the group conversation owns the memory domain.
    Group,
    /// Scheduler- or CLI-driven execution: ephemeral session, synthetic tier 1.
    #[default]
    Synthetic,
}

/// The persistent-session key (C1). `project` is only set after routing.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct SessionKey {
    pub source: ChannelSource,
    pub conversation_id: String,
    pub project: String,
}

impl SessionKey {
    pub fn new(
        source: ChannelSource,
        conversation_id: impl Into<String>,
        project: impl Into<String>,
    ) -> Self {
        Self {
            source,
            conversation_id: conversation_id.into(),
            project: project.into(),
        }
    }

    /// Canonical, unambiguous encoding used for hashing and the index.
    pub fn canonical(&self) -> String {
        canonical_parts(&[self.source.as_str(), &self.conversation_id, &self.project])
    }

    pub fn key_hash(&self) -> String {
        sha256_hex(self.canonical().as_bytes())
    }

    /// Opaque session filename — never a concatenation of raw identifiers.
    pub fn opaque_file_name(&self) -> String {
        format!("s-{}.jsonl", &self.key_hash()[..32])
    }
}

/// Authenticated principal identity for tier 3. Cross-channel sharing requires
/// an explicit user-confirmed account-linking map (post-v2.0); until then each
/// channel carries its own namespace.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct PrincipalKey {
    pub source: ChannelSource,
    pub principal_id: String,
}

impl PrincipalKey {
    pub fn new(source: ChannelSource, principal_id: impl Into<String>) -> Self {
        Self {
            source,
            principal_id: principal_id.into(),
        }
    }

    pub fn canonical(&self) -> String {
        canonical_parts(&["principal", self.source.as_str(), &self.principal_id])
    }
}

/// What a memory operation is allowed to see (computed at task-context time).
#[derive(Debug, Clone)]
pub struct MemoryContext {
    pub source: ChannelSource,
    pub kind: ConversationKind,
    pub conversation_id: String,
    pub principal: PrincipalKey,
    pub project: String,
    /// Legacy scheduler entries have no authenticated creator, so they must not
    /// reach tier 3. Set false only for those; everything else keeps tier 3.
    allow_global: bool,
}

impl MemoryContext {
    pub fn direct(
        source: ChannelSource,
        conversation_id: impl Into<String>,
        principal_id: impl Into<String>,
        project: impl Into<String>,
    ) -> Self {
        Self {
            source,
            kind: ConversationKind::Direct,
            conversation_id: conversation_id.into(),
            principal: PrincipalKey::new(source, principal_id),
            project: project.into(),
            allow_global: true,
        }
    }

    pub fn group(
        source: ChannelSource,
        conversation_id: impl Into<String>,
        sender_id: impl Into<String>,
        project: impl Into<String>,
    ) -> Self {
        Self {
            source,
            kind: ConversationKind::Group,
            conversation_id: conversation_id.into(),
            principal: PrincipalKey::new(source, sender_id),
            project: project.into(),
            allow_global: true,
        }
    }

    pub fn synthetic(
        conversation_id: impl Into<String>,
        principal_id: impl Into<String>,
        project: impl Into<String>,
    ) -> Self {
        Self::synthetic_for(
            ChannelSource::Scheduler,
            conversation_id,
            principal_id,
            project,
        )
    }

    /// Scheduler and CLI executions attach a synthetic tier-1 domain owned by
    /// the entry creator (`source` distinguishes the two identity spaces).
    pub fn synthetic_for(
        source: ChannelSource,
        conversation_id: impl Into<String>,
        principal_id: impl Into<String>,
        project: impl Into<String>,
    ) -> Self {
        Self {
            source,
            kind: ConversationKind::Synthetic,
            conversation_id: conversation_id.into(),
            principal: PrincipalKey::new(source, principal_id),
            project: project.into(),
            allow_global: true,
        }
    }

    /// Legacy scheduler tasks cannot authenticate a creator, so they get no
    /// tier 3 while keeping the old recipient-based tier-1/2 scoping.
    pub fn without_global(mut self) -> Self {
        self.allow_global = false;
        self
    }

    /// Tier 3 is visible only to an authenticated owner acting alone — never in
    /// a group conversation, and never for an unauthenticated legacy task.
    pub fn can_read_global(&self) -> bool {
        self.allow_global && !matches!(self.kind, ConversationKind::Group)
    }

    /// Group sessions must never write tier 3 either.
    pub fn can_write_global(&self) -> bool {
        self.can_read_global()
    }
}

/// A memory namespace is identified by tier + stable opaque scope parts.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum MemoryNamespace {
    /// Tier 1: personal conversation tier.
    Personal {
        source: ChannelSource,
        owner: String,
        project: String,
    },
    /// Tier 2: project-shared tier.
    Project { project: String },
    /// Tier 3: user-global tier, keyed by the authenticated principal.
    Global {
        source: ChannelSource,
        principal_id: String,
    },
}

impl MemoryNamespace {
    pub fn personal(ctx: &MemoryContext) -> Self {
        let owner = match ctx.kind {
            ConversationKind::Group => ctx.conversation_id.clone(),
            _ => ctx.principal.principal_id.clone(),
        };
        Self::Personal {
            source: ctx.source,
            owner,
            project: ctx.project.clone(),
        }
    }

    pub fn project(project: impl Into<String>) -> Self {
        Self::Project {
            project: project.into(),
        }
    }

    pub fn global(ctx: &MemoryContext) -> Self {
        Self::Global {
            source: ctx.principal.source,
            principal_id: ctx.principal.principal_id.clone(),
        }
    }

    /// Every namespace this task may read, most-specific first.
    pub fn visible_set(ctx: &MemoryContext) -> Vec<Self> {
        let mut out = vec![Self::personal(ctx), Self::project(ctx.project.clone())];
        if ctx.can_read_global() {
            out.push(Self::global(ctx));
        }
        out
    }

    /// Namespaces this task may write, gated by the tier's explicit rules.
    pub fn writable_set(ctx: &MemoryContext) -> Vec<Self> {
        let mut out = vec![Self::personal(ctx), Self::project(ctx.project.clone())];
        if ctx.can_write_global() {
            out.push(Self::global(ctx));
        }
        out
    }

    pub fn canonical(&self) -> String {
        match self {
            Self::Personal {
                source,
                owner,
                project,
            } => canonical_parts(&["tier1", source.as_str(), owner, project]),
            Self::Project { project } => canonical_parts(&["tier2", project]),
            Self::Global {
                source,
                principal_id,
            } => canonical_parts(&["tier3", source.as_str(), principal_id]),
        }
    }

    /// Opaque on-disk name (never raw identity strings).
    pub fn opaque_file_name(&self) -> String {
        format!("m-{}.jsonl", &sha256_hex(self.canonical().as_bytes())[..32])
    }

    pub fn tier(&self) -> u8 {
        match self {
            Self::Personal { .. } => 1,
            Self::Project { .. } => 2,
            Self::Global { .. } => 3,
        }
    }
}

/// Length-prefixed join: `3:abc|1:x` can never collide with `2:ab|2:cx`.
pub fn canonical_parts(parts: &[&str]) -> String {
    let mut out = String::new();
    for part in parts {
        out.push_str(&part.len().to_string());
        out.push(':');
        out.push_str(part);
        out.push('|');
    }
    out
}

pub fn sha256_hex(bytes: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(bytes);
    hex::encode(hasher.finalize())
}

#[cfg(test)]
mod tests {
    use proptest::prelude::*;

    use super::*;

    #[test]
    fn session_keys_are_channel_namespaced() {
        let tg = SessionKey::new(ChannelSource::Telegram, "123", "notes");
        let fs = SessionKey::new(ChannelSource::Feishu, "123", "notes");
        assert_ne!(tg, fs);
        assert_ne!(tg.opaque_file_name(), fs.opaque_file_name());
    }

    #[test]
    fn canonical_encoding_has_no_separator_collisions() {
        assert_ne!(
            canonical_parts(&["a:b", "c"]),
            canonical_parts(&["a", "b:c"])
        );
    }

    #[test]
    fn opaque_names_are_path_safe() {
        let key = SessionKey::new(ChannelSource::Wechat, "../../etc/passwd", "p");
        let name = key.opaque_file_name();
        assert!(!name.contains('/') && !name.contains(".."));
        assert!(name.starts_with("s-") && name.ends_with(".jsonl"));
    }

    #[test]
    fn group_memory_never_sees_global() {
        let ctx = MemoryContext::group(ChannelSource::Telegram, "g1", "u1", "notes");
        let visible = MemoryNamespace::visible_set(&ctx);
        assert_eq!(visible.len(), 2);
        assert!(!visible.iter().any(|n| n.tier() == 3));
    }

    #[test]
    fn direct_memory_sees_all_three_tiers() {
        let ctx = MemoryContext::direct(ChannelSource::Telegram, "u1", "u1", "notes");
        let visible = MemoryNamespace::visible_set(&ctx);
        assert_eq!(
            visible
                .iter()
                .map(MemoryNamespace::tier)
                .collect::<Vec<_>>(),
            vec![1, 2, 3]
        );
    }

    #[test]
    fn identity_collision_across_channels_stays_separate() {
        let tg = MemoryContext::direct(ChannelSource::Telegram, "u1", "u1", "p");
        let fs = MemoryContext::direct(ChannelSource::Feishu, "u1", "u1", "p");
        assert_ne!(
            MemoryNamespace::global(&tg).canonical(),
            MemoryNamespace::global(&fs).canonical()
        );
    }

    proptest! {
        #![proptest_config(ProptestConfig::with_cases(48))]

        // Scenario 10: hostile conversation ids must still yield a safe,
        // fixed-shape opaque filename (no separators, no traversal, no NUL).
        #[test]
        fn opaque_session_names_are_always_path_safe(
            conversation_id in any::<String>(),
            project in any::<String>(),
        ) {
            let key = SessionKey::new(ChannelSource::Telegram, conversation_id, project);
            let name = key.opaque_file_name();
            prop_assert_eq!(name.len(), 2 + 32 + 6);
            prop_assert!(name.starts_with("s-") && name.ends_with(".jsonl"));
            let digest = &name[2..34];
            prop_assert!(digest.chars().all(|c| c.is_ascii_hexdigit()));
            prop_assert!(!name.contains('/') && !name.contains('\\') && !name.contains(".."));
        }

        #[test]
        fn distinct_session_keys_never_share_an_opaque_name(
            a in any::<String>(),
            b in any::<String>(),
        ) {
            let first = SessionKey::new(ChannelSource::Telegram, &a, "notes");
            let second = SessionKey::new(ChannelSource::Telegram, &b, "notes");
            if a != b {
                prop_assert_ne!(first.opaque_file_name(), second.opaque_file_name());
            }
        }

        #[test]
        fn opaque_memory_names_are_always_path_safe(
            conversation in any::<String>(),
            principal in any::<String>(),
            project in any::<String>(),
        ) {
            let ctx = MemoryContext::direct(ChannelSource::Wechat, conversation, principal, project);
            for ns in MemoryNamespace::visible_set(&ctx) {
                let name = ns.opaque_file_name();
                prop_assert!(name.starts_with("m-") && name.ends_with(".jsonl"));
                prop_assert!(!name.contains('/') && !name.contains('\\') && !name.contains(".."));
            }
        }
    }
}
