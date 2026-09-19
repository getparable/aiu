use aiu::{
    app::App,
    config::{Config, StoreMode},
    model::{Index, IndexEntry, Provider, Record},
    storage::Store,
};
use base64::Engine;
use serde_json::json;
use std::{fs, sync::mpsc, thread, time::Duration};
use tempfile::TempDir;

fn store() -> (TempDir, Store) {
    let dir = TempDir::new().unwrap();
    let mut config = Config::for_home(dir.path().into(), dir.path().join("cfg"));
    config.store_mode = StoreMode::File;
    (dir, Store::new(config))
}

fn config(dir: &TempDir) -> Config {
    let mut c = Config::for_home(dir.path().into(), dir.path().join("cfg"));
    c.store_mode = StoreMode::File;
    c.claude_use_keychain = false;
    c
}

fn claude_login(c: &Config, access: &str, refresh: &str, expires: i64) {
    fs::create_dir_all(&c.claude_dir).unwrap();
    fs::write(
        c.claude_dir.join(".credentials.json"),
        json!({
            "claudeAiOauth": {"accessToken": access, "refreshToken": refresh, "expiresAt": expires}
        })
        .to_string(),
    )
    .unwrap();
}

#[test]
fn pending_rotated_credentials_round_trip_in_protected_store() {
    let (_dir, store) = store();
    let record = Record {
        provider: Provider::Claude,
        access_token: "rotated-access".into(),
        refresh_token: "rotated-refresh".into(),
        ..Default::default()
    };
    store
        .set_pending(Provider::Claude, "spent-refresh", &record)
        .unwrap();
    let (spent, recovered) = store.pending(Provider::Claude).unwrap().unwrap();
    assert_eq!(spent, "spent-refresh");
    assert_eq!(recovered.access_token, "rotated-access");
    store
        .clear_pending(Provider::Claude, "spent-refresh")
        .unwrap();
    assert!(store.pending(Provider::Claude).unwrap().is_none());
}

#[test]
fn pending_slots_are_separate_per_provider() {
    let (_dir, store) = store();
    let claude = Record {
        provider: Provider::Claude,
        access_token: "c".into(),
        ..Default::default()
    };
    let codex = Record {
        provider: Provider::Codex,
        access_token: "x".into(),
        ..Default::default()
    };
    store
        .set_pending(Provider::Claude, "c-old", &claude)
        .unwrap();
    store.set_pending(Provider::Codex, "x-old", &codex).unwrap();
    assert_eq!(
        store
            .pending(Provider::Claude)
            .unwrap()
            .unwrap()
            .1
            .access_token,
        "c"
    );
    assert_eq!(
        store
            .pending(Provider::Codex)
            .unwrap()
            .unwrap()
            .1
            .access_token,
        "x"
    );
}

#[test]
fn account_refresh_ownership_serializes_same_account() {
    let (_dir, store) = store();
    let held = store.account_lock("claude:one@example.test").unwrap();
    let (tx, rx) = mpsc::channel();
    let path_store = Store::new({
        let mut config = Config::for_home(_dir.path().into(), _dir.path().join("cfg"));
        config.store_mode = StoreMode::File;
        config
    });
    thread::spawn(move || {
        let lock = path_store.account_lock("claude:one@example.test").unwrap();
        tx.send(()).unwrap();
        drop(lock);
    });
    assert!(rx.recv_timeout(Duration::from_millis(50)).is_err());
    drop(held);
    assert!(rx.recv_timeout(Duration::from_secs(2)).is_ok());
}

#[test]
fn add_keeps_rotated_pair_when_handback_fails_and_recovers_without_refresh() {
    let dir = TempDir::new().unwrap();
    let mut c = config(&dir);
    let cred = c.claude_dir.join(".credentials.json");
    claude_login(&c, "old-access", "spent-refresh", 1);
    let server = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let url = format!("http://{}", server.server_addr());
    c.claude_token_urls = vec![url.clone()];
    let worker = thread::spawn(move || {
        let mut req = server.recv().unwrap();
        let mut body = Vec::new();
        req.as_reader().read_to_end(&mut body).unwrap();
        fs::remove_file(&cred).unwrap();
        fs::create_dir(&cred).unwrap();
        req.respond(tiny_http::Response::from_string(
            r#"{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}"#,
        )).unwrap();
    });
    let app = App::new(c.clone()).unwrap();
    assert!(app.add(Provider::Claude, Some("recover")).is_err());
    worker.join().unwrap();
    let store = Store::new(c.clone());
    let pending = store
        .pending_matching(Provider::Claude, "old-access", "spent-refresh")
        .unwrap()
        .unwrap();
    assert_eq!(pending.1.access_token, "rotated-access");
    assert!(store.load_index().unwrap().accounts.is_empty());

    fs::remove_dir(c.claude_dir.join(".credentials.json")).unwrap();
    claude_login(&c, "old-access", "spent-refresh", 1);
    let profile_server = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let profile_url = format!("http://{}", profile_server.server_addr());
    let profile_worker = thread::spawn(move || {
        let req = profile_server.recv().unwrap();
        req.respond(tiny_http::Response::from_string(
            r#"{"emailAddress":"recover@example.test","organizationUuid":"org"}"#,
        ))
        .unwrap();
    });
    c.claude_profile_url = profile_url;
    c.claude_token_urls = vec!["http://127.0.0.1:9/unexpected-refresh".into()];
    let entry = App::new(c.clone())
        .unwrap()
        .add(Provider::Claude, Some("recover"))
        .unwrap();
    profile_worker.join().unwrap();
    assert_eq!(entry.email, "recover@example.test");
    assert_eq!(
        store.get(&entry.key()).unwrap().unwrap().access_token,
        "rotated-access"
    );
    assert!(store.pending(Provider::Claude).unwrap().is_none());
}

