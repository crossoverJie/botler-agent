use anyhow::{Context, Result};
use async_trait::async_trait;
use serde_json::{json, Value};

use crate::paths::PathGuard;
use crate::tools::task_context::task_projects;
use crate::tools::{Tool, ToolOutput};

pub struct ReadTool {
    guard: PathGuard,
}

impl ReadTool {
    pub fn new(guard: PathGuard) -> Self {
        Self { guard }
    }
}

#[async_trait]
impl Tool for ReadTool {
    fn name(&self) -> &str {
        "read"
    }

    fn label(&self) -> &str {
        "Read file"
    }

    fn description(&self) -> &str {
        "Read the content of a file inside the data directory. The path is relative to DATA_ROOT and limited to data subprojects under DATA_ROOT. Returns the raw file content (JSON files as JSON text) so you can understand the current data before deciding how to modify it."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "path": { "type": "string", "description": "File path relative to DATA_ROOT, e.g. project-name/data/xxx.json" }
            },
            "required": ["path"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let path = args
            .get("path")
            .and_then(Value::as_str)
            .context("read requires a string path")?;
        let abs = self.guard.safe_path(path, task_projects().as_deref())?;
        let content = tokio::fs::read_to_string(&abs)
            .await
            .with_context(|| format!("read {path}"))?;
        Ok(ToolOutput::text(content))
    }
}
