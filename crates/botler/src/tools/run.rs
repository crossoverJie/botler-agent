//! Controlled in-project script execution.
//!
//! Not a shell: only an existing `.py` / `.js` / `.mjs` file inside an
//! allowlisted data subproject, with a fixed interpreter, arguments passed as
//! an array, `cwd` locked to the subproject root, and a 60s timeout. Reused by
//! the `run` tool and the scheduler's direct-script path, so there is one
//! execution core and one security boundary.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use anyhow::{Context, Result};
use async_trait::async_trait;
use serde_json::{json, Value};
use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::process::Command;

use crate::paths::PathGuard;
use crate::tools::task_context::task_projects;
use crate::tools::{Tool, ToolOutput};

const TIMEOUT: Duration = Duration::from_secs(60);
const MAX_OUTPUT_BYTES: usize = 1024 * 1024;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ErrorKind {
    /// Bad path / missing file / wrong type — the script never started.
    Precondition,
    /// The script started but exited non-zero, was killed, or timed out.
    Runtime,
}

#[derive(Debug, Clone)]
pub struct RunResult {
    pub ok: bool,
    pub text: String,
    /// Subproject root, resolved as soon as the path is allowlisted.
    pub cwd: Option<PathBuf>,
    pub error_kind: Option<ErrorKind>,
}

fn interpreter_of(script: &str) -> Option<&'static str> {
    if script.ends_with(".py") {
        Some("python3")
    } else if script.ends_with(".js") || script.ends_with(".mjs") {
        Some("node")
    } else {
        None
    }
}

fn truncate_output(raw: &str) -> String {
    if raw.len() <= MAX_OUTPUT_BYTES {
        return raw.to_string();
    }
    let mut end = MAX_OUTPUT_BYTES;
    while end > 0 && !raw.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}\n…(output truncated)", &raw[..end])
}

/// Drain a child pipe while keeping at most `limit` bytes. The rest is read
/// and discarded so the child is never blocked on a full pipe (which would
/// turn a noisy script into a timeout) and memory stays bounded.
async fn drain_bounded<R: AsyncRead + Unpin>(
    mut reader: R,
    limit: usize,
) -> std::io::Result<(Vec<u8>, bool)> {
    let mut kept = Vec::new();
    let mut truncated = false;
    let mut buf = [0u8; 8192];
    loop {
        let n = reader.read(&mut buf).await?;
        if n == 0 {
            return Ok((kept, truncated));
        }
        let room = limit.saturating_sub(kept.len());
        let take = n.min(room);
        kept.extend_from_slice(&buf[..take]);
        truncated |= take < n;
    }
}

/// Run an existing script without throwing. Pre-condition problems are
/// returned as data so the scheduler can treat them like any other failure.
pub async fn run_script(
    guard: &PathGuard,
    projects: Option<&[String]>,
    script: &str,
    args: &[String],
) -> RunResult {
    let abs = match guard.safe_path(script, projects) {
        Ok(abs) => abs,
        Err(err) => {
            return RunResult {
                ok: false,
                text: format!("Script path error: {err}"),
                cwd: None,
                error_kind: Some(ErrorKind::Precondition),
            }
        }
    };
    // Resolve the owning subproject early so precondition failures can still be
    // attributed to the right project in history and commits.
    let project = guard.project_of(&abs);
    let cwd = project
        .as_deref()
        .and_then(|name| guard.project_root(name))
        .map(Path::to_path_buf);

    if !abs.is_file() {
        return RunResult {
            ok: false,
            text: format!("Script does not exist or is not a file: {script}"),
            cwd,
            error_kind: Some(ErrorKind::Precondition),
        };
    }
    let Some(cwd) = cwd else {
        return RunResult {
            ok: false,
            text: format!("Script is not inside any data subproject: {script}"),
            cwd: None,
            error_kind: Some(ErrorKind::Precondition),
        };
    };
    let Some(interpreter) = interpreter_of(script) else {
        return RunResult {
            ok: false,
            text: format!("Unsupported script type (only .py / .js / .mjs): {script}"),
            cwd: Some(cwd),
            error_kind: Some(ErrorKind::Precondition),
        };
    };

    let mut command = Command::new(interpreter);
    command
        .arg(&abs)
        .args(args)
        .current_dir(&cwd)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);

    let mut child = match command.spawn() {
        Ok(child) => child,
        Err(err) => {
            return RunResult {
                ok: false,
                text: format!("Script failed to start: {err}"),
                cwd: Some(cwd),
                error_kind: Some(ErrorKind::Runtime),
            }
        }
    };
    let (Some(stdout_pipe), Some(stderr_pipe)) = (child.stdout.take(), child.stderr.take()) else {
        let _ = child.kill().await;
        return RunResult {
            ok: false,
            text: "Script failed to start: pipes are unavailable".to_string(),
            cwd: Some(cwd),
            error_kind: Some(ErrorKind::Runtime),
        };
    };
    let stdout_task = tokio::spawn(drain_bounded(stdout_pipe, MAX_OUTPUT_BYTES));
    let stderr_task = tokio::spawn(drain_bounded(stderr_pipe, MAX_OUTPUT_BYTES));
    let stdout_abort = stdout_task.abort_handle();
    let stderr_abort = stderr_task.abort_handle();

    // One deadline covers the whole lifecycle: the child's exit AND draining
    // its pipes. A script that leaks a descendant holding the stdout/stderr
    // write ends would otherwise keep the drain tasks alive forever after the
    // child itself has exited, stalling the single dispatcher worker.
    let run = async move {
        let status = child.wait().await;
        let stdout = stdout_task.await;
        let stderr = stderr_task.await;
        (status, stdout, stderr)
    };
    let (status, stdout_join, stderr_join) = match tokio::time::timeout(TIMEOUT, run).await {
        Ok(triple) => triple,
        Err(_) => {
            // Dropping run drops the child (kill_on_drop reaps it). Drain tasks
            // are aborted explicitly: dropping a JoinHandle only detaches.
            stdout_abort.abort();
            stderr_abort.abort();
            return RunResult {
                ok: false,
                text: format!("Script timed out after {}s: {script}", TIMEOUT.as_secs()),
                cwd: Some(cwd),
                error_kind: Some(ErrorKind::Runtime),
            };
        }
    };
    let status = match status {
        Ok(status) => status,
        Err(err) => {
            return RunResult {
                ok: false,
                text: format!("Script failed: {err}"),
                cwd: Some(cwd),
                error_kind: Some(ErrorKind::Runtime),
            };
        }
    };
    let (stdout, stdout_truncated) = stdout_join
        .ok()
        .and_then(|result| result.ok())
        .unwrap_or_default();
    let (stderr, stderr_truncated) = stderr_join
        .ok()
        .and_then(|result| result.ok())
        .unwrap_or_default();
    let truncated = stdout_truncated || stderr_truncated;
    let stdout = String::from_utf8_lossy(&stdout);
    let stderr = String::from_utf8_lossy(&stderr);

    if status.success() {
        let text = if stdout.trim().is_empty() {
            "(script ran successfully, no output)".to_string()
        } else {
            truncate_output(&stdout)
        };
        let text = if truncated && !text.ends_with("…(output truncated)") {
            format!("{text}\n…(output truncated)")
        } else {
            text
        };
        RunResult {
            ok: true,
            text,
            cwd: Some(cwd),
            error_kind: None,
        }
    } else {
        let detail = truncate_output(format!("{stdout}{stderr}").trim());
        let detail = if truncated && !detail.ends_with("…(output truncated)") {
            format!("{detail}\n…(output truncated)")
        } else {
            detail
        };
        let msg = if detail.is_empty() {
            format!("Script failed: exited with {status}")
        } else {
            format!("Script failed ({status}):\n{detail}")
        };
        RunResult {
            ok: false,
            text: msg,
            cwd: Some(cwd),
            error_kind: Some(ErrorKind::Runtime),
        }
    }
}

