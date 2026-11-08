//! `botler init` — generate the externalized user config directory.
//!
//! Port of v1 `src/init.ts`. Everything is written only when absent, with
//! 0700 on the directory and 0600 on the files. No secrets are embedded: the
//! `.env` is copied from the source `.env` (if present) or `.env.example`.
//! `DATA_ROOT` is never pointed at the repository by these templates.

use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result};

use crate::config;
use crate::fsutil::atomic_write;
use crate::prompt::DEFAULT_SYSTEM_PROMPT;

const PROMPT_HEADER: &str = r#"<!-- 外置系统提示词。botler-agent 每次运行都会加载本文件作为 Agent 的 system prompt。
支持占位符（自动注入）：
  __DATA_ROOT__         数据根目录
  __PROJECT_CONTEXT__   各子项目清单 + 其 AGENTS.md/CLAUDE.md 约定全文
  __TODAY__             今天日期（YYYY-MM-DD）

定制方式：把下面的模板替换成你自己项目的约定（各数据子项目的文件结构、操作规则等），
或让 AI 助手根据你的项目背景帮你生成。安全边界（白名单目录、无 bash 等）由
botler-agent 代码强制，不因本文件内容而改变。
-->
"#;

/// Run against the real config dir / source tree. Called before `Config::load`
/// and `PathGuard::new` so a fresh machine with no `DATA_ROOT` can bootstrap.
pub fn run_cli() -> Result<()> {
    let user_dir = config::user_config_dir();
    let app_root = config::app_root();
    for line in run(&user_dir, &app_root)? {
        println!("{line}");
    }
    println!(
        "\nNext steps:\n  \
         1. Edit {}/.env and fill in DATA_ROOT, PI_PROVIDER / PI_MODEL, channel credentials.\n  \
         2. Edit {}/providers.json with your gateway (baseUrl / apiKey / models).\n  \
         3. Customize {}/system-prompt.md if needed.\n  \
         4. (Optional) Set SCHEDULER_ENABLED=1 and edit schedules.json.\n  \
         5. Run `botler` (server mode) or `botler \"your message\"` (one-shot).",
        user_dir.display(),
        user_dir.display(),
        user_dir.display()
    );
    Ok(())
}

/// Pure, injectable variant used by unit tests. Returns the human-readable log.
pub fn run(user_dir: &Path, app_root: &Path) -> Result<Vec<String>> {
    fs::create_dir_all(user_dir).with_context(|| format!("create {}", user_dir.display()))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(user_dir, fs::Permissions::from_mode(0o700))?;
    }

    let mut out = Vec::new();

    // 1. .env — migrate the real source .env if present, else the template.
    let env_target = user_dir.join(".env");
    if env_target.exists() {
        out.push(format!("{} already exists; skipped", env_target.display()));
    } else {
        let source_env = app_root.join(".env");
        let example = app_root.join(".env.example");
        let (src, from_source) = if source_env.exists() {
            (source_env, true)
        } else {
            (example, false)
        };
        if src.exists() {
            let bytes = fs::read(&src).with_context(|| format!("read {}", src.display()))?;
            write_once(
                &env_target,
                &bytes,
                if from_source {
                    "migrated from source .env; please review"
                } else {
                    "from .env.example template; please review"
                },
                &mut out,
            )?;
        } else {
            out.push("No .env or .env.example found; skipping .env generation".into());
        }
    }

    // 2. system-prompt.md — Chinese header + the built-in default template.
    let prompt_target = user_dir.join("system-prompt.md");
    let mut prompt = String::from(PROMPT_HEADER);
    prompt.push_str(DEFAULT_SYSTEM_PROMPT);
    write_once(
        &prompt_target,
        prompt.as_bytes(),
        "default template; customize or have AI generate it",
        &mut out,
    )?;

    // 3. providers.json — custom OpenAI-compatible gateway template.
    let providers = serde_json::json!({
        "providers": {
            "custom": {
                "api": "openai-completions",
                "baseUrl": "https://your-gateway.example.com/v1",
                "apiKey": "sk-REPLACE_ME",
                "models": [
                    { "id": "your-model-pro", "name": "Your Model Pro", "reasoning": true,
                      "contextWindow": 1048576, "maxTokens": 393216, "vision": true },
                    { "id": "your-model-flash", "name": "Your Model Flash", "reasoning": false,
                      "contextWindow": 1048576, "maxTokens": 393216, "vision": true }
                ]
            }
        }
    });
    let mut providers_bytes = serde_json::to_vec_pretty(&providers)?;
    providers_bytes.push(b'\n');
    write_once(
        &user_dir.join("providers.json"),
        &providers_bytes,
        "template; edit baseUrl / apiKey / models, then select via PI_PROVIDER / PI_MODEL",
        &mut out,
    )?;

    // 4. schedules.json — one disabled example.
    let schedules = serde_json::json!({
        "schedules": [{
            "id": "daily-example-reminder",
            "enabled": false,
            "cron": "0 8 * * *",
            "timezone": "Asia/Shanghai",
            "message": "Example reminder: log today's activity",
            "silentHours": { "from": "22:00", "to": "07:00" }
        }]
    });
    let mut schedules_bytes = serde_json::to_vec_pretty(&schedules)?;
    schedules_bytes.push(b'\n');
    write_once(
        &user_dir.join("schedules.json"),
        &schedules_bytes,
        "template with a disabled example; set SCHEDULER_ENABLED=1 to enable",
        &mut out,
    )?;

    Ok(out)
}

