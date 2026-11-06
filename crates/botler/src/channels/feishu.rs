//! Feishu channel: event-subscription webhook.
//!
//! Handles URL verification, optional AES event-body decryption, the sender
//! allowlist, and text messages. Like every channel, the message is enqueued
//! inline in the HTTP handler (C2 arrival order) and the reply is sent after
//! the serial worker completes.

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Duration, Instant};

use aes::Aes128;
use anyhow::{bail, Context, Result};
use async_trait::async_trait;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::routing::post;
use axum::{Json, Router};
use base64::engine::general_purpose::STANDARD;
use base64::Engine as _;
use cbc::cipher::block_padding::Pkcs7;
use cbc::cipher::{BlockDecryptMut, KeyIvInit};
use cbc::Decryptor;
use md5::{Digest, Md5};
use reqwest::Client;
use serde_json::{json, Value};
use sha2::Sha256;
use tokio::sync::Mutex;

use crate::channels::allowlist::is_allowed_sender;
use crate::dispatcher::{Conversation, DispatchOptions, Dispatcher};
use crate::identity::{ChannelSource, ConversationKind};
use crate::push::{ChannelSink, ContactsStore};

const FEISHU_API: &str = "https://open.feishu.cn/open-apis";
const API_TIMEOUT: Duration = Duration::from_secs(15);

/// Feishu encrypts the event body with AES-128-CBC: the key is
/// `md5(encrypt_key)` and the IV is the first 16 bytes of the payload.
/// (`FEISHU_ENCRYPT_KEY` is the passphrase, not the key.)
pub fn decrypt_event(encrypt_key: &str, encrypt_b64: &str) -> Result<String> {
    let data = STANDARD
        .decode(encrypt_b64.trim())
        .context("feishu event is not valid base64")?;
    if data.len() < 32 {
        bail!("feishu encrypted event is too short");
    }
    let key = Md5::digest(encrypt_key.as_bytes());
    let (iv, ciphertext) = data.split_at(16);
    let mut buffer = ciphertext.to_vec();
    let plain = Decryptor::<Aes128>::new_from_slices(&key, iv)?
        .decrypt_padded_mut::<Pkcs7>(&mut buffer)
        .map_err(|_| anyhow::anyhow!("feishu event padding is invalid"))?;
    String::from_utf8(plain.to_vec()).context("feishu event is not UTF-8")
}

/// Feishu signs requests with `sha256(timestamp + nonce + encrypt_key + body)`.
pub fn verify_signature(
    encrypt_key: &str,
    timestamp: &str,
    nonce: &str,
    body: &str,
    signature: &str,
) -> bool {
    let mut hasher = Sha256::new();
    hasher.update(timestamp.as_bytes());
    hasher.update(nonce.as_bytes());
    hasher.update(encrypt_key.as_bytes());
    hasher.update(body.as_bytes());
    hex::encode(hasher.finalize()).eq_ignore_ascii_case(signature.trim())
}

/// Decode a webhook body. An `encrypt_key` always means the event must arrive
/// encrypted: accepting plaintext alongside it would let a caller skip the
/// signature check and forge events.
fn decode_event_with_key(encrypt_key: Option<&str>, body: &str) -> Result<Value> {
    let mut event: Value = serde_json::from_str(body).context("feishu event is not JSON")?;
    match event.get("encrypt").and_then(Value::as_str) {
        Some(encrypted) => {
            let Some(key) = encrypt_key.filter(|k| !k.is_empty()) else {
                bail!("encrypted event received but FEISHU_ENCRYPT_KEY is not configured");
            };
            let plain = decrypt_event(key, encrypted)?;
            event = serde_json::from_str(&plain).context("decrypted feishu event is not JSON")?;
        }
        None => {
            if encrypt_key.is_some_and(|key| !key.is_empty()) {
                bail!("plaintext event rejected while FEISHU_ENCRYPT_KEY is configured");
            }
        }
    }
    Ok(event)
}

/// The verification token lives in `header.token` for v2 events and at the
/// top level for the `url_verification` handshake.
fn event_token(event: &Value) -> Option<&str> {
    event
        .get("header")
        .and_then(|header| header.get("token"))
        .and_then(Value::as_str)
        .or_else(|| event.get("token").and_then(Value::as_str))
}

