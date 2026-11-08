//! The single seam between the deterministic core and the embedded agent SDK.
//!
//! Everything pi-specific (session files, tool adaptation, provider registry)
//! lives behind these traits. In production `pi_bridge` implements them; in
//! tests `StubRuntime` implements them deterministically, so the dispatcher,
//! registry, rotation and memory rules are testable without a network or an
//! LLM. Spike S-1/S-5 gate the real implementation; this seam is what keeps
//! the rest of the crate independent of that answer.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use anyhow::{bail, Result};
use async_trait::async_trait;

use crate::identity::SessionKey;
use crate::prompt::RouteDecision;
use crate::tools::Toolset;

/// Decoded inbound image held until the routed project is known.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InboundImage {
    pub bytes: Vec<u8>,
    pub mime_type: String,
    /// `jpg` | `png` | `gif` | `webp`
    pub ext: String,
}

/// What one agent turn produced.
#[derive(Debug, Clone, Default)]
pub struct AgentReply {
    pub text: String,
    /// Tool names in call order; `write`/`edit`/`run` drive the validate/commit
    /// path (`run` executes project scripts that may rewrite data files).
    pub tool_names: Vec<String>,
    /// Set when the model call failed or was aborted; the runner turns it into
    /// a user-visible error instead of an empty reply.
    pub error: Option<String>,
}

impl AgentReply {
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            tool_names: Vec::new(),
            error: None,
        }
    }

    pub fn mutated(&self) -> bool {
        // `run` is included: a project script can rewrite data without any
        // write/edit call, so the validate/commit pass must still happen.
        self.tool_names
            .iter()
            .any(|name| matches!(name.as_str(), "write" | "edit" | "run"))
    }
}

/// Everything needed to open (or resume) one session. `session_file` is
/// opaque (§4.8); the runtime owns its format.
#[derive(Clone)]
pub struct SessionSpec {
    pub key: SessionKey,
    pub workspace: PathBuf,
    pub session_file: PathBuf,
    pub system_prompt: String,
    pub provider: String,
    pub model: String,
    pub tools: Toolset,
    /// Scheduler / CLI / self-heal: no persisted session.
    pub ephemeral: bool,
}

/// A live handle. The registry keeps at most `hot_limit` of them.
#[async_trait]
pub trait AgentSession: Send {
    async fn prompt(&mut self, text: &str, images: &[InboundImage]) -> Result<AgentReply>;
    fn session_path(&self) -> &Path;
}

#[derive(Debug, Clone)]
pub struct RouteRequest {
    pub message: String,
    pub candidates: Vec<String>,
    pub include_scheduler: bool,
    pub has_images: bool,
    pub allow_reset: bool,
    pub provider: String,
    pub model: String,
    pub prompt: String,
}

#[async_trait]
pub trait AgentRuntime: Send + Sync {
    async fn open_session(&self, spec: SessionSpec) -> Result<Box<dyn AgentSession>>;
    async fn route(&self, request: RouteRequest) -> Result<RouteDecision>;
}

/// Test hook invoked inside a simulated turn (e.g. to simulate a tool that
/// requests a context reset or writes the session file).
pub type PromptHook = Arc<dyn Fn(&SessionKey, &Path, &str) + Send + Sync>;

/// Deterministic runtime for tests and for running the skeleton without a
/// provider. It never touches the network.
#[derive(Default)]
pub struct StubRuntime {
    route_override: Mutex<Option<RouteDecision>>,
    reply: Mutex<AgentReply>,
    prompts: Arc<Mutex<Vec<(SessionKey, String)>>>,
    opened: Arc<Mutex<Vec<(SessionKey, bool)>>>,
    fail_resume_once: AtomicBool,
    on_prompt: Mutex<Option<PromptHook>>,
}

impl StubRuntime {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn with_reply(self, text: impl Into<String>) -> Self {
        *self.reply.lock().unwrap() = AgentReply::text(text);
        self
    }

