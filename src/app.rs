use crate::{
    config::Config,
    live,
    model::*,
    provider::{Api, redact},
    storage::{self, Store},
    usage,
};
use anyhow::{Context, Result, bail};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{collections::BTreeMap, time::Duration};

const SPACING: i64 = 300_000;
const REFRESH_MARGIN: i64 = 300_000;

pub struct App {
    pub config: Config,
    pub api: Api,
    store: Store,
}

#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct CachedUsage {
    usage: Option<Value>,
    fetched_at: i64,
    attempted_at: i64,
    limited_until: i64,
    strikes: u32,
    last_limited_at: i64,
    last_error: String,
    needs_login: bool,
    token_updated_at: i64,
}
type Cache = BTreeMap<String, CachedUsage>;

#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
struct ProfileCache {
    profile: serde_json::Map<String, Value>,
    attempted_at: i64,
}

#[derive(Clone, Debug, Serialize)]
pub struct LiveView {
    pub provider: Provider,
    pub email: String,
    pub org: String,
    pub verified: bool,
}

impl App {
    pub fn new(config: Config) -> Result<Self> {
        Ok(Self {
            api: Api::new(config.clone())?,
            store: Store::new(config.clone()),
            config,
        })
    }
    fn guard(&self) -> Result<storage::FileLock> {
        storage::lock(&self.config.dir.join("app.lock"))
    }

    fn identify(&self, record: &mut Record) -> Result<()> {
        if record.provider == Provider::Claude {
            let profile = self.api.profile(&record.access_token)?;
            apply_profile(record, profile)?;
        } else {
            let identity = live::codex_identity(&record.id_token, &record.account_id);
            record.email = identity.email;
            record.account_id = identity.account_id;
            record.org_uuid = record.account_id.clone();
            record.plan_type = identity.plan_type;
            record.user_id = identity.user_id;
            if record.email.is_empty() {
                bail!("Codex login has no email in its ID token; run codex login again");
            }
        }
        Ok(())
    }

    fn save_account(&self, mut record: Record, label: Option<&str>) -> Result<IndexEntry> {
        let mut index = self.store.load_index()?;
        let previous = index
            .accounts
            .iter()
            .find(|a| a.key() == record.key())
            .cloned();
        let label = label
            .filter(|s| !s.trim().is_empty())
            .map(str::to_owned)
            .or_else(|| previous.as_ref().map(|e| e.label.clone()))
            .unwrap_or_else(|| auto_label(&index, &record));
        if index
            .accounts
            .iter()
            .any(|a| a.provider == record.provider && a.label == label && a.key() != record.key())
        {
            bail!(
                "label {label:?} is already used for {}; choose another --label",
                record.provider
            );
        }
        record.label = label.clone();
        let now = Utc::now();
        record.updated_at = now.timestamp_millis();
        if let Some(old) = self.store.get(&record.key())? {
            record.captured_at = old.captured_at;
            if record.subscription_type.is_empty() {
                record.subscription_type = old.subscription_type;
            }
            if record.rate_limit_tier.is_empty() {
                record.rate_limit_tier = old.rate_limit_tier;
            }
        }
        if record.captured_at == 0 {
            record.captured_at = record.updated_at;
        }
        let entry = IndexEntry {
            email: record.email.clone(),
            label,
            org: record.org_uuid.clone(),
            org_name: record.org_name.clone(),
            provider: record.provider,
            added_at: previous
                .as_ref()
                .map(|e| e.added_at.clone())
                .unwrap_or_else(|| now.to_rfc3339()),
        };
        self.store.set(&record)?;
        if let Some(existing) = index.accounts.iter_mut().find(|e| e.key() == entry.key()) {
            *existing = entry.clone();
        } else {
            index.accounts.push(entry.clone());
        }
        self.store.save_index(&index)?;
        // A new login clears cached authentication failures, but never bypasses a 429.
        let mut cache: Cache = storage::read_json(&self.config.cache_file())?.unwrap_or_default();
        if let Some(cached) = cache.get_mut(&entry.key()) {
            cached.needs_login = false;
            cached.last_error.clear();
            if cached.limited_until <= now.timestamp_millis() {
                cached.attempted_at = 0;
            }
            storage::write_json(&self.config.cache_file(), &cache)?;
        }
        Ok(entry)
    }

