//! Botler v2 — Rust rewrite on the pi Rust SDK.

pub mod channels;
pub mod config;
pub mod dispatcher;
pub mod fsutil;
pub mod greeting;
pub mod identity;
pub mod memory;
pub mod paths;
pub mod prompt;
pub mod push;
pub mod retention;
pub mod rotation;
pub mod runner;
pub mod runtime;
pub mod safety;
pub mod scheduler;
pub mod session_index;
pub mod session_registry;
pub mod tools;

pub fn init_tracing() {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(false)
        .try_init();
}
