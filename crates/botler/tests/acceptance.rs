//! §10.1 behavioural acceptance scenarios on the real dispatcher, runner and
//! session registry, driven by a fake channel adapter (no network, no LLM).

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use botler::channels::fake::FakeChannel;
use botler::config::Config;
use botler::dispatcher::{DispatchOptions, Dispatcher};
use botler::identity::{ChannelSource, ConversationKind};
use botler::paths::PathGuard;
use botler::push::{ContactsStore, PushService};
use botler::runner::Runner;
use botler::runtime::{AgentRuntime, StubRuntime};
use botler::scheduler::engine::{SchedulerDispatch, SchedulerEngine};
use botler::scheduler::holidays::HolidayCalendar;
use serde_json::json;
use tempfile::TempDir;

/// The task context is process-global by design (one serial turn at a time);
/// tokio tests inside one binary run concurrently, so they take turns here.
static TEST_LOCK: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

async fn serial() -> tokio::sync::MutexGuard<'static, ()> {
    TEST_LOCK.lock().await
}

struct Harness {
    dir: TempDir,
    config: Arc<Config>,
    guard: PathGuard,
}

impl Harness {
    fn new(projects: &[&str]) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let data = dir.path().join("data");
        for project in projects {
            let root = data.join(project);
            fs::create_dir_all(&root).unwrap();
            fs::write(
                root.join("AGENTS.md"),
                format!("# {project}\n\nRules for {project}.\n"),
            )
            .unwrap();
        }
        let config = Arc::new(Config::with_paths(&data, dir.path().join("cfg")));
        let guard = PathGuard::new(&data).unwrap();
        Self { dir, config, guard }
    }

    fn config(&self) -> &Config {
        &self.config
    }

    fn dispatcher(&self, runtime: Arc<dyn AgentRuntime>) -> Arc<Dispatcher> {
        let runner = Runner::new(
            Arc::clone(&self.config),
            self.guard.clone(),
            runtime,
            HolidayCalendar::from_data(HashMap::new()),
        )
        .unwrap();
        Dispatcher::new(runner, (*self.config).clone(), self.guard.clone())
    }

    fn fake(
        &self,
        dispatcher: &Arc<Dispatcher>,
        source: ChannelSource,
        conversation: &str,
        sender: &str,
        kind: ConversationKind,
    ) -> FakeChannel {
        FakeChannel::new(Arc::clone(dispatcher), source, conversation, sender, kind)
    }

    fn sessions_dir(&self) -> PathBuf {
        self.config.sessions_dir.clone()
    }

    fn archived_files(&self) -> Vec<PathBuf> {
        list_files(&self.sessions_dir().join("archived"))
    }

    fn live_session_files(&self) -> Vec<PathBuf> {
        list_files(&self.sessions_dir())
            .into_iter()
            .filter(|path| {
                path.file_name()
                    .and_then(|name| name.to_str())
                    .map(|name| name.starts_with("s-") && name.ends_with(".jsonl"))
                    .unwrap_or(false)
            })
            .collect()
    }
}

fn list_files(dir: &Path) -> Vec<PathBuf> {
    let Ok(entries) = fs::read_dir(dir) else {
        return Vec::new();
    };
    entries
        .flatten()
        .map(|entry| entry.path())
        .filter(|path| path.is_file())
        .collect()
}

// 1. Cross-project alternation + channel id collision.
#[tokio::test]
async fn cross_project_sessions_never_interleave_and_channels_do_not_collide() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes", "vocab"]);
    let runtime = Arc::new(StubRuntime::new().with_reply("ok"));
    let dispatcher = harness.dispatcher(runtime.clone());
    let telegram = harness.fake(
        &dispatcher,
        ChannelSource::Telegram,
        "123",
        "42",
        ConversationKind::Direct,
    );
    let feishu = harness.fake(
        &dispatcher,
        ChannelSource::Feishu,
        "123",
        "42",
        ConversationKind::Direct,
    );

    telegram.say("notes first").await;
    telegram.say("vocab second").await;
    telegram.say("notes third").await;
    feishu.say("notes from another channel").await;

    let prompts = runtime.prompts();
    assert_eq!(prompts.len(), 4);
    assert_eq!(prompts[0].0.project, "notes");
    assert_eq!(prompts[1].0.project, "vocab");
    assert_eq!(prompts[2].0.project, "notes");
    // notes -> vocab -> notes reuses the first notes session.
    assert_eq!(prompts[0].0, prompts[2].0);
    // Same raw ids on two channels are different sessions.
    assert_eq!(prompts[0].0.conversation_id, "123");
    assert_eq!(prompts[3].0.conversation_id, "123");
    assert_ne!(prompts[0].0, prompts[3].0);
    assert_ne!(
        prompts[0].0.opaque_file_name(),
        prompts[3].0.opaque_file_name()
    );

    // One hot session per (conversation, project); the third notes turn reused.
    let opened = runtime.opened();
    assert_eq!(opened.len(), 3);
    assert!(opened.iter().all(|(_, resumed)| !resumed));
}

