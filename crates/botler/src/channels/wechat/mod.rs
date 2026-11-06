//! WeChat app layer.
//!
//! The transport (`weixin-agent`) owns QR login and the long-poll loop; this
//! module contains everything v1 layered on top of it: the owner +
//! `WECHAT_ALLOW_FROM` gate, `context_token` + contact persistence, the
//! image-batching hand-off, reply rendering, and the 24h-window renewal
//! reminder. Everything except the transport is deterministic and unit-tested.

pub mod context;
pub mod handler;
pub mod reminder;

pub use context::{WechatContact, WechatContextStore};
pub use handler::{WechatGate, WechatHandler, WechatItem, WechatMessage, WechatTransport};
pub use reminder::{reminder_text, run_reminder_loop, should_remind, REMINDER_TICK_MS};