fn write_once(path: &Path, bytes: &[u8], label: &str, out: &mut Vec<String>) -> Result<()> {
    if path.exists() {
        out.push(format!("{} already exists; skipped", path.display()));
        return Ok(());
    }
    atomic_write(path, bytes)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    }
    out.push(format!("Generated {} ({label})", path.display()));
    Ok(())
}

/// Helper for callers that want the resolved paths (used by tests/docs).
pub fn config_paths(user_dir: &Path) -> Vec<PathBuf> {
    [
        ".env",
        "system-prompt.md",
        "providers.json",
        "schedules.json",
    ]
    .iter()
    .map(|name| user_dir.join(name))
    .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generates_all_four_templates_only_when_absent() {
        let tmp = tempfile::tempdir().unwrap();
        let app = tmp.path().join("app");
        let user = tmp.path().join("user");
        fs::create_dir_all(&app).unwrap();
        fs::write(app.join(".env.example"), "DATA_ROOT=/from/example\n").unwrap();

        let log = run(&user, &app).unwrap();
        assert_eq!(log.len(), 4);
        assert_eq!(
            fs::read_to_string(user.join(".env")).unwrap(),
            "DATA_ROOT=/from/example\n"
        );
        assert!(fs::read_to_string(user.join("system-prompt.md"))
            .unwrap()
            .contains("你是运行在个人数据目录下的轻量助手"));
        let providers: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(user.join("providers.json")).unwrap())
                .unwrap();
        assert_eq!(providers["providers"]["custom"]["apiKey"], "sk-REPLACE_ME");
        let schedules: serde_json::Value =
            serde_json::from_str(&fs::read_to_string(user.join("schedules.json")).unwrap())
                .unwrap();
        assert_eq!(schedules["schedules"][0]["enabled"], false);

        // Idempotent: existing files are never overwritten.
        fs::write(user.join(".env"), "DATA_ROOT=/custom\n").unwrap();
        let second = run(&user, &app).unwrap();
        assert_eq!(second.len(), 4);
        assert!(second.iter().all(|line| line.contains("skipped")));
        assert_eq!(
            fs::read_to_string(user.join(".env")).unwrap(),
            "DATA_ROOT=/custom\n"
        );
    }

    #[test]
    fn migrates_source_env_when_present() {
        let tmp = tempfile::tempdir().unwrap();
        let app = tmp.path().join("app");
        let user = tmp.path().join("user");
        fs::create_dir_all(&app).unwrap();
        fs::write(app.join(".env"), "DATA_ROOT=/real\n").unwrap();
        fs::write(app.join(".env.example"), "DATA_ROOT=/example\n").unwrap();
        run(&user, &app).unwrap();
        assert_eq!(
            fs::read_to_string(user.join(".env")).unwrap(),
            "DATA_ROOT=/real\n"
        );
    }

    #[test]
    fn skips_env_generation_when_no_template_exists() {
        let tmp = tempfile::tempdir().unwrap();
        let log = run(&tmp.path().join("user"), &tmp.path().join("missing-app")).unwrap();
        assert!(log.iter().any(|l| l.contains("No .env or .env.example")));
        assert!(!tmp.path().join("user/.env").exists());
    }

    #[cfg(unix)]
    #[test]
    fn dir_and_files_are_private() {
        use std::os::unix::fs::PermissionsExt;
        let tmp = tempfile::tempdir().unwrap();
        let app = tmp.path().join("app");
        fs::create_dir_all(&app).unwrap();
        fs::write(app.join(".env.example"), "X=1\n").unwrap();
        let user = tmp.path().join("user");
        run(&user, &app).unwrap();
        assert_eq!(
            fs::metadata(&user).unwrap().permissions().mode() & 0o777,
            0o700
        );
        assert_eq!(
            fs::metadata(user.join(".env"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
    }
}
