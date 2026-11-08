//! Two-phase task runner: route → select/create session → execute.
//!
//! Routing runs for every message (C1) and inside the dispatcher's queued
//! task (C2). The session key is chosen only after routing resolves a project.
//! Scheduler / CLI / self-heal executions are ephemeral (`no_session`), while
//! IM executions persist under `(source, conversation_id, project)`.

use std::path::Path;
use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::sync::Mutex as AsyncMutex;

use crate::config::Config;
use crate::greeting::{context_reset_reply, fallback_unknown_reply, greeting_reply, is_greeting};
use crate::identity::{ChannelSource, ConversationKind, MemoryContext, SessionKey};
use crate::memory::MemoryStore;
use crate::paths::PathGuard;
use crate::prompt::{
    build_preamble, build_route_prompt, build_system_prompt, list_project_dirs, load_base_template,
    project_capabilities, RouteDecision, SCHEDULER_VIRTUAL_PROJECT,
};
use crate::runtime::{AgentRuntime, InboundImage, RouteRequest, SessionSpec};
use crate::scheduler::holidays::HolidayCalendar;
use crate::scheduler::types::{Recipient, ScheduleOrigin};
use crate::session_registry::{
    file_fingerprint, fingerprint, SessionMeta, SessionRegistry, TOOL_POLICY_VERSION,
};
use crate::tools::clear::ClearConversationTool;
use crate::tools::edit::EditTool;
use crate::tools::memory::{RecallTool, RetainTool};
use crate::tools::read::ReadTool;
use crate::tools::run::RunTool;
use crate::tools::schedule::ScheduleTool;
use crate::tools::task_context::{set_task_context, take_reset_requested, TaskContext};
use crate::tools::write::WriteTool;
use crate::tools::{Tool, Toolset};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Phase {
    Execute,
    SelfHeal,
}

#[derive(Debug, Clone)]
pub struct RunContext {
    pub task_id: String,
    pub source: ChannelSource,
    pub phase: Phase,
    pub project_hint: Option<String>,
    pub projects_hint: Option<Vec<String>>,
    pub recipient: Option<Recipient>,
    pub inbound_images: Vec<InboundImage>,
    /// IM peer/group id. Absent for scheduler / CLI / self-heal.
    pub conversation_id: Option<String>,
    pub sender_id: Option<String>,
    pub conversation_kind: ConversationKind,
    /// Scheduler-created task: the entry creator (memory ownership), separate
    /// from `recipient` (the push address).
    pub origin: Option<ScheduleOrigin>,
}

impl RunContext {
    pub fn cli(task_id: impl Into<String>) -> Self {
        Self {
            task_id: task_id.into(),
            source: ChannelSource::Cli,
            phase: Phase::Execute,
            project_hint: None,
            projects_hint: None,
            recipient: None,
            inbound_images: Vec::new(),
            conversation_id: None,
            sender_id: None,
            conversation_kind: ConversationKind::Synthetic,
            origin: None,
        }
    }

    pub fn im(
        task_id: impl Into<String>,
        source: ChannelSource,
        conversation_id: impl Into<String>,
        sender_id: impl Into<String>,
        kind: ConversationKind,
    ) -> Self {
        Self {
            task_id: task_id.into(),
            source,
            phase: Phase::Execute,
            project_hint: None,
            projects_hint: None,
            recipient: None,
            inbound_images: Vec::new(),
            conversation_id: Some(conversation_id.into()),
            sender_id: Some(sender_id.into()),
            conversation_kind: kind,
            origin: None,
        }
    }
}

#[derive(Debug, Clone)]
pub struct TaskResult {
    pub text: String,
    pub images: Vec<String>,
    pub mutated: bool,
    /// Real data subprojects selected by routing (never the virtual project).
    pub projects: Vec<String>,
    pub session_key: Option<SessionKey>,
    /// A `clear_conversation_context` request to execute after this turn.
    pub reset_session: Option<SessionKey>,
    /// A routing-level reset: rotate every session of this conversation.
    pub reset_conversation: bool,
    /// User-visible context-recovery notice (corrupt resume).
    pub notice: Option<String>,
    pub failed: bool,
}

