//! `schedule` tool: create / list / update / delete / enable / disable.
//!
//! A narrow, deliberate exception to the DATA_ROOT allowlist: no file-path
//! parameter exists, and every write goes through `store::save_entries`
//! (normalization + 10KB message cap + atomic write + backup). The push
//! recipient is injected by the framework through task-context, never guessed.

use anyhow::{bail, Context, Result};
use async_trait::async_trait;
use chrono::Local;
use serde_json::{json, Value};

use crate::config::Config;
use crate::scheduler::cron::{epoch_to_wall, is_valid_timezone, next_fire_epoch, parse_once_epoch};
use crate::scheduler::holidays::HolidayCalendar;
use crate::scheduler::store::{
    load_schedules, save_entries, RETRY_BACKOFF_MS_LIMIT, RETRY_MAX_LIMIT,
};
use crate::scheduler::types::{ScheduleEntry, ScheduleRetry};
use crate::tools::task_context::{task_origin, task_recipient};
use crate::tools::{Tool, ToolOutput};

const DEFAULT_TZ: &str = "Asia/Shanghai";
const MESSAGE_MAX_CHARS: usize = 10 * 1024;

pub struct ScheduleTool {
    config: Config,
    holidays: HolidayCalendar,
}

impl ScheduleTool {
    pub fn new(config: Config, holidays: HolidayCalendar) -> Self {
        Self { config, holidays }
    }

    fn entries(&self) -> Vec<ScheduleEntry> {
        load_schedules(&self.config)
    }

    fn next_fire_text(&self, entry: &ScheduleEntry) -> Option<String> {
        let next =
            next_fire_epoch(entry, Local::now().timestamp_millis(), &self.holidays).ok()??;
        let wall = epoch_to_wall(next, entry.timezone_or_default())?;
        Some(wall.format("%Y-%m-%d %H:%M").to_string())
    }

    fn trigger_of(entry: &ScheduleEntry) -> String {
        if let Some(cron) = &entry.cron {
            format!("cron {cron}")
        } else if let Some(interval) = &entry.interval {
            format!("interval {interval}")
        } else if let Some(at) = &entry.at {
            format!("at {at} (daily)")
        } else if let Some(once) = &entry.once {
            format!("once {once}")
        } else {
            "(no trigger)".to_string()
        }
    }

    fn response(&self, text: String) -> ToolOutput {
        ToolOutput::text(text)
    }

