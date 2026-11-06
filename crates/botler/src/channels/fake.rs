//! Deterministic channel adapter for offline tests and the §10.1 acceptance
//! scenarios. It drives the real dispatcher (and therefore the real runner,
//! session registry and tools) without any network transport.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use crate::dispatcher::{DispatchOptions, DispatchResult, Dispatcher, EnqueuedTask};
use crate::identity::{ChannelSource, ConversationKind};
use crate::runtime::InboundImage;

pub struct FakeChannel {
    dispatcher: Arc<Dispatcher>,
    pub source: ChannelSource,
    pub conversation_id: String,
    pub sender_id: String,
    pub kind: ConversationKind,
    seq: AtomicU64,
}

impl FakeChannel {
    pub fn new(
        dispatcher: Arc<Dispatcher>,
        source: ChannelSource,
        conversation_id: impl Into<String>,
        sender_id: impl Into<String>,
        kind: ConversationKind,
    ) -> Self {
        Self {
            dispatcher,
            source,
            conversation_id: conversation_id.into(),
            sender_id: sender_id.into(),
            kind,
            seq: AtomicU64::new(0),
        }
    }

    fn options(&self, images: Vec<InboundImage>) -> DispatchOptions {
        let seq = self.seq.fetch_add(1, Ordering::SeqCst) + 1;
        DispatchOptions {
            id: Some(format!(
                "{}:{}:{seq}",
                self.source.as_str(),
                self.conversation_id
            )),
            source: self.source,
            project_hint: None,
            projects_hint: None,
            recipient: None,
            inbound_images: images,
            conversation: Some(crate::dispatcher::Conversation {
                id: self.conversation_id.clone(),
                sender_id: self.sender_id.clone(),
                kind: self.kind,
            }),
            origin: None,
        }
    }

    /// Enqueue at arrival time; the caller may await later (ordering tests).
    pub fn enqueue(&self, text: &str) -> EnqueuedTask {
        self.dispatcher.enqueue(text, self.options(Vec::new()))
    }

    pub fn enqueue_with_images(&self, text: &str, images: Vec<InboundImage>) -> EnqueuedTask {
        self.dispatcher.enqueue(text, self.options(images))
    }

    pub async fn say(&self, text: &str) -> DispatchResult {
        self.enqueue(text).wait().await
    }

    pub async fn say_with_images(&self, text: &str, images: Vec<InboundImage>) -> DispatchResult {
        self.enqueue_with_images(text, images).wait().await
    }
}
