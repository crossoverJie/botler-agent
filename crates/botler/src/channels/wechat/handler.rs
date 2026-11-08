//! WeChat message handler: allowlist gate, batching, dispatch and reply.
//!
//! The transport hands us fully decoded messages (text already extracted and
//! images already decrypted). This layer follows the same contract as every
//! other channel: enqueue on the global queue at receive time (C2), send the
//! result when it completes.

use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use async_trait::async_trait;

use crate::channels::allowlist::is_allowed_sender;
use crate::channels::image_batch::{BatchAction, BatchDispatch, BatchScheduler, PendingPiece};
use crate::dispatcher::{Conversation, DispatchOptions, Dispatcher};
use crate::identity::{ChannelSource, ConversationKind};
use crate::runtime::InboundImage;

use super::context::WechatContextStore;

/// One decoded message item from the transport.
#[derive(Debug, Clone, Default)]
pub struct WechatItem {
    pub text: Option<String>,
    pub image: Option<InboundImage>,
}

/// One fully decoded inbound WeChat message.
#[derive(Debug, Clone, Default)]
pub struct WechatMessage {
    pub sender_id: String,
    /// Normally equal to `sender_id` (iLink conversations are per-user).
    pub conversation_id: String,
    pub context_token: Option<String>,
    pub items: Vec<WechatItem>,
}

impl WechatMessage {
    /// Joined text plus every image in the message.
    pub fn piece(&self) -> PendingPiece {
        let text = self
            .items
            .iter()
            .filter_map(|item| item.text.as_deref())
            .map(str::trim)
            .filter(|text| !text.is_empty())
            .collect::<Vec<_>>()
            .join("\n");
        let images = self
            .items
            .iter()
            .filter_map(|item| item.image.clone())
            .collect();
        PendingPiece {
            text,
            images,
            context_token: self.context_token.clone(),
        }
    }
}

/// The transport owned by the SDK integration. Kept behind a trait so the
/// batching/reply logic can be tested without the network, and so the
/// `weixin-agent` seam (spike S-4) stays in one place.
#[async_trait]
pub trait WechatTransport: Send + Sync {
    async fn send_text(&self, to: &str, text: &str, context_token: Option<&str>) -> Result<()>;
    async fn send_image(
        &self,
        _to: &str,
        _path: &str,
        _caption: &str,
        _context_token: Option<&str>,
    ) -> Result<()> {
        anyhow::bail!("image delivery is not supported by this transport")
    }
}

/// Owner plus optional extra senders. The owner is always allowed; when
/// `WECHAT_ALLOW_FROM` is set, only listed extra senders join. Before login
/// (no owner known) an unset allowlist keeps the personal-deployment default.
#[derive(Debug, Clone, Default)]
pub struct WechatGate {
    pub owner: Option<String>,
    pub allow_from: Option<String>,
}

impl WechatGate {
    pub fn new(owner: Option<String>, allow_from: Option<String>) -> Self {
        Self { owner, allow_from }
    }

    pub fn allows(&self, sender: &str) -> bool {
        if self.owner.as_deref() == Some(sender) {
            return true;
        }
        match (&self.owner, &self.allow_from) {
            // Known owner without extra senders configured: owner only.
            (Some(_), None) => false,
            // Before login an unset allowlist keeps the personal default.
            (None, None) => true,
            // Extra senders configured: the list is the gate.
            (_, Some(_)) => is_allowed_sender(self.allow_from.as_deref(), &[Some(sender)]),
        }
    }
}

pub struct WechatHandler {
    dispatcher: Arc<Dispatcher>,
    transport: Arc<dyn WechatTransport>,
    gate: WechatGate,
    context: WechatContextStore,
    /// Per-sender batching + flush generations. The scheduler holds the batch
    /// lock across both the batch update and the timer's claim/flush so a stale
    /// timer can never flush a batch a newer image has already extended.
    scheduler: BatchScheduler,
    window_ms: u64,
}

impl WechatHandler {
    pub fn new(
        dispatcher: Arc<Dispatcher>,
        transport: Arc<dyn WechatTransport>,
        gate: WechatGate,
        context: WechatContextStore,
        window_ms: u64,
    ) -> Arc<Self> {
        Arc::new(Self {
            dispatcher,
            transport,
            gate,
            context,
            scheduler: BatchScheduler::new(window_ms),
            window_ms,
        })
    }

    pub fn gate(&self) -> &WechatGate {
        &self.gate
    }

    pub fn set_owner(&mut self, owner: Option<String>) {
        self.gate.owner = owner;
    }