    pub fn with_tool_names(self, names: &[&str]) -> Self {
        self.reply.lock().unwrap().tool_names = names.iter().map(|s| s.to_string()).collect();
        self
    }

    pub fn with_route(self, projects: &[&str]) -> Self {
        *self.route_override.lock().unwrap() = Some(RouteDecision {
            projects: projects.iter().map(|s| s.to_string()).collect(),
            attachment_project: projects.first().map(|s| s.to_string()),
            reset_context: false,
        });
        self
    }

    pub fn fail_resume_once(&self) {
        self.fail_resume_once.store(true, Ordering::SeqCst);
    }

    pub fn on_prompt(
        self,
        hook: impl Fn(&SessionKey, &Path, &str) + Send + Sync + 'static,
    ) -> Self {
        *self.on_prompt.lock().unwrap() = Some(Arc::new(hook));
        self
    }

    pub fn prompts(&self) -> Vec<(SessionKey, String)> {
        self.prompts.lock().unwrap().clone()
    }

    pub fn opened(&self) -> Vec<(SessionKey, bool)> {
        self.opened.lock().unwrap().clone()
    }
}

#[async_trait]
impl AgentRuntime for StubRuntime {
    async fn open_session(&self, spec: SessionSpec) -> Result<Box<dyn AgentSession>> {
        let resume = spec.session_file.exists();
        if resume && self.fail_resume_once.swap(false, Ordering::SeqCst) {
            bail!("simulated incompatible session format");
        }
        self.opened.lock().unwrap().push((spec.key.clone(), resume));
        Ok(Box::new(StubSession {
            path: spec.session_file,
            reply: self.reply.lock().unwrap().clone(),
            prompts: Arc::clone(&self.prompts),
            key: spec.key,
            hook: self.on_prompt.lock().unwrap().clone(),
        }))
    }

    async fn route(&self, request: RouteRequest) -> Result<RouteDecision> {
        if let Some(decision) = self.route_override.lock().unwrap().clone() {
            return Ok(decision);
        }
        let lower = request.message.to_lowercase();
        if request.allow_reset
            && (lower.contains("重置上下文")
                || lower.contains("reset context")
                || lower.contains("忽略上文"))
        {
            return Ok(RouteDecision {
                projects: vec![],
                attachment_project: None,
                reset_context: true,
            });
        }
        let matched: Vec<String> = request
            .candidates
            .iter()
            .filter(|name| lower.contains(&name.to_lowercase()))
            .cloned()
            .collect();
        if matched.is_empty() {
            return Ok(RouteDecision {
                projects: vec![],
                attachment_project: None,
                reset_context: false,
            });
        }
        Ok(RouteDecision {
            projects: matched.clone(),
            attachment_project: matched.first().cloned(),
            reset_context: false,
        })
    }
}

struct StubSession {
    path: PathBuf,
    reply: AgentReply,
    prompts: Arc<Mutex<Vec<(SessionKey, String)>>>,
    key: SessionKey,
    hook: Option<PromptHook>,
}

#[async_trait]
impl AgentSession for StubSession {
    async fn prompt(&mut self, text: &str, _images: &[InboundImage]) -> Result<AgentReply> {
        self.prompts
            .lock()
            .unwrap()
            .push((self.key.clone(), text.to_string()));
        if let Some(hook) = &self.hook {
            hook(&self.key, &self.path, text);
        }
        if let Some(error) = &self.reply.error {
            bail!("{error}");
        }
        Ok(self.reply.clone())
    }

    fn session_path(&self) -> &Path {
        &self.path
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn run_turns_count_as_mutating() {
        let reply = AgentReply {
            tool_names: vec!["read".into(), "run".into()],
            ..AgentReply::default()
        };
        assert!(reply.mutated());
        let read_only = AgentReply {
            tool_names: vec!["read".into(), "memory_recall".into()],
            ..AgentReply::default()
        };
        assert!(!read_only.mutated());
    }
}