pub struct RunTool {
    guard: PathGuard,
}

impl RunTool {
    pub fn new(guard: PathGuard) -> Self {
        Self { guard }
    }
}

#[async_trait]
impl Tool for RunTool {
    fn name(&self) -> &str {
        "run"
    }

    fn label(&self) -> &str {
        "Run in-project script"
    }

    fn description(&self) -> &str {
        "Run an existing script inside a data subproject (only python3 .py or node .js/.mjs), used for build/refresh scripts defined by the project's conventions (e.g. build.py). No shell is used; args are passed directly; the working directory is locked to the script's subproject root with a 60-second timeout. Only run it when the project's conventions (AGENTS.md) require it."
    }

    fn parameters(&self) -> Value {
        json!({
            "type": "object",
            "properties": {
                "script": { "type": "string", "description": "Script path relative to DATA_ROOT, e.g. my-project/scripts/build.py" },
                "args": { "type": "array", "items": { "type": "string" }, "description": "Arguments passed to the script (optional)" }
            },
            "required": ["script"]
        })
    }

    async fn execute(&self, args: Value) -> Result<ToolOutput> {
        let script = args
            .get("script")
            .and_then(Value::as_str)
            .context("run requires a string script path")?;
        let script_args: Vec<String> = match args.get("args") {
            None => Vec::new(),
            Some(value) => {
                let items = value
                    .as_array()
                    .context("run requires args to be an array of strings")?;
                items
                    .iter()
                    .enumerate()
                    .map(|(i, item)| {
                        item.as_str()
                            .map(str::to_string)
                            .with_context(|| format!("run requires args[{i}] to be a string"))
                    })
                    .collect::<Result<Vec<_>>>()?
            }
        };
        let projects = task_projects();
        let result = run_script(&self.guard, projects.as_deref(), script, &script_args).await;
        // Preserve v1 behaviour: a precondition error throws so the agent sees
        // an error and can self-heal; a runtime failure is returned as text.
        if !result.ok && result.error_kind == Some(ErrorKind::Precondition) {
            anyhow::bail!(result.text);
        }
        // A project script can rewrite data files (and may do so before
        // failing), so always trigger validation and the commit pass.
        Ok(ToolOutput::mutated(result.text))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn captured_output_is_bounded_but_fully_drained() {
        let data = vec![b'a'; 100_000];
        let (kept, truncated) = drain_bounded(&data[..], 1024).await.unwrap();
        assert_eq!(kept.len(), 1024);
        assert!(truncated);

        let (kept, truncated) = drain_bounded(&data[..], 1_000_000).await.unwrap();
        assert_eq!(kept.len(), data.len());
        assert!(!truncated);
    }

    #[tokio::test]
    async fn malformed_args_are_rejected_before_running() {
        use crate::paths::PathGuard;
        let dir = tempfile::tempdir().unwrap();
        // PathGuard requires at least one project subdir under the data root.
        std::fs::create_dir(dir.path().join("proj")).unwrap();
        let tool = RunTool::new(PathGuard::new(dir.path()).unwrap());
        let cases = [
            serde_json::json!({ "script": "x.py", "args": "not-an-array" }),
            serde_json::json!({ "script": "x.py", "args": [1, 2, 3] }),
            serde_json::json!({ "script": "x.py", "args": ["ok", null] }),
        ];
        for args in cases {
            let err = tool.execute(args).await.err().unwrap();
            assert!(err.to_string().contains("args"), "{err}");
        }
    }
}