    /// Entry point called by the SDK's `MessageHandler`.
    pub async fn handle(self: &Arc<Self>, message: WechatMessage) -> Result<()> {
        let sender = message.sender_id.trim();
        if sender.is_empty() {
            return Ok(());
        }
        if !self.gate.allows(sender) {
            tracing::warn!("[wechat] ignoring message from non-allowed sender {sender}");
            return Ok(());
        }
        let now = crate::fsutil::now_epoch_ms();
        self.context
            .record_message(sender, message.context_token.as_deref(), now);

        let piece = message.piece();
        // Buffer/dispatch the piece and arm a flush generation atomically. The
        // scheduler takes the batch lock first, so a timer that later claims a
        // stale generation cannot slip a flush in after this newer image has
        // extended the batch (see BatchScheduler).
        let (action, generation) = self.scheduler.on_message(sender, piece).await;
        match (action, generation) {
            (BatchAction::Buffer, Some(generation)) => {
                // The caption may arrive moments later; flush on expiry. Each
                // buffered image restarts the window via a fresh generation, so
                // an older timer cannot flush while a newer image is waiting.
                let this = Arc::clone(self);
                let sender = sender.to_string();
                let window = self.window_ms;
                tokio::spawn(async move {
                    tokio::time::sleep(Duration::from_millis(window)).await;
                    // flush_if_current holds the batch lock across claim + flush,
                    // so a newer image that arrived in the meantime has already
                    // bumped the generation and this stale claim returns None.
                    if let Some(batch) = this.scheduler.flush_if_current(&sender, generation).await
                    {
                        this.dispatch_batch(&sender, batch);
                    }
                });
            }
            (BatchAction::Dispatch(batch), _) => self.dispatch_batch(sender, batch),
            _ => {}
        }
        Ok(())
    }

    fn dispatch_batch(self: &Arc<Self>, sender: &str, batch: BatchDispatch) {
        let options = DispatchOptions {
            id: Some(format!("wechat:{sender}:{}", batch.id)),
            source: ChannelSource::Wechat,
            project_hint: None,
            projects_hint: None,
            recipient: None,
            inbound_images: batch.images.clone(),
            conversation: Some(Conversation {
                id: sender.to_string(),
                sender_id: sender.to_string(),
                kind: ConversationKind::Direct,
            }),
            origin: None,
        };
        let task = self.dispatcher.enqueue(&batch.text, options);
        let transport = Arc::clone(&self.transport);
        let sender = sender.to_string();
        let context_token = batch.context_token;
        tokio::spawn(async move {
            let result = task.wait().await;
            let text = strip_markdown(&result.text);
            if !text.trim().is_empty() {
                if let Err(err) = transport
                    .send_text(&sender, &text, context_token.as_deref())
                    .await
                {
                    tracing::error!("[wechat] send text to {sender} failed: {err}");
                }
            }
            // WeChat is the only channel that sends images.
            for image in &result.images {
                if let Err(err) = transport
                    .send_image(&sender, image, "", context_token.as_deref())
                    .await
                {
                    tracing::error!("[wechat] send image to {sender} failed: {err}");
                }
            }
        });
    }
}

/// Minimal plain-text rendering for WeChat. The SDK's streaming filter is the
/// long-term answer (design §5.3); this keeps parity until that seam lands.
pub fn strip_markdown(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for line in text.lines() {
        let trimmed = line.trim_start();
        let content = if trimmed.starts_with('#') {
            trimmed.trim_start_matches('#').trim_start()
        } else if let Some(rest) = trimmed.strip_prefix("- ") {
            out.push_str("• ");
            rest
        } else {
            line
        };
        out.push_str(content);
        out.push('\n');
    }
    let out = out
        .replace("**", "")
        .replace("__", "")
        .replace('`', "")
        .replace("~~", "");
    // [label](url) -> label (url)
    let mut rendered = String::with_capacity(out.len());
    let mut rest = out.as_str();
    while let Some(open) = rest.find('[') {
        rendered.push_str(&rest[..open]);
        let after = &rest[open + 1..];
        let Some(mid) = after.find("](") else {
            rendered.push_str(&rest[open..]);
            rest = "";
            break;
        };
        let label = &after[..mid];
        let tail = &after[mid + 2..];
        let Some(close) = tail.find(')') else {
            rendered.push_str(&rest[open..]);
            rest = "";
            break;
        };
        let url = &tail[..close];
        rendered.push_str(label);
        if !url.is_empty() {
            rendered.push_str(" (");
            rendered.push_str(url);
            rendered.push(')');
        }
        rest = &tail[close + 1..];
    }
    rendered.push_str(rest);
    rendered.trim_end().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn owner_and_allowlist_gate() {
        let gate = WechatGate::new(Some("owner".into()), None);
        assert!(gate.allows("owner"));
        assert!(!gate.allows("stranger"));

        let gate = WechatGate::new(Some("owner".into()), Some("friend".into()));
        assert!(gate.allows("owner"));
        assert!(gate.allows("friend"));
        assert!(!gate.allows("stranger"));

        let gate = WechatGate::new(None, None);
        assert!(gate.allows("anyone"));
    }

    #[test]
    fn message_piece_joins_text_and_collects_images() {
        let message = WechatMessage {
            sender_id: "u".into(),
            items: vec![
                WechatItem {
                    text: Some("look".into()),
                    image: None,
                },
                WechatItem {
                    text: Some("  ".into()),
                    image: Some(InboundImage {
                        bytes: vec![1],
                        mime_type: "image/png".into(),
                        ext: "png".into(),
                    }),
                },
            ],
            ..Default::default()
        };
        let piece = message.piece();
        assert_eq!(piece.text, "look");
        assert_eq!(piece.images.len(), 1);
    }

    #[test]
    fn markdown_becomes_plain_text() {
        assert_eq!(
            strip_markdown("# Title\n**bold** and `code`\n- item\n[link](https://x)"),
            "Title\nbold and code\n• item\nlink (https://x)"
        );
    }
}
