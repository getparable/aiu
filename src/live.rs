use crate::{
    config::Config,
    model::{Provider, Record},
    storage::{lock, read_json, write_json},
};
use anyhow::Result;
use base64::Engine;
use serde_json::{Map, Value, json};
use std::path::PathBuf;

fn claude_path(c: &Config) -> PathBuf {
    c.claude_dir.join(".credentials.json")
}
fn read_doc(path: &std::path::Path) -> Result<Option<Map<String, Value>>> {
    read_json(path)
}
#[cfg(target_os = "macos")]
fn claude_keychain_account(c: &Config) -> Result<Option<String>> {
    use security_framework::item::{ItemClass, ItemSearchOptions};
    let mut q = ItemSearchOptions::new();
    q.class(ItemClass::generic_password())
        .service(&c.claude_service)
        .load_attributes(true)
        .load_data(true)
        .limit(1);
    let results = match q.search() {
        Ok(results) => results,
        Err(error) if error.code() == -25300 => return Ok(None),
        Err(error) => return Err(error.into()),
    };
    Ok(results
        .first()
        .and_then(|x| x.simplify_dict())
        .and_then(|m| m.get("acct").or_else(|| m.get("account")).cloned()))
}
fn read_claude_doc(c: &Config) -> Result<Option<Map<String, Value>>> {
    #[cfg(target_os = "macos")]
    if c.claude_use_keychain {
        use security_framework::passwords::get_generic_password;
        let Some(account) = claude_keychain_account(c)? else {
            return Ok(None);
        };
        return match get_generic_password(&c.claude_service, &account) {
            Ok(bytes) => Ok(Some(serde_json::from_slice(&bytes)?)),
            Err(e) if e.code() == -25300 => Ok(None),
            Err(e) => Err(e.into()),
        };
    }
    read_doc(&claude_path(c))
}

fn write_claude_doc(c: &Config, doc: &Map<String, Value>) -> Result<()> {
    #[cfg(target_os = "macos")]
    if c.claude_use_keychain {
        let account = claude_keychain_account(c)?
            .unwrap_or_else(|| std::env::var("USER").unwrap_or_else(|_| "claude-code-user".into()));
        security_framework::passwords::set_generic_password(
            &c.claude_service,
            &account,
            &serde_json::to_vec(doc)?,
        )?;
        return Ok(());
    }
    write_json(&claude_path(c), doc)
}
fn strv(m: &Map<String, Value>, k: &str) -> String {
    m.get(k)
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned()
}
fn i64v(m: &Map<String, Value>, k: &str) -> i64 {
    m.get(k).and_then(Value::as_i64).unwrap_or(0)
}

pub fn read(c: &Config, provider: Provider) -> Result<Option<Record>> {
    let lock_path = match provider {
        Provider::Claude => c.claude_dir.join(".aiu-credentials.lock"),
        Provider::Codex => c.codex_home.join(".aiu-auth.lock"),
    };
    let _g = lock(&lock_path)?;
    read_unlocked(c, provider)
}

