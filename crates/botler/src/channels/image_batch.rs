//! Per-sender image batching state machine for the WeChat channel.
//!
//! WeChat delivers a selected photo immediately while the user may still be
//! typing the caption ("选图即发，文字后到"), so image-bearing messages are held
//! for a short window; a following text from the same sender joins the batch
//! and is dispatched as ONE task (caption + vision); otherwise the image(s)
//! alone are dispatched when the window expires. Multiple photos in the window
//! also merge into one batch. Verbatim port of v1 `channels/wechat/image-batch.ts`.

use std::collections::HashMap;

use sha2::{Digest, Sha256};
use tokio::sync::Mutex;

use crate::runtime::InboundImage;

/// One inbound message offered to the coordinator.
#[derive(Debug, Clone, Default)]
pub struct PendingPiece {
    pub text: String,
    pub images: Vec<InboundImage>,
    pub context_token: Option<String>,
}

/// A ready-to-run task: joined text plus all buffered images.
#[derive(Debug, Clone)]
pub struct BatchDispatch {
    /// Joined user texts, or the "[图片]" placeholder for a pure image batch.
    pub text: String,
    pub images: Vec<InboundImage>,
    pub context_token: Option<String>,
    /// Deterministic dedup id derived from the content.
    pub id: String,
}

#[derive(Debug, Clone)]
pub enum BatchAction {
    Dispatch(BatchDispatch),
    Buffer,
}

#[derive(Debug, Default)]
struct PendingBatch {
    images: Vec<InboundImage>,
    texts: Vec<String>,
    context_token: Option<String>,
}

/// Per-sender flush generations. Every buffered image arms a new generation;
/// a timer may only flush when it still owns the latest one, so an image that
/// arrives mid-window restarts the wait instead of being flushed early.
#[derive(Debug, Default)]
pub struct FlushTimers {
    generations: HashMap<String, u64>,
}

impl FlushTimers {
    pub fn arm(&mut self, sender: &str) -> u64 {
        let generation = self.generations.entry(sender.to_string()).or_insert(0);
        *generation += 1;
        *generation
    }

    /// Consume the sender's timer if `generation` is still current.
    pub fn claim(&mut self, sender: &str, generation: u64) -> bool {
        if self.generations.get(sender).copied() != Some(generation) {
            return false;
        }
        self.generations.remove(sender);
        true
    }

    pub fn is_armed(&self, sender: &str) -> bool {
        self.generations.contains_key(sender)
    }
}

// Owns the per-sender batch state and the flush generations together so the
// batch update and the generation bump (and, on the timer side, the claim and
// the flush) share one lock ordering.
//
// The invariant that stops a stale timer from flushing a batch a newer image
// has already extended: on_message and flush_if_current both take the
// coordinator lock first and the timer lock second. A timer that armed an older
// generation therefore cannot slip a flush in after a newer image has updated
// the batch and bumped the generation -- by the time the timer holds the
// coordinator, the generation has already been bumped and the stale claim
// fails.
pub struct BatchScheduler {
    coordinator: Mutex<ImageBatchCoordinator>,
    flush_timers: Mutex<FlushTimers>,
}

impl BatchScheduler {
    pub fn new(window_ms: u64) -> Self {
        Self {
            coordinator: Mutex::new(ImageBatchCoordinator::new(window_ms)),
            flush_timers: Mutex::new(FlushTimers::default()),
        }
    }

    // Buffer or dispatch one inbound piece. Returns the action and, for a
    // buffered image, the freshly armed flush generation.
    pub async fn on_message(
        &self,
        sender: &str,
        piece: PendingPiece,
    ) -> (BatchAction, Option<u64>) {
        let mut coordinator = self.coordinator.lock().await;
        let mut timers = self.flush_timers.lock().await;
        let action = coordinator.on_message(sender, piece);
        let generation = if matches!(action, BatchAction::Buffer) {
            Some(timers.arm(sender))
        } else {
            None
        };
        (action, generation)
    }

    // Claim generation and, if it is still current, flush the sender's batch.
    // Both steps run while holding the coordinator lock, so a newer image that
    // arrives between arming and this call has already bumped the generation
    // and extended the batch -- the stale generation therefore fails to claim
    // and nothing is flushed early.
    pub async fn flush_if_current(&self, sender: &str, generation: u64) -> Option<BatchDispatch> {
        let mut coordinator = self.coordinator.lock().await;
        let mut timers = self.flush_timers.lock().await;
        if !timers.claim(sender, generation) {
            return None;
        }
        coordinator.flush(sender)
    }
}

