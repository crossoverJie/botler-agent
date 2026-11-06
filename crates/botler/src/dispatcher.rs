//! Process-wide serial task queue (C2).
//!
//! Messages are enqueued at channel receive time in global arrival order; a
//! single worker owns routing → session selection → tool calls → validate →
//! self-heal → commit → post-turn rotation. Scheduler and CLI go through the
//! same queue. `dispatch` never rejects: every failure becomes readable text.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::{mpsc, oneshot};

use crate::config::Config;
use crate::identity::{ChannelSource, ConversationKind};
use crate::paths::PathGuard;
use crate::runner::{Phase, RunContext, Runner};
use crate::runtime::InboundImage;
use crate::safety::git::{commit_if_changed, commit_subject};
use crate::safety::validate::validate_state;
use crate::scheduler::types::{Recipient, ScheduleOrigin};
use crate::tools::run::{run_script, ErrorKind};

pub const DUPLICATE_SENTINEL: &str = "(duplicate message ignored)";
const DEDUP_WINDOW: Duration = Duration::from_secs(5 * 60);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskStatus {
    Success,
    AutoFixed,
    UnknownProject,
    ValidationFailed,
    Error,
    Duplicate,
}

impl TaskStatus {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Success => "success",
            Self::AutoFixed => "auto-fixed",
            Self::UnknownProject => "unknown-project",
            Self::ValidationFailed => "validation-failed",
            Self::Error => "error",
            Self::Duplicate => "duplicate",
        }
    }
}

#[derive(Debug, Clone)]
pub struct DispatchResult {
    pub text: String,
    pub images: Vec<String>,
    pub status: TaskStatus,
}

impl DispatchResult {
    pub fn failed(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            images: Vec::new(),
            status: TaskStatus::Error,
        }
    }
}

#[derive(Debug, Clone)]
pub struct Conversation {
    pub id: String,
    pub sender_id: String,
    pub kind: ConversationKind,
}

#[derive(Debug, Clone)]
pub struct DispatchOptions {
    /// Dedup key, e.g. `telegram:<chat>:<message>`. Duplicates within five
    /// minutes are ignored.
    pub id: Option<String>,
    pub source: ChannelSource,
    pub project_hint: Option<String>,
    pub projects_hint: Option<Vec<String>>,
    pub recipient: Option<Recipient>,
    pub inbound_images: Vec<InboundImage>,
    /// IM only. Absent for scheduler / CLI / self-heal.
    pub conversation: Option<Conversation>,
    /// Scheduler only: the identity that created the entry (memory ownership).
    /// Live IM tasks derive this from `conversation` instead.
    pub origin: Option<ScheduleOrigin>,
}

impl Default for DispatchOptions {
    fn default() -> Self {
        Self {
            id: None,
            source: ChannelSource::Cli,
            project_hint: None,
            projects_hint: None,
            recipient: None,
            inbound_images: Vec::new(),
            conversation: None,
            origin: None,
        }
    }
}

impl DispatchOptions {
    pub fn cli() -> Self {
        Self::default()
    }
}

#[derive(Debug, Clone)]
pub struct ScriptTaskOptions {
    pub id: String,
    pub script: String,
    pub script_args: Vec<String>,
    pub commit: Option<bool>,
    pub source: ChannelSource,
}

#[derive(Default)]
pub struct DispatchStats {
    pub enqueued: AtomicU64,
    pub started: AtomicU64,
    pub finished: AtomicU64,
    pub duplicates: AtomicU64,
    pub failures: AtomicU64,
}

struct Inner {
    runner: Runner,
    config: Config,
    guard: PathGuard,
    dedup: Mutex<HashMap<String, std::time::Instant>>,
    stats: DispatchStats,
    task_seq: AtomicU64,
}

enum WorkerRequest {
    Message {
        message: String,
        options: DispatchOptions,
        reply: oneshot::Sender<DispatchResult>,
    },
    Script {
        options: ScriptTaskOptions,
        reply: oneshot::Sender<DispatchResult>,
    },
}

pub struct Dispatcher {
    inner: Arc<Inner>,
    tx: mpsc::UnboundedSender<WorkerRequest>,
}

/// A task already placed on the global queue. Channels call `enqueue` inline
/// in the receive handler (arrival order, C2) and may await the result later.
pub struct EnqueuedTask {
    result: Option<DispatchResult>,
    wait: Option<oneshot::Receiver<DispatchResult>>,
}

impl EnqueuedTask {
    pub async fn wait(mut self) -> DispatchResult {
        if let Some(result) = self.result.take() {
            return result;
        }
        match self.wait.take() {
            Some(rx) => rx.await.unwrap_or_else(|_| {
                DispatchResult::failed("⚠️ Task execution error: worker stopped")
            }),
            None => DispatchResult::failed("⚠️ Task execution error: unknown state"),
        }
    }
}

