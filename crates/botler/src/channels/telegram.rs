//! Telegram channel via the Bot API long-poll endpoints.
//!
//! v1 used grammy; the wire behavior we depend on is small (getUpdates with an
//! offset, sendMessage), so this adapter talks to the Bot API directly with
//! `reqwest`. Text-only, like v1.

use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use async_trait::async_trait;
use reqwest::Client;
use serde_json::{json, Value};

use crate::channels::allowlist::is_allowed_sender;
use crate::dispatcher::{Conversation, DispatchOptions, Dispatcher};
use crate::identity::{ChannelSource, ConversationKind};
use crate::push::{ChannelSink, ContactsStore};

const API_ROOT: &str = "https://api.telegram.org";
const LONG_POLL_SECONDS: u64 = 30;
const TEXT_LIMIT: usize = 4000;

pub struct TelegramApi {
    client: Client,
    token: String,
}

impl TelegramApi {
    pub fn new(token: impl Into<String>) -> Self {
        let client = Client::builder()
            .timeout(Duration::from_secs(LONG_POLL_SECONDS + 15))
            .build()
            .unwrap_or_default();
        Self {
            client,
            token: token.into(),
        }
    }

    async fn call(&self, method: &str, body: Value) -> Result<Value> {
        let url = format!("{API_ROOT}/bot{}/{method}", self.token);
        let response = self
            .client
            .post(&url)
            .json(&body)
            .send()
            .await
            .with_context(|| format!("telegram {method} request"))?;
        let status = response.status();
        let payload: Value = response
            .json()
            .await
            .with_context(|| format!("telegram {method} response"))?;
        if !status.is_success() || payload.get("ok").and_then(Value::as_bool) != Some(true) {
            anyhow::bail!("telegram {method} failed ({status}): {payload}");
        }
        Ok(payload)
    }

    pub async fn send_message(&self, chat_id: &str, text: &str) -> Result<()> {
        for chunk in split_text(text, TEXT_LIMIT) {
            self.call("sendMessage", json!({ "chat_id": chat_id, "text": chunk }))
                .await?;
        }
        Ok(())
    }

    async fn get_updates(&self, offset: i64) -> Result<Vec<Value>> {
        let payload = self
            .call(
                "getUpdates",
                json!({
                    "offset": offset,
                    "timeout": LONG_POLL_SECONDS,
                    "allowed_updates": ["message"],
                }),
            )
            .await?;
        Ok(payload
            .get("result")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default())
    }
}

/// Push channel: address is the chat id.
pub struct TelegramSink {
    api: Arc<TelegramApi>,
}

impl TelegramSink {
    pub fn new(api: Arc<TelegramApi>) -> Self {
        Self { api }
    }
}

#[async_trait]
impl ChannelSink for TelegramSink {
    fn source(&self) -> ChannelSource {
        ChannelSource::Telegram
    }

    async fn send_text(&self, address: &str, text: &str) -> Result<()> {
        self.api.send_message(address, text).await
    }
}

pub struct TelegramChannel {
    api: Arc<TelegramApi>,
    dispatcher: Arc<Dispatcher>,
    allow_from: Option<String>,
    contacts: ContactsStore,
}

impl TelegramChannel {
    pub fn new(
        api: Arc<TelegramApi>,
        dispatcher: Arc<Dispatcher>,
        allow_from: Option<String>,
        contacts: ContactsStore,
    ) -> Self {
        Self {
            api,
            dispatcher,
            allow_from,
            contacts,
        }
    }

    /// Long-poll until the process stops. Transient errors back off and retry.
    pub async fn run(self) -> Result<()> {
        tracing::info!("[telegram] starting long polling");
        let mut offset = 0i64;
        loop {
            match self.api.get_updates(offset).await {
                Ok(updates) => {
                    for update in updates {
                        if let Some(id) = update.get("update_id").and_then(Value::as_i64) {
                            offset = offset.max(id + 1);
                        }
                        self.accept(update);
                    }
                }
                Err(err) => {
                    tracing::error!("[telegram] getUpdates failed: {err}");
                    tokio::time::sleep(Duration::from_secs(5)).await;
                }
            }
        }
    }

    fn accept(&self, update: Value) {
        let Some(message) = update.get("message") else {
            return;
        };
        let Some(text) = message.get("text").and_then(Value::as_str) else {
            tracing::info!("[telegram] ignored non-text message");
            return;
        };
        let Some(chat_id) = message
            .get("chat")
            .and_then(|chat| chat.get("id"))
            .and_then(Value::as_i64)
            .map(|id| id.to_string())
        else {
            return;
        };
        let Some(message_id) = message.get("message_id").and_then(Value::as_i64) else {
            return;
        };
        let from = message.get("from");
        let from_id = from
            .and_then(|f| f.get("id"))
            .and_then(Value::as_i64)
            .map(|id| id.to_string());
        let username = from
            .and_then(|f| f.get("username"))
            .and_then(Value::as_str)
            .map(str::to_string);
        let identities = [
            from_id.as_deref(),
            username.as_deref(),
            Some(chat_id.as_str()),
        ];
        if !is_allowed_sender(self.allow_from.as_deref(), &identities) {
            tracing::warn!("[telegram] ignoring sender outside allowlist (chat {chat_id})");
            return;
        }
        let chat_type = message
            .get("chat")
            .and_then(|chat| chat.get("type"))
            .and_then(Value::as_str)
            .unwrap_or("private");
        let kind = if chat_type == "private" {
            ConversationKind::Direct
        } else {
            ConversationKind::Group
        };

        self.contacts.record(ChannelSource::Telegram, &chat_id);
        let options = DispatchOptions {
            id: Some(format!("{chat_id}:{message_id}")),
            source: ChannelSource::Telegram,
            project_hint: None,
            projects_hint: None,
            recipient: Some(crate::push::Recipient {
                source: ChannelSource::Telegram,
                user_id: chat_id.clone(),
            }),
            inbound_images: Vec::new(),
            conversation: Some(Conversation {
                id: chat_id.clone(),
                sender_id: from_id.unwrap_or_else(|| chat_id.clone()),
                kind,
            }),
            origin: None,
        };
        // Enqueue inline: arrival order (C2), then reply asynchronously.
        let task = self.dispatcher.enqueue(text, options);
        let api = Arc::clone(&self.api);
        let reply_chat = chat_id.clone();
        tokio::spawn(async move {
            let result = task.wait().await;
            // Text-only channel: an images-only reply still needs a body.
            let body = if result.text.trim().is_empty() && !result.images.is_empty() {
                "(image generated, but the Telegram channel does not support sending images yet)"
                    .to_string()
            } else {
                result.text
            };
            if let Err(err) = api.send_message(&reply_chat, &body).await {
                tracing::error!("[telegram] reply to {reply_chat} failed: {err}");
            }
        });
    }
}

fn split_text(text: &str, limit: usize) -> Vec<String> {
    if text.chars().count() <= limit {
        return vec![text.to_string()];
    }
    let mut out = Vec::new();
    let mut current = String::new();
    for ch in text.chars() {
        if current.chars().count() >= limit {
            out.push(std::mem::take(&mut current));
        }
        current.push(ch);
    }
    if !current.is_empty() {
        out.push(current);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn long_replies_are_split_below_the_api_limit() {
        let text = "a".repeat(TEXT_LIMIT * 2 + 10);
        let chunks = split_text(&text, TEXT_LIMIT);
        assert_eq!(chunks.len(), 3);
        assert!(chunks
            .iter()
            .all(|chunk| chunk.chars().count() <= TEXT_LIMIT));
        assert_eq!(chunks.concat().len(), text.len());
    }
}