pub struct FeishuApi {
    client: Client,
    app_id: String,
    app_secret: String,
    token: Mutex<Option<(String, Instant)>>,
}

impl FeishuApi {
    pub fn new(app_id: impl Into<String>, app_secret: impl Into<String>) -> Self {
        Self {
            client: Client::builder()
                .timeout(API_TIMEOUT)
                .build()
                .unwrap_or_default(),
            app_id: app_id.into(),
            app_secret: app_secret.into(),
            token: Mutex::new(None),
        }
    }

    async fn tenant_token(&self) -> Result<String> {
        let mut cached = self.token.lock().await;
        if let Some((token, expires_at)) = cached.as_ref() {
            if *expires_at > Instant::now() + Duration::from_secs(60) {
                return Ok(token.clone());
            }
        }
        let response = self
            .client
            .post(format!("{FEISHU_API}/auth/v3/tenant_access_token/internal"))
            .json(&json!({ "app_id": self.app_id, "app_secret": self.app_secret }))
            .send()
            .await
            .context("feishu tenant token request")?;
        let payload: Value = response.json().await.context("feishu tenant token body")?;
        let token = payload
            .get("tenant_access_token")
            .and_then(Value::as_str)
            .filter(|token| !token.is_empty())
            .ok_or_else(|| anyhow::anyhow!("feishu tenant_access_token missing: {payload}"))?
            .to_string();
        let expire = payload
            .get("expire")
            .and_then(Value::as_u64)
            .unwrap_or(7200);
        *cached = Some((
            token.clone(),
            Instant::now() + Duration::from_secs(expire.saturating_sub(60)),
        ));
        Ok(token)
    }

    pub async fn reply_text(&self, chat_id: &str, text: &str) -> Result<()> {
        let token = self.tenant_token().await?;
        let response = self
            .client
            .post(format!(
                "{FEISHU_API}/im/v1/messages?receive_id_type=chat_id"
            ))
            .bearer_auth(token)
            .json(&json!({
                "receive_id": chat_id,
                "msg_type": "text",
                "content": json!({ "text": text }).to_string(),
            }))
            .send()
            .await
            .context("feishu send request")?;
        let status = response.status();
        let payload: Value = response.json().await.unwrap_or(Value::Null);
        if !status.is_success() || payload.get("code").and_then(Value::as_i64) != Some(0) {
            bail!("feishu send failed ({status}): {payload}");
        }
        Ok(())
    }
}

/// Push channel: address is the chat id.
pub struct FeishuSink {
    api: Arc<FeishuApi>,
}

impl FeishuSink {
    pub fn new(api: Arc<FeishuApi>) -> Self {
        Self { api }
    }
}

#[async_trait]
impl ChannelSink for FeishuSink {
    fn source(&self) -> ChannelSource {
        ChannelSource::Feishu
    }

    async fn send_text(&self, address: &str, text: &str) -> Result<()> {
        self.api.reply_text(address, text).await
    }
}

pub struct FeishuChannel {
    dispatcher: Arc<Dispatcher>,
    api: Arc<FeishuApi>,
    verification_token: String,
    encrypt_key: Option<String>,
    allow_from: Option<String>,
    contacts: ContactsStore,
    port: u16,
}

