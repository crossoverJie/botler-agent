//! Botler v2 entry point.
//!
//! - `botler "message"` runs one CLI task through the real dispatcher.
//! - no arguments starts the configured IM channels, the scheduler loop and
//!   push delivery, and waits.

use std::collections::HashSet;
use std::sync::Arc;

use anyhow::{bail, Context, Result};

use botler::config::Config;
use botler::dispatcher::{DispatchOptions, Dispatcher};
use botler::identity::{ChannelSource, MemoryContext};
use botler::memory::{MemoryStore, MemoryTier};
use botler::paths::PathGuard;
use botler::push::{ContactsStore, PushService};
use botler::retention::{self, RetentionPolicy};
use botler::runner::Runner;
use botler::runtime::{AgentRuntime, StubRuntime};
use botler::scheduler::engine::{SchedulerDispatch, SchedulerEngine};
use botler::scheduler::holidays::HolidayCalendar;

#[tokio::main]
async fn main() -> Result<()> {
    botler::init_tracing();
    let args: Vec<String> = std::env::args().skip(1).collect();
    // `init` must run before Config::load / PathGuard::new: on a fresh machine
    // DATA_ROOT does not exist yet, and scaffolding must not require it.
    if args.len() == 1 && args[0] == "init" {
        return botler::init::run_cli();
    }
    let config = Arc::new(Config::load()?);
    let guard = PathGuard::new(&config.data_root)?;
    tracing::info!(
        "[botler] DATA_ROOT={} projects={:?}",
        guard.data_root().display(),
        guard.project_names()
    );

    if let Some((command, rest)) = admin_command(&args) {
        return match command {
            "sessions" => handle_sessions(&config, rest),
            _ => handle_memory(&config, &guard, rest),
        };
    }
    if let Err(err) = retention::sweep(
        &config.sessions_dir,
        RetentionPolicy::new(config.retention_days, config.retention_max_bytes),
    ) {
        tracing::warn!("[retention] startup sweep failed: {err}");
    }

    let holidays =
        HolidayCalendar::load(config.holidays_file.clone(), config.holiday_api_url.clone());
    // The pi embedding is the only piece gated on spikes S-1/S-5; the rest of
    // the pipeline (routing, sessions, tools, validation, commits, scheduler)
    // runs unchanged against this seam.
    let runtime: Arc<dyn AgentRuntime> = Arc::new(StubRuntime::new());
    tracing::warn!(
        "[runtime] pi embedding is not linked in this build (spikes S-1/S-5); replies use the deterministic stub"
    );

    let runner = Runner::new(
        Arc::clone(&config),
        guard.clone(),
        runtime,
        holidays.clone(),
    )?;
    let dispatcher = Dispatcher::new(runner, (*config).clone(), guard.clone());

    let contacts = ContactsStore::new(config.user_config_dir.join("contacts.json"));
    let mut push = PushService::new(contacts.clone());
    register_channel_sinks(&config, &mut push);
    let push = Arc::new(push);

    if !args.is_empty() {
        let message = args.join(" ");
        let result = dispatcher.dispatch(&message, DispatchOptions::cli()).await;
        if !result.text.trim().is_empty() {
            println!("{}", result.text);
        }
        if !result.images.is_empty() {
            println!("[images] {}", result.images.join(", "));
        }
        return Ok(());
    }
    if !config.has_channels() {
        tracing::warn!("[botler] no IM channel configured; running scheduler-only");
    }
    start_channel_tasks(&config, &dispatcher, &contacts, &push).await;

    // The WebUI needs the holiday calendar for next-fire / preview, but the
    // engine takes ownership below.
    let webui_holidays = holidays.clone();
    let engine = SchedulerEngine::new(
        Arc::clone(&config),
        Arc::clone(&dispatcher) as Arc<dyn SchedulerDispatch>,
        Arc::clone(&push),
        holidays,
    );
    tokio::spawn(async move {
        engine.start().await;
    });
    spawn_retention(Arc::clone(&config));
    start_webui_tasks(&config, &dispatcher, webui_holidays);

    // Server mode never exits: channel and scheduler tasks are detached.
    std::future::pending::<()>().await;
    Ok(())
}

