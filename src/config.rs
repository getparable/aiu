use anyhow::{Context, Result, bail};
use std::{env, path::PathBuf};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StoreMode {
    Native,
    File,
}

#[derive(Clone, Debug)]
pub struct Config {
    pub dir: PathBuf,
    pub store_mode: StoreMode,
    pub claude_dir: PathBuf,
    pub claude_global_config: PathBuf,
    pub claude_service: String,
    pub claude_use_keychain: bool,
    pub codex_home: PathBuf,
    pub claude_usage_url: String,
    pub claude_profile_url: String,
    pub claude_token_urls: Vec<String>,
    pub claude_authorize_url: String,
    pub claude_authorize_console_url: String,
    pub claude_manual_redirect_url: String,
    pub codex_usage_url: String,
    pub codex_token_url: String,
    pub codex_authorize_url: String,
    pub release_api_url: String,
}

impl Config {
    pub fn from_env() -> Result<Self> {
        let home = dirs::home_dir().context("cannot locate your home directory")?;
        let config_base = dirs::config_dir().unwrap_or_else(|| home.join(".config"));
        let mut cfg = Self::for_home(home, config_base.join("aiu-rs"));
        if let Some(dir) = env::var_os("AIU_CONFIG_DIR").filter(|v| !v.is_empty()) {
            cfg.dir = dir.into();
        }
        if let Some(dir) = env::var_os("CODEX_HOME").filter(|v| !v.is_empty()) {
            cfg.codex_home = dir.into();
        }
        if let Some(dir) = env::var_os("CLAUDE_CONFIG_DIR").filter(|v| !v.is_empty()) {
            use sha2::{Digest, Sha256};
            use unicode_normalization::UnicodeNormalization;
            let normalized: String = dir.to_string_lossy().nfc().collect();
            let hash = format!("{:x}", Sha256::digest(normalized.as_bytes()));
            cfg.claude_service = format!("Claude Code-credentials-{}", &hash[..8]);
            cfg.claude_dir = dir.into();
            cfg.claude_global_config = cfg.claude_dir.join(".claude.json");
        }
        if let Ok(service) = env::var("AIU_CLAUDE_SERVICE") {
            cfg.claude_service = service;
        }
        match env::var("AIU_STORE").as_deref() {
            Ok("file") => cfg.store_mode = StoreMode::File,
            Ok("native") => cfg.store_mode = StoreMode::Native,
            Ok("") | Err(_) => {}
            Ok(other) => bail!("unknown AIU_STORE {other:?}; use native or file"),
        }
        Ok(cfg)
    }

    pub fn for_home(home: PathBuf, dir: PathBuf) -> Self {
        Self {
            dir,
            store_mode: if cfg!(any(windows, target_os = "macos")) {
                StoreMode::Native
            } else {
                StoreMode::File
            },
            claude_dir: home.join(".claude"),
            claude_global_config: home.join(".claude.json"),
            claude_service: "Claude Code-credentials".into(),
            claude_use_keychain: cfg!(target_os = "macos"),
            codex_home: home.join(".codex"),
            claude_usage_url: "https://api.anthropic.com/api/oauth/usage".into(),
            claude_profile_url: "https://api.anthropic.com/api/oauth/profile".into(),
            claude_token_urls: vec![
                "https://platform.claude.com/v1/oauth/token".into(),
                "https://console.anthropic.com/v1/oauth/token".into(),
            ],
            claude_authorize_url: "https://claude.ai/oauth/authorize".into(),
            claude_authorize_console_url: "https://platform.claude.com/oauth/authorize".into(),
            claude_manual_redirect_url: "https://console.anthropic.com/oauth/code/callback".into(),
            codex_usage_url: "https://chatgpt.com/backend-api/wham/usage".into(),
            codex_token_url: "https://auth.openai.com/oauth/token".into(),
            codex_authorize_url: "https://auth.openai.com/oauth/authorize".into(),
            release_api_url: "https://api.github.com/repos/krflol/aiu-rs/releases/latest".into(),
        }
    }
    pub fn index_file(&self) -> PathBuf {
        self.dir.join("accounts.json")
    }
    pub fn cache_file(&self) -> PathBuf {
        self.dir.join("usage-cache.json")
    }
    pub fn codex_auth_file(&self) -> PathBuf {
        self.codex_home.join("auth.json")
    }
    pub fn storage_description(&self) -> String {
        match self.store_mode {
            StoreMode::File => format!("{} (plaintext)", self.dir.join("tokens.json").display()),
            StoreMode::Native if cfg!(windows) => {
                "Windows DPAPI (encrypted for your Windows user)".into()
            }
            StoreMode::Native if cfg!(target_os = "macos") => "macOS Keychain (aiu-rs)".into(),
            StoreMode::Native => {
                "native storage is unavailable on this platform; use AIU_STORE=file".into()
            }
        }
    }
}
