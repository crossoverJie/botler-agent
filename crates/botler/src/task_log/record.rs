//! Task-log record schema, kept JSON-shape compatible with the v1 WebUI.
//!
//! One record is appended per dispatch execution, plus one `self-heal` record
//! when validation failed and the framework retried. The deterministic core
//! runs against `StubRuntime`: there are no LLM calls, so tool/usage/
//! conversation fields stay empty and `degraded` marks that honestly.

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::prompt::SCHEDULER_VIRTUAL_PROJECT;

fn is_false(value: &bool) -> bool {
    !*value
}

fn is_none<T>(value: &Option<T>) -> bool {
    value.is_none()
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct TokenUsageLog {
    pub input: u64,
    pub output: u64,
    pub cache_read: u64,
    pub cache_write: u64,
    #[serde(skip_serializing_if = "is_none")]
    pub reasoning: Option<u64>,
    pub total: u64,
    pub cost_usd: f64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct ModelCacheStats {
    pub queries: u64,
    pub hits: u64,
    pub hit_rate: f64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct ToolCallLog {
    pub id: String,
    pub name: String,
    pub arguments: serde_json::Value,
    pub result_text: String,
    pub result_truncated: bool,
    #[serde(skip_serializing_if = "is_none")]
    pub result_chars: Option<u64>,
    pub is_error: bool,
    pub started_at: i64,
    pub duration_ms: i64,
    #[serde(skip_serializing_if = "is_none")]
    pub details: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct ConversationEntry {
    pub role: String,
    pub timestamp: i64,
    #[serde(skip_serializing_if = "is_none")]
    pub text: Option<String>,
    #[serde(skip_serializing_if = "is_none")]
    pub thinking: Option<String>,
    #[serde(skip_serializing_if = "is_none")]
    pub chars: Option<u64>,
    #[serde(skip_serializing_if = "is_none")]
    pub stop_reason: Option<String>,
    #[serde(skip_serializing_if = "is_none")]
    pub tool_name: Option<String>,
    #[serde(skip_serializing_if = "is_none")]
    pub is_error: Option<bool>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct CallUsage {
    pub timestamp: i64,
    #[serde(skip_serializing_if = "is_none")]
    pub stop_reason: Option<String>,
    pub input: u64,
    pub output: u64,
    pub cache_read: u64,
    pub cache_write: u64,
    #[serde(skip_serializing_if = "is_none")]
    pub reasoning: Option<u64>,
    pub total: u64,
    pub cost_usd: f64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct RoutingLog {
    pub candidates: Vec<String>,
    pub decision: Option<String>,
    pub usage: TokenUsageLog,
    #[serde(skip_serializing_if = "is_none")]
    pub prompt: Option<String>,
}

/// A single task-log line. Field names/shapes mirror `src/logging/types.ts` on
/// the v1 branch so both branches can read the same JSONL files during a
/// migration. Unknown fields in v1 records are ignored by serde.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
#[serde(default, rename_all = "camelCase")]
pub struct TaskLog {
    pub id: String,
    pub task_id: String,
    pub phase: String,
    pub source: String,
    pub provider: String,
    pub model: String,
    pub project: Option<String>,
    pub projects: Vec<String>,
    pub status: String,
    pub started_at: i64,
    pub ended_at: i64,
    pub duration_ms: i64,
    pub user_message: String,
    pub reply_text: String,
    pub images: Vec<String>,
    #[serde(skip_serializing_if = "is_none")]
    pub inbound_image_count: Option<usize>,
    pub mutated: bool,
    pub tools: Vec<ToolCallLog>,
    pub conversation: Vec<ConversationEntry>,
    pub usage: TokenUsageLog,
    pub calls: Vec<CallUsage>,
    #[serde(skip_serializing_if = "is_none")]
    pub system_prompt: Option<String>,
    pub model_cache: ModelCacheStats,
    #[serde(skip_serializing_if = "is_none")]
    pub routing: Option<RoutingLog>,
    /// v2-only marker: no real model ran, so usage/tool/conversation data is
    /// empty. v1 readers ignore this extra field.
    #[serde(skip_serializing_if = "is_false")]
    pub degraded: bool,
}

/// Everything a dispatcher turn can supply. Timestamps and ids are explicit so
/// the constructor stays pure and testable (no clock or randomness inside).
#[derive(Debug, Clone)]
pub struct RecordInput {
    pub id: String,
    pub task_id: String,
    pub phase: String,
    pub source: String,
    pub provider: String,
    pub model: String,
    pub projects: Vec<String>,
    pub status: String,
    pub started_ms: i64,
    pub ended_ms: i64,
    pub user_message: String,
    pub reply_text: String,
    pub images: Vec<String>,
    pub inbound_image_count: Option<usize>,
    pub mutated: bool,
}

impl TaskLog {
    pub fn new(input: RecordInput) -> Self {
        let project = legacy_project(&input.projects);
        Self {
            id: input.id,
            task_id: input.task_id,
            phase: input.phase,
            source: input.source,
            provider: input.provider,
            model: input.model,
            project,
            projects: input.projects,
            status: input.status,
            started_at: input.started_ms,
            ended_at: input.ended_ms,
            duration_ms: (input.ended_ms - input.started_ms).max(0),
            user_message: input.user_message,
            reply_text: input.reply_text,
            images: input.images,
            inbound_image_count: input.inbound_image_count,
            mutated: input.mutated,
            degraded: true,
            ..Self::default()
        }
    }
}

/// Legacy singular display field: non-null only for exactly one real data
/// project. The virtual `__scheduler__` project never counts.
pub fn legacy_project(projects: &[String]) -> Option<String> {
    (projects.len() == 1 && projects[0] != SCHEDULER_VIRTUAL_PROJECT).then(|| projects[0].clone())
}

/// Deterministic, opaque id derived from explicit inputs plus a caller-supplied
/// sequence. Avoids `uuid`/randomness so tests are reproducible.
pub fn make_id(task_id: &str, phase: &str, ended_ms: i64, seq: u64) -> String {
    let mut hasher = Sha256::new();
    hasher.update(task_id.as_bytes());
    hasher.update(b"|");
    hasher.update(phase.as_bytes());
    hasher.update(b"|");
    hasher.update(ended_ms.to_string().as_bytes());
    hasher.update(b"|");
    hasher.update(seq.to_string().as_bytes());
    let digest = hasher.finalize();
    hex::encode(&digest[..16])
}

#[cfg(test)]
mod tests {
    use super::*;

    fn input() -> RecordInput {
        RecordInput {
            id: "id-1".into(),
            task_id: "task-1".into(),
            phase: "execute".into(),
            source: "cli".into(),
            provider: "stub".into(),
            model: "stub".into(),
            projects: vec!["cook".into()],
            status: "success".into(),
            started_ms: 1_700_000_000_000,
            ended_ms: 1_700_000_000_123,
            user_message: "hi".into(),
            reply_text: "ok".into(),
            images: vec![],
            inbound_image_count: Some(0),
            mutated: false,
        }
    }

    #[test]
    fn record_round_trips_with_v1_field_names() {
        let log = TaskLog::new(input());
        let value = serde_json::to_value(&log).unwrap();
        let obj = value.as_object().unwrap();
        for key in [
            "id",
            "taskId",
            "phase",
            "source",
            "provider",
            "model",
            "project",
            "projects",
            "status",
            "startedAt",
            "endedAt",
            "durationMs",
            "userMessage",
            "replyText",
            "images",
            "inboundImageCount",
            "mutated",
            "tools",
            "conversation",
            "usage",
            "calls",
            "modelCache",
            "degraded",
        ] {
            assert!(obj.contains_key(key), "missing {key}");
        }
        assert_eq!(obj["durationMs"], 123);
        assert_eq!(obj["project"], "cook");
        assert_eq!(obj["degraded"], true);
        assert_eq!(obj["usage"]["cacheRead"], 0);
        assert!(!obj.contains_key("systemPrompt"));
        assert!(!obj.contains_key("routing"));

        let back: TaskLog = serde_json::from_value(value).unwrap();
        assert_eq!(back, log);
    }

    #[test]
    fn legacy_project_only_for_a_single_real_project() {
        assert_eq!(legacy_project(&["cook".into()]), Some("cook".into()));
        assert_eq!(legacy_project(&[]), None);
        assert_eq!(legacy_project(&["a".into(), "b".into()]), None);
        assert_eq!(legacy_project(&[SCHEDULER_VIRTUAL_PROJECT.into()]), None);
    }

    #[test]
    fn make_id_is_deterministic_and_seq_sensitive() {
        let a = make_id("task-1", "execute", 42, 0);
        let b = make_id("task-1", "execute", 42, 0);
        let c = make_id("task-1", "execute", 42, 1);
        let d = make_id("task-1", "self-heal", 42, 0);
        assert_eq!(a, b);
        assert_ne!(a, c);
        assert_ne!(a, d);
        assert_eq!(a.len(), 32);
    }

    #[test]
    fn v1_shaped_record_deserializes_without_degraded() {
        let raw = r#"{"id":"x","taskId":"t","phase":"execute","source":"cli","provider":"p","model":"m","project":null,"projects":[],"status":"success","startedAt":1,"endedAt":2,"durationMs":1,"userMessage":"u","replyText":"r","images":[],"mutated":false,"tools":[],"conversation":[],"usage":{"input":5,"output":2,"cacheRead":1,"cacheWrite":0,"total":8,"costUsd":0},"calls":[],"modelCache":{"queries":0,"hits":0,"hitRate":0}}"#;
        let log: TaskLog = serde_json::from_str(raw).unwrap();
        assert_eq!(log.usage.input, 5);
        assert!(!log.degraded);
        assert!(log.projects.is_empty());
    }
}