impl Dispatcher {
    pub fn new(runner: Runner, config: Config, guard: PathGuard) -> Arc<Self> {
        let inner = Arc::new(Inner {
            runner,
            config,
            guard,
            dedup: Mutex::new(HashMap::new()),
            stats: DispatchStats::default(),
            task_seq: AtomicU64::new(0),
        });
        let (tx, mut rx) = mpsc::unbounded_channel::<WorkerRequest>();
        let worker_inner = Arc::clone(&inner);
        tokio::spawn(async move {
            while let Some(request) = rx.recv().await {
                match request {
                    WorkerRequest::Message {
                        message,
                        options,
                        reply,
                    } => {
                        let result = process_message(&worker_inner, &message, &options).await;
                        let _ = reply.send(result);
                    }
                    WorkerRequest::Script { options, reply } => {
                        let result = process_script(&worker_inner, &options).await;
                        let _ = reply.send(result);
                    }
                }
            }
        });
        Arc::new(Self { inner, tx })
    }

    pub fn stats(&self) -> &DispatchStats {
        &self.inner.stats
    }

    /// Enqueue at arrival time and wait for the serial worker to finish.
    pub async fn dispatch(&self, message: &str, options: DispatchOptions) -> DispatchResult {
        self.enqueue(message, options).wait().await
    }

    /// Place a message on the global queue now; never blocks. Dedup and the
    /// stopped-worker case resolve immediately inside the returned task.
    pub fn enqueue(&self, message: &str, options: DispatchOptions) -> EnqueuedTask {
        if let Some(id) = options.id.as_deref() {
            if self.is_duplicate(id) {
                self.inner.stats.duplicates.fetch_add(1, Ordering::SeqCst);
                tracing::info!("[dispatch] ignoring duplicate message {id}");
                return EnqueuedTask {
                    result: Some(DispatchResult {
                        text: DUPLICATE_SENTINEL.to_string(),
                        images: Vec::new(),
                        status: TaskStatus::Duplicate,
                    }),
                    wait: None,
                };
            }
        }
        let (reply, wait) = oneshot::channel();
        self.inner.stats.enqueued.fetch_add(1, Ordering::SeqCst);
        if self
            .tx
            .send(WorkerRequest::Message {
                message: message.to_string(),
                options,
                reply,
            })
            .is_err()
        {
            return EnqueuedTask {
                result: Some(DispatchResult::failed(
                    "⚠️ Task execution error: dispatcher is stopped",
                )),
                wait: None,
            };
        }
        EnqueuedTask {
            result: None,
            wait: Some(wait),
        }
    }

    /// Direct script execution on the same queue (no LLM).
    pub async fn dispatch_scripted(&self, options: ScriptTaskOptions) -> DispatchResult {
        let (reply, wait) = oneshot::channel();
        self.inner.stats.enqueued.fetch_add(1, Ordering::SeqCst);
        if self
            .tx
            .send(WorkerRequest::Script { options, reply })
            .is_err()
        {
            return DispatchResult::failed("⚠️ Script task error: dispatcher is stopped");
        }
        wait.await
            .unwrap_or_else(|_| DispatchResult::failed("⚠️ Script task error: worker stopped"))
    }

    fn is_duplicate(&self, id: &str) -> bool {
        let Ok(mut seen) = self.inner.dedup.lock() else {
            return false;
        };
        let now = std::time::Instant::now();
        seen.retain(|_, at| now.duration_since(*at) < DEDUP_WINDOW);
        if let Some(previous) = seen.get(id) {
            if now.duration_since(*previous) < DEDUP_WINDOW {
                return true;
            }
        }
        seen.insert(id.to_string(), now);
        false
    }
}