pub struct ImageBatchCoordinator {
    window_ms: u64,
    batches: HashMap<String, PendingBatch>,
}

impl ImageBatchCoordinator {
    pub fn new(window_ms: u64) -> Self {
        Self {
            window_ms,
            batches: HashMap::new(),
        }
    }

    pub fn window_ms(&self) -> u64 {
        self.window_ms
    }

    /// Feed one inbound message from a sender.
    ///
    /// - `Buffer`: an image message was held; the caller must (re)arm the
    ///   flush timer for `window_ms` so it is eventually dispatched.
    /// - `Dispatch`: run this task now (standalone text, a caption that joined
    ///   a pending batch, or an image when batching is disabled).
    pub fn on_message(&mut self, sender: &str, piece: PendingPiece) -> BatchAction {
        let PendingPiece {
            text,
            images,
            context_token,
        } = piece;

        if !images.is_empty() && self.window_ms > 0 {
            let batch = self.batches.entry(sender.to_string()).or_default();
            batch.images.extend(images);
            if !text.is_empty() {
                batch.texts.push(text);
            }
            if context_token.is_some() {
                batch.context_token = context_token;
            }
            return BatchAction::Buffer;
        }

        // Text-only (or batching disabled): a pending batch makes this the
        // caption; join and dispatch as one task now.
        if !text.is_empty() {
            if let Some(batch) = self.batches.remove(sender) {
                let mut merged = batch;
                merged.texts.push(text);
                if context_token.is_some() {
                    merged.context_token = context_token;
                }
                return BatchAction::Dispatch(build(
                    sender,
                    merged.texts,
                    merged.images,
                    merged.context_token,
                    true,
                ));
            }
        }

        BatchAction::Dispatch(build(
            sender,
            if text.is_empty() {
                Vec::new()
            } else {
                vec![text]
            },
            images,
            context_token,
            false,
        ))
    }

    /// Flush a sender's pending batch (window expired with no caption).
    pub fn flush(&mut self, sender: &str) -> Option<BatchDispatch> {
        let batch = self.batches.remove(sender)?;
        Some(build(
            sender,
            batch.texts,
            batch.images,
            batch.context_token,
            true,
        ))
    }

    pub fn has_pending(&self, sender: &str) -> bool {
        self.batches.contains_key(sender)
    }

    pub fn pending_image_count(&self, sender: &str) -> usize {
        self.batches
            .get(sender)
            .map(|b| b.images.len())
            .unwrap_or(0)
    }

    pub fn pending_count(&self) -> usize {
        self.batches.len()
    }
}

fn build(
    sender: &str,
    texts: Vec<String>,
    images: Vec<InboundImage>,
    context_token: Option<String>,
    is_batch: bool,
) -> BatchDispatch {
    let mut text = texts.join("\n");
    // Image-only message: keep a placeholder so greeting/routing/logs work.
    if text.is_empty() {
        text = "[图片]".to_string();
    }
    let image_sig = image_signature(&images);
    let text_sig: String = text.chars().take(20).collect();
    let id = if is_batch {
        format!("{sender}:batch:{image_sig}:{text_sig}")
    } else {
        format!("{sender}:{text_sig}:{image_sig}")
    };
    BatchDispatch {
        text,
        images,
        context_token,
        id,
    }
}

