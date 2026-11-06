//! Per-task context injected by the runner before an agent turn.
//!
//! Tools read the active task's sender (for `schedule`), selected projects (for
//! the path-tool subset check), memory context (for retain/recall), and the
//! reset flag (for `clear_conversation_context`). The dispatcher's process-wide
//! serial queue guarantees exactly one task is active, so a single slot is
//! race-free; the reset flag itself is atomic because a tool sets it while the
//! runner still holds the same context.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use crate::identity::{MemoryContext, SessionKey};
use crate::scheduler::types::{Recipient, ScheduleOrigin};

#[derive(Debug)]
pub struct TaskContext {
    pub recipient: Option<Recipient>,
    /// Creator identity for `schedule` entries, kept apart from the delivery
    /// address (a Feishu recipient is a chat id, not a principal).
    pub origin: Option<ScheduleOrigin>,
    /// Real data subprojects selected by routing (never the scheduler virtual
    /// project). Used by the path tools as an additive subset constraint.
    pub projects: Vec<String>,
    pub memory: Option<MemoryContext>,
    /// Present for persistent IM sessions; absent for scheduler / CLI / self-heal.
    pub session_key: Option<SessionKey>,
    pub(crate) reset_requested: AtomicBool,
}

impl TaskContext {
    pub fn request_reset(&self) -> bool {
        if self.session_key.is_none() {
            return false;
        }
        self.reset_requested.store(true, Ordering::SeqCst);
        true
    }

    pub fn reset_requested(&self) -> bool {
        self.reset_requested.load(Ordering::SeqCst)
    }
}

static CURRENT: OnceLock<Mutex<Option<Arc<TaskContext>>>> = OnceLock::new();

fn slot() -> &'static Mutex<Option<Arc<TaskContext>>> {
    CURRENT.get_or_init(|| Mutex::new(None))
}

pub fn set_task_context(ctx: Option<Arc<TaskContext>>) {
    if let Ok(mut current) = slot().lock() {
        *current = ctx;
    }
}

pub fn current_task_context() -> Option<Arc<TaskContext>> {
    slot().lock().ok().and_then(|current| current.clone())
}

/// Task-local selected projects for path-tool enforcement.
pub fn task_projects() -> Option<Vec<String>> {
    current_task_context().map(|ctx| ctx.projects.clone())
}

/// The active task's sender, if any (scheduler recipient for the `schedule` tool).
pub fn task_recipient() -> Option<Recipient> {
    current_task_context().and_then(|ctx| ctx.recipient.clone())
}

/// The active task's creator identity, persisted on new schedule entries.
pub fn task_origin() -> Option<ScheduleOrigin> {
    current_task_context().and_then(|ctx| ctx.origin.clone())
}

pub fn task_memory_context() -> Option<MemoryContext> {
    current_task_context().and_then(|ctx| ctx.memory.clone())
}

/// Flag a conversation reset. Returns false outside an IM session.
pub fn request_conversation_reset() -> bool {
    current_task_context()
        .map(|ctx| ctx.request_reset())
        .unwrap_or(false)
}

/// Read and clear the flag. Called by the dispatcher after the turn completes.
pub fn take_reset_requested() -> Option<SessionKey> {
    let ctx = current_task_context()?;
    if ctx.reset_requested.swap(false, Ordering::SeqCst) {
        ctx.session_key.clone()
    } else {
        None
    }
}

/// Serializes tests that install the process-global task context.
#[cfg(test)]
pub(crate) fn test_lock() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reset_requires_an_im_session() {
        let _guard = test_lock();
        let plain = Arc::new(TaskContext {
            recipient: None,
            origin: None,
            projects: vec![],
            memory: None,
            session_key: None,
            reset_requested: AtomicBool::new(false),
        });
        set_task_context(Some(plain));
        assert!(!request_conversation_reset());

        let key = SessionKey::new(crate::identity::ChannelSource::Telegram, "1", "notes");
        let im = Arc::new(TaskContext {
            recipient: None,
            origin: None,
            projects: vec![],
            memory: None,
            session_key: Some(key.clone()),
            reset_requested: AtomicBool::new(false),
        });
        set_task_context(Some(im));
        assert!(request_conversation_reset());
        assert_eq!(take_reset_requested(), Some(key));
        assert_eq!(take_reset_requested(), None);
        set_task_context(None);
    }
}
