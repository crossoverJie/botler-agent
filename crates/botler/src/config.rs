//! Configuration loading.
//!
//! Priority (highest first): process env > `~/.botler-agent/.env` > source-dir
//! `.env` > built-in defaults. This mirrors v1's two-level loader: user-level
//! values are parsed first and source-level values never override them.

use std::collections::HashMap;
use std::env;
use std::fs;
use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};

/// The app root (this repository). Used only for the dev-phase source `.env`
/// fallback; the binary may be moved, so this is resolved at build time.
pub fn app_root() -> PathBuf {
    let manifest = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    // crates/botler -> crates -> repo root
    manifest
        .parent()
        .and_then(Path::parent)
        .map(Path::to_path_buf)
        .unwrap_or(manifest)
}

/// Externalized system prompt template file name (inside the config dir).
pub const USER_CONFIG_FILE_NAME: &str = "system-prompt.md";

/// `BOTLER_CONFIG_DIR` (default `~/.botler-agent`). Comes only from the real
/// process env: reading it from `.env` would be chicken-and-egg.
pub fn user_config_dir() -> PathBuf {
    env::var_os("BOTLER_CONFIG_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| home_dir().join(".botler-agent"))
}

fn home_dir() -> PathBuf {
    env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("."))
}

/// Parse a `.env` file, filling only variables that are not already set.
pub fn parse_dotenv_into(path: &Path, target: &mut HashMap<String, String>) {
    let Ok(raw) = fs::read_to_string(path) else {
        return;
    };
    for line in raw.lines() {
        let trimmed = line.trim();
        if trimmed.is_empty() || trimmed.starts_with('#') {
            continue;
        }
        let Some(eq) = trimmed.find('=') else {
            continue;
        };
        let key = trimmed[..eq].trim();
        if key.is_empty() {
            continue;
        }
        let mut value = trimmed[eq + 1..].trim().to_string();
        if (value.starts_with('"') && value.ends_with('"') && value.len() >= 2)
            || (value.starts_with('\'') && value.ends_with('\'') && value.len() >= 2)
        {
            value = value[1..value.len() - 1].to_string();
        }
        target.entry(key.to_string()).or_insert(value);
    }
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum ProviderApi {
    #[default]
    #[serde(alias = "openai", alias = "openai-completions")]
    OpenAiCompletions,
    #[serde(alias = "anthropic", alias = "anthropic-messages")]
    AnthropicMessages,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ModelMeta {
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub reasoning: bool,
    #[serde(default)]
    pub context_window: u64,
    #[serde(default)]
    pub max_tokens: u64,
    /// Whether the model accepts image input. Absent means "assume yes",
    /// matching v1/pi's text+image default.
    #[serde(default = "default_true")]
    pub vision: bool,
}

fn default_true() -> bool {
    true
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CustomProviderConfig {
    pub id: String,
    #[serde(default)]
    pub api: ProviderApi,
    pub base_url: String,
    pub api_key: String,
    #[serde(default)]
    pub models: Vec<ModelMeta>,
}

#[derive(Debug, Clone)]
pub struct Config {
    pub data_root: PathBuf,
    pub provider: String,
    pub model: String,
    pub custom_providers: Vec<CustomProviderConfig>,

    pub telegram_token: Option<String>,
    pub telegram_allow_from: Option<String>,
    pub feishu_app_id: Option<String>,
    pub feishu_app_secret: Option<String>,
    pub feishu_verification_token: Option<String>,
    pub feishu_encrypt_key: Option<String>,
    pub feishu_port: u16,
    pub feishu_allow_from: Option<String>,
    pub wechat_enabled: bool,
    pub wechat_allow_from: Option<String>,
    pub wechat_reminder_hours: u32,
    pub wechat_image_batch_seconds: u64,
    pub git_push: bool,
    pub scheduler_enabled: bool,
    pub schedules_file: PathBuf,
    pub holidays_file: PathBuf,
    pub holiday_api_url: String,
    pub log_dir: PathBuf,
    pub monitor_enabled: bool,
    pub monitor_port: u16,
    pub max_tool_turns: usize,

    // v2 session / memory / concurrency settings.
    pub user_config_dir: PathBuf,
    pub sessions_dir: PathBuf,
    pub sessions_index_file: PathBuf,
    pub memory_dir: PathBuf,
    pub session_hot_limit: usize,
    pub retention_days: u64,
    pub retention_max_bytes: u64,
    pub memory_content_max_bytes: usize,
    pub recall_token_budget: usize,
    pub memory_digest_max_chars: usize,
}

impl Config {
    /// Explicit-path constructor for embedders and tests (no env/files read).
    /// Mirrors the defaults of [`Config::load`] with every channel disabled.
    pub fn with_paths(data_root: impl Into<PathBuf>, user_config_dir: impl Into<PathBuf>) -> Self {
        let user_dir = user_config_dir.into();
        Self {
            data_root: data_root.into(),
            provider: "stub".into(),
            model: "stub".into(),
            custom_providers: Vec::new(),
            telegram_token: None,
            telegram_allow_from: None,
            feishu_app_id: None,
            feishu_app_secret: None,
            feishu_verification_token: None,
            feishu_encrypt_key: None,
            feishu_port: 3000,
            feishu_allow_from: None,
            wechat_enabled: false,
            wechat_allow_from: None,
            wechat_reminder_hours: 23,
            wechat_image_batch_seconds: 60,
            git_push: false,
            scheduler_enabled: false,
            schedules_file: user_dir.join("schedules.json"),
            holidays_file: user_dir.join("holidays.json"),
            holiday_api_url:
                "https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/{year}.json".into(),
            log_dir: user_dir.join("task-logs"),
            monitor_enabled: false,
            monitor_port: 8899,
            max_tool_turns: 20,
            sessions_dir: user_dir.join("sessions"),
            sessions_index_file: user_dir.join("sessions-index.json"),
            memory_dir: user_dir.join("memory"),
            session_hot_limit: 32,
            retention_days: 90,
            retention_max_bytes: 2 * 1024 * 1024 * 1024,
            memory_content_max_bytes: 2048,
            recall_token_budget: 1200,
            memory_digest_max_chars: 1500,
            user_config_dir: user_dir,
        }
    }

    pub fn load() -> Result<Self> {
        let user_dir = user_config_dir();
        let mut vars = HashMap::new();
        // User-level first (higher priority); source-level never overrides it.
        parse_dotenv_into(&user_dir.join(".env"), &mut vars);
        parse_dotenv_into(&app_root().join(".env"), &mut vars);

        // Process env wins over both files.
        let get =
            |key: &str| -> Option<String> { env::var(key).ok().or_else(|| vars.get(key).cloned()) };
        let get_non_empty = |key: &str| -> Option<String> { get(key).filter(|v| !v.is_empty()) };

        let data_root_raw = get("DATA_ROOT").unwrap_or_default();
        let data_root = PathBuf::from(&data_root_raw);

        let provider = get("PI_PROVIDER").unwrap_or_else(|| "anthropic".into());
        let model = get("PI_MODEL").unwrap_or_else(|| "claude-sonnet-4-5".into());
        let custom_providers = build_custom_providers(&user_dir, &vars, &get, &provider)
            .context("load custom providers")?;

        let sessions_dir = get_non_empty("BOTLER_SESSIONS_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(|| user_dir.join("sessions"));

        let config = Self {
            data_root,
            provider,
            model,
            custom_providers,
            telegram_token: get_non_empty("TELEGRAM_BOT_TOKEN"),
            telegram_allow_from: get_non_empty("TELEGRAM_ALLOW_FROM"),
            feishu_app_id: get_non_empty("FEISHU_APP_ID"),
            feishu_app_secret: get_non_empty("FEISHU_APP_SECRET"),
            feishu_verification_token: get_non_empty("FEISHU_VERIFICATION_TOKEN"),
            feishu_encrypt_key: get_non_empty("FEISHU_ENCRYPT_KEY"),
            feishu_port: parse_u16(&get, "FEISHU_PORT", 3000),
            feishu_allow_from: get_non_empty("FEISHU_ALLOW_FROM"),
            wechat_enabled: get("WECHAT_ENABLED").as_deref() == Some("1"),
            wechat_allow_from: get_non_empty("WECHAT_ALLOW_FROM"),
            wechat_reminder_hours: parse_reminder_hours(&get),
            wechat_image_batch_seconds: parse_image_batch_seconds(&get),
            git_push: get("GIT_PUSH").as_deref() == Some("1"),
            scheduler_enabled: get("SCHEDULER_ENABLED").as_deref() == Some("1"),
            schedules_file: get_non_empty("BOTLER_SCHEDULES_FILE")
                .map(PathBuf::from)
                .unwrap_or_else(|| user_dir.join("schedules.json")),
            holidays_file: get_non_empty("BOTLER_HOLIDAYS_FILE")
                .map(PathBuf::from)
                .unwrap_or_else(|| user_dir.join("holidays.json")),
            holiday_api_url: get_non_empty("BOTLER_HOLIDAY_API_URL").unwrap_or_else(|| {
                "https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/{year}.json".into()
            }),
            log_dir: get_non_empty("BOTLER_LOG_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|| user_dir.join("task-logs")),
            monitor_enabled: get("MONITOR_ENABLED").as_deref() != Some("0"),
            monitor_port: parse_u16(&get, "MONITOR_PORT", 8899),
            max_tool_turns: parse_max_tool_turns(&get),
            sessions_dir,
            sessions_index_file: get_non_empty("BOTLER_SESSIONS_INDEX")
                .map(PathBuf::from)
                .unwrap_or_else(|| user_dir.join("sessions-index.json")),
            memory_dir: get_non_empty("BOTLER_MEMORY_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|| user_dir.join("memory")),
            session_hot_limit: get_non_empty("BOTLER_SESSION_HOT_LIMIT")
                .and_then(|v| v.parse().ok())
                .filter(|n| *n > 0)
                .unwrap_or(32),
            retention_days: get_non_empty("BOTLER_SESSION_RETENTION_DAYS")
                .and_then(|v| v.parse().ok())
                .unwrap_or(90),
            retention_max_bytes: get_non_empty("BOTLER_SESSION_RETENTION_MAX_BYTES")
                .and_then(|v| v.parse().ok())
                .unwrap_or(2 * 1024 * 1024 * 1024),
            memory_content_max_bytes: 2048,
            recall_token_budget: 1200,
            memory_digest_max_chars: 1500,
            user_config_dir: user_dir,
        };
        config.validate()?;
        Ok(config)
    }

    /// Fail fast on configurations that would expose an unauthenticated or
    /// unverified channel.
    pub fn validate(&self) -> Result<()> {
        let feishu_enabled = self.feishu_app_id.is_some() && self.feishu_app_secret.is_some();
        if feishu_enabled
            && self
                .feishu_verification_token
                .as_deref()
                .is_none_or(|token| token.trim().is_empty())
        {
            bail!(
                "FEISHU_VERIFICATION_TOKEN is required when Feishu is enabled; \
                 refusing to start a webhook that accepts unauthenticated events"
            );
        }
        Ok(())
    }

    pub fn has_channels(&self) -> bool {
        self.telegram_token.is_some()
            || (self.feishu_app_id.is_some() && self.feishu_app_secret.is_some())
            || self.wechat_enabled
    }
}

fn parse_u16(get: &impl Fn(&str) -> Option<String>, key: &str, default: u16) -> u16 {
    get(key)
        .and_then(|v| v.trim().parse().ok())
        .unwrap_or(default)
}

fn parse_max_tool_turns(get: &impl Fn(&str) -> Option<String>) -> usize {
    get("MAX_TOOL_TURNS")
        .and_then(|v| v.trim().parse::<usize>().ok())
        .filter(|n| *n >= 1)
        .unwrap_or(20)
}

fn parse_reminder_hours(get: &impl Fn(&str) -> Option<String>) -> u32 {
    match get("WECHAT_REMINDER_HOURS").as_deref() {
        Some("0") => 0,
        Some(raw) => raw
            .trim()
            .parse::<f64>()
            .ok()
            .filter(|n| *n >= 1.0)
            .map(|n| n.min(24.0) as u32)
            .unwrap_or(23),
        None => 23,
    }
}

fn parse_image_batch_seconds(get: &impl Fn(&str) -> Option<String>) -> u64 {
    match get("WECHAT_IMAGE_BATCH_SECONDS").as_deref() {
        Some("0") => 0,
        Some(raw) => raw.trim().parse::<u64>().unwrap_or(60),
        None => 60,
    }
}

#[derive(Deserialize)]
struct ProvidersFile {
    #[serde(default)]
    providers: HashMap<String, RawProvider>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawProvider {
    #[serde(default)]
    api: Option<String>,
    #[serde(default)]
    base_url: Option<String>,
    #[serde(default)]
    api_key: Option<String>,
    #[serde(default)]
    models: Option<Vec<ModelMeta>>,
}

fn parse_provider_api(raw: Option<&str>) -> Option<ProviderApi> {
    match raw {
        None | Some("") | Some("openai") | Some("openai-completions") => {
            Some(ProviderApi::OpenAiCompletions)
        }
        Some("anthropic") | Some("anthropic-messages") => Some(ProviderApi::AnthropicMessages),
        Some(_) => None,
    }
}

/// providers.json is the primary source of truth. A malformed entry is skipped
/// individually; a missing/unusable file falls back to the legacy CUSTOM_* vars.
pub fn load_providers_file(user_dir: &Path) -> Vec<CustomProviderConfig> {
    let file = user_dir.join("providers.json");
    let Ok(raw) = fs::read_to_string(&file) else {
        return Vec::new();
    };
    let parsed: ProvidersFile = match serde_json::from_str(&raw) {
        Ok(v) => v,
        Err(err) => {
            tracing::warn!("[config] failed to parse {}: {err}", file.display());
            return Vec::new();
        }
    };
    let mut out = Vec::new();
    for (id, p) in parsed.providers {
        let Some(base_url) = p.base_url.filter(|v| !v.is_empty()) else {
            continue;
        };
        let Some(api_key) = p.api_key.filter(|v| !v.is_empty()) else {
            continue;
        };
        let Some(api) = parse_provider_api(p.api.as_deref()) else {
            tracing::warn!("[config] skipping provider {id}: unsupported api");
            continue;
        };
        let mut models = Vec::new();
        for m in p.models.unwrap_or_default() {
            if m.id.is_empty() {
                continue;
            }
            let mut m = m;
            if m.name.is_empty() {
                m.name = m.id.clone();
            }
            models.push(m);
        }
        if models.is_empty() {
            continue;
        }
        out.push(CustomProviderConfig {
            id,
            api,
            base_url,
            api_key,
            models,
        });
    }
    out
}

fn build_custom_providers(
    user_dir: &Path,
    _vars: &HashMap<String, String>,
    get: &impl Fn(&str) -> Option<String>,
    provider: &str,
) -> Result<Vec<CustomProviderConfig>> {
    let from_file = load_providers_file(user_dir);
    if !from_file.is_empty() {
        return Ok(from_file);
    }
    if provider == "anthropic" {
        return Ok(Vec::new());
    }
    let Some(base_url) = get("CUSTOM_BASE_URL") else {
        return Ok(Vec::new());
    };
    let Some(api_key) = get("CUSTOM_API_KEY") else {
        return Ok(Vec::new());
    };
    Ok(vec![CustomProviderConfig {
        id: get("PI_PROVIDER").unwrap_or_else(|| "custom".into()),
        api: ProviderApi::OpenAiCompletions,
        base_url,
        api_key,
        models: vec![
            ModelMeta {
                id: "your-model-pro".into(),
                name: "Your Model Pro".into(),
                reasoning: true,
                context_window: 1_048_576,
                max_tokens: 393_216,
                vision: true,
            },
            ModelMeta {
                id: "your-model-flash".into(),
                name: "Your Model Flash".into(),
                reasoning: false,
                context_window: 1_048_576,
                max_tokens: 393_216,
                vision: true,
            },
        ],
    }])
}

/// Resolve the selected provider block (if it is a custom one).
pub fn selected_provider(config: &Config) -> Option<&CustomProviderConfig> {
    config
        .custom_providers
        .iter()
        .find(|p| p.id == config.provider)
}

pub fn find_model<'a>(provider: &'a CustomProviderConfig, model: &str) -> Option<&'a ModelMeta> {
    provider.models.iter().find(|m| m.id == model)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write(path: &Path, content: &str) {
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, content).unwrap();
    }

    #[test]
    fn dotenv_strips_quotes_and_keeps_first_value() {
        let tmp = tempfile::tempdir().unwrap();
        let file = tmp.path().join(".env");
        write(
            &file,
            "A=1\nB=\"two words\"\nC='single'\n# comment\nA=override\n",
        );
        let mut vars = HashMap::new();
        parse_dotenv_into(&file, &mut vars);
        assert_eq!(vars.get("A").unwrap(), "1");
        assert_eq!(vars.get("B").unwrap(), "two words");
        assert_eq!(vars.get("C").unwrap(), "single");
        assert!(!vars.contains_key("# comment"));
    }

    #[test]
    fn providers_file_skips_malformed_entries() {
        let tmp = tempfile::tempdir().unwrap();
        write(
            &tmp.path().join("providers.json"),
            r#"{
              "providers": {
                "good": {
                  "baseUrl": "https://example.test/v1",
                  "apiKey": "k",
                  "models": [{ "id": "m1", "name": "M1", "contextWindow": 1000, "maxTokens": 100 }]
                },
                "no-key": { "baseUrl": "https://x", "models": [{ "id": "m" }] },
                "bad-api": { "api": "grpc", "baseUrl": "https://x", "apiKey": "k", "models": [{ "id": "m" }] }
              }
            }"#,
        );
        let got = load_providers_file(tmp.path());
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].id, "good");
        assert_eq!(got[0].models[0].id, "m1");
        assert!(got[0].models[0].vision);
    }
}