// 2. Arrival-order queue inside one conversation, and cross-conversation work
// is serialized rather than interleaved.
#[tokio::test]
async fn messages_are_enqueued_in_arrival_order() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(StubRuntime::new().with_reply("ok"));
    let dispatcher = harness.dispatcher(runtime.clone());
    let channel = harness.fake(
        &dispatcher,
        ChannelSource::Telegram,
        "c1",
        "u1",
        ConversationKind::Direct,
    );
    let other = harness.fake(
        &dispatcher,
        ChannelSource::Feishu,
        "c2",
        "u2",
        ConversationKind::Direct,
    );

    let first = channel.enqueue("notes first");
    let second = channel.enqueue("notes second");
    let third = other.enqueue("notes other conversation");
    first.wait().await;
    second.wait().await;
    third.wait().await;

    let order: Vec<String> = runtime
        .prompts()
        .into_iter()
        .map(|(_, text)| text)
        .collect();
    assert_eq!(order.len(), 3);
    assert!(order[0].contains("first"), "{order:?}");
    assert!(order[1].contains("second"), "{order:?}");
    assert!(order[2].contains("other conversation"), "{order:?}");
    assert!(
        dispatcher
            .stats()
            .enqueued
            .load(std::sync::atomic::Ordering::SeqCst)
            >= 3
    );
}

// 3. A due scheduler entry goes through the same queue (no bypass path).
#[tokio::test]
async fn scheduler_fires_through_the_global_queue() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(StubRuntime::new().with_reply("scheduled"));
    let dispatcher = harness.dispatcher(runtime.clone());

    let schedules = json!({
        "schedules": [{
            "id": "due",
            "enabled": true,
            "cron": "* * * * *",
            "timezone": "Asia/Shanghai",
            "message": "notes scheduled task"
        }]
    });
    fs::write(
        &harness.config().schedules_file,
        serde_json::to_string_pretty(&schedules).unwrap(),
    )
    .unwrap();

    let push = Arc::new(PushService::new(ContactsStore::new(
        harness.dir.path().join("contacts.json"),
    )));
    let engine = SchedulerEngine::new(
        Arc::new(harness.config().clone()),
        Arc::clone(&dispatcher) as Arc<dyn SchedulerDispatch>,
        push,
        HolidayCalendar::from_data(HashMap::new()),
    );
    // Move the watermark back so the entry is due now.
    engine.seed_watermark("due", botler::fsutil::now_epoch_ms() - 120_000);
    let _delay = engine.tick().await;

    assert_eq!(
        dispatcher
            .stats()
            .started
            .load(std::sync::atomic::Ordering::SeqCst),
        1
    );
    let prompts = runtime.prompts();
    assert_eq!(prompts.len(), 1);
    assert_eq!(prompts[0].0.project, "notes");
    assert!(prompts[0].1.contains("scheduled task"));
}

// 4. clear_conversation_context: mid-turn flag only, rotation after the turn.
#[tokio::test]
async fn clear_flag_rotates_the_session_after_the_turn() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(StubRuntime::new().with_reply("context cleared").on_prompt(
        |_key, path, text| {
            fs::write(path, "{\"turn\":1}").unwrap();
            if text.contains("clear it") {
                botler::tools::task_context::request_conversation_reset();
            }
        },
    ));
    let dispatcher = harness.dispatcher(runtime.clone());
    let channel = harness.fake(
        &dispatcher,
        ChannelSource::Telegram,
        "c1",
        "u1",
        ConversationKind::Direct,
    );

    let result = channel.say("notes clear it").await;
    assert_eq!(result.text, "context cleared");

    // The pre-reset file was archived whole after the turn; no live file remains.
    let archived = harness.archived_files();
    assert_eq!(archived.len(), 1);
    assert_eq!(fs::read_to_string(&archived[0]).unwrap(), "{\"turn\":1}");
    assert!(harness.live_session_files().is_empty());
}

// 6. Editing AGENTS.md mid-conversation rotates the hot session (C3).
#[tokio::test]
async fn editing_project_docs_rotates_the_session() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(
        StubRuntime::new()
            .with_reply("ok")
            .on_prompt(|_key, path, _text| {
                fs::write(path, "{\"turn\":1}").unwrap();
            }),
    );
    let dispatcher = harness.dispatcher(runtime.clone());
    let channel = harness.fake(
        &dispatcher,
        ChannelSource::Telegram,
        "c1",
        "u1",
        ConversationKind::Direct,
    );

    channel.say("notes first").await;
    fs::write(
        harness.dir.path().join("data/notes/AGENTS.md"),
        "# Notes\n\nChanged rules.\n",
    )
    .unwrap();
    channel.say("notes second").await;

    let opened = runtime.opened();
    assert_eq!(opened.len(), 2, "doc drift must open a fresh session");
    assert!(!opened[0].1 && !opened[1].1);
    assert_eq!(harness.archived_files().len(), 1);
}