    pub fn add(&self, provider: Provider, label: Option<&str>) -> Result<IndexEntry> {
        let _guard = self.guard()?;
        let mut record = live::read(&self.config, provider)?.with_context(|| {
            format!(
                "no {} login found; sign in there first or run aiu login",
                provider.client()
            )
        })?;
        if record.is_expired(Utc::now().timestamp_millis(), REFRESH_MARGIN) {
            let refreshed = self.api.refresh(&record)?;
            live::hand_back(&self.config, &record.refresh_token, &refreshed)?;
            record = refreshed;
        }
        self.identify(&mut record)?;
        record.source = format!("{}-cli", provider);
        self.save_account(record, label)
    }

    pub fn save_login(&self, mut record: Record, label: Option<&str>) -> Result<IndexEntry> {
        let _guard = self.guard()?;
        self.identify(&mut record)?;
        record.source = "oauth-login".into();
        self.save_account(record, label)
    }

    fn verified_live(
        &self,
        provider: Provider,
        records: &[Record],
        verify: bool,
    ) -> Result<Option<Record>> {
        let Some(mut current) = live::read(&self.config, provider)? else {
            return Ok(None);
        };
        if provider == Provider::Codex {
            return Ok(Some(current));
        }
        if let Some(known) = records.iter().find(|r| {
            r.provider == provider
                && !r.missing
                && ((!current.refresh_token.is_empty() && current.refresh_token == r.refresh_token)
                    || (!current.access_token.is_empty() && current.access_token == r.access_token))
        }) {
            current.email = known.email.clone();
            current.org_uuid = known.org_uuid.clone();
            current.org_name = known.org_name.clone();
            current.profile = known.profile.clone();
            return Ok(Some(current));
        }
        // Claude's cached .claude.json email is a hint, never proof for token adoption.
        current.email.clear();
        current.org_uuid.clear();
        if verify && !current.is_expired(Utc::now().timestamp_millis(), 0) {
            let path = self.config.dir.join("profile-cache.json");
            let mut cache: BTreeMap<String, ProfileCache> =
                storage::read_json(&path)?.unwrap_or_default();
            let key = format!("{:x}", Sha256::digest(current.access_token.as_bytes()));
            let now = Utc::now().timestamp_millis();
            let cached = cache.entry(key).or_default();
            if cached.attempted_at == 0 || now - cached.attempted_at >= 600_000 {
                cached.attempted_at = now;
                match self.api.profile(&current.access_token) {
                    Ok(profile) => cached.profile = profile,
                    Err(error) => {
                        storage::write_json(&path, &cache)?;
                        return Err(error.context("could not verify the active Claude account"));
                    }
                }
            }
            if !cached.profile.is_empty() {
                apply_profile(&mut current, cached.profile.clone())?;
            }
            storage::write_json(&path, &cache)?;
        }
        Ok(Some(current))
    }

    fn sync_records(
        &self,
        records: &mut [Record],
        provider: Option<Provider>,
        adopt: bool,
        warnings: &mut Vec<String>,
    ) -> Vec<Record> {
        let mut active = Vec::new();
        for p in [Provider::Claude, Provider::Codex] {
            if provider.is_some_and(|selected| selected != p) {
                continue;
            }
            match self.verified_live(p, records, adopt) {
                Ok(Some(current)) => {
                    if adopt && !current.email.is_empty() {
                        for saved in records
                            .iter_mut()
                            .filter(|r| r.provider == p && r.key() == current.key() && !r.missing)
                        {
                            let newer = match p {
                                Provider::Claude => current.expires_at > saved.expires_at,
                                Provider::Codex => current.last_refresh > saved.last_refresh,
                            };
                            if newer && current.access_token != saved.access_token {
                                let mut next = current.clone();
                                next.label = saved.label.clone();
                                next.source = saved.source.clone();
                                next.captured_at = saved.captured_at;
                                next.updated_at = Utc::now().timestamp_millis();
                                if next.refresh_token.is_empty() {
                                    next.refresh_token = saved.refresh_token.clone();
                                }
                                match self.store.set(&next) {
                                    Ok(()) => *saved = next,
                                    Err(e) => warnings.push(redact(&e.to_string())),
                                }
                            }
                        }
                    }
                    active.push(current);
                }
                Ok(None) => {}
                Err(e) => warnings.push(redact(&e.to_string())),
            }
        }
        active
    }