#[cfg(feature = "channels")]
fn register_channel_sinks(config: &Config, push: &mut PushService) {
    use botler::channels::feishu::{FeishuApi, FeishuSink};
    use botler::channels::telegram::{TelegramApi, TelegramSink};

    if let Some(token) = config.telegram_token.clone() {
        push.register(Arc::new(TelegramSink::new(Arc::new(TelegramApi::new(
            token,
            config.tg_proxy.as_deref(),
        )))));
    }
    if let (Some(app_id), Some(app_secret)) = (
        config.feishu_app_id.clone(),
        config.feishu_app_secret.clone(),
    ) {
        push.register(Arc::new(FeishuSink::new(Arc::new(FeishuApi::new(
            app_id, app_secret,
        )))));
    }
}

#[cfg(not(feature = "channels"))]
fn register_channel_sinks(_config: &Config, _push: &mut PushService) {
    tracing::warn!("[botler] built without the `channels` feature; IM transports are disabled");
}

#[cfg(feature = "channels")]
async fn start_channel_tasks(
    config: &Arc<Config>,
    dispatcher: &Arc<Dispatcher>,
    contacts: &ContactsStore,
    push: &Arc<PushService>,
) {
    use botler::channels::feishu::FeishuChannel;
    use botler::channels::telegram::{TelegramApi, TelegramChannel};
    use botler::channels::wechat::{run_reminder_loop, WechatContextStore};

    if let Some(token) = config.telegram_token.clone() {
        let channel = TelegramChannel::new(
            Arc::new(TelegramApi::new(token, config.tg_proxy.as_deref())),
            Arc::clone(dispatcher),
            config.telegram_allow_from.clone(),
            contacts.clone(),
        );
        tokio::spawn(async move {
            if let Err(err) = channel.run().await {
                tracing::error!("[telegram] channel stopped: {err}");
            }
        });
    }

    if let (Some(app_id), Some(app_secret)) = (
        config.feishu_app_id.clone(),
        config.feishu_app_secret.clone(),
    ) {
        match config.feishu_verification_token.clone() {
            Some(verification_token) => {
                let channel = FeishuChannel::new(
                    Arc::clone(dispatcher),
                    Arc::new(botler::channels::feishu::FeishuApi::new(app_id, app_secret)),
                    verification_token,
                    config.feishu_encrypt_key.clone(),
                    config.feishu_allow_from.clone(),
                    contacts.clone(),
                    config.feishu_port,
                );
                tokio::spawn(async move {
                    if let Err(err) = channel.run().await {
                        tracing::error!("[feishu] channel stopped: {err}");
                    }
                });
            }
            None => tracing::error!(
                "[feishu] FEISHU_VERIFICATION_TOKEN is required; refusing to start an unauthenticated webhook"
            ),
        }
    }

    if config.wechat_enabled {
        // The weixin-agent transport is spike S-4; the app layer (gate,
        // context tokens, batching, reminder) is ready behind it.
        tracing::warn!(
            "[wechat] enabled, but the weixin-agent transport is not linked in this build (spike S-4); messages will not arrive"
        );
        let context = WechatContextStore::new(
            config.user_config_dir.join("wechat-context.json"),
            contacts.clone(),
        );
        if let Some(owner) = std::env::var("WECHAT_OWNER_ID")
            .ok()
            .filter(|owner| !owner.is_empty())
        {
            let push = Arc::clone(push);
            let context = context.clone();
            let hours = config.wechat_reminder_hours;
            tokio::spawn(async move {
                run_reminder_loop(push, context, owner, hours).await;
            });
        }
    }
}

#[cfg(not(feature = "channels"))]
async fn start_channel_tasks(
    _config: &Arc<Config>,
    _dispatcher: &Arc<Dispatcher>,
    _contacts: &ContactsStore,
    _push: &Arc<PushService>,
) {
}