    fn create(&self, args: &Value) -> Result<ToolOutput> {
        let id = args
            .get("id")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
            .unwrap_or_else(|| format!("sched-{}", base36(Local::now().timestamp_millis())));
        let message = non_empty_str(args, "message");
        let script = non_empty_str(args, "script");
        match (&message, &script) {
            (None, None) => bail!("create requires a non-empty message OR script"),
            (Some(_), Some(_)) => bail!("create must specify exactly one of message/script"),
            _ => {}
        }
        if let Some(message) = &message {
            if message.chars().count() > MESSAGE_MAX_CHARS {
                bail!("message exceeds {MESSAGE_MAX_CHARS} chars");
            }
        }

        let tz = non_empty_str(args, "timezone").unwrap_or_else(|| DEFAULT_TZ.to_string());
        if !is_valid_timezone(&tz) {
            bail!("invalid timezone \"{tz}\"");
        }
        let triggers: Vec<(&str, String)> = ["cron", "interval", "at", "once"]
            .into_iter()
            .filter_map(|key| non_empty_str(args, key).map(|value| (key, value)))
            .collect();
        if triggers.len() != 1 {
            bail!("create must specify exactly one of cron / interval / at / once");
        }
        let (trigger_key, trigger_value) = &triggers[0];
        if *trigger_key == "once" {
            let Some(t) = parse_once_epoch(trigger_value, &tz) else {
                bail!("invalid once value \"{trigger_value}\"; use an ISO 8601 date-time");
            };
            if t <= Local::now().timestamp_millis() {
                bail!("once time must be in the future (got {trigger_value})");
            }
        }
        let mut entries = self.entries();
        if entries.iter().any(|entry| entry.id == id) {
            bail!("Scheduled task id already exists: {id}. Use update to modify it.");
        }

        let mut raw = serde_json::Map::new();
        raw.insert("id".into(), json!(id));
        raw.insert("enabled".into(), json!(true));
        raw.insert("timezone".into(), json!(tz));
        raw.insert(trigger_key.to_string(), json!(trigger_value));
        if let Some(script) = script {
            raw.insert("script".into(), json!(script));
            if let Some(script_args) = args.get("scriptArgs") {
                raw.insert("scriptArgs".into(), script_args.clone());
            }
            if let Some(commit) = args.get("commit") {
                raw.insert("commit".into(), commit.clone());
            }
        } else if let Some(message) = message {
            raw.insert("message".into(), json!(message));
        }
        let retry_max = args.get("retryMax").and_then(Value::as_u64).unwrap_or(0) as u32;
        let retry_backoff = args
            .get("retryBackoffMs")
            .and_then(Value::as_u64)
            .unwrap_or(0);
        if retry_max > 0 {
            if retry_max > RETRY_MAX_LIMIT {
                bail!("retryMax exceeds {RETRY_MAX_LIMIT}");
            }
            if retry_backoff > RETRY_BACKOFF_MS_LIMIT {
                bail!("retryBackoffMs exceeds {RETRY_BACKOFF_MS_LIMIT}");
            }
            raw.insert(
                "retry".into(),
                serde_json::to_value(ScheduleRetry {
                    max: retry_max,
                    backoff_ms: retry_backoff,
                })?,
            );
        }
        if let Some(project) = non_empty_str(args, "project") {
            raw.insert("project".into(), json!(project));
        }
        if args.get("holidayMode").and_then(Value::as_str) == Some("workday") {
            raw.insert("holidayMode".into(), json!("workday"));
        }
        if let Some(recipient) = task_recipient() {
            raw.insert("recipient".into(), serde_json::to_value(recipient)?);
        }
        if let Some(origin) = task_origin() {
            // Creator identity: the delivery address above can be a group chat
            // id, which must never be used as a memory principal.
            raw.insert("origin".into(), serde_json::to_value(origin)?);
        }

        entries.push(crate::scheduler::store::normalize_entry(&Value::Object(
            raw,
        ))?);
        save_entries(&self.config, &entries)?;
        let entry = self
            .entries()
            .into_iter()
            .find(|entry| entry.id == id)
            .context("saved schedule not found")?;
        let next = self
            .next_fire_text(&entry)
            .map(|next| format!(", next fire: {next}"))
            .unwrap_or_default();
        Ok(self.response(format!(
            "Created scheduled task \"{}\": {} (timezone {}){next}",
            entry.id,
            Self::trigger_of(&entry),
            entry.timezone
        )))
    }