impl TaskResult {
    fn text(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            images: Vec::new(),
            mutated: false,
            projects: Vec::new(),
            session_key: None,
            reset_session: None,
            reset_conversation: false,
            notice: None,
            failed: false,
        }
    }

    pub fn failed(message: impl Into<String>) -> Self {
        Self {
            failed: true,
            ..Self::text(message)
        }
    }
}

pub struct Runner {
    config: Arc<Config>,
    guard: PathGuard,
    runtime: Arc<dyn AgentRuntime>,
    registry: AsyncMutex<SessionRegistry>,
    memory: MemoryStore,
    holidays: HolidayCalendar,
}

impl Runner {
    pub fn new(
        config: Arc<Config>,
        guard: PathGuard,
        runtime: Arc<dyn AgentRuntime>,
        holidays: HolidayCalendar,
    ) -> Result<Self> {
        let registry = SessionRegistry::new(
            Arc::clone(&runtime),
            &config.sessions_dir,
            &config.sessions_index_file,
            config.session_hot_limit,
        )?;
        let memory = MemoryStore::new(
            &config.memory_dir,
            config.memory_content_max_bytes,
            config.recall_token_budget,
            config.memory_digest_max_chars,
        );
        Ok(Self {
            config,
            guard,
            runtime,
            registry: AsyncMutex::new(registry),
            memory,
            holidays,
        })
    }

    pub fn memory(&self) -> &MemoryStore {
        &self.memory
    }

    /// Archive a key after the turn's critical section (C4).
    pub async fn rotate_session(&self, key: &SessionKey) -> bool {
        self.registry
            .lock()
            .await
            .rotate_now(key)
            .await
            .unwrap_or(false)
    }

    /// Routing-level reset: rotate every live project session of a conversation.
    pub async fn rotate_conversation(&self, source: ChannelSource, conversation_id: &str) -> usize {
        let mut registry = self.registry.lock().await;
        let keys = registry.keys_for_conversation(source, conversation_id);
        let mut rotated = 0;
        for key in keys {
            if registry.rotate_now(&key).await.unwrap_or(false) {
                rotated += 1;
            }
        }
        rotated
    }

    pub async fn run(&self, message: &str, ctx: &RunContext) -> TaskResult {
        let mut result = match self.run_inner(message, ctx).await {
            Ok(result) => result,
            Err(err) => TaskResult::failed(format!("⚠️ Task execution error: {err}")),
        };
        if let Some(key) = take_reset_requested() {
            result.reset_session = Some(key);
        }
        set_task_context(None);
        result
    }

