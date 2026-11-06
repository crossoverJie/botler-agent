use anyhow::{bail, Context, Result};
use async_trait::async_trait;
use serde_json::{json, Value};

use crate::paths::PathGuard;
use crate::tools::task_context::task_projects;
use crate::tools::{Tool, ToolOutput};

pub struct WriteTool {
    guard: PathGuard,
}

impl WriteTool {
    pub fn new(guard: PathGuard) -> Self {
        Self { guard }
    }
}

#[async_trait]
impl Tool for WriteTool {
    fn name(&self) -> &str {
        "write"
    }

    fn label(&self) -> &str {
        "Write JSON file"
    }

    fn description(&self) -> &str {
        "Write data (object or array) to a file, automatically serialized into formatted valid JSON. Used to create or modify JSON files inside a data subproject under DATA_ROOT. Pass the data structure itself (not JSON text): this tool handles serialization."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "path": { "type": "string", "description": "Target file path relative to DATA_ROOT (inside a data subproject)" },
                "content": { "description": "JSON value to write (object or array). Pass the data structure itself, not a JSON text string." }
            },
            "required": ["path", "content"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let path = args
            .get("path")
            .and_then(Value::as_str)
            .context("write requires a string path")?;
        let raw = args.get("content").context("write requires content")?;
        // Recovery: models occasionally pass a JSON text string; parse once so
        // it is not double-encoded, and reject anything that is not an object.
        let value = match raw {
            Value::String(text) => serde_json::from_str::<Value>(text).map_err(|_| {
                anyhow::anyhow!(
                    "The write tool received a string for content, and it is not valid JSON text. Pass the data structure itself (object or array)."
                )
            })?,
            other => other.clone(),
        };
        if !value.is_object() && !value.is_array() {
            bail!("The write tool's content must be an object or array");
        }
        let abs = self.guard.safe_path(path, task_projects().as_deref())?;
        if let Some(parent) = abs.parent() {
            tokio::fs::create_dir_all(parent)
                .await
                .with_context(|| format!("create {}", parent.display()))?;
        }
        let text = serde_json::to_string_pretty(&value)?;
        tokio::fs::write(&abs, &text)
            .await
            .with_context(|| format!("write {path}"))?;
        Ok(ToolOutput::mutated(format!(
            "Wrote {path} ({} bytes, valid JSON)",
            text.len()
        )))
    }
}