fn read_unlocked(c: &Config, provider: Provider) -> Result<Option<Record>> {
    match provider {
        Provider::Claude => {
            let Some(doc) = read_claude_doc(c)? else {
                return Ok(None);
            };
            let Some(o) = doc.get("claudeAiOauth").and_then(Value::as_object) else {
                return Ok(None);
            };
            let token = strv(o, "accessToken");
            if token.is_empty() {
                return Ok(None);
            };
            let mut r = Record {
                provider,
                email: String::new(),
                access_token: token,
                refresh_token: strv(o, "refreshToken"),
                expires_at: i64v(o, "expiresAt"),
                refresh_token_expires_at: i64v(o, "refreshTokenExpiresAt"),
                subscription_type: strv(o, "subscriptionType"),
                rate_limit_tier: strv(o, "rateLimitTier"),
                ..Default::default()
            };
            if let Some(a) = o.get("scopes").and_then(Value::as_array) {
                r.scopes = a
                    .iter()
                    .filter_map(Value::as_str)
                    .map(str::to_owned)
                    .collect();
            }
            if let Some(global) = read_json::<Map<String, Value>>(&c.claude_global_config)?
                && let Some(a) = global.get("oauthAccount").and_then(Value::as_object)
            {
                r.email = strv(a, "emailAddress");
                r.org_uuid = strv(a, "organizationUuid");
                r.org_name = strv(a, "organizationName");
                r.profile = a.clone();
            }
            Ok(Some(r))
        }
        Provider::Codex => {
            let Some(doc) = read_doc(&c.codex_auth_file())? else {
                return Ok(None);
            };
            if doc
                .get("auth_mode")
                .and_then(Value::as_str)
                .map(|x| x != "chatgpt")
                .unwrap_or(false)
            {
                return Ok(None);
            }
            let Some(t) = doc.get("tokens").and_then(Value::as_object) else {
                return Ok(None);
            };
            let at = strv(t, "access_token");
            if at.is_empty() {
                return Ok(None);
            };
            let id = strv(t, "id_token");
            let mut r = codex_identity(&id, &strv(t, "account_id"));
            r.provider = Provider::Codex;
            r.access_token = at;
            r.refresh_token = strv(t, "refresh_token");
            r.id_token = id;
            r.last_refresh = chrono::DateTime::parse_from_rfc3339(&strv(&doc, "last_refresh"))
                .map(|x| x.timestamp_millis())
                .unwrap_or(0);
            r.expires_at = jwt_expiry_ms(&r.access_token);
            if r.expires_at == 0 && r.last_refresh > 0 {
                r.expires_at = r.last_refresh.saturating_add(8 * 24 * 60 * 60 * 1000);
            }
            Ok(Some(r))
        }
    }
}

pub fn write(c: &Config, r: &Record) -> Result<()> {
    let lock_path = match r.provider {
        Provider::Claude => c.claude_dir.join(".aiu-credentials.lock"),
        Provider::Codex => c.codex_home.join(".aiu-auth.lock"),
    };
    let _g = lock(&lock_path)?;
    write_unlocked(c, r, true)
}

fn write_unlocked(c: &Config, r: &Record, update_profile: bool) -> Result<()> {
    match r.provider {
        Provider::Claude => {
            let mut doc = read_claude_doc(c)?.unwrap_or_default();
            let mut o = doc
                .remove("claudeAiOauth")
                .and_then(|v| v.as_object().cloned())
                .unwrap_or_default();
            for (k, v) in [
                ("accessToken", json!(r.access_token)),
                ("refreshToken", json!(r.refresh_token)),
                ("idToken", json!(r.id_token)),
                ("accountId", json!(r.account_id)),
                ("expiresAt", json!(r.expires_at)),
                ("refreshTokenExpiresAt", json!(r.refresh_token_expires_at)),
                ("subscriptionType", json!(r.subscription_type)),
                ("rateLimitTier", json!(r.rate_limit_tier)),
            ] {
                o.insert(k.into(), v);
            }
            o.insert("scopes".into(), json!(r.scopes));
            doc.insert("claudeAiOauth".into(), Value::Object(o));
            write_claude_doc(c, &doc)?;
            if update_profile {
                update_claude_profile(c, r)?;
            }
            Ok(())
        }
        Provider::Codex => {
            let path = c.codex_auth_file();
            let mut doc = read_doc(&path)?.unwrap_or_default();
            let mut t = doc
                .remove("tokens")
                .and_then(|v| v.as_object().cloned())
                .unwrap_or_default();
            t.insert("access_token".into(), json!(r.access_token));
            t.insert("refresh_token".into(), json!(r.refresh_token));
            t.insert("id_token".into(), json!(r.id_token));
            t.insert("account_id".into(), json!(r.account_id));
            doc.insert("auth_mode".into(), json!("chatgpt"));
            doc.insert("tokens".into(), Value::Object(t));
            let millis = if r.last_refresh > 0 {
                r.last_refresh
            } else {
                chrono::Utc::now().timestamp_millis()
            };
            doc.insert(
                "last_refresh".into(),
                json!(
                    chrono::DateTime::<chrono::Utc>::from_timestamp_millis(millis)
                        .unwrap_or_else(chrono::Utc::now)
                        .to_rfc3339_opts(chrono::SecondsFormat::Millis, true)
                ),
            );
            write_json(&path, &doc)
        }
    }
}

