//! `clear_conversation_context` — flag now, rotate after the turn (C4).
//!
//! The tool never touches session files. It only sets the active task's reset
//! flag; the dispatcher rotates after validate/commit, when the live handle is
//! no longer writing.

use anyhow::{bail, Result};
use async_trait::async_trait;
use serde_json::{json, Value};

use crate::tools::task_context::request_conversation_reset;
use crate::tools::{Tool, ToolOutput};

pub struct ClearConversationTool;

#[async_trait]
impl Tool for ClearConversationTool {
    fn name(&self) -> &str {
        "clear_conversation_context"
    }

    fn label(&self) -> &str {
        "Clear conversation context"
    }

    fn description(&self) -> &str {
        "Start a fresh conversation context for this chat and project. Call it only when the user explicitly asks to clear / ignore / reset the previous context; then finish any concrete task in the current message. The actual rotation happens after this turn, never inside it."
    }

    fn parameters(&self) -> Value {
        json!({ "type": "object", "properties": {} })
    }

    async fn execute(&self, _args: Value) -> Result<ToolOutput> {
        if !request_conversation_reset() {
            bail!("clear_conversation_context is only available inside an IM conversation");
        }
        Ok(ToolOutput::text(
            "Conversation reset requested. It takes effect after this turn completes.",
        ))
    }
}