    fn update(&self, args: &Value) -> Result<ToolOutput> {
        let id = required_id(args, "update")?;
        let mut entries = self.entries();
        let idx = entries
            .iter()
            .position(|entry| entry.id == id)
            .with_context(|| format!("Scheduled task {id} not found"))?;
        let mut patch = serde_json::to_value(&entries[idx])?
            .as_object()
            .cloned()
            .unwrap_or_default();

        let new_message = non_empty_str(args, "message");
        let new_script = non_empty_str(args, "script");
        if new_message.is_some() && new_script.is_some() {
            bail!("update must not set both message and script; pick exactly one action");
        }
        let was_script = entries[idx].script.is_some();
        if let Some(script) = new_script {
            patch.insert("script".into(), json!(script));
            patch.remove("message");
            patch.remove("scriptArgs");
            patch.remove("commit");
            if !was_script {
                patch.remove("retry");
            }
        } else if let Some(message) = new_message {
            if message.chars().count() > MESSAGE_MAX_CHARS {
                bail!("message exceeds {MESSAGE_MAX_CHARS} chars");
            }
            patch.insert("message".into(), json!(message));
            patch.remove("script");
            patch.remove("scriptArgs");
            patch.remove("commit");
            if was_script {
                patch.remove("retry");
            }
        }
        if patch.contains_key("script") {
            if let Some(script_args) = args.get("scriptArgs") {
                patch.insert("scriptArgs".into(), script_args.clone());
            }
            if let Some(commit) = args.get("commit") {
                patch.insert("commit".into(), commit.clone());
            }
        } else {
            patch.remove("scriptArgs");
            patch.remove("commit");
        }

        if args.get("retryMax").is_some() || args.get("retryBackoffMs").is_some() {
            let existing = patch.get("retry").cloned();
            let existing = existing.as_ref().map(|value| {
                (
                    value.get("max").and_then(Value::as_u64).unwrap_or(0) as u32,
                    value.get("backoffMs").and_then(Value::as_u64).unwrap_or(0),
                )
            });
            let max = args
                .get("retryMax")
                .and_then(Value::as_u64)
                .map(|v| v as u32)
                .or_else(|| existing.map(|(max, _)| max))
                .unwrap_or(0);
            let backoff = args
                .get("retryBackoffMs")
                .and_then(Value::as_u64)
                .or_else(|| existing.map(|(_, backoff)| backoff))
                .unwrap_or(0);
            if max > 0 {
                if max > RETRY_MAX_LIMIT {
                    bail!("retryMax exceeds {RETRY_MAX_LIMIT}");
                }
                if backoff > RETRY_BACKOFF_MS_LIMIT {
                    bail!("retryBackoffMs exceeds {RETRY_BACKOFF_MS_LIMIT}");
                }
                patch.insert(
                    "retry".into(),
                    serde_json::to_value(ScheduleRetry {
                        max,
                        backoff_ms: backoff,
                    })?,
                );
            } else {
                patch.remove("retry");
            }
        }

        if let Some(tz) = args.get("timezone").and_then(Value::as_str) {
            if !is_valid_timezone(tz) {
                bail!("invalid timezone \"{tz}\"");
            }
            patch.insert("timezone".into(), json!(tz));
        }
        if let Some(project) = args.get("project") {
            let project = project.as_str().map(str::trim).unwrap_or("");
            if project.is_empty() {
                patch.remove("project");
            } else {
                patch.insert("project".into(), json!(project));
            }
        }
        if let Some(mode) = args.get("holidayMode").and_then(Value::as_str) {
            if mode == "workday" {
                patch.insert("holidayMode".into(), json!("workday"));
            } else {
                patch.remove("holidayMode");
            }
        }

        let mut new_once: Option<(String, String)> = None;
        for trigger in ["cron", "interval", "at", "once"] {
            if non_empty_str(args, trigger).is_some() {
                let value = non_empty_str(args, trigger).unwrap();
                patch.insert(trigger.into(), json!(value));
                for other in ["cron", "interval", "at", "once"] {
                    if other != trigger {
                        patch.remove(other);
                    }
                }
                if trigger == "once" {
                    let tz = patch
                        .get("timezone")
                        .and_then(Value::as_str)
                        .unwrap_or(DEFAULT_TZ)
                        .to_string();
                    new_once = Some((value, tz));
                }
                break;
            }
        }
        if let Some((value, tz)) = new_once {
            let Some(t) = parse_once_epoch(&value, &tz) else {
                bail!("invalid once value \"{value}\"; use an ISO 8601 date-time");
            };
            if t <= Local::now().timestamp_millis() {
                bail!("once time must be in the future (got {value})");
            }
        }
        if !patch.contains_key("message") && !patch.contains_key("script") {
            bail!("schedule must have a message or script");
        }

        entries[idx] = crate::scheduler::store::normalize_entry(&Value::Object(patch))?;
        save_entries(&self.config, &entries)?;
        let entry = self
            .entries()
            .into_iter()
            .find(|entry| entry.id == id)
            .context("saved schedule not found")?;
        let next = self
            .next_fire_text(&entry)
            .map(|next| format!(", next fire: {next}"))
            .unwrap_or_default();
        Ok(self.response(format!(
            "Updated scheduled task \"{id}\": {}{next}",
            Self::trigger_of(&entry)
        )))
    }

    fn list(&self) -> ToolOutput {
        let entries = self.entries();
        if entries.is_empty() {
            return ToolOutput::text("There are no scheduled tasks right now.");
        }
        let mut lines = Vec::new();
        for entry in &entries {
            let next = if entry.enabled {
                self.next_fire_text(entry)
            } else {
                None
            };
            let recipient = entry
                .recipient
                .as_ref()
                .map(|r| format!(" push={}:{}", r.source, r.user_id))
                .unwrap_or_default();
            let project = entry
                .project
                .as_ref()
                .map(|p| format!(" project={p}"))
                .unwrap_or_default();
            let holiday = entry
                .holiday_mode
                .as_ref()
                .map(|m| format!(" holidayMode={m}"))
                .unwrap_or_default();
            let action = entry
                .script
                .as_ref()
                .map(|s| format!("▶ {s}"))
                .unwrap_or_else(|| format!("msg={}", entry.message.clone().unwrap_or_default()));
            let retry = entry
                .retry
                .as_ref()
                .map(|r| format!(" retry={}", r.max))
                .unwrap_or_default();
            let fire = if !entry.enabled {
                " disabled".to_string()
            } else if let Some(next) = next {
                format!(" next-fire={next}")
            } else if entry.once.is_some() {
                " (expired)".to_string()
            } else {
                String::new()
            };
            lines.push(format!(
                "- {} [{}] {} {action}{retry} tz={}{project}{recipient}{holiday}{fire}",
                entry.id,
                if entry.enabled { "enabled" } else { "disabled" },
                Self::trigger_of(entry),
                entry.timezone
            ));
        }
        ToolOutput::text(format!(
            "Scheduled tasks ({}):\n{}",
            entries.len(),
            lines.join("\n")
        ))
    }
}