pub fn hand_back(c: &Config, spent: &str, r: &Record) -> Result<bool> {
    if spent.is_empty() {
        return Ok(false);
    }
    let lock_path = match r.provider {
        Provider::Claude => c.claude_dir.join(".aiu-credentials.lock"),
        Provider::Codex => c.codex_home.join(".aiu-auth.lock"),
    };
    let _g = lock(&lock_path)?;
    let live = read_unlocked(c, r.provider)?;
    if live.as_ref().map(|x| x.refresh_token.as_str()) != Some(spent) {
        return Ok(false);
    };
    write_unlocked(c, r, false)?;
    Ok(true)
}
pub fn jwt_claims(token: &str) -> Value {
    let p = token.split('.').nth(1).unwrap_or("");
    let Ok(b) = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(p) else {
        return Value::Null;
    };
    serde_json::from_slice(&b).unwrap_or(Value::Null)
}
pub fn jwt_expiry_ms(token: &str) -> i64 {
    jwt_claims(token)
        .get("exp")
        .and_then(Value::as_i64)
        .map(|x| x.saturating_mul(1000))
        .unwrap_or(0)
}
pub fn codex_identity(id_token: &str, account_id: &str) -> Record {
    let claims = jwt_claims(id_token);
    let auth = claims
        .get("https://api.openai.com/auth")
        .and_then(Value::as_object);
    let profile = claims
        .get("https://api.openai.com/profile")
        .and_then(Value::as_object);
    let email = claims
        .get("email")
        .and_then(Value::as_str)
        .or_else(|| profile.and_then(|x| x.get("email").and_then(Value::as_str)))
        .unwrap_or_default();
    let mut r = Record {
        provider: Provider::Codex,
        email: email.into(),
        account_id: account_id.into(),
        org_uuid: account_id.into(),
        ..Default::default()
    };
    if let Some(a) = auth {
        if r.account_id.is_empty() {
            r.account_id = strv(a, "chatgpt_account_id");
        }
        r.plan_type = strv(a, "chatgpt_plan_type");
        r.user_id = strv(a, "chatgpt_user_id");
    }
    r
}

