use anyhow::{bail, Context, Result};
use async_trait::async_trait;
use serde_json::{json, Value};

use crate::paths::PathGuard;
use crate::tools::task_context::task_projects;
use crate::tools::{Tool, ToolOutput};

pub struct EditTool {
    guard: PathGuard,
}

impl EditTool {
    pub fn new(guard: PathGuard) -> Self {
        Self { guard }
    }
}

#[async_trait]
impl Tool for EditTool {
    fn name(&self) -> &str {
        "edit"
    }

    fn label(&self) -> &str {
        "Text replacement"
    }

    fn description(&self) -> &str {
        "Replace oldText with newText in a file. oldText must match the file content character-for-character (including spaces and newlines), otherwise it fails. all=true replaces all matches, otherwise only the first."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "path": { "type": "string", "description": "File path relative to DATA_ROOT" },
                "oldText": { "type": "string", "description": "Original text to be replaced; must match exactly" },
                "newText": { "type": "string", "description": "Replacement text" },
                "all": { "type": "boolean", "description": "Replace all matches; default false" }
            },
            "required": ["path", "oldText", "newText"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let path = args
            .get("path")
            .and_then(Value::as_str)
            .context("edit requires a string path")?;
        let old_text = args
            .get("oldText")
            .and_then(Value::as_str)
            .context("edit requires oldText")?;
        let new_text = args
            .get("newText")
            .and_then(Value::as_str)
            .context("edit requires newText")?;
        let all = args.get("all").and_then(Value::as_bool).unwrap_or(false);
        if old_text.is_empty() {
            bail!("oldText must not be empty");
        }
        let abs = self.guard.safe_path(path, task_projects().as_deref())?;
        let current = tokio::fs::read_to_string(&abs)
            .await
            .with_context(|| format!("read {path}"))?;
        if !current.contains(old_text) {
            let preview: String = old_text.chars().take(60).collect();
            bail!("oldText not found in the original file (first 60 chars): {preview}");
        }
        let next = if all {
            current.replace(old_text, new_text)
        } else {
            current.replacen(old_text, new_text, 1)
        };
        tokio::fs::write(&abs, next)
            .await
            .with_context(|| format!("write {path}"))?;
        Ok(ToolOutput::mutated(format!("Updated {path}")))
    }
}
