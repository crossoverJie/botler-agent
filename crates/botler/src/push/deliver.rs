//! Push delivery with channel fallback.
//!
//! Primary channel first; on failure degrade through
//! `telegram → feishu → wechat`, only over channels that are registered
//! (configured) and have recorded contact addresses. Degraded channels get
//! plain text; images remain WeChat-only, matching v1.

use std::collections::HashMap;
use std::sync::Arc;

use anyhow::{bail, Result};
use async_trait::async_trait;

use crate::identity::ChannelSource;
use crate::push::contacts::ContactsStore;
use crate::push::types::{PushPayload, Recipient};

pub const FALLBACK_ORDER: [ChannelSource; 3] = [
    ChannelSource::Telegram,
    ChannelSource::Feishu,
    ChannelSource::Wechat,
];

#[async_trait]
pub trait ChannelSink: Send + Sync {
    fn source(&self) -> ChannelSource;
    async fn send_text(&self, address: &str, text: &str) -> Result<()>;
    /// Default: this channel cannot send images.
    async fn send_image(&self, _address: &str, _path: &str, _caption: &str) -> Result<()> {
        bail!("image delivery is not supported on this channel")
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DeliveryOutcome {
    pub ok: bool,
    pub via: Option<ChannelSource>,
    pub error: Option<String>,
}

pub struct PushService {
    sinks: HashMap<ChannelSource, Arc<dyn ChannelSink>>,
    contacts: ContactsStore,
}

impl PushService {
    pub fn new(contacts: ContactsStore) -> Self {
        Self {
            sinks: HashMap::new(),
            contacts,
        }
    }

    pub fn register(&mut self, sink: Arc<dyn ChannelSink>) {
        self.sinks.insert(sink.source(), sink);
    }

    pub fn contacts(&self) -> &ContactsStore {
        &self.contacts
    }

    fn configured(&self, source: ChannelSource) -> bool {
        self.sinks.contains_key(&source)
    }

    async fn deliver_to(
        &self,
        source: ChannelSource,
        payload: &PushPayload,
        address: &str,
        with_images: bool,
    ) -> Result<()> {
        let Some(sink) = self.sinks.get(&source) else {
            bail!("{source}: channel not configured");
        };
        if !payload.text.trim().is_empty() {
            sink.send_text(address, &payload.text).await?;
        }
        if with_images {
            for image in &payload.images {
                sink.send_image(address, image, "").await?;
            }
        }
        Ok(())
    }

    pub async fn deliver(&self, payload: &PushPayload, recipient: &Recipient) -> DeliveryOutcome {
        let primary = recipient.source;
        if !matches!(
            primary,
            ChannelSource::Telegram | ChannelSource::Feishu | ChannelSource::Wechat
        ) {
            return DeliveryOutcome {
                ok: false,
                via: None,
                error: Some(format!("{primary} is not a push channel")),
            };
        }
        match self
            .deliver_to(
                primary,
                payload,
                &recipient.user_id,
                primary == ChannelSource::Wechat,
            )
            .await
        {
            Ok(()) => {
                return DeliveryOutcome {
                    ok: true,
                    via: Some(primary),
                    error: None,
                }
            }
            Err(err) => tracing::warn!("[push] primary {primary} failed: {err}"),
        }

        let text_only = PushPayload {
            text: payload.text.clone(),
            images: vec![],
        };
        for source in FALLBACK_ORDER {
            if source == primary || !self.configured(source) {
                continue;
            }
            let addresses = self.contacts.get(source);
            for address in addresses {
                match self.deliver_to(source, &text_only, &address, false).await {
                    Ok(()) => {
                        tracing::info!("[push] degraded to {source} -> {address}");
                        return DeliveryOutcome {
                            ok: true,
                            via: Some(source),
                            error: None,
                        };
                    }
                    Err(err) => {
                        tracing::error!("[push] fallback {source} -> {address} failed: {err}")
                    }
                }
            }
        }
        DeliveryOutcome {
            ok: false,
            via: None,
            error: Some(format!(
                "primary {primary} failed and no fallback succeeded"
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    struct FakeSink {
        source: ChannelSource,
        fail: bool,
        texts: AtomicUsize,
        images: AtomicUsize,
    }

    #[async_trait]
    impl ChannelSink for FakeSink {
        fn source(&self) -> ChannelSource {
            self.source
        }
        async fn send_text(&self, _address: &str, _text: &str) -> Result<()> {
            self.texts.fetch_add(1, Ordering::SeqCst);
            if self.fail {
                bail!("boom")
            }
            Ok(())
        }
        async fn send_image(&self, _address: &str, _path: &str, _caption: &str) -> Result<()> {
            self.images.fetch_add(1, Ordering::SeqCst);
            Ok(())
        }
    }

    #[tokio::test]
    async fn falls_back_only_to_configured_channels_with_contacts() {
        let dir = tempfile::tempdir().unwrap();
        let contacts = ContactsStore::new(dir.path().join("contacts.json"));
        contacts.record(ChannelSource::Feishu, "oc_1");
        let feishu = Arc::new(FakeSink {
            source: ChannelSource::Feishu,
            fail: false,
            texts: AtomicUsize::new(0),
            images: AtomicUsize::new(0),
        });
        let mut service = PushService::new(contacts);
        service.register(Arc::new(FakeSink {
            source: ChannelSource::Wechat,
            fail: true,
            texts: AtomicUsize::new(0),
            images: AtomicUsize::new(0),
        }));
        service.register(feishu.clone());

        let outcome = service
            .deliver(
                &PushPayload {
                    text: "hi".into(),
                    images: vec!["/x.png".into()],
                },
                &Recipient {
                    source: ChannelSource::Wechat,
                    user_id: "u1".into(),
                },
            )
            .await;
        assert!(outcome.ok);
        assert_eq!(outcome.via, Some(ChannelSource::Feishu));
        assert_eq!(feishu.texts.load(Ordering::SeqCst), 1);
        // Degraded delivery never sends images.
        assert_eq!(feishu.images.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn unconfigured_channel_is_never_tried() {
        let dir = tempfile::tempdir().unwrap();
        let contacts = ContactsStore::new(dir.path().join("contacts.json"));
        contacts.record(ChannelSource::Telegram, "42");
        let service = PushService::new(contacts);
        let outcome = service
            .deliver(
                &PushPayload {
                    text: "hi".into(),
                    images: vec![],
                },
                &Recipient {
                    source: ChannelSource::Wechat,
                    user_id: "u1".into(),
                },
            )
            .await;
        assert!(!outcome.ok);
    }
}