    async fn run_inner(&self, message: &str, ctx: &RunContext) -> Result<TaskResult> {
        let projects = list_project_dirs(&self.guard);
        if projects.is_empty() {
            return Ok(TaskResult::text(
                "⚠️ No data subprojects under DATA_ROOT; please check your config.",
            ));
        }

        let is_im =
            ctx.phase == Phase::Execute && ctx.source.is_im() && ctx.conversation_id.is_some();

        if ctx.phase == Phase::Execute
            && ctx.project_hint.is_none()
            && ctx.projects_hint.is_none()
            && ctx.source != ChannelSource::Scheduler
            && ctx.inbound_images.is_empty()
            && is_greeting(message)
        {
            return Ok(TaskResult::text(greeting_reply(&self.guard)));
        }

        let include_scheduler = ctx.source != ChannelSource::Scheduler;
        let mut reset_conversation = false;
        let selected: Vec<String>;
        if let Some(hint) = ctx.project_hint.as_deref() {
            if projects.iter().any(|p| p == hint) {
                selected = vec![hint.to_string()];
            } else if hint == SCHEDULER_VIRTUAL_PROJECT && include_scheduler {
                selected = vec![SCHEDULER_VIRTUAL_PROJECT.to_string()];
            } else {
                selected = Vec::new();
            }
        } else if let Some(hints) = &ctx.projects_hint {
            selected = hints
                .iter()
                .filter(|name| projects.iter().any(|p| p == *name))
                .cloned()
                .collect();
        } else if projects.len() == 1 {
            selected = projects.clone();
        } else {
            let mut candidates = projects.clone();
            if include_scheduler {
                candidates.push(SCHEDULER_VIRTUAL_PROJECT.to_string());
            }
            let route_prompt = build_route_prompt(
                &self.guard,
                message,
                include_scheduler,
                !ctx.inbound_images.is_empty(),
                is_im,
            );
            let decision = self
                .runtime
                .route(RouteRequest {
                    message: message.to_string(),
                    candidates: candidates.clone(),
                    include_scheduler,
                    has_images: !ctx.inbound_images.is_empty(),
                    allow_reset: is_im,
                    provider: self.config.provider.clone(),
                    model: self.config.model.clone(),
                    prompt: route_prompt,
                })
                .await
                .with_context(|| "routing call failed")?;
            let decision = sanitize_route(decision, &candidates);
            if decision.reset_context && is_im {
                reset_conversation = true;
                selected = Vec::new();
            } else {
                selected = decision.projects;
            }
        }

        if reset_conversation {
            return Ok(TaskResult {
                reset_conversation: true,
                ..TaskResult::text(context_reset_reply())
            });
        }
        if selected.is_empty() {
            return Ok(TaskResult::text(fallback_unknown_reply(
                &self.guard,
                !ctx.inbound_images.is_empty(),
            )));
        }

        let real_selected: Vec<String> = selected
            .iter()
            .filter(|name| *name != SCHEDULER_VIRTUAL_PROJECT)
            .cloned()
            .collect();
        if real_selected.is_empty() && !ctx.inbound_images.is_empty() {
            return Ok(TaskResult::text(fallback_unknown_reply(&self.guard, true)));
        }

        // Persist inbound originals after a project is known, through safePath.
        let saved_paths = self
            .persist_images(&ctx.inbound_images, &real_selected)
            .await;
        let attachment_note = if saved_paths.is_empty() {
            String::new()
        } else {
            format!(
                "\n[图片已保存到] {}（可在记录中引用这些相对路径）",
                saved_paths.join(", ")
            )
        };

        let memory_ctx = Self::memory_context(ctx, &real_selected);
        let memory_digest = self.memory.mental_model(&memory_ctx);
        let subject = if real_selected.is_empty() {
            SCHEDULER_VIRTUAL_PROJECT.to_string()
        } else {
            real_selected.join("+")
        };
        let persistent = is_im && !real_selected.is_empty();
        let session_key = if persistent {
            Some(SessionKey::new(
                ctx.source,
                ctx.conversation_id.clone().unwrap_or_default(),
                subject.clone(),
            ))
        } else {
            None
        };
        let can_control = persistent;

        let fingerprint_value = self.fingerprint(&real_selected, &memory_digest);
        let system_prompt = self.system_prompt(&selected, &memory_digest, can_control);
        let tools = self.toolset(can_control);
        let (preamble_source, preamble_sender) = match &ctx.origin {
            Some(origin) => (origin.source.as_str(), Some(origin.principal_id.as_str())),
            None => (
                ctx.source.as_str(),
                ctx.sender_id
                    .as_deref()
                    .or(ctx.recipient.as_ref().map(|r| r.user_id.as_str())),
            ),
        };
        let preamble = build_preamble(preamble_source, preamble_sender, &subject);
        let user_text = format!("{preamble}{message}{attachment_note}");
        let session_id = session_key.clone().unwrap_or_else(|| {
            SessionKey::new(
                ctx.source,
                ctx.conversation_id
                    .clone()
                    .unwrap_or_else(|| ctx.task_id.clone()),
                subject.clone(),
            )
        });

        let origin = ctx.origin.clone().or_else(|| {
            match (
                ctx.source.is_im(),
                ctx.conversation_id.clone(),
                ctx.sender_id.clone(),
            ) {
                (true, Some(conversation_id), Some(sender_id)) => Some(ScheduleOrigin {
                    source: ctx.source,
                    principal_id: sender_id,
                    conversation_id: Some(conversation_id),
                    kind: ctx.conversation_kind,
                }),
                _ => None,
            }
        });
        set_task_context(Some(Arc::new(TaskContext {
            recipient: ctx.recipient.clone(),
            origin,
            projects: real_selected.clone(),
            memory: Some(memory_ctx),
            session_key: session_key.clone(),
            reset_requested: std::sync::atomic::AtomicBool::new(false),
        })));

        let spec = SessionSpec {
            key: session_id.clone(),
            workspace: self
                .guard
                .project_root(real_selected.first().map(String::as_str).unwrap_or(""))
                .unwrap_or_else(|| self.guard.data_root())
                .to_path_buf(),
            session_file: {
                let registry = self.registry.lock().await;
                registry.session_file(&session_id)
            },
            system_prompt,
            provider: self.config.provider.clone(),
            model: self.config.model.clone(),
            tools,
            ephemeral: !persistent,
        };
        let meta = SessionMeta::from_spec(&spec, fingerprint_value);
        let acquired = {
            let mut registry = self.registry.lock().await;
            registry.acquire(spec, meta).await?
        };
        let handle = Arc::clone(&acquired.handle);
        let notice = acquired.notice.clone();
        let reply = {
            let mut session = handle.lock().await;
            session
                .prompt(&user_text, &ctx.inbound_images)
                .await
                .with_context(|| "agent turn failed")?
        };
        drop(handle);

        let (text, images) = extract_images(&reply.text, &self.guard, &real_selected);
        let text = match (&notice, text.is_empty()) {
            (Some(notice), true) => notice.clone(),
            (Some(notice), false) => format!("{notice}\n\n{text}"),
            (None, _) => text,
        };
        let mutated = reply.mutated() || !saved_paths.is_empty();
        Ok(TaskResult {
            text,
            images,
            mutated,
            projects: real_selected,
            session_key,
            reset_session: None,
            reset_conversation: false,
            notice,
            failed: false,
        })
    }

