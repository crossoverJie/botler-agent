//! Agent tools over the DATA_ROOT allowlist.
//!
//! The trait is intentionally small so the pi bridge can adapt it to pi's
//! `Tool` without dragging pi types through the deterministic core (spike
//! S-2). Every filesystem tool resolves its path through [`PathGuard`], which
//! remains the single security boundary.

pub mod clear;
pub mod edit;
pub mod memory;
pub mod read;
pub mod run;
pub mod schedule;
pub mod task_context;
pub mod write;

use std::sync::Arc;

use anyhow::Result;
use async_trait::async_trait;
use serde_json::Value;

/// Output of a tool call. `mutated` drives the dispatcher's validate/commit
/// decision; it is set by the tool itself so a failed write cannot pretend to
/// have changed data.
#[derive(Debug, Clone)]
pub struct ToolOutput {
    pub text: String,
    pub mutated: bool,
}

impl ToolOutput {
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            mutated: false,
        }
    }

    pub fn mutated(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            mutated: true,
        }
    }
}

#[async_trait]
pub trait Tool: Send + Sync {
    fn name(&self) -> &str;
    fn label(&self) -> &str;
    fn description(&self) -> &str;
    fn parameters(&self) -> Value;
    async fn execute(&self, args: Value) -> Result<ToolOutput>;
}

/// Per-turn toolset handed to the runtime. Keep order stable: the model sees it.
#[derive(Clone, Default)]
pub struct Toolset {
    tools: Vec<Arc<dyn Tool>>,
}

impl Toolset {
    pub fn new(tools: Vec<Arc<dyn Tool>>) -> Self {
        Self { tools }
    }

    pub fn tools(&self) -> &[Arc<dyn Tool>] {
        &self.tools
    }

    pub fn get(&self, name: &str) -> Option<Arc<dyn Tool>> {
        self.tools.iter().find(|t| t.name() == name).cloned()
    }

    pub fn names(&self) -> Vec<&str> {
        self.tools.iter().map(|t| t.name()).collect()
    }

    pub fn is_empty(&self) -> bool {
        self.tools.is_empty()
    }
}
