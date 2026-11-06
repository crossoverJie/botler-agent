//! Channel adapters.
//!
//! Every adapter shares one contract: a message is turned into a
//! `DispatchOptions` with its channel-scoped identity, **enqueued inline at
//! receive time** (C2 arrival order), and the reply is sent when the serial
//! worker finishes. The transport is the only channel-specific part; routing,
//! sessions, tools, validation and commits stay in the dispatcher.

pub mod allowlist;
pub mod fake;
pub mod image_batch;
pub mod wechat;

#[cfg(feature = "channels")]
pub mod feishu;
#[cfg(feature = "channels")]
pub mod telegram;

pub use allowlist::is_allowed_sender;
