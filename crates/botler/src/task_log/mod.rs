//! Append-only task logs in the v1 WebUI JSONL format.
//!
//! Always compiled: the dispatcher records every turn regardless of whether
//! the `webui` viewer feature is built. See `record.rs` for the schema and
//! `store.rs` for the per-day JSONL store.

pub mod record;
pub mod store;