async fn process_message(
    inner: &Arc<Inner>,
    message: &str,
    options: &DispatchOptions,
) -> DispatchResult {
    inner.stats.started.fetch_add(1, Ordering::SeqCst);
    let started = std::time::Instant::now();
    let conversation = options.conversation.clone();
    let ctx = RunContext {
        task_id: options.id.clone().unwrap_or_else(|| {
            format!("task-{}", inner.task_seq.fetch_add(1, Ordering::SeqCst) + 1)
        }),
        source: options.source,
        phase: Phase::Execute,
        project_hint: options.project_hint.clone(),
        projects_hint: options.projects_hint.clone(),
        recipient: options.recipient.clone(),
        inbound_images: options.inbound_images.clone(),
        conversation_id: conversation.as_ref().map(|c| c.id.clone()),
        sender_id: conversation.as_ref().map(|c| c.sender_id.clone()),
        conversation_kind: conversation
            .as_ref()
            .map(|c| c.kind)
            .unwrap_or(ConversationKind::Synthetic),
        origin: options.origin.clone(),
    };

    let mut result = inner.runner.run(message, &ctx).await;
    let status = if result.failed {
        TaskStatus::Error
    } else if !result.mutated {
        if result.projects.is_empty() {
            TaskStatus::UnknownProject
        } else {
            TaskStatus::Success
        }
    } else {
        let scope = if result.projects.is_empty() {
            None
        } else {
            Some(result.projects.as_slice())
        };
        let mut validation = validate_state(&inner.guard, scope);
        if !validation.ok {
            let fix = validation.fix.clone().unwrap_or_default();
            tracing::warn!("[dispatch] validation failed; self-heal retry: {fix}");
            let heal_ctx = RunContext {
                task_id: ctx.task_id.clone(),
                source: ctx.source,
                phase: Phase::SelfHeal,
                project_hint: None,
                projects_hint: Some(result.projects.clone()),
                recipient: None,
                inbound_images: Vec::new(),
                conversation_id: None,
                sender_id: None,
                conversation_kind: ConversationKind::Synthetic,
                origin: None,
            };
            let _ = inner.runner.run(&fix, &heal_ctx).await;
            validation = validate_state(&inner.guard, scope);
            if !validation.ok {
                let fix = validation.fix.unwrap_or_default();
                result.text = format!("⚠️ Could not save safely; please check the data: {fix}");
                TaskStatus::ValidationFailed
            } else {
                commit_if_changed(
                    &inner.config,
                    &format!("agent: {}", commit_subject(message)),
                    Some(result.projects.as_slice()),
                )
                .await;
                result.text = format!("{}\n\n(auto-fixed and saved)", result.text);
                TaskStatus::AutoFixed
            }
        } else {
            commit_if_changed(
                &inner.config,
                &format!("agent: {}", commit_subject(message)),
                Some(result.projects.as_slice()),
            )
            .await;
            TaskStatus::Success
        }
    };

    // Post-turn session lifecycle (C4/C5). Nothing rotates inside the turn.
    if let Some(key) = result.reset_session.clone() {
        inner.runner.rotate_session(&key).await;
    }
    if result.reset_conversation {
        if let Some(conversation) = &conversation {
            inner
                .runner
                .rotate_conversation(options.source, &conversation.id)
                .await;
        }
    }

    if status == TaskStatus::Error || status == TaskStatus::ValidationFailed {
        inner.stats.failures.fetch_add(1, Ordering::SeqCst);
    }
    inner.stats.finished.fetch_add(1, Ordering::SeqCst);
    tracing::info!(
        "[dispatch] {:?} -> {} in {}ms",
        options.source,
        status.as_str(),
        started.elapsed().as_millis()
    );
    DispatchResult {
        text: result.text,
        images: result.images,
        status,
    }
}

async fn process_script(inner: &Arc<Inner>, options: &ScriptTaskOptions) -> DispatchResult {
    inner.stats.started.fetch_add(1, Ordering::SeqCst);
    let started = std::time::Instant::now();
    let result = run_script(&inner.guard, None, &options.script, &options.script_args).await;
    let project = result
        .cwd
        .as_deref()
        .and_then(|cwd| inner.guard.project_of(cwd));
    let ran = result.ok || result.error_kind == Some(ErrorKind::Runtime);

    let (text, status) = if !result.ok && result.error_kind == Some(ErrorKind::Precondition) {
        (result.text, TaskStatus::Error)
    } else {
        let scope = project.as_deref().map(|p| vec![p.to_string()]);
        let validation = validate_state(&inner.guard, scope.as_deref());
        if !validation.ok {
            let warning = validation.fix.unwrap_or_default();
            let script_error = if !result.ok {
                format!("Script failed: {}\n", result.text)
            } else {
                String::new()
            };
            (
                format!(
                    "⚠️ {script_error}Script produced invalid data (left as-is, not committed): {warning}"
                ),
                TaskStatus::ValidationFailed,
            )
        } else if !result.ok {
            (result.text, TaskStatus::Error)
        } else {
            if options.commit.unwrap_or(true) {
                let message = format!("agent: scheduled script {}", options.id);
                let scope = project.as_deref().map(|p| vec![p.to_string()]);
                commit_if_changed(&inner.config, &message, scope.as_deref()).await;
            }
            (result.text, TaskStatus::Success)
        }
    };

    if status == TaskStatus::Error || status == TaskStatus::ValidationFailed {
        inner.stats.failures.fetch_add(1, Ordering::SeqCst);
    }
    inner.stats.finished.fetch_add(1, Ordering::SeqCst);
    tracing::info!(
        "[dispatch] script {} -> {} (ran={ran}) in {}ms",
        options.id,
        status.as_str(),
        started.elapsed().as_millis()
    );
    DispatchResult {
        text,
        images: Vec::new(),
        status,
    }
}

/// Kept for parity with v1's script log attribution.
pub fn script_project(guard: &PathGuard, cwd: Option<&std::path::Path>) -> Option<String> {
    cwd.and_then(|cwd| guard.project_of(cwd))
}
