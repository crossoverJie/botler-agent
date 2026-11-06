//! Scheduler config store: read / validate / write `schedules.json`.
//!
//! This module performs file I/O and validation only. A successful save fires a
//! module-level listener that the engine registers, which wakes the loop
//! immediately without creating a store -> engine dependency.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, OnceLock};

use anyhow::{bail, Context, Result};
use serde_json::Value;

use crate::config::Config;
use crate::fsutil::{atomic_write, ensure_dir, now_epoch_ms};
use crate::identity::{ChannelSource, ConversationKind};
use crate::scheduler::cron::{
    compile_schedule, compile_workday_cron, is_valid_timezone, parse_once_epoch,
};
use crate::scheduler::types::{
    Recipient, ScheduleEntry, ScheduleOrigin, ScheduleRetry, SilentHours,
};

pub const RETRY_MAX_LIMIT: u32 = 10;
pub const RETRY_BACKOFF_MS_LIMIT: u64 = 60 * 60 * 1000;
const MESSAGE_MAX_CHARS: usize = 10 * 1024;

type SavedListener = Arc<dyn Fn() + Send + Sync>;

static SAVED_LISTENER: OnceLock<Mutex<Option<SavedListener>>> = OnceLock::new();

fn listener_slot() -> &'static Mutex<Option<SavedListener>> {
    SAVED_LISTENER.get_or_init(|| Mutex::new(None))
}

/// Registered by the engine at startup. Kept here (not in the engine) so the
/// dependency direction stays store -> listener.
pub fn set_schedules_saved_listener(fn_: Option<SavedListener>) {
    if let Ok(mut slot) = listener_slot().lock() {
        *slot = fn_;
    }
}

fn notify_saved() {
    let listener = listener_slot().lock().ok().and_then(|slot| slot.clone());
    if let Some(listener) = listener {
        listener();
    }
}

fn parse_hm(s: &str) -> Option<(u32, u32)> {
    let (h, m) = s.split_once(':')?;
    if m.len() != 2 {
        return None;
    }
    let h: u32 = h.parse().ok()?;
    let m: u32 = m.parse().ok()?;
    if h > 23 || m > 59 {
        return None;
    }
    Some((h, m))
}

fn valid_script_path(s: &str) -> bool {
    if s.starts_with('/') || s.starts_with('~') || s.starts_with("..") {
        return false;
    }
    if s.split('/').any(|seg| seg == "..") {
        return false;
    }
    let supported = s.ends_with(".py") || s.ends_with(".js") || s.ends_with(".mjs");
    supported
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '.' | '/' | '-'))
}

fn str_field<'a>(o: &'a Value, key: &str) -> Option<&'a str> {
    o.get(key).and_then(Value::as_str).filter(|s| !s.is_empty())
}