    fn memory_context(ctx: &RunContext, projects: &[String]) -> MemoryContext {
        let project = projects
            .first()
            .cloned()
            .unwrap_or_else(|| ctx.project_hint.clone().unwrap_or_default());
        // A scheduler task acts as the identity that created the entry, never
        // as the push address: a Feishu `chat_id` is a delivery target and for
        // a group it is the whole group, not a principal.
        if let Some(origin) = &ctx.origin {
            let conversation_id = origin
                .conversation_id
                .clone()
                .unwrap_or_else(|| ctx.task_id.clone());
            return match origin.kind {
                ConversationKind::Group => MemoryContext::group(
                    origin.source,
                    conversation_id,
                    &origin.principal_id,
                    project,
                ),
                ConversationKind::Direct => MemoryContext::direct(
                    origin.source,
                    conversation_id,
                    &origin.principal_id,
                    project,
                ),
                ConversationKind::Synthetic => MemoryContext::synthetic_for(
                    origin.source,
                    conversation_id,
                    &origin.principal_id,
                    project,
                ),
            };
        }
        // Legacy entries without an origin keep the old recipient-based scoping
        // for tier 1/2. A legacy scheduler task has no authenticated creator, so
        // it must not reach tier 3 -- even when its recipient is a group id.
        let source = ctx
            .recipient
            .as_ref()
            .map(|recipient| recipient.source)
            .unwrap_or(ctx.source);
        let conversation_id = ctx
            .conversation_id
            .clone()
            .unwrap_or_else(|| ctx.task_id.clone());
        let principal = ctx
            .sender_id
            .clone()
            .or_else(|| ctx.recipient.as_ref().map(|r| r.user_id.clone()))
            .unwrap_or_else(|| "local-owner".to_string());
        let memory =
            if ctx.conversation_kind == ConversationKind::Group && ctx.conversation_id.is_some() {
                MemoryContext::group(source, conversation_id, principal, project)
            } else if source.is_im() && ctx.conversation_id.is_some() {
                MemoryContext::direct(source, conversation_id, principal, project)
            } else {
                MemoryContext::synthetic_for(source, conversation_id, principal, project)
            };
        if ctx.source == ChannelSource::Scheduler {
            memory.without_global()
        } else {
            memory
        }
    }