#[cfg(feature = "webui")]
fn start_webui_tasks(
    config: &Arc<Config>,
    dispatcher: &Arc<Dispatcher>,
    holidays: HolidayCalendar,
) {
    botler::webui::start_tasks(config, dispatcher, holidays);
}

#[cfg(not(feature = "webui"))]
fn start_webui_tasks(
    _config: &Arc<Config>,
    _dispatcher: &Arc<Dispatcher>,
    _holidays: HolidayCalendar,
) {
    tracing::info!("[webui] built without the `webui` feature; HTTP surface disabled");
}

/// Admin subcommands only take over when the second token is a known verb, so a
/// normal message that happens to start with "sessions" still reaches the agent.
fn admin_command(args: &[String]) -> Option<(&'static str, &[String])> {
    let verb = args.get(1).map(String::as_str);
    match (args.first().map(String::as_str), verb) {
        (Some("sessions"), Some("list" | "inspect" | "purge")) => Some(("sessions", &args[1..])),
        (Some("memory"), Some("list" | "delete" | "clear")) => Some(("memory", &args[1..])),
        _ => None,
    }
}

fn human_bytes(bytes: u64) -> String {
    const UNITS: [&str; 4] = ["B", "KB", "MB", "GB"];
    let mut value = bytes as f64;
    let mut unit = 0;
    while value >= 1024.0 && unit + 1 < UNITS.len() {
        value /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{bytes} B")
    } else {
        format!("{value:.1} {}", UNITS[unit])
    }
}

fn format_ms(ms: i64) -> String {
    chrono::DateTime::<chrono::Utc>::from_timestamp_millis(ms)
        .map(|dt| {
            dt.with_timezone(&chrono::Local)
                .format("%Y-%m-%d %H:%M")
                .to_string()
        })
        .unwrap_or_else(|| ms.to_string())
}

fn usage_sessions() -> &'static str {
    "usage: botler sessions list | inspect <file> | purge [--older-than-days N] [--max-bytes N] [--all]"
}

fn handle_sessions(config: &Config, args: &[String]) -> Result<()> {
    let dir = &config.sessions_dir;
    match args.first().map(String::as_str).unwrap_or("list") {
        "list" => {
            let files = retention::discover(dir)?;
            if files.is_empty() {
                println!("No session files under {}.", dir.display());
                return Ok(());
            }
            println!("{:<9} {:>10}  {:<16}  PATH", "KIND", "SIZE", "MODIFIED");
            let mut total = 0u64;
            for file in &files {
                total += file.bytes;
                println!(
                    "{:<9} {:>10}  {:<16}  {}",
                    retention::describe_kind(file.kind),
                    human_bytes(file.bytes),
                    format_ms(file.modified_ms),
                    file.path
                );
            }
            println!(
                "\n{} file(s), {} total (retention: {} days, cap {})",
                files.len(),
                human_bytes(total),
                config.retention_days,
                human_bytes(config.retention_max_bytes)
            );
            Ok(())
        }
        "inspect" => {
            let target = args
                .get(1)
                .context("sessions inspect requires a file name")?;
            let files = retention::discover(dir)?;
            let Some(file) = retention::find(&files, target) else {
                bail!("no session file matches {target:?}\n{}", usage_sessions());
            };
            println!(
                "path: {}\nkind: {}\nsize: {}\nmodified: {}\n",
                file.path,
                retention::describe_kind(file.kind),
                human_bytes(file.bytes),
                format_ms(file.modified_ms)
            );
            println!("{}", retention::inspect(dir, file, 4000)?);
            Ok(())
        }
        "purge" => {
            let mut ttl_days = config.retention_days;
            let mut max_bytes = config.retention_max_bytes;
            let mut all = false;
            let mut index = 1;
            while index < args.len() {
                match args[index].as_str() {
                    "--older-than-days" => {
                        index += 1;
                        ttl_days = args
                            .get(index)
                            .context("--older-than-days requires a value")?
                            .parse()
                            .context("--older-than-days must be a number")?;
                    }
                    "--max-bytes" => {
                        index += 1;
                        max_bytes = args
                            .get(index)
                            .context("--max-bytes requires a value")?
                            .parse()
                            .context("--max-bytes must be a number")?;
                    }
                    "--all" => all = true,
                    other => bail!("unknown purge option {other:?}\n{}", usage_sessions()),
                }
                index += 1;
            }

            let report = if all {
                retention::purge_all(dir)?
            } else {
                retention::sweep_at(
                    dir,
                    RetentionPolicy::new(ttl_days, max_bytes),
                    botler::fsutil::now_epoch_ms(),
                )?
            };
            for path in &report.removed {
                println!("removed {path}");
            }
            println!(
                "{} file(s) removed, {} freed, {} kept.",
                report.removed_count(),
                human_bytes(report.bytes_freed),
                human_bytes(report.bytes_kept)
            );
            Ok(())
        }
        other => bail!("unknown sessions command {other:?}\n{}", usage_sessions()),
    }
}