impl FeishuChannel {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        dispatcher: Arc<Dispatcher>,
        api: Arc<FeishuApi>,
        verification_token: String,
        encrypt_key: Option<String>,
        allow_from: Option<String>,
        contacts: ContactsStore,
        port: u16,
    ) -> Arc<Self> {
        Arc::new(Self {
            dispatcher,
            api,
            verification_token,
            encrypt_key,
            allow_from,
            contacts,
            port,
        })
    }

    pub async fn run(self: Arc<Self>) -> Result<()> {
        let app = Router::new()
            .route("/webhook", post(webhook))
            .with_state(Arc::clone(&self));
        let addr = SocketAddr::from(([0, 0, 0, 0], self.port));
        let listener = tokio::net::TcpListener::bind(addr)
            .await
            .with_context(|| format!("bind feishu webhook on {addr}"))?;
        tracing::info!("[feishu] webhook listening on {addr}/webhook");
        axum::serve(listener, app)
            .await
            .context("feishu webhook server")
    }

    fn decode_event(&self, body: &str) -> Result<Value> {
        decode_event_with_key(self.encrypt_key.as_deref(), body)
    }

    /// Verify the request signature before parsing anything, so a forged
    /// request can never reach the dispatcher.
    fn verify_request(&self, headers: &HeaderMap, body: &str) -> Result<()> {
        let Some(key) = self.encrypt_key.as_deref().filter(|k| !k.is_empty()) else {
            return Ok(());
        };
        let header = |name: &str| headers.get(name).and_then(|value| value.to_str().ok());
        let (Some(timestamp), Some(nonce), Some(signature)) = (
            header("x-lark-request-timestamp"),
            header("x-lark-request-nonce"),
            header("x-lark-signature"),
        ) else {
            bail!("encrypted request is missing Feishu signature headers");
        };
        if !verify_signature(key, timestamp, nonce, body, signature) {
            bail!("Feishu request signature mismatch");
        }
        Ok(())
    }

    fn token_matches(&self, event: &Value) -> bool {
        matches!(event_token(event), Some(token) if token == self.verification_token)
    }

    fn handle_event(self: &Arc<Self>, event: Value) {
        let header = event.get("header");
        if !self.token_matches(&event) {
            tracing::warn!("[feishu] event verification token mismatch; ignored");
            return;
        }
        if header
            .and_then(|h| h.get("event_type"))
            .and_then(Value::as_str)
            != Some("im.message.receive_v1")
        {
            return;
        }
        let Some(event_body) = event.get("event") else {
            return;
        };
        let Some(message) = event_body.get("message") else {
            return;
        };
        let Some(chat_id) = message.get("chat_id").and_then(Value::as_str) else {
            return;
        };
        let Some(message_id) = message.get("message_id").and_then(Value::as_str) else {
            return;
        };
        let sender = event_body.get("sender").and_then(|s| s.get("sender_id"));
        let open_id = sender
            .and_then(|s| s.get("open_id"))
            .and_then(Value::as_str);
        let user_id = sender
            .and_then(|s| s.get("user_id"))
            .and_then(Value::as_str);
        let union_id = sender
            .and_then(|s| s.get("union_id"))
            .and_then(Value::as_str);
        let identities = [Some(chat_id), open_id, user_id, union_id];
        if !is_allowed_sender(self.allow_from.as_deref(), &identities) {
            tracing::warn!("[feishu] ignoring sender outside allowlist (chat {chat_id})");
            return;
        }
        let message_type = message
            .get("message_type")
            .and_then(Value::as_str)
            .unwrap_or_default();
        if message_type != "text" {
            tracing::info!("[feishu] ignored non-text message ({message_type})");
            return;
        }
        let content = message
            .get("content")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let text = serde_json::from_str::<Value>(content)
            .ok()
            .and_then(|value| {
                value
                    .get("text")
                    .and_then(Value::as_str)
                    .map(str::to_string)
            })
            .unwrap_or_default();
        if text.trim().is_empty() {
            return;
        }
        let chat_type = message
            .get("chat_type")
            .and_then(Value::as_str)
            .unwrap_or("p2p");
        let kind = if chat_type == "p2p" {
            ConversationKind::Direct
        } else {
            ConversationKind::Group
        };

        self.contacts.record(ChannelSource::Feishu, chat_id);
        let options = DispatchOptions {
            id: Some(message_id.to_string()),
            source: ChannelSource::Feishu,
            project_hint: None,
            projects_hint: None,
            recipient: Some(crate::push::Recipient {
                source: ChannelSource::Feishu,
                user_id: chat_id.to_string(),
            }),
            inbound_images: Vec::new(),
            conversation: Some(Conversation {
                id: chat_id.to_string(),
                sender_id: open_id.or(user_id).unwrap_or(chat_id).to_string(),
                kind,
            }),
            origin: None,
        };
        // Enqueue inline: arrival order (C2), reply asynchronously.
        let task = self.dispatcher.enqueue(&text, options);
        let api = Arc::clone(&self.api);
        let chat_id = chat_id.to_string();
        tokio::spawn(async move {
            let result = task.wait().await;
            let body = if result.text.trim().is_empty() && !result.images.is_empty() {
                "(image generated, but the Feishu channel does not support sending images yet)"
                    .to_string()
            } else {
                result.text
            };
            if let Err(err) = api.reply_text(&chat_id, &body).await {
                tracing::error!("[feishu] reply to {chat_id} failed: {err}");
            }
        });
    }
}