/// Validate and normalize one raw schedule object. Any fatal problem is an
/// error so load can skip it and save can reject the whole batch.
pub fn normalize_entry(item: &Value) -> Result<ScheduleEntry> {
    let o = item
        .as_object()
        .ok_or_else(|| anyhow::anyhow!("entry must be an object"))?;

    let id = str_field(item, "id")
        .ok_or_else(|| anyhow::anyhow!("missing id"))?
        .to_string();

    let message = str_field(item, "message").map(str::to_string);
    let script = str_field(item, "script").map(str::to_string);
    let has_message = message.is_some();
    let has_script = script.is_some();
    if !has_message && !has_script {
        bail!("schedule \"{id}\" needs a message or a script");
    }
    if has_message && has_script {
        bail!("schedule \"{id}\" must specify exactly one of message/script");
    }
    if let Some(m) = &message {
        if m.chars().count() > MESSAGE_MAX_CHARS {
            bail!("schedule \"{id}\" message exceeds {MESSAGE_MAX_CHARS} chars");
        }
    }
    let mut script_args = None;
    let mut commit = None;
    if let Some(s) = &script {
        if !valid_script_path(s) {
            bail!(
                "schedule \"{id}\" invalid script path \"{s}\" (relative path, .py/.js/.mjs only)"
            );
        }
        if let Some(raw_args) = o.get("scriptArgs") {
            let arr = raw_args
                .as_array()
                .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid scriptArgs"))?;
            if arr.len() > 64 {
                bail!("schedule \"{id}\" too many scriptArgs");
            }
            let mut args = Vec::new();
            for a in arr {
                let s = a
                    .as_str()
                    .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid scriptArgs"))?;
                if s.len() > 256 {
                    bail!("schedule \"{id}\" invalid scriptArgs");
                }
                args.push(s.to_string());
            }
            script_args = Some(args);
        }
        if let Some(c) = o.get("commit") {
            commit = Some(
                c.as_bool()
                    .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid commit"))?,
            );
        }
    }

    let triggers = ["cron", "interval", "at", "once"]
        .iter()
        .filter(|k| str_field(item, k).is_some())
        .count();
    if triggers == 0 {
        bail!("schedule \"{id}\" needs one of cron/interval/at/once");
    }
    if triggers > 1 {
        bail!("schedule \"{id}\" must specify exactly one of cron/interval/at/once");
    }

    let timezone = str_field(item, "timezone")
        .unwrap_or(ScheduleEntry::DEFAULT_TZ)
        .to_string();
    if !is_valid_timezone(&timezone) {
        bail!("schedule \"{id}\" invalid timezone \"{timezone}\"");
    }

    let enabled = !matches!(o.get("enabled"), Some(Value::Bool(false)));
    let mut entry = ScheduleEntry {
        id: id.clone(),
        enabled,
        cron: None,
        interval: None,
        at: None,
        once: None,
        timezone,
        message,
        script,
        script_args,
        commit,
        project: str_field(item, "project").map(str::to_string),
        retry: None,
        silent_hours: None,
        holiday_mode: None,
        recipient: None,
        origin: None,
    };

    if let Some(cron) = str_field(item, "cron") {
        entry.cron = Some(cron.to_string());
    }
    if let Some(interval) = str_field(item, "interval") {
        let (num, unit) = match interval.char_indices().last() {
            Some((i, c)) if matches!(c, 'm' | 'h' | 'd') => (&interval[..i], c),
            _ => bail!("schedule \"{id}\" invalid interval \"{interval}\""),
        };
        let n: u32 = num
            .parse()
            .map_err(|_| anyhow::anyhow!("schedule \"{id}\" invalid interval \"{interval}\""))?;
        let bad =
            n < 1 || (unit == 'm' && n > 59) || (unit == 'h' && n > 24) || (unit == 'd' && n != 1);
        if bad {
            bail!("schedule \"{id}\" invalid interval \"{interval}\": minutes 1-59, hours 1-24, days only \"1d\"");
        }
        entry.interval = Some(interval.to_string());
    }
    if let Some(at) = str_field(item, "at") {
        if parse_hm(at).is_none() {
            bail!("schedule \"{id}\" invalid at \"{at}\"");
        }
        entry.at = Some(at.to_string());
    }
    if let Some(once) = str_field(item, "once") {
        if parse_once_epoch(once, &entry.timezone).is_none() {
            bail!("schedule \"{id}\" invalid once \"{once}\"");
        }
        entry.once = Some(once.to_string());
    }

    if let Some(retry) = o.get("retry") {
        let max = retry
            .get("max")
            .and_then(Value::as_i64)
            .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid retry.max"))?;
        let backoff = retry
            .get("backoffMs")
            .and_then(Value::as_i64)
            .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid retry.backoffMs"))?;
        if max < 0 || max > i64::from(RETRY_MAX_LIMIT) {
            bail!("schedule \"{id}\" retry.max exceeds {RETRY_MAX_LIMIT}");
        }
        if backoff < 0 || backoff as u64 > RETRY_BACKOFF_MS_LIMIT {
            bail!("schedule \"{id}\" retry.backoffMs exceeds {RETRY_BACKOFF_MS_LIMIT}ms");
        }
        entry.retry = Some(ScheduleRetry {
            max: max as u32,
            backoff_ms: backoff as u64,
        });
    }

    if let Some(sh) = o.get("silentHours") {
        let from = sh.get("from").and_then(Value::as_str).unwrap_or_default();
        let to = sh.get("to").and_then(Value::as_str).unwrap_or_default();
        if parse_hm(from).is_none() || parse_hm(to).is_none() {
            bail!("schedule \"{id}\" invalid silentHours");
        }
        entry.silent_hours = Some(SilentHours {
            from: from.to_string(),
            to: to.to_string(),
        });
    }

    if let Some(r) = o.get("recipient") {
        let source = r.get("source").and_then(Value::as_str).unwrap_or_default();
        let source = ChannelSource::parse(source)
            .filter(|s| s.is_im())
            .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid recipient.source"))?;
        let user_id = r
            .get("userId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid recipient.userId"))?;
        entry.recipient = Some(Recipient {
            source,
            user_id: user_id.to_string(),
        });
    }

    if let Some(origin) = o.get("origin") {
        let source = origin
            .get("source")
            .and_then(Value::as_str)
            .and_then(ChannelSource::parse)
            .filter(|source| source.is_im())
            .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid origin.source"))?;
        let principal_id = origin
            .get("principalId")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| anyhow::anyhow!("schedule \"{id}\" invalid origin.principalId"))?;
        let kind = match origin.get("kind").and_then(Value::as_str) {
            None | Some("synthetic") => ConversationKind::Synthetic,
            Some("direct") => ConversationKind::Direct,
            Some("group") => ConversationKind::Group,
            Some(other) => bail!("schedule \"{id}\" invalid origin.kind \"{other}\""),
        };
        entry.origin = Some(ScheduleOrigin {
            source,
            principal_id: principal_id.to_string(),
            conversation_id: origin
                .get("conversationId")
                .and_then(Value::as_str)
                .filter(|s| !s.is_empty())
                .map(str::to_string),
            kind,
        });
    }

    if matches!(o.get("holidayMode"), Some(Value::String(m)) if m == "workday") {
        entry.holiday_mode = Some("workday".into());
    }
    if entry.once.is_some() && entry.holiday_mode.is_some() {
        bail!("schedule \"{id}\" holidayMode:\"workday\" cannot be combined with once");
    }
    if entry.once.is_none() {
        if entry.is_workday_mode() {
            if let Some(cron) = entry.cron.as_deref() {
                compile_workday_cron(cron)?;
            } else {
                compile_schedule(&entry)?;
            }
        } else {
            compile_schedule(&entry)?;
        }
    }

    Ok(entry)
}