fn usage_memory() -> &'static str {
    "usage: botler memory list [project] | delete <id> [project] | clear <tier> [project]"
}

fn cli_memory_context(project: &str) -> MemoryContext {
    MemoryContext::synthetic_for(ChannelSource::Cli, "cli", "local-owner", project)
}

fn handle_memory(config: &Config, guard: &PathGuard, args: &[String]) -> Result<()> {
    let store = MemoryStore::new(
        &config.memory_dir,
        config.memory_content_max_bytes,
        config.recall_token_budget,
        config.memory_digest_max_chars,
    );
    match args.first().map(String::as_str).unwrap_or("list") {
        "list" => {
            let projects = match args.get(1) {
                Some(project) => vec![project.clone()],
                None => guard.project_names(),
            };
            let mut seen = HashSet::new();
            for project in &projects {
                for hit in store.list(&cli_memory_context(project)) {
                    if seen.insert(hit.record.id.clone()) {
                        println!(
                            "{}\t[tier {}]\t{}",
                            hit.record.id,
                            hit.tier.tier(),
                            hit.record.content
                        );
                    }
                }
            }
            if seen.is_empty() {
                println!("No memories.");
            }
            Ok(())
        }
        "delete" => {
            let id = args.get(1).context("memory delete requires an id")?;
            let project = args.get(2).cloned().unwrap_or_else(|| first_project(guard));
            let ctx = cli_memory_context(&project);
            if store.delete(&ctx, id)? {
                println!("Deleted {id} from project {project}.");
            } else {
                println!("No memory {id} in project {project}.");
            }
            Ok(())
        }
        "clear" => {
            let raw = args.get(1).context("memory clear requires a tier")?;
            let tier = MemoryTier::parse(raw)
                .with_context(|| format!("unknown tier {raw:?} (personal/project/global)"))?;
            let project = args.get(2).cloned().unwrap_or_else(|| first_project(guard));
            let ctx = cli_memory_context(&project);
            // The explicit CLI command is the confirmation for a project-wide clear.
            let existed = store.clear(&ctx, tier, true)?;
            println!(
                "Cleared tier {} for project {project} ({}).",
                tier.as_str(),
                if existed { "had data" } else { "was empty" }
            );
            Ok(())
        }
        other => bail!("unknown memory command {other:?}\n{}", usage_memory()),
    }
}

fn first_project(guard: &PathGuard) -> String {
    guard.project_names().into_iter().next().unwrap_or_default()
}

fn spawn_retention(config: Arc<Config>) {
    let dir = config.sessions_dir.clone();
    let policy = RetentionPolicy::new(config.retention_days, config.retention_max_bytes);
    tokio::spawn(async move {
        loop {
            tokio::time::sleep(std::time::Duration::from_secs(24 * 60 * 60)).await;
            if let Err(err) = retention::sweep(&dir, policy) {
                tracing::warn!("[retention] periodic sweep failed: {err}");
            }
        }
    });
}
