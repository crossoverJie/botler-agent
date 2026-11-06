//! `retain` / `recall` memory tools.
//!
//! The active task's [`MemoryContext`] decides the visible/writable union: a
//! group session never sees or writes tier 3, and tier 2/3 writes require the
//! explicit `tier` parameter (never the default).

use anyhow::{Context, Result};
use async_trait::async_trait;
use serde_json::{json, Value};

use crate::memory::{MemoryStore, MemoryTier};
use crate::tools::task_context::task_memory_context;
use crate::tools::{Tool, ToolOutput};

pub struct RetainTool {
    store: MemoryStore,
}

impl RetainTool {
    pub fn new(store: MemoryStore) -> Self {
        Self { store }
    }
}

#[async_trait]
impl Tool for RetainTool {
    fn name(&self) -> &str {
        "retain"
    }

    fn label(&self) -> &str {
        "Retain a memory"
    }

    fn description(&self) -> &str {
        "Persist a durable fact for later recall. Default tier is this conversation (personal). Writing to \"project\" (shared by everyone in this data project) or \"global\" (cross-project facts about the user) requires an explicit tier parameter. Never retain health, relationship, precise-location, credential, or detailed financial data to \"global\" unless the user explicitly asked. Group chats cannot write \"global\"."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "content": { "type": "string", "description": "The fact to remember, concise and self-contained." },
                "tier": { "type": "string", "enum": ["personal", "project", "global"], "description": "Default personal. project/global require this explicit choice." }
            },
            "required": ["content"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let ctx = task_memory_context().context("retain is not available outside a task")?;
        let content = args
            .get("content")
            .and_then(Value::as_str)
            .context("retain requires content")?;
        let tier = match args.get("tier").and_then(Value::as_str) {
            None => MemoryTier::Personal,
            Some(raw) => {
                MemoryTier::parse(raw).with_context(|| format!("unknown memory tier \"{raw}\""))?
            }
        };
        let record = self.store.retain(&ctx, tier, content)?;
        Ok(ToolOutput::text(format!(
            "Retained memory {} in tier {}.",
            record.id,
            tier.as_str()
        )))
    }
}

pub struct RecallTool {
    store: MemoryStore,
}

impl RecallTool {
    pub fn new(store: MemoryStore) -> Self {
        Self { store }
    }
}

#[async_trait]
impl Tool for RecallTool {
    fn name(&self) -> &str {
        "recall"
    }

    fn label(&self) -> &str {
        "Recall memories"
    }

    fn description(&self) -> &str {
        "Search memories visible to this conversation (personal + project-shared, plus user-global in a 1:1 chat). Use it when a fact from an earlier task or another project may matter."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "query": { "type": "string", "description": "Keywords to search for." },
                "limit": { "type": "integer", "minimum": 1, "maximum": 20 }
            },
            "required": ["query"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let ctx = task_memory_context().context("recall is not available outside a task")?;
        let query = args
            .get("query")
            .and_then(Value::as_str)
            .context("recall requires a query")?;
        let limit = args
            .get("limit")
            .and_then(Value::as_u64)
            .unwrap_or(5)
            .clamp(1, 20) as usize;
        let hits = self.store.recall(&ctx, query, limit);
        if hits.is_empty() {
            return Ok(ToolOutput::text("No matching memories."));
        }
        let mut out = String::from("Matching memories:\n");
        for hit in hits {
            out.push_str(&format!(
                "- [tier {}] ({}) {}\n",
                hit.tier.tier(),
                hit.record.created_at,
                hit.record.content
            ));
        }
        Ok(ToolOutput::text(out.trim_end().to_string()))
    }
}