fn update_claude_profile(c: &Config, r: &Record) -> Result<()> {
    let Some(mut cfg) = read_json::<Map<String, Value>>(&c.claude_global_config)? else {
        return Ok(());
    };
    let mut acct = cfg
        .remove("oauthAccount")
        .and_then(|v| v.as_object().cloned())
        .unwrap_or_default();
    for k in [
        "accountUuid",
        "emailAddress",
        "displayName",
        "fullName",
        "organizationUuid",
        "organizationName",
        "organizationType",
        "organizationRole",
        "organizationRateLimitTier",
        "billingType",
    ] {
        if let Some(v) = r.profile.get(k) {
            acct.insert(k.into(), v.clone());
        } else {
            acct.remove(k);
        }
    }
    if !r.email.is_empty() {
        acct.insert("emailAddress".into(), json!(r.email));
    }
    if !r.org_uuid.is_empty() {
        acct.insert("organizationUuid".into(), json!(r.org_uuid));
    }
    if !r.org_name.is_empty() {
        acct.insert("organizationName".into(), json!(r.org_name));
    }
    cfg.insert("oauthAccount".into(), Value::Object(acct));
    write_json(&c.claude_global_config, &cfg)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::StoreMode;
    use std::fs;
    use tempfile::tempdir;

    fn config() -> (tempfile::TempDir, Config) {
        let d = tempdir().unwrap();
        let mut c = Config::for_home(d.path().into(), d.path().join("cfg"));
        c.store_mode = StoreMode::File;
        c.claude_use_keychain = false;
        (d, c)
    }
    fn claude_record() -> Record {
        Record {
            provider: Provider::Claude,
            email: "new@example.com".into(),
            refresh_token: "new-refresh".into(),
            access_token: "new-access".into(),
            expires_at: 123,
            org_uuid: "org-new".into(),
            org_name: "New Org".into(),
            profile: {
                let mut m = Map::new();
                m.insert("displayName".into(), json!("New"));
                m
            },
            ..Default::default()
        }
    }
    #[test]
    fn jwt() {
        let t = "eyJhbGciOiJub25lIn0.eyJleHAiOjE3MDAwMDAwMDB9.x";
        assert_eq!(jwt_expiry_ms(t), 1700000000000);
    }

    #[test]
    fn claude_write_preserves_unknown_clears_old_and_updates_global_profile() {
        let (_d, c) = config();
        fs::create_dir_all(&c.claude_dir).unwrap();
        fs::write(c.claude_dir.join(".credentials.json"), br#"{"mcpOAuth":{"keep":true},"claudeAiOauth":{"accessToken":"old","refreshToken":"old-r","idToken":"old-id","accountId":"old-account","scopes":["old"]}}"#).unwrap();
        fs::write(&c.claude_global_config, br#"{"settings":7,"oauthAccount":{"emailAddress":"old@example.com","organizationUuid":"old-org","oldField":"keep"}}"#).unwrap();
        write(&c, &claude_record()).unwrap();
        let doc: Map<String, Value> = read_json(&c.claude_dir.join(".credentials.json"))
            .unwrap()
            .unwrap();
        let o = doc["claudeAiOauth"].as_object().unwrap();
        assert_eq!(doc["mcpOAuth"]["keep"], true);
        assert_eq!(o["refreshToken"], "new-refresh");
        assert_eq!(o["idToken"], "");
        assert_eq!(o["accountId"], "");
        let global: Map<String, Value> = read_json(&c.claude_global_config).unwrap().unwrap();
        assert_eq!(global["settings"], 7);
        assert_eq!(global["oauthAccount"]["emailAddress"], "new@example.com");
        assert_eq!(global["oauthAccount"]["oldField"], "keep");
    }

    #[test]
    fn handback_requires_nonempty_matching_refresh() {
        let (_d, c) = config();
        fs::create_dir_all(&c.claude_dir).unwrap();
        fs::write(
            c.claude_dir.join(".credentials.json"),
            br#"{"claudeAiOauth":{"accessToken":"a","refreshToken":"live"}}"#,
        )
        .unwrap();
        let r = claude_record();
        assert!(!hand_back(&c, "", &r).unwrap());
        assert!(!hand_back(&c, "wrong", &r).unwrap());
        assert!(hand_back(&c, "live", &r).unwrap());
        let live = read(&c, Provider::Claude).unwrap().unwrap();
        assert_eq!(live.access_token, "new-access");
    }

    #[test]
    fn codex_auth_mode_and_identity_org() {
        let id = "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6IngifQ.x";
        let r = codex_identity(id, "acct");
        assert_eq!(r.provider, Provider::Codex);
        assert_eq!(r.org_uuid, "acct");
        let (_d, c) = config();
        fs::create_dir_all(&c.codex_home).unwrap();
        fs::write(
            c.codex_auth_file(),
            br#"{"auth_mode":"api_key","tokens":{"access_token":"a"}}"#,
        )
        .unwrap();
        assert!(read(&c, Provider::Codex).unwrap().is_none());
    }
}