/// Load and normalize schedules. A missing/invalid file yields `[]`; malformed
/// entries are skipped individually so one bad entry cannot kill the scheduler.
pub fn load_schedules(config: &Config) -> Vec<ScheduleEntry> {
    read_schedules(&config.schedules_file).unwrap_or_default()
}

pub fn read_schedules(file: &Path) -> Result<Vec<ScheduleEntry>> {
    let raw = match fs::read_to_string(file) {
        Ok(raw) => raw,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(err) => return Err(err).context("read schedules.json"),
    };
    let parsed: Value = match serde_json::from_str(&raw) {
        Ok(v) => v,
        Err(err) => {
            tracing::warn!("[scheduler] failed to parse schedules.json: {err}");
            return Ok(Vec::new());
        }
    };
    let Some(items) = parsed.get("schedules").and_then(Value::as_array) else {
        tracing::warn!("[scheduler] schedules.json missing 'schedules' array");
        return Ok(Vec::new());
    };
    let mut out = Vec::new();
    let mut ids = std::collections::HashSet::new();
    for item in items {
        match normalize_entry(item) {
            Ok(entry) => {
                if !ids.insert(entry.id.clone()) {
                    tracing::warn!(
                        "[scheduler] duplicate schedule id \"{}\", skipping",
                        entry.id
                    );
                    continue;
                }
                out.push(entry);
            }
            Err(err) => tracing::warn!("[scheduler] skipping invalid entry: {err}"),
        }
    }
    Ok(out)
}

/// Normalize a full batch, rejecting duplicates. Writes are all-or-nothing.
pub fn normalize_schedules(items: &[Value]) -> Result<Vec<ScheduleEntry>> {
    let mut out = Vec::new();
    let mut ids = std::collections::HashSet::new();
    for item in items {
        let entry = normalize_entry(item)?;
        if !ids.insert(entry.id.clone()) {
            bail!("duplicate schedule id \"{}\"", entry.id);
        }
        out.push(entry);
    }
    Ok(out)
}

/// Validate + normalize, back up, and atomically write `schedules.json`.
pub fn save_schedules(config: &Config, items: &[Value]) -> Result<()> {
    let entries = normalize_schedules(items)?;
    write_entries(&config.schedules_file, &entries)?;
    notify_saved();
    Ok(())
}

pub fn save_entries(config: &Config, entries: &[ScheduleEntry]) -> Result<()> {
    let value = serde_json::to_value(entries)?;
    let items = value.as_array().cloned().unwrap_or_default();
    save_schedules(config, &items)
}

fn write_entries(file: &Path, entries: &[ScheduleEntry]) -> Result<()> {
    if let Some(parent) = file.parent() {
        ensure_dir(parent)?;
        backup(file, parent)?;
    }
    let doc = serde_json::json!({ "schedules": entries });
    let mut bytes = serde_json::to_vec_pretty(&doc)?;
    bytes.push(b'\n');
    atomic_write(file, &bytes)
}

fn backup(file: &Path, parent: &Path) -> Result<()> {
    if !file.exists() {
        return Ok(());
    }
    let backup_dir = parent.join(".backups");
    ensure_dir(&backup_dir)?;
    let name = format!(
        "{}.{}",
        file.file_name().unwrap_or_default().to_string_lossy(),
        now_epoch_ms()
    );
    fs::copy(file, backup_dir.join(name))?;
    prune_backups(&backup_dir, 10);
    Ok(())
}