    fn ensure_fresh(
        &self,
        record: &mut Record,
        force: bool,
        warnings: &mut Vec<String>,
    ) -> Result<()> {
        if !force && !record.is_expired(Utc::now().timestamp_millis(), REFRESH_MARGIN) {
            return Ok(());
        }
        let spent = record.refresh_token.clone();
        let mut next = self.api.refresh(record)?;
        next.updated_at = Utc::now().timestamp_millis();
        // Persist the rotated pair before attempting hand-back so it cannot be lost.
        self.store.set(&next)?;
        if let Err(e) = live::hand_back(&self.config, &spent, &next) {
            warnings.push(redact(&format!(
                "refreshed login saved, but could not update {}: {e}",
                record.provider.client()
            )));
        }
        *record = next;
        Ok(())
    }

    pub fn status(&self, provider: Option<Provider>, no_sync: bool) -> Result<Snapshot> {
        let guard = self.guard();
        let may_fetch = guard.is_ok();
        let mut warnings = Vec::new();
        if !may_fetch {
            warnings.push("another AIU process is busy; showing cached usage".into());
        }
        let mut records = self.store.records()?;
        let empty = records.is_empty();
        if empty {
            return Ok(Snapshot {
                empty: true,
                generated_at: Utc::now().to_rfc3339(),
                warnings,
                ..Default::default()
            });
        }
        records.retain(|r| provider.is_none_or(|p| r.provider == p));
        let active =
            self.sync_records(&mut records, provider, !no_sync && may_fetch, &mut warnings);
        let mut cache: Cache = storage::read_json(&self.config.cache_file())?.unwrap_or_default();
        let mut accounts = Vec::new();
        let mut fetched = false;
        for mut record in records {
            let key = record.key();
            let mut cached = cache.get(&key).cloned().unwrap_or_default();
            let now = Utc::now().timestamp_millis();
            let spacing = if cached.last_limited_at > 0 && now - cached.last_limited_at < 3_600_000
            {
                SPACING * 2
            } else {
                SPACING
            };
            if !record.missing
                && may_fetch
                && now >= cached.limited_until
                && (cached.attempted_at == 0 || now - cached.attempted_at >= spacing)
            {
                if fetched {
                    std::thread::sleep(Duration::from_millis(300));
                }
                fetched = true;
                cached.attempted_at = Utc::now().timestamp_millis();
                cache.insert(key.clone(), cached.clone());
                // Claim before I/O: a crash still consumes the shared request slot.
                storage::write_json(&self.config.cache_file(), &cache)?;
                let response = (|| {
                    self.ensure_fresh(&mut record, false, &mut warnings)?;
                    let mut response = self.api.usage(&record)?;
                    if response.status == 401 {
                        self.ensure_fresh(&mut record, true, &mut warnings)?;
                        response = self.api.usage(&record)?;
                    }
                    Ok::<_, anyhow::Error>(response)
                })();
                let now = Utc::now().timestamp_millis();
                match response {
                    Ok(response) if (200..300).contains(&response.status) => {
                        if let Some(plan) = response.body.get("plan_type").and_then(Value::as_str) {
                            record.plan_type = plan.to_owned();
                            self.store.set(&record)?;
                        }
                        cached.usage = Some(response.body);
                        cached.fetched_at = now;
                        cached.limited_until = 0;
                        cached.strikes = 0;
                        cached.last_error.clear();
                        cached.needs_login = false;
                    }
                    Ok(response) if response.status == 429 => {
                        cached.strikes = cached.strikes.saturating_add(1);
                        let cooldown = (600_000i64
                            * (1i64 << cached.strikes.saturating_sub(1).min(3)))
                        .min(3_600_000);
                        let retry = response
                            .retry_after
                            .unwrap_or(0)
                            .min(i64::MAX as u64 / 1000) as i64
                            * 1000;
                        cached.limited_until = now.saturating_add(cooldown.max(retry));
                        cached.last_limited_at = now;
                        cached.last_error = "usage endpoint throttled requests; this is separate from subscription quota".into();
                    }
                    Ok(response) => {
                        cached.last_error =
                            format!("usage endpoint returned HTTP {}", response.status);
                        cached.needs_login = response.status == 401 || response.status == 403;
                    }
                    Err(error) => {
                        cached.last_error = redact(&error.to_string());
                        let message = cached.last_error.to_ascii_lowercase();
                        cached.needs_login = error
                            .downcast_ref::<crate::provider::LoginRejected>()
                            .is_some()
                            || message.contains("invalid_grant")
                            || message.contains("refresh token")
                            || message.contains("refresh failed (401");
                    }
                }
                cached.token_updated_at = record.updated_at;
                cache.insert(key.clone(), cached.clone());
                storage::write_json(&self.config.cache_file(), &cache)?;
            }
            let mut view = view_of(
                &record,
                active.iter().any(|a| !a.email.is_empty() && a.key() == key),
            );
            view.login =
                usage::login_health(&record, cached.needs_login, Utc::now().timestamp_millis());
            if record.missing {
                view.error = "token missing from storage; sign in again".into();
            } else if cached.needs_login {
                view.error = cached.last_error.clone();
            } else if let Some(raw) = &cached.usage {
                // Relative reset durations are anchored to the fetch, not each redraw.
                let fetched_at =
                    DateTime::from_timestamp_millis(cached.fetched_at).unwrap_or_else(Utc::now);
                view.windows = usage::normalize_windows(raw, fetched_at);
                view.fetched_at = cached.fetched_at;
                view.extra_usage = raw.get("extra_usage").cloned().filter(|v| !v.is_null());
                view.stale = cached.last_error.clone();
            } else {
                view.error = if cached.last_error.is_empty() {
                    "no usage data yet".into()
                } else {
                    cached.last_error.clone()
                };
            }
            if cached.limited_until > Utc::now().timestamp_millis() {
                let retry = DateTime::from_timestamp_millis(cached.limited_until)
                    .map(|t| t.to_rfc3339())
                    .unwrap_or_default();
                let note = format!("requests throttled; next attempt after {retry}");
                if view.windows.is_empty() {
                    view.error = note;
                } else {
                    view.stale = note;
                }
            }
            accounts.push(view);
        }
        usage::recommend(&mut accounts, Utc::now());
        Ok(Snapshot {
            accounts,
            empty,
            generated_at: Utc::now().to_rfc3339(),
            warnings,
        })
    }

