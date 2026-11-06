//! Scheduler configuration types.
//!
//! One entry has exactly one trigger (`cron` / `interval` / `at` / `once`) and
//! exactly one action (`message` -> LLM pipeline, or `script` -> direct run).

use serde::{Deserialize, Serialize};

use crate::identity::{ChannelSource, ConversationKind};

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct Recipient {
    pub source: ChannelSource,
    pub user_id: String,
}

/// Who created a scheduled task, as distinct from where its result is pushed.
///
/// A Feishu recipient is a `chat_id` (a delivery address, and for a group it is
/// the whole group), so it cannot double as the memory principal. Persisting
/// the creator's authenticated principal and conversation kind keeps tier-1
/// memory attributed correctly and keeps group-created schedules out of tier 3.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleOrigin {
    pub source: ChannelSource,
    pub principal_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub conversation_id: Option<String>,
    #[serde(default)]
    pub kind: ConversationKind,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleRetry {
    pub max: u32,
    pub backoff_ms: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct SilentHours {
    pub from: String,
    pub to: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ScheduleEntry {
    pub id: String,
    #[serde(default = "default_true")]
    pub enabled: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cron: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub interval: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub at: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub once: Option<String>,
    pub timezone: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub script: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub script_args: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub commit: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub project: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub retry: Option<ScheduleRetry>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub silent_hours: Option<SilentHours>,
    /// China legal-workday gating: `"workday"` or absent.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub holiday_mode: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub recipient: Option<Recipient>,
    /// Creator identity; absent on hand-edited legacy entries.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub origin: Option<ScheduleOrigin>,
}

fn default_true() -> bool {
    true
}

impl ScheduleEntry {
    pub const DEFAULT_TZ: &'static str = "Asia/Shanghai";

    pub fn timezone_or_default(&self) -> &str {
        if self.timezone.is_empty() {
            Self::DEFAULT_TZ
        } else {
            &self.timezone
        }
    }

    pub fn is_workday_mode(&self) -> bool {
        self.holiday_mode.as_deref() == Some("workday")
    }

    /// The fired text: script entries carry no message.
    pub fn action_message(&self) -> Option<&str> {
        self.message.as_deref()
    }
}