fn image_signature(images: &[InboundImage]) -> String {
    if images.is_empty() {
        return String::new();
    }
    let mut hasher = Sha256::new();
    for image in images {
        hasher.update(&image.bytes);
    }
    hex::encode(hasher.finalize())[..8].to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn image(byte: u8) -> InboundImage {
        InboundImage {
            bytes: vec![byte],
            mime_type: "image/jpeg".into(),
            ext: "jpg".into(),
        }
    }

    #[test]
    fn holds_images_then_joins_a_late_caption() {
        let mut coordinator = ImageBatchCoordinator::new(30_000);
        let action = coordinator.on_message(
            "u1",
            PendingPiece {
                text: String::new(),
                images: vec![image(1)],
                context_token: Some("t1".into()),
            },
        );
        assert!(matches!(action, BatchAction::Buffer));
        assert_eq!(coordinator.pending_image_count("u1"), 1);

        let action = coordinator.on_message(
            "u1",
            PendingPiece {
                text: "what is this".into(),
                images: Vec::new(),
                context_token: Some("t2".into()),
            },
        );
        let BatchAction::Dispatch(task) = action else {
            panic!("expected dispatch");
        };
        assert_eq!(task.text, "what is this");
        assert_eq!(task.images.len(), 1);
        assert_eq!(task.context_token.as_deref(), Some("t2"));
        assert!(!coordinator.has_pending("u1"));
    }

    #[test]
    fn multiple_photos_merge_and_flush_uses_placeholder() {
        let mut coordinator = ImageBatchCoordinator::new(30_000);
        coordinator.on_message(
            "u1",
            PendingPiece {
                images: vec![image(1)],
                ..Default::default()
            },
        );
        coordinator.on_message(
            "u1",
            PendingPiece {
                images: vec![image(2)],
                ..Default::default()
            },
        );
        let task = coordinator.flush("u1").expect("pending batch");
        assert_eq!(task.images.len(), 2);
        assert_eq!(task.text, "[图片]");
        assert!(task.id.starts_with("u1:batch:"));
        assert!(coordinator.flush("u1").is_none());
    }

    #[test]
    fn zero_window_dispatches_images_immediately() {
        let mut coordinator = ImageBatchCoordinator::new(0);
        let action = coordinator.on_message(
            "u1",
            PendingPiece {
                images: vec![image(7)],
                ..Default::default()
            },
        );
        assert!(matches!(action, BatchAction::Dispatch(task) if task.images.len() == 1));
        assert!(!coordinator.has_pending("u1"));
    }

    #[test]
    fn a_new_image_restarts_the_flush_window() {
        let mut timers = FlushTimers::default();
        let first = timers.arm("u1");
        let second = timers.arm("u1");
        assert!(second > first);
        assert!(!timers.claim("u1", first), "stale timer must not flush");
        assert!(timers.is_armed("u1"));
        assert!(timers.claim("u1", second));
        assert!(!timers.is_armed("u1"));
        assert!(!timers.claim("u1", second), "a timer fires only once");
    }

    #[test]
    fn senders_are_isolated() {
        let mut coordinator = ImageBatchCoordinator::new(30_000);
        coordinator.on_message(
            "a",
            PendingPiece {
                images: vec![image(1)],
                ..Default::default()
            },
        );
        coordinator.on_message(
            "b",
            PendingPiece {
                images: vec![image(2)],
                ..Default::default()
            },
        );
        assert_eq!(coordinator.pending_count(), 2);
        let a = coordinator.flush("a").unwrap();
        assert_eq!(a.images.len(), 1);
        assert!(coordinator.has_pending("b"));
    }

    #[tokio::test]
    async fn newer_image_invalidates_a_stale_timer() {
        let sched = BatchScheduler::new(30_000);
        let a = sched
            .on_message(
                "u1",
                PendingPiece {
                    images: vec![image(1)],
                    ..Default::default()
                },
            )
            .await;
        assert!(matches!(a.0, BatchAction::Buffer));
        assert_eq!(a.1, Some(1));
        // A second image arrives before the window: it bumps the generation and
        // extends the batch. The stale timer for generation 1 must not flush.
        let b = sched
            .on_message(
                "u1",
                PendingPiece {
                    images: vec![image(2)],
                    ..Default::default()
                },
            )
            .await;
        assert_eq!(b.1, Some(2));
        assert!(
            sched.flush_if_current("u1", 1).await.is_none(),
            "stale generation must not flush"
        );
        // Only the current generation flushes, carrying both images.
        let batch = sched.flush_if_current("u1", 2).await.unwrap();
        assert_eq!(batch.images.len(), 2);
    }

    #[tokio::test]
    async fn late_caption_merges_and_stale_timer_noops() {
        let sched = BatchScheduler::new(30_000);
        let _ = sched
            .on_message(
                "u1",
                PendingPiece {
                    images: vec![image(1)],
                    ..Default::default()
                },
            )
            .await;
        // Caption joins the pending batch and dispatches as one task; the armed
        // timer (generation 1) must never also fire.
        let action = sched
            .on_message(
                "u1",
                PendingPiece {
                    text: "what is this".into(),
                    ..Default::default()
                },
            )
            .await;
        let BatchAction::Dispatch(task) = action.0 else {
            panic!("expected dispatch");
        };
        assert_eq!(task.text, "what is this");
        assert_eq!(task.images.len(), 1);
        // The stale timer must no-op; the batch was already consumed.
        assert!(sched.flush_if_current("u1", 1).await.is_none());
    }
}