async fn webhook(
    State(channel): State<Arc<FeishuChannel>>,
    headers: HeaderMap,
    body: String,
) -> impl IntoResponse {
    if body.trim().is_empty() {
        return (StatusCode::OK, Json(json!({})));
    }
    if let Err(err) = channel.verify_request(&headers, &body) {
        tracing::warn!("[feishu] rejected unauthenticated request: {err}");
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({ "code": 401, "msg": err.to_string() })),
        );
    }
    match channel.decode_event(&body) {
        Ok(event) => {
            if !channel.token_matches(&event) {
                tracing::warn!("[feishu] rejected request with a mismatched verification token");
                return (
                    StatusCode::UNAUTHORIZED,
                    Json(json!({ "code": 401, "msg": "verification token mismatch" })),
                );
            }
            if event.get("type").and_then(Value::as_str) == Some("url_verification") {
                tracing::info!("[feishu] URL verification");
                let challenge = event.get("challenge").cloned().unwrap_or(Value::Null);
                return (StatusCode::OK, Json(json!({ "challenge": challenge })));
            }
            channel.handle_event(event);
            (StatusCode::OK, Json(json!({ "code": 0 })))
        }
        Err(err) => {
            tracing::warn!("[feishu] rejected event: {err}");
            (
                StatusCode::BAD_REQUEST,
                Json(json!({ "code": 400, "msg": err.to_string() })),
            )
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use aes::cipher::BlockEncryptMut;
    use base64::engine::general_purpose::STANDARD;

    /// Round-trip a body the way Feishu encrypts it: AES-128-CBC, key =
    /// md5(passphrase), IV prefixed to the ciphertext, PKCS7 padding.
    #[test]
    fn decrypts_a_feishu_event_body() {
        let passphrase = "secret-key";
        let plain = r#"{"type":"url_verification","challenge":"abc"}"#;
        let key = Md5::digest(passphrase.as_bytes());
        let iv = [7u8; 16];
        let mut buffer = plain.as_bytes().to_vec();
        let len = buffer.len();
        buffer.resize(len + 16 - len % 16, 0);
        let encrypted = cbc::Encryptor::<Aes128>::new_from_slices(&key, &iv)
            .unwrap()
            .encrypt_padded_mut::<Pkcs7>(&mut buffer, len)
            .unwrap()
            .to_vec();
        let mut payload = iv.to_vec();
        payload.extend(encrypted);
        let b64 = STANDARD.encode(payload);

        assert_eq!(decrypt_event(passphrase, &b64).unwrap(), plain);
    }

    #[test]
    fn rejects_malformed_payloads() {
        assert!(decrypt_event("k", "not base64!").is_err());
        assert!(decrypt_event("k", &STANDARD.encode([0u8; 8])).is_err());
    }

    #[test]
    fn rejects_plaintext_events_when_encryption_is_configured() {
        let plain = r#"{"header":{"token":"t"},"type":"url_verification"}"#;
        assert!(decode_event_with_key(None, plain).is_ok());
        assert!(decode_event_with_key(Some(""), plain).is_ok());
        assert!(decode_event_with_key(Some("key"), plain).is_err());
    }

    #[test]
    fn extracts_the_verification_token_from_both_envelopes() {
        let v2 = json!({ "header": { "token": "abc" } });
        let handshake = json!({ "token": "abc" });
        let missing = json!({});
        assert_eq!(event_token(&v2), Some("abc"));
        assert_eq!(event_token(&handshake), Some("abc"));
        assert_eq!(event_token(&missing), None);
    }

    #[test]
    fn verifies_the_request_signature() {
        let key = "encrypt-key";
        let (timestamp, nonce, body) = ("1700000000", "nonce", r#"{"encrypt":"x"}"#);
        let mut hasher = Sha256::new();
        hasher.update(timestamp.as_bytes());
        hasher.update(nonce.as_bytes());
        hasher.update(key.as_bytes());
        hasher.update(body.as_bytes());
        let signature = hex::encode(hasher.finalize());

        assert!(verify_signature(key, timestamp, nonce, body, &signature));
        assert!(verify_signature(
            key,
            timestamp,
            nonce,
            body,
            &signature.to_uppercase()
        ));
        assert!(!verify_signature(key, timestamp, nonce, body, "deadbeef"));
        assert!(!verify_signature(key, timestamp, nonce, "{}}", &signature));
    }
}
