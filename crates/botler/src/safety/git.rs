//! Per-subproject git commit (and optional push).
//!
//! Only first-level subdirs of DATA_ROOT that are their own git repositories
//! and actually changed are committed. Failure is logged and never blocks the
//! turn's reply.

use std::path::Path;

use tokio::process::Command;

use crate::config::Config;

/// Flatten and truncate the user message for the commit subject (v1: 60 chars).
pub fn commit_subject(message: &str) -> String {
    let flat = message.split_whitespace().collect::<Vec<_>>().join(" ");
    let truncated: String = flat.chars().take(60).collect();
    if flat.chars().count() > 60 {
        format!("{truncated}…")
    } else {
        truncated
    }
}

async fn git(dir: &Path, args: &[&str]) -> std::io::Result<std::process::Output> {
    Command::new("git")
        .args(args)
        .current_dir(dir)
        .output()
        .await
}

/// Commit changed subprojects. Returns the names that were committed.
pub async fn commit_if_changed(
    config: &Config,
    message: &str,
    projects: Option<&[String]>,
) -> Vec<String> {
    let root = &config.data_root;
    if root.as_os_str().is_empty() || !root.exists() {
        return Vec::new();
    }
    let Ok(mut entries) = tokio::fs::read_dir(root).await else {
        return Vec::new();
    };
    let mut committed = Vec::new();
    while let Ok(Some(entry)) = entries.next_entry().await {
        let name = entry.file_name().to_string_lossy().to_string();
        if name.starts_with('.') {
            continue;
        }
        let Ok(file_type) = entry.file_type().await else {
            continue;
        };
        if !file_type.is_dir() {
            continue;
        }
        if let Some(allowed) = projects {
            if !allowed.iter().any(|p| p == &name) {
                continue;
            }
        }
        let dir = entry.path();
        if !dir.join(".git").exists() {
            continue;
        }
        let _ = git(&dir, &["add", "-A"]).await;
        let status = match git(&dir, &["status", "--porcelain"]).await {
            Ok(output) => String::from_utf8_lossy(&output.stdout).to_string(),
            Err(err) => {
                tracing::error!("[git] status failed for {name}: {err}");
                continue;
            }
        };
        if status.trim().is_empty() {
            continue;
        }
        match git(&dir, &["commit", "-m", message]).await {
            Ok(output) if output.status.success() => {
                tracing::info!("[git] committed {name}: {message}");
                committed.push(name.clone());
                if config.git_push {
                    match git(&dir, &["push"]).await {
                        Ok(push) if push.status.success() => {
                            tracing::info!("[git] pushed {name}");
                        }
                        Ok(push) => tracing::warn!(
                            "[git] push failed for {name}: {}",
                            String::from_utf8_lossy(&push.stderr).trim()
                        ),
                        Err(err) => tracing::warn!("[git] push failed for {name}: {err}"),
                    }
                }
            }
            Ok(output) => tracing::error!(
                "[git] commit failed for {name}: {}",
                String::from_utf8_lossy(&output.stderr).trim()
            ),
            Err(err) => tracing::error!("[git] commit failed for {name}: {err}"),
        }
    }
    committed
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn commit_subject_truncates_on_char_boundaries() {
        assert_eq!(commit_subject("  hello   world "), "hello world");
        assert_eq!(commit_subject(&"a".repeat(61)).chars().count(), 61);
        assert!(commit_subject(&"a".repeat(61)).ends_with('…'));
    }
}