    pub fn list(&self, provider: Option<Provider>) -> Result<Vec<AccountView>> {
        let _guard = self.guard()?;
        let mut records = self.store.records()?;
        let active = self.sync_records(&mut records, provider, false, &mut Vec::new());
        Ok(records
            .iter()
            .filter(|r| provider.is_none_or(|p| p == r.provider))
            .map(|r| {
                view_of(
                    r,
                    active
                        .iter()
                        .any(|a| !a.email.is_empty() && a.key() == r.key()),
                )
            })
            .collect())
    }

    pub fn sync(&self, provider: Option<Provider>) -> Result<Vec<String>> {
        let _guard = self.guard()?;
        let mut records = self.store.records()?;
        let mut warnings = Vec::new();
        self.sync_records(&mut records, provider, true, &mut warnings);
        Ok(warnings)
    }

    pub fn whoami(&self, provider: Option<Provider>) -> Result<Vec<LiveView>> {
        let _guard = self.guard()?;
        let records = self.store.records()?;
        let mut out = Vec::new();
        for p in [Provider::Claude, Provider::Codex] {
            if provider.is_some_and(|selected| selected != p) {
                continue;
            }
            let current = self.verified_live(p, &records, true)?;
            out.push(LiveView {
                provider: p,
                verified: current.as_ref().is_some_and(|r| !r.email.is_empty()),
                email: current
                    .as_ref()
                    .map(|r| r.email.clone())
                    .unwrap_or_default(),
                org: current.map(|r| r.org_uuid).unwrap_or_default(),
            });
        }
        Ok(out)
    }

    pub fn switch(&self, target: &str, provider: Option<Provider>) -> Result<Vec<String>> {
        let _guard = self.guard()?;
        let mut records = self.store.records()?;
        let index = self.store.load_index()?;
        let entry = find_account(&index, target, provider)?;
        let mut warnings = Vec::new();
        let current = self.sync_records(&mut records, Some(entry.provider), true, &mut warnings);
        if current.iter().any(|live| {
            !records
                .iter()
                .any(|r| !r.missing && !live.email.is_empty() && r.key() == live.key())
        }) {
            warnings.push("The previous CLI login was not tracked in AIU and has been replaced. Add a login before switching to keep a copy.".into());
        }
        let mut record = records
            .into_iter()
            .find(|r| r.key() == entry.key() && !r.missing)
            .context("stored token missing; sign in again for this account")?;
        if record.is_read_only() {
            bail!("this Claude login is read-only; sign in with full scopes before switching");
        }
        self.ensure_fresh(&mut record, false, &mut warnings)?;
        live::write(&self.config, &record)?;
        Ok(warnings)
    }