#[test]
fn sync_adoption_clears_stale_auth_but_preserves_future_throttle() {
    let dir = TempDir::new().unwrap();
    let mut c = config(&dir);
    c.codex_usage_url = "http://127.0.0.1:9/no-usage-call".into();
    fs::create_dir_all(&c.codex_home).unwrap();
    let payload = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(r#"{"email":"sync@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"org"}}"#);
    let id = format!("e30.{payload}.sig");
    let old = Record {
        provider: Provider::Codex,
        email: "sync@example.test".into(),
        org_uuid: "org".into(),
        account_id: "org".into(),
        access_token: "old-access".into(),
        refresh_token: "old-refresh".into(),
        last_refresh: 1,
        updated_at: 10,
        expires_at: chrono::Utc::now().timestamp_millis() + 3_600_000,
        ..Default::default()
    };
    let store = Store::new(c.clone());
    store.set(&old).unwrap();
    store
        .save_index(&Index {
            version: 1,
            accounts: vec![IndexEntry {
                email: old.email.clone(),
                org: old.org_uuid.clone(),
                provider: old.provider,
                label: "sync".into(),
                ..Default::default()
            }],
        })
        .unwrap();
    fs::write(c.codex_auth_file(), json!({"auth_mode":"chatgpt","tokens":{"access_token":"new-access","refresh_token":"new-refresh","id_token":id,"account_id":"org"},"last_refresh":"2026-01-01T00:00:00Z"}).to_string()).unwrap();
    let future = chrono::Utc::now().timestamp_millis() + 3_600_000;
    aiu::storage::write_json(&c.cache_file(), &json!({old.key(): {"needsLogin":true,"lastError":"401","tokenUpdatedAt":10,"limitedUntil":future,"strikes":3,"lastLimitedAt":123}})).unwrap();
    let out = App::new(c.clone())
        .unwrap()
        .status(Some(Provider::Codex), false)
        .unwrap();
    assert_ne!(out.accounts[0].login.state, "expired");
    let cache: serde_json::Value = aiu::storage::read_json(&c.cache_file()).unwrap().unwrap();
    assert_eq!(cache[old.key()]["limitedUntil"].as_i64().unwrap(), future);
    assert_eq!(cache[old.key()]["strikes"].as_u64().unwrap(), 3);
    assert!(!cache[old.key()]["needsLogin"].as_bool().unwrap());
    assert_eq!(
        store.get(&old.key()).unwrap().unwrap().access_token,
        "new-access"
    );
}

#[test]
fn add_keeps_pending_rotation_when_cli_changes_to_another_account() {
    let dir = TempDir::new().unwrap();
    let mut c = config(&dir);
    let cred = c.claude_dir.join(".credentials.json");
    claude_login(&c, "old-access", "spent-refresh", 1);
    let token_server = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let token_url = format!("http://{}", token_server.server_addr());
    c.claude_token_urls = vec![token_url];
    let worker = thread::spawn(move || {
        let req = token_server.recv().unwrap();
        fs::remove_file(&cred).unwrap();
        fs::write(&cred, json!({"claudeAiOauth":{"accessToken":"other-access","refreshToken":"other-refresh","expiresAt":9999999999999i64}}).to_string()).unwrap();
        req.respond(tiny_http::Response::from_string(r#"{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}"#)).unwrap();
    });
    let profile_server = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let profile_url = format!("http://{}", profile_server.server_addr());
    let profile_worker = thread::spawn(move || {
        let req = profile_server.recv().unwrap();
        req.respond(tiny_http::Response::from_string("profile unavailable").with_status_code(500))
            .unwrap();
    });
    c.claude_profile_url = profile_url;
    let app = App::new(c.clone()).unwrap();
    assert!(app.add(Provider::Claude, Some("changed")).is_err());
    worker.join().unwrap();
    profile_worker.join().unwrap();
    let store = Store::new(c.clone());
    let pending = store
        .pending_matching(Provider::Claude, "old-access", "spent-refresh")
        .unwrap()
        .unwrap();
    assert_eq!(pending.1.access_token, "rotated-access");
    assert!(store.load_index().unwrap().accounts.is_empty());
    let live = aiu::live::read(&c, Provider::Claude).unwrap().unwrap();
    assert_eq!(live.refresh_token, "other-refresh");
    assert_eq!(live.access_token, "other-access");
}

#[test]
fn pending_same_provider_lineages_are_isolated_and_empty_tokens_do_not_match() {
    let (_dir, store) = store();
    let first = Record {
        provider: Provider::Claude,
        access_token: "first-access".into(),
        refresh_token: "first-refresh".into(),
        ..Default::default()
    };
    let second = Record {
        provider: Provider::Claude,
        access_token: "second-access".into(),
        refresh_token: "second-refresh".into(),
        ..Default::default()
    };
    store
        .set_pending(Provider::Claude, "first-spent", &first)
        .unwrap();
    store
        .set_pending(Provider::Claude, "second-spent", &second)
        .unwrap();
    assert_eq!(
        store
            .pending_matching(Provider::Claude, "first-access", "first-spent")
            .unwrap()
            .unwrap()
            .1
            .access_token,
        "first-access"
    );
    assert_eq!(
        store
            .pending_matching(Provider::Claude, "second-access", "second-spent")
            .unwrap()
            .unwrap()
            .1
            .access_token,
        "second-access"
    );
    assert!(
        store
            .pending_matching(Provider::Claude, "", "")
            .unwrap()
            .is_none()
    );
}
