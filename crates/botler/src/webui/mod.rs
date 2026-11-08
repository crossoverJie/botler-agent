//! Local WebUI + health endpoints. Gated behind the `webui` feature.

pub mod config_store;
pub mod schedule_history;
pub mod server;

use std::sync::{Arc, Mutex};

use anyhow::Result;
use tokio::net::TcpListener;

use crate::config::Config;
use crate::dispatcher::Dispatcher;
use crate::fsutil::now_epoch_ms;
use crate::monitor::history::History;
use crate::monitor::state::{spawn_sampler, MonitorState, SystemClock};
use crate::scheduler::holidays::HolidayCalendar;
use crate::task_log::store::TaskLogStore;

/// Read-only state shared by both HTTP servers. It deliberately contains no
/// `Arc<Dispatcher>`, so no handler can enqueue work or bypass the serial
/// worker.
pub type AppState = MonitorState;

/// Build the shared state, start the single sampler, then spawn the WebUI
/// (8900) and/or standalone monitor (8899) servers per the runtime switches.
pub fn start_tasks(config: &Arc<Config>, dispatcher: &Arc<Dispatcher>, holidays: HolidayCalendar) {
    let now_ms = now_epoch_ms();
    let state: Arc<AppState> = Arc::new(MonitorState {
        config: Arc::clone(config),
        stats: dispatcher.stats_handle(),
        store: TaskLogStore::new(config.log_dir.clone()),
        history: Arc::new(Mutex::new(History::default())),
        started_at: now_ms,
        holidays,
        clock: Arc::new(SystemClock),
    });
    spawn_sampler(Arc::clone(&state));

    if config.webui_enabled {
        let state = Arc::clone(&state);
        tokio::spawn(async move {
            if let Err(err) = serve_webui(state).await {
                tracing::error!("[webui] server failed: {err}");
            }
        });
    }
    if config.monitor_enabled {
        let state = Arc::clone(&state);
        tokio::spawn(async move {
            if let Err(err) = serve_monitor(state).await {
                tracing::error!("[monitor] server failed: {err}");
            }
        });
    }
}

async fn serve_webui(state: Arc<AppState>) -> Result<()> {
    let addr = format!("{}:{}", state.config.webui_host, state.config.webui_port);
    let listener = TcpListener::bind(addr.as_str()).await?;
    tracing::info!("[webui] task log UI on http://{addr}");
    axum::serve(listener, server::build_router(state))
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

async fn serve_monitor(state: Arc<AppState>) -> Result<()> {
    // v1 binds the standalone health server to loopback only, always.
    let addr = format!("127.0.0.1:{}", state.config.monitor_port);
    let listener = TcpListener::bind(addr.as_str()).await?;
    tracing::info!("[monitor] health endpoints on http://{addr}");
    axum::serve(listener, server::monitor_router(state))
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}