#[async_trait]
impl Tool for ScheduleTool {
    fn name(&self) -> &str {
        "schedule"
    }

    fn label(&self) -> &str {
        "Manage scheduled tasks"
    }

    fn description(&self) -> &str {
        "Create / view / modify / delete / enable / disable scheduled tasks. `create` needs exactly one trigger (cron / interval / at / once) and exactly one action (message OR script). Use `once` for one-off reminders; `at` repeats every day. After creating or modifying, confirm the next fire time."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "action": { "type": "string", "enum": ["list","create","update","delete","enable","disable"] },
                "id": { "type": "string" },
                "message": { "type": "string" },
                "script": { "type": "string" },
                "scriptArgs": { "type": "array", "items": { "type": "string" } },
                "commit": { "type": "boolean" },
                "retryMax": { "type": "integer", "maximum": RETRY_MAX_LIMIT },
                "retryBackoffMs": { "type": "integer", "maximum": RETRY_BACKOFF_MS_LIMIT },
                "cron": { "type": "string" },
                "interval": { "type": "string" },
                "at": { "type": "string" },
                "once": { "type": "string" },
                "timezone": { "type": "string" },
                "project": { "type": "string" },
                "holidayMode": { "type": "string", "enum": ["workday","off"] }
            },
            "required": ["action"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let action = args
            .get("action")
            .and_then(Value::as_str)
            .context("schedule requires an action")?;
        match action {
            "list" => Ok(self.list()),
            "create" => self.create(&args),
            "update" => self.update(&args),
            "delete" => {
                let id = required_id(&args, "delete")?;
                let mut entries = self.entries();
                let before = entries.len();
                entries.retain(|entry| entry.id != id);
                if entries.len() == before {
                    bail!("Scheduled task {id} not found");
                }
                save_entries(&self.config, &entries)?;
                Ok(ToolOutput::text(format!(
                    "Deleted scheduled task \"{id}\"."
                )))
            }
            "enable" | "disable" => {
                let id = required_id(&args, action)?;
                let mut entries = self.entries();
                let entry = entries
                    .iter_mut()
                    .find(|entry| entry.id == id)
                    .with_context(|| format!("Scheduled task {id} not found"))?;
                entry.enabled = action == "enable";
                let entry = entry.clone();
                save_entries(&self.config, &entries)?;
                let next = if entry.enabled {
                    self.next_fire_text(&entry)
                        .map(|next| format!(" next fire: {next}"))
                        .unwrap_or_default()
                } else {
                    String::new()
                };
                let verb = if action == "enable" {
                    "Enabled"
                } else {
                    "Disabled"
                };
                Ok(ToolOutput::text(format!(
                    "{verb} scheduled task \"{id}\".{next}"
                )))
            }
            other => bail!("unknown schedule action: {other}"),
        }
    }
}

fn non_empty_str(args: &Value, key: &str) -> Option<String> {
    args.get(key)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

fn required_id(args: &Value, action: &str) -> Result<String> {
    non_empty_str(args, "id").with_context(|| format!("{action} requires an id"))
}

fn base36(mut value: i64) -> String {
    const DIGITS: &[u8] = b"0123456789abcdefghijklmnopqrstuvwxyz";
    if value <= 0 {
        return "0".to_string();
    }
    let mut out = Vec::new();
    while value > 0 {
        out.push(DIGITS[(value % 36) as usize]);
        value /= 36;
    }
    out.reverse();
    String::from_utf8(out).unwrap_or_default()
}