// 7. Corrupt resume degrades to an archive + fresh session + visible notice.
#[tokio::test]
async fn corrupt_resume_degrades_without_a_crash_loop() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(
        StubRuntime::new()
            .with_reply("ok")
            .on_prompt(|_key, path, _text| {
                fs::write(path, "{\"turn\":1}").unwrap();
            }),
    );
    let dispatcher = harness.dispatcher(runtime.clone());
    let channel = harness.fake(
        &dispatcher,
        ChannelSource::Telegram,
        "c1",
        "u1",
        ConversationKind::Direct,
    );

    channel.say("notes first").await;
    // Simulate a restart: a new dispatcher means an empty hot pool.
    let restarted = harness.dispatcher(runtime.clone());
    let channel = harness.fake(
        &restarted,
        ChannelSource::Telegram,
        "c1",
        "u1",
        ConversationKind::Direct,
    );
    // Simulate a format upgrade the new binary cannot read.
    runtime.fail_resume_once();
    let result = channel.say("notes second").await;
    assert!(
        result.text.contains("无法读取") || result.text.contains("归档"),
        "unexpected reply: {}",
        result.text
    );
    assert!(harness
        .archived_files()
        .iter()
        .any(|path| path.to_string_lossy().contains(".corrupt-")));
}

// 9. A hot session survives turn boundaries without rotating; the preamble is
// rebuilt per turn (current date/sender), not frozen in the session file.
#[tokio::test]
async fn hot_session_is_reused_across_turns_with_a_fresh_preamble() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(
        StubRuntime::new()
            .with_reply("ok")
            .on_prompt(|_key, path, _text| {
                fs::write(path, "{\"turn\":1}").unwrap();
            }),
    );
    let dispatcher = harness.dispatcher(runtime.clone());
    let channel = harness.fake(
        &dispatcher,
        ChannelSource::Telegram,
        "c1",
        "u1",
        ConversationKind::Direct,
    );

    channel.say("notes first").await;
    channel.say("notes second").await;

    // Only one open_session call, and it was a fresh file.
    assert_eq!(runtime.opened().len(), 1);
    let prompts = runtime.prompts();
    assert_eq!(prompts.len(), 2);
    // Each turn carries the dynamic preamble, including today's date.
    let today = chrono::Local::now().format("%Y-%m-%d").to_string();
    assert!(prompts[0].1.contains(&today), "{}", prompts[0].1);
    assert!(prompts[1].1.contains(&today), "{}", prompts[1].1);
    // The session file was not rewritten between turns (no rotation).
    assert!(harness.archived_files().is_empty());
}

// 10. Every dispatched turn appends a v1-shaped JSONL record, and a dedup hit
// is logged through the worker funnel (never from a concurrent context).
#[tokio::test]
async fn dispatched_tasks_write_v1_shaped_jsonl_logs() {
    let _serial = serial().await;
    let harness = Harness::new(&["notes"]);
    let runtime = Arc::new(StubRuntime::new().with_route(&["notes"]).with_reply("ok"));
    let dispatcher = harness.dispatcher(runtime);

    let mut options = DispatchOptions::cli();
    options.id = Some("telegram:c1:42".into());
    options.source = ChannelSource::Telegram;

    dispatcher.dispatch("notes hello", options.clone()).await;
    let duplicate = dispatcher.dispatch("notes hello", options.clone()).await;
    assert!(duplicate.text.contains("duplicate"));

    // Awaiting a third, non-duplicate dispatch guarantees the worker has
    // already processed the fire-and-forget duplicate request ahead of it.
    let mut third = options;
    third.id = Some("telegram:c1:43".into());
    dispatcher.dispatch("notes again", third).await;

    let log_dir = harness.config().log_dir.clone();
    let mut records = Vec::new();
    for entry in fs::read_dir(&log_dir).unwrap().flatten() {
        let name = entry.file_name().to_string_lossy().to_string();
        if !name.ends_with(".jsonl") {
            continue;
        }
        for line in fs::read_to_string(entry.path()).unwrap().lines() {
            if !line.trim().is_empty() {
                records.push(serde_json::from_str::<serde_json::Value>(line).unwrap());
            }
        }
    }
    assert_eq!(records.len(), 3, "records: {records:?}");
    for record in &records {
        assert!(record["taskId"].is_string());
        assert_eq!(record["source"], "telegram");
        assert_eq!(record["degraded"], true);
        assert_eq!(record["usage"]["total"], 0);
        assert!(record["startedAt"].is_number());
        assert!(record["durationMs"].is_number());
    }
    assert_eq!(
        records
            .iter()
            .filter(|r| r["status"] == "duplicate")
            .count(),
        1
    );
    assert_eq!(
        dispatcher.stats().status_counts().get("duplicate"),
        Some(&1)
    );
}