    fn fingerprint(&self, projects: &[String], memory_digest: &str) -> String {
        let template = load_base_template(&self.config);
        let mut parts: Vec<String> = vec![
            template,
            self.config.provider.clone(),
            self.config.model.clone(),
            TOOL_POLICY_VERSION.to_string(),
            "compaction=summary".to_string(),
            format!("memory:{memory_digest}"),
        ];
        for project in projects {
            parts.push(self.project_doc(project));
        }
        let refs: Vec<&str> = parts.iter().map(String::as_str).collect();
        fingerprint(&refs)
    }

    fn project_doc(&self, project: &str) -> String {
        let Some(root) = self.guard.project_root(project) else {
            return String::new();
        };
        for name in ["AGENTS.md", "CLAUDE.md", "CODEBUDDY.md"] {
            if let Ok(content) = std::fs::read_to_string(root.join(name)) {
                return content;
            }
        }
        String::new()
    }

    fn system_prompt(&self, selected: &[String], memory_digest: &str, can_control: bool) -> String {
        let base = build_system_prompt(&self.guard, &self.config, selected, Some(memory_digest));
        let clear = if can_control {
            "\n\n# 会话上下文控制\n你可以使用 clear_conversation_context 工具。仅当用户明确要求清空 / 忽略 / 重置之前的上下文时，在处理当前请求前调用该工具一次。调用后，如果用户还带有具体任务，继续完成该任务；如果没有具体任务，只需简短确认已清空。"
        } else {
            ""
        };
        let caps = project_capabilities(&self.guard)
            .iter()
            .map(|(name, hint)| match hint {
                Some(hint) => format!("- {name}: {hint}"),
                None => format!("- {name}"),
            })
            .collect::<Vec<_>>()
            .join("\n");
        format!("{base}{clear}\n\n# 可用数据子项目\n{caps}")
    }

    fn toolset(&self, can_control: bool) -> Toolset {
        let mut tools: Vec<Arc<dyn Tool>> = vec![
            Arc::new(ReadTool::new(self.guard.clone())),
            Arc::new(WriteTool::new(self.guard.clone())),
            Arc::new(EditTool::new(self.guard.clone())),
            Arc::new(RunTool::new(self.guard.clone())),
            Arc::new(ScheduleTool::new(
                (*self.config).clone(),
                self.holidays.clone(),
            )),
            Arc::new(RetainTool::new(self.memory.clone())),
            Arc::new(RecallTool::new(self.memory.clone())),
        ];
        if can_control {
            tools.push(Arc::new(ClearConversationTool));
        }
        Toolset::new(tools)
    }

    async fn persist_images(&self, images: &[InboundImage], projects: &[String]) -> Vec<String> {
        if images.is_empty() || projects.is_empty() {
            return Vec::new();
        }
        let project = &projects[0];
        let mut out = Vec::new();
        for image in images {
            let ext = match image.ext.as_str() {
                "jpg" | "png" | "gif" | "webp" => image.ext.as_str(),
                _ => "jpg",
            };
            let stamp = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_nanos())
                .unwrap_or(0);
            let relative = format!(
                "{project}/photos/{}-{:x}.{ext}",
                crate::prompt::today(),
                stamp
            );
            let Ok(abs) = self.guard.safe_path(&relative, Some(projects)) else {
                tracing::warn!("[runner] refusing inbound image path {relative}");
                continue;
            };
            if let Some(parent) = abs.parent() {
                let _ = tokio::fs::create_dir_all(parent).await;
            }
            if tokio::fs::write(&abs, &image.bytes).await.is_ok() {
                out.push(relative);
            }
        }
        out
    }
}