    pub fn remove(&self, target: &str, provider: Option<Provider>) -> Result<IndexEntry> {
        let _guard = self.guard()?;
        let mut index = self.store.load_index()?;
        let entry = find_account(&index, target, provider)?.clone();
        self.store.delete(&entry.key())?;
        index.accounts.retain(|a| a.key() != entry.key());
        self.store.save_index(&index)?;
        let mut cache: Cache = storage::read_json(&self.config.cache_file())?.unwrap_or_default();
        cache.remove(&entry.key());
        storage::write_json(&self.config.cache_file(), &cache)?;
        Ok(entry)
    }
}

fn apply_profile(record: &mut Record, profile: serde_json::Map<String, Value>) -> Result<()> {
    record.email = profile
        .get("emailAddress")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .into();
    if record.email.is_empty() {
        bail!("profile response has no email address");
    }
    record.org_uuid = profile
        .get("organizationUuid")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .into();
    record.org_name = profile
        .get("organizationName")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .into();
    record.profile = profile;
    Ok(())
}

fn view_of(record: &Record, active: bool) -> AccountView {
    AccountView {
        key: record.key(),
        provider: record.provider,
        email: record.email.clone(),
        label: record.label.clone(),
        org: record.org_uuid.clone(),
        org_name: record.org_name.clone(),
        tier: usage::tier_label(record),
        active,
        read_only: record.is_read_only(),
        login: usage::login_health(record, false, Utc::now().timestamp_millis()),
        ..Default::default()
    }
}

pub fn find_account<'a>(
    index: &'a Index,
    target: &str,
    mut provider: Option<Provider>,
) -> Result<&'a IndexEntry> {
    let mut name = target;
    if let Some((prefix, rest)) = target.split_once(':') {
        let parsed = match prefix.to_ascii_lowercase().as_str() {
            "claude" => Some(Provider::Claude),
            "codex" => Some(Provider::Codex),
            _ => None,
        };
        if let Some(p) = parsed {
            provider = Some(p);
            name = rest;
        }
    }
    let entries: Vec<_> = index
        .accounts
        .iter()
        .filter(|e| provider.is_none_or(|p| p == e.provider))
        .collect();
    let matches: Vec<_> = if let Some((email, org)) = name.split_once('#') {
        entries
            .into_iter()
            .filter(|e| e.email == email && e.org == org)
            .collect()
    } else {
        let emails: Vec<_> = entries
            .iter()
            .copied()
            .filter(|e| e.email == name)
            .collect();
        if emails.is_empty() {
            entries.into_iter().filter(|e| e.label == name).collect()
        } else {
            emails
        }
    };
    match matches.as_slice() {
        [entry] => Ok(*entry),
        [] => bail!("no tracked account matches {target:?}"),
        _ => bail!(
            "{target:?} matches several accounts; use one of: {}",
            matches
                .iter()
                .map(|e| e.key())
                .collect::<Vec<_>>()
                .join(", ")
        ),
    }
}

fn auto_label(index: &Index, record: &Record) -> String {
    let base = record
        .email
        .split('@')
        .next()
        .filter(|s| !s.is_empty())
        .unwrap_or("account");
    let taken = |label: &str| {
        index
            .accounts
            .iter()
            .any(|a| a.provider == record.provider && a.label == label && a.key() != record.key())
    };
    if !taken(base) {
        return base.into();
    }
    let org = record.org_name.to_ascii_lowercase();
    let candidate = if org.is_empty() || org.ends_with("'s organization") {
        format!("{base}-personal")
    } else {
        org.split(|c: char| !c.is_ascii_alphanumeric())
            .filter(|s| !s.is_empty())
            .collect::<Vec<_>>()
            .join("-")
    };
    let candidate = if candidate.is_empty() {
        format!("{base}-account")
    } else {
        candidate
    };
    if !taken(&candidate) {
        return candidate;
    }
    for n in 2.. {
        let label = format!("{candidate}-{n}");
        if !taken(&label) {
            return label;
        }
    }
    unreachable!()
}
