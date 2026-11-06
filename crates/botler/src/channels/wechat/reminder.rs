//! WeChat 24h-window renewal reminder.
//!
//! `context_token` expires 24h after the last inbound message. If the owner has
//! been quiet for `WECHAT_REMINDER_HOURS`, nudge them once per quiet stretch so
//! they can refresh the window. Delivery goes through `push::deliver` (WeChat
//! primary, then channel fallback). Ported from v1 `reminder.ts`.

use std::sync::Arc;

use crate::identity::ChannelSource;
use crate::push::Recipient;
use crate::push::{PushPayload, PushService};

use super::context::WechatContextStore;

/// Reminder check interval (v1 parity).
pub const REMINDER_TICK_MS: u64 = 10 * 60 * 1000;

/// Reminder copy; hours are clamped defensively like v1.
pub fn reminder_text(elapsed_hours: f64) -> String {
    let h = elapsed_hours.max(0.0).round() as i64;
    let remaining = (24 - h).max(0);
    format!(
        "[System reminder] Your bot has not received a message for over {h} hours; the session expires in about {remaining} hours.\
Please reply with any message in WeChat to refresh the 24-hour window."
    )
}

/// Pure decision: has this quiet stretch crossed the threshold without a nudge?
pub fn should_remind(
    hours: u32,
    now_ms: i64,
    last_msg_at: i64,
    last_reminded_at: Option<i64>,
) -> bool {
    if hours < 1 || last_msg_at <= 0 {
        return false;
    }
    let quiet_ms = now_ms.saturating_sub(last_msg_at);
    if quiet_ms < hours as i64 * 3_600_000 {
        return false;
    }
    match last_reminded_at {
        Some(reminded) => reminded < last_msg_at,
        None => true,
    }
}

/// Start the reminder loop (idempotence is the caller's job). No-op when
/// `hours < 1`. The owner is supplied by the transport after login.
pub async fn run_reminder_loop(
    push: Arc<PushService>,
    context: WechatContextStore,
    owner: String,
    hours: u32,
) {
    if hours < 1 || owner.is_empty() {
        return;
    }
    tracing::info!("[wechat-reminder] started (threshold {hours}h)");
    loop {
        let now = crate::fsutil::now_epoch_ms();
        if let Some(entry) = context.get(&owner) {
            if should_remind(hours, now, entry.last_msg_at, entry.last_reminded_at) {
                let elapsed = (now - entry.last_msg_at) as f64 / 3_600_000.0;
                let payload = PushPayload {
                    text: reminder_text(elapsed),
                    images: Vec::new(),
                };
                let recipient = Recipient {
                    source: ChannelSource::Wechat,
                    user_id: owner.clone(),
                };
                let outcome = push.deliver(&payload, &recipient).await;
                if !outcome.ok {
                    tracing::error!(
                        "[wechat-reminder] deliver failed: {}",
                        outcome.error.unwrap_or_else(|| "unknown".into())
                    );
                }
                // Record regardless of outcome: never retry-spam one stretch.
                context.mark_reminded(&owner, now);
            }
        }
        tokio::time::sleep(std::time::Duration::from_millis(REMINDER_TICK_MS)).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reminds_once_per_quiet_stretch() {
        let hour = 3_600_000;
        let last = 1_000_000;
        assert!(!should_remind(23, last + hour, last, None));
        assert!(should_remind(23, last + 23 * hour, last, None));
        // Already nudged during this stretch -> no repeat.
        assert!(!should_remind(
            23,
            last + 24 * hour,
            last,
            Some(last + 23 * hour)
        ));
        // A newer message starts a new stretch.
        assert!(should_remind(
            23,
            last + 48 * hour,
            last + 24 * hour,
            Some(last + 23 * hour)
        ));
    }

    #[test]
    fn disabled_and_never_messaged_cases_are_silent() {
        assert!(!should_remind(0, 10_000_000, 1, None));
        assert!(!should_remind(23, 10_000_000, 0, None));
    }

    #[test]
    fn reminder_text_clamps_extremes() {
        assert!(reminder_text(-3.0).contains("over 0 hours"));
        assert!(reminder_text(99.0).contains("about 0 hours"));
    }
}