fn sanitize_route(mut decision: RouteDecision, candidates: &[String]) -> RouteDecision {
    decision
        .projects
        .retain(|project| candidates.iter().any(|candidate| candidate == project));
    decision.projects.dedup();
    if decision.projects.is_empty() {
        decision.attachment_project = None;
    } else if let Some(attachment) = &decision.attachment_project {
        if !decision
            .projects
            .iter()
            .any(|project| project == attachment)
            || attachment == SCHEDULER_VIRTUAL_PROJECT
        {
            decision.attachment_project = None;
        }
    }
    if decision
        .projects
        .contains(&SCHEDULER_VIRTUAL_PROJECT.to_string())
    {
        decision.projects = vec![SCHEDULER_VIRTUAL_PROJECT.to_string()];
        decision.attachment_project = None;
    }
    decision
}

const IMAGE_EXTENSIONS: &[&str] = &[".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp"];

/// Split markdown image refs from the reply. Local paths go through safePath
/// and must be an existing image file; invalid refs are dropped silently while
/// the text still gets through.
pub fn extract_images(text: &str, guard: &PathGuard, projects: &[String]) -> (String, Vec<String>) {
    let mut clean = String::new();
    let mut images: Vec<String> = Vec::new();
    let mut cursor = 0usize;
    while let Some(offset) = text[cursor..].find("![") {
        let open = cursor + offset;
        let Some(alt_offset) = text[open + 2..].find("](") else {
            break;
        };
        let alt_end = open + 2 + alt_offset;
        let Some(close_offset) = text[alt_end + 2..].find(')') else {
            break;
        };
        let close = alt_end + 2 + close_offset;
        clean.push_str(&text[cursor..open]);
        let reference = text[alt_end + 2..close].trim();
        if reference.starts_with("https://") || reference.starts_with("http://") {
            if !images.iter().any(|existing| existing == reference) {
                images.push(reference.to_string());
            }
        } else if IMAGE_EXTENSIONS
            .iter()
            .any(|ext| reference.to_ascii_lowercase().ends_with(ext))
        {
            if let Ok(abs) = guard.safe_path(reference, Some(projects)) {
                if abs.is_file() {
                    let abs = abs.display().to_string();
                    if !images.iter().any(|existing| existing == &abs) {
                        images.push(abs);
                    }
                } else {
                    tracing::warn!("[runner] ignoring unusable image reference: {reference}");
                }
            }
        }
        cursor = close + 1;
    }
    clean.push_str(&text[cursor..]);
    (clean.trim().to_string(), images)
}

/// Used by tests to prove the fingerprint ignores nothing static.
pub fn static_fingerprint(parts: &[&str]) -> String {
    fingerprint(parts)
}