fn prune_backups(dir: &Path, keep: usize) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    let mut names: Vec<String> = entries
        .flatten()
        .map(|e| e.file_name().to_string_lossy().to_string())
        .filter(|n| n.starts_with("schedules.json."))
        .collect();
    names.sort();
    while names.len() > keep {
        let old = names.remove(0);
        let _ = fs::remove_file(dir.join(old));
    }
}

/// Convenience used by tests and the schedule tool.
pub fn schedules_path(config: &Config) -> &PathBuf {
    &config.schedules_file
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn normalizes_valid_entry() {
        let entry = normalize_entry(&json!({
            "id": "a",
            "cron": "0 8 * * *",
            "timezone": "Asia/Shanghai",
            "message": "hello",
            "recipient": { "source": "telegram", "userId": "42" }
        }))
        .unwrap();
        assert!(entry.enabled);
        assert_eq!(entry.message.as_deref(), Some("hello"));
        assert_eq!(entry.recipient.unwrap().user_id, "42");
    }

    #[test]
    fn normalizes_and_round_trips_the_creator_origin() {
        let entry = normalize_entry(&json!({
            "id": "a",
            "cron": "0 8 * * *",
            "message": "hello",
            "recipient": { "source": "feishu", "userId": "oc_group" },
            "origin": {
                "source": "feishu",
                "principalId": "ou_alice",
                "conversationId": "oc_group",
                "kind": "group"
            }
        }))
        .unwrap();
        let origin = entry.origin.clone().expect("origin kept");
        assert_eq!(origin.principal_id, "ou_alice");
        assert_eq!(origin.kind, ConversationKind::Group);
        assert_eq!(origin.conversation_id.as_deref(), Some("oc_group"));

        let raw = serde_json::to_value(&entry).unwrap();
        let reloaded = normalize_entry(&raw).unwrap();
        assert_eq!(reloaded.origin, Some(origin));
    }

    #[test]
    fn rejects_invalid_origin() {
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","message":"x",
            "origin":{"source":"scheduler","principalId":"u"}
        }))
        .is_err());
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","message":"x",
            "origin":{"source":"feishu","principalId":""}
        }))
        .is_err());
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","message":"x",
            "origin":{"source":"feishu","principalId":"u","kind":"galaxy"}
        }))
        .is_err());
    }

    #[test]
    fn rejects_action_and_trigger_ambiguity() {
        assert!(normalize_entry(&json!({"id":"a","cron":"0 8 * * *"})).is_err());
        assert!(normalize_entry(&json!({"id":"a","message":"x"})).is_err());
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","at":"08:00","message":"x"
        }))
        .is_err());
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","message":"x","script":"p.py"
        }))
        .is_err());
    }

    #[test]
    fn rejects_unsafe_script_paths() {
        for script in ["/abs.py", "../x.py", "~/x.js", "a/shell.sh", "a/../b.py"] {
            let err = normalize_entry(&json!({
                "id":"a","cron":"0 8 * * *","script":script
            }));
            assert!(err.is_err(), "{script} should be rejected");
        }
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","script":"notes/scripts/build.py",
            "scriptArgs":["--fast"], "commit": false
        }))
        .is_ok());
    }

    #[test]
    fn enforces_retry_caps() {
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","message":"x",
            "retry":{"max":11,"backoffMs":1}
        }))
        .is_err());
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 8 * * *","message":"x",
            "retry":{"max":1,"backoffMs":3600001}
        }))
        .is_err());
    }

    #[test]
    fn holiday_mode_cannot_combine_with_once() {
        assert!(normalize_entry(&json!({
            "id":"a","once":"2026-11-03T22:00:00+08:00","message":"x",
            "holidayMode":"workday"
        }))
        .is_err());
    }

    #[test]
    fn workday_cron_ignores_date_fields() {
        assert!(normalize_entry(&json!({
            "id":"a","cron":"0 18 31 2 *","message":"x","holidayMode":"workday"
        }))
        .is_ok());
    }

    #[test]
    fn save_and_load_round_trip() {
        let tmp = tempfile::tempdir().unwrap();
        let mut config = Config::load().unwrap();
        config.schedules_file = tmp.path().join("schedules.json");
        save_schedules(
            &config,
            &[json!({"id":"a","cron":"0 8 * * *","message":"x"})],
        )
        .unwrap();
        let loaded = load_schedules(&config);
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].id, "a");
        // The normalized write keeps enabled=true on disk.
        let raw = fs::read_to_string(&config.schedules_file).unwrap();
        assert!(raw.contains("\"enabled\": true"));
    }
}
