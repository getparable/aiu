use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use std::fmt;

#[derive(
    Clone,
    Copy,
    Debug,
    Default,
    PartialEq,
    Eq,
    PartialOrd,
    Ord,
    Serialize,
    Deserialize,
    clap::ValueEnum,
)]
#[serde(rename_all = "lowercase")]
pub enum Provider {
    #[default]
    Claude,
    Codex,
}

impl fmt::Display for Provider {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Claude => "claude",
            Self::Codex => "codex",
        })
    }
}

impl Provider {
    pub fn client(self) -> &'static str {
        match self {
            Self::Claude => "Claude Code",
            Self::Codex => "Codex",
        }
    }
}

// Secrets deliberately have no Debug implementation. Only AccountView leaves the core.
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Record {
    pub provider: Provider,
    pub email: String,
    pub label: String,
    pub access_token: String,
    pub refresh_token: String,
    pub id_token: String,
    pub expires_at: i64,
    pub refresh_token_expires_at: i64,
    pub last_refresh: i64,
    pub scopes: Vec<String>,
    pub subscription_type: String,
    pub rate_limit_tier: String,
    pub plan_type: String,
    pub account_id: String,
    pub user_id: String,
    pub org_uuid: String,
    pub org_name: String,
    pub profile: Map<String, Value>,
    pub source: String,
    pub captured_at: i64,
    pub updated_at: i64,
    #[serde(skip)]
    pub missing: bool,
}

impl Record {
    pub fn key(&self) -> String {
        account_key(self.provider, &self.email, &self.org_uuid)
    }
    pub fn is_expired(&self, now: i64, margin: i64) -> bool {
        self.expires_at == 0 || self.expires_at.saturating_sub(now) <= margin
    }
    pub fn is_read_only(&self) -> bool {
        self.provider == Provider::Claude
            && !self.scopes.is_empty()
            && !self.scopes.iter().any(|s| s == "user:inference")
    }
}

pub fn account_key(provider: Provider, email: &str, org: &str) -> String {
    if org.is_empty() {
        format!("{provider}:{email}")
    } else {
        format!("{provider}:{email}#{org}")
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct IndexEntry {
    pub email: String,
    pub label: String,
    pub org: String,
    pub org_name: String,
    pub provider: Provider,
    pub added_at: String,
}

impl IndexEntry {
    pub fn key(&self) -> String {
        account_key(self.provider, &self.email, &self.org)
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Index {
    pub version: u32,
    pub accounts: Vec<IndexEntry>,
}
impl Default for Index {
    fn default() -> Self {
        Self {
            version: 1,
            accounts: Vec::new(),
        }
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Window {
    pub key: String,
    pub group: String,
    pub label: String,
    pub percent: f64,
    pub known: bool,
    pub resets_at: String,
    pub severity: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct LoginHealth {
    pub state: String,
    pub message: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct AccountView {
    pub key: String,
    pub provider: Provider,
    pub email: String,
    pub label: String,
    pub org: String,
    pub org_name: String,
    pub tier: String,
    pub active: bool,
    pub read_only: bool,
    pub windows: Vec<Window>,
    pub fetched_at: i64,
    pub stale: String,
    pub error: String,
    pub login: LoginHealth,
    pub recommended: bool,
    pub why: String,
    pub all_spent: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub extra_usage: Option<Value>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Snapshot {
    pub accounts: Vec<AccountView>,
    pub empty: bool,
    pub generated_at: String,
    pub warnings: Vec<String>,
}