pub fn read_file_fingerprint(path: &Path) -> Option<String> {
    file_fingerprint(path)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::identity::MemoryNamespace;

    #[test]
    fn image_refs_are_validated_and_stripped() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(dir.path().join("p/photos")).unwrap();
        std::fs::write(dir.path().join("p/photos/a.png"), b"x").unwrap();
        let guard = PathGuard::new(dir.path()).unwrap();
        let (text, images) = extract_images(
            "see ![alt](p/photos/a.png) and ![b](p/photos/missing.png) and ![c](https://x/y.png)",
            &guard,
            &["p".into()],
        );
        assert_eq!(text, "see  and  and");
        assert_eq!(images.len(), 2);
        assert!(images.iter().any(|i| i.ends_with("a.png")));
        assert!(images.iter().any(|i| i == "https://x/y.png"));
    }

    #[test]
    fn legacy_scheduler_keeps_recipient_tier1_but_denies_global() {
        let mut ctx = RunContext::cli("sched-1");
        ctx.source = ChannelSource::Scheduler;
        // A legacy group entry has no origin field; the only identity we have
        // is the group chat id in recipient, which authenticates no creator.
        ctx.recipient = Some(Recipient {
            source: ChannelSource::Telegram,
            user_id: "-1001234567890".into(),
        });
        let scheduled = Runner::memory_context(&ctx, &["ledger".into()]);

        let dm = MemoryContext::direct(
            ChannelSource::Telegram,
            "chat-7",
            "-1001234567890",
            "ledger",
        );
        assert_eq!(scheduled.source, ChannelSource::Telegram);
        // Tier 1/2 still use the recipient, as designed for legacy entries.
        assert_eq!(
            MemoryNamespace::personal(&scheduled),
            MemoryNamespace::personal(&dm)
        );
        // No creator is authenticated, so tier 3 is off on read and write.
        assert!(!scheduled.can_read_global());
        assert!(!scheduled.can_write_global());
        let tiers: Vec<u8> = MemoryNamespace::visible_set(&scheduled)
            .iter()
            .map(MemoryNamespace::tier)
            .collect();
        assert_eq!(tiers, vec![1, 2]);
    }

    #[test]
    fn scheduler_origin_uses_the_creator_not_the_delivery_address() {
        let mut ctx = RunContext::cli("sched-2");
        ctx.source = ChannelSource::Scheduler;
        // Feishu delivery address is the chat id, not the sender's open_id.
        ctx.recipient = Some(Recipient {
            source: ChannelSource::Feishu,
            user_id: "oc_chat".into(),
        });
        ctx.origin = Some(ScheduleOrigin {
            source: ChannelSource::Feishu,
            principal_id: "ou_alice".into(),
            conversation_id: Some("oc_chat".into()),
            kind: ConversationKind::Direct,
        });
        let scheduled = Runner::memory_context(&ctx, &["notes".into()]);

        let dm = MemoryContext::direct(ChannelSource::Feishu, "oc_chat", "ou_alice", "notes");
        assert_eq!(scheduled.principal, dm.principal);
        assert_eq!(
            MemoryNamespace::global(&scheduled),
            MemoryNamespace::global(&dm)
        );
        assert_ne!(
            scheduled.principal,
            crate::identity::PrincipalKey::new(ChannelSource::Feishu, "oc_chat")
        );
    }

    #[test]
    fn group_created_schedule_never_reaches_tier_three() {
        let mut ctx = RunContext::cli("sched-3");
        ctx.source = ChannelSource::Scheduler;
        ctx.origin = Some(ScheduleOrigin {
            source: ChannelSource::Feishu,
            principal_id: "ou_alice".into(),
            conversation_id: Some("oc_group".into()),
            kind: ConversationKind::Group,
        });
        let scheduled = Runner::memory_context(&ctx, &["notes".into()]);
        assert_eq!(scheduled.kind, ConversationKind::Group);
        assert!(!scheduled.can_read_global());
        assert!(!scheduled.can_write_global());
        let tiers: Vec<u8> = MemoryNamespace::visible_set(&scheduled)
            .iter()
            .map(MemoryNamespace::tier)
            .collect();
        assert_eq!(tiers, vec![1, 2]);
        assert_eq!(
            MemoryNamespace::personal(&scheduled),
            MemoryNamespace::personal(&MemoryContext::group(
                ChannelSource::Feishu,
                "oc_group",
                "ou_alice",
                "notes"
            ))
        );
    }

    #[test]
    fn route_sanitizer_drops_unknown_projects() {
        let decision = RouteDecision {
            projects: vec![
                "notes".into(),
                "nope".into(),
                SCHEDULER_VIRTUAL_PROJECT.into(),
            ],
            attachment_project: Some("nope".into()),
            reset_context: false,
        };
        let got = sanitize_route(
            decision,
            &["notes".into(), SCHEDULER_VIRTUAL_PROJECT.into()],
        );
        assert_eq!(got.projects, vec![SCHEDULER_VIRTUAL_PROJECT.to_string()]);
        assert!(got.attachment_project.is_none());
    }
}
