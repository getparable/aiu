use aiu::{
    app::App,
    config::{Config, StoreMode},
    model::{Index, IndexEntry, Provider, Record},
    storage::Store,
};
use serde_json::json;
use std::{
    fs,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    thread,
    time::Duration,
};
use tempfile::TempDir;

fn config(dir: &TempDir) -> Config {
    let mut c = Config::for_home(dir.path().into(), dir.path().join("cfg"));
    c.store_mode = StoreMode::File;
    c.claude_use_keychain = false;
    c.claude_profile_url = "http://127.0.0.1:9/profile".into();
    c
}

fn record(email: &str, access: &str, refresh: &str, expires: i64) -> Record {
    Record {
        provider: Provider::Claude,
        email: email.into(),
        access_token: access.into(),
        refresh_token: refresh.into(),
        expires_at: expires,
        org_uuid: "org".into(),
        ..Default::default()
    }
}

fn seed(c: &Config, r: &Record) {
    let s = Store::new(c.clone());
    s.set(r).unwrap();
    s.save_index(&Index {
        version: 1,
        accounts: vec![IndexEntry {
            email: r.email.clone(),
            org: r.org_uuid.clone(),
            provider: r.provider,
            label: "main".into(),
            ..Default::default()
        }],
    })
    .unwrap();
}

fn live(c: &Config, r: &Record) {
    fs::create_dir_all(&c.claude_dir).unwrap();
    fs::write(c.claude_dir.join(".credentials.json"), json!({"claudeAiOauth":{"accessToken":r.access_token,"refreshToken":r.refresh_token,"expiresAt":r.expires_at}}).to_string()).unwrap();
    fs::write(
        &c.claude_global_config,
        json!({"oauthAccount":{"emailAddress":r.email,"organizationUuid":r.org_uuid}}).to_string(),
    )
    .unwrap();
}

fn server(status: u16, body: &'static str) -> (String, Arc<AtomicUsize>, thread::JoinHandle<()>) {
    let s = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let url = format!("http://{}", s.server_addr());
    let count = Arc::new(AtomicUsize::new(0));
    let seen = count.clone();
    let h = thread::spawn(move || {
        if let Ok(Some(req)) = s.recv_timeout(Duration::from_secs(2)) {
            seen.fetch_add(1, Ordering::SeqCst);
            req.respond(tiny_http::Response::from_string(body).with_status_code(status))
                .unwrap();
        }
    });
    (url, count, h)
}

#[test]
fn passive_status_does_not_refresh_expired_shared_cli_login() {
    let d = TempDir::new().unwrap();
    let mut c = config(&d);
    let r = record("owned@example.test", "same-access", "same-refresh", 1);
    seed(&c, &r);
    live(&c, &r);
    let (url, calls, worker) = server(
        200,
        r#"{"access_token":"should-not-be-used","refresh_token":"rotated"}"#,
    );
    c.claude_token_urls = vec![url];
    let out = App::new(c.clone())
        .unwrap()
        .status(Some(Provider::Claude), false)
        .unwrap();
    worker.join().unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    assert!(out.accounts[0].error.to_ascii_lowercase().contains("cli"));
    assert_eq!(
        Store::new(c.clone())
            .get(&r.key())
            .unwrap()
            .unwrap()
            .access_token,
        "same-access"
    );
    assert_eq!(
        aiu::live::read(&c, Provider::Claude)
            .unwrap()
            .unwrap()
            .access_token,
        "same-access"
    );
}

#[test]
fn shared_login_401_does_not_refresh_until_cli_sync_adopts_new_pair() {
    let d = TempDir::new().unwrap();
    let mut c = config(&d);
    let r = record(
        "shared@example.test",
        "same-access",
        "same-refresh",
        chrono::Utc::now().timestamp_millis() + 3_600_000,
    );
    seed(&c, &r);
    live(&c, &r);
    let (usage, usage_calls, usage_worker) = server(401, "{}");
    let (token, token_calls, token_worker) = server(
        200,
        r#"{"access_token":"bad-refresh","refresh_token":"bad-refresh","expires_in":3600}"#,
    );
    c.claude_usage_url = usage;
    c.claude_token_urls = vec![token];
    let out = App::new(c.clone())
        .unwrap()
        .status(Some(Provider::Claude), false)
        .unwrap();
    usage_worker.join().unwrap();
    token_worker.join().unwrap();
    assert_eq!(usage_calls.load(Ordering::SeqCst), 1);
    assert_eq!(token_calls.load(Ordering::SeqCst), 0);
    assert!(out.accounts[0].login.state != "ok");
    assert_eq!(
        Store::new(c.clone())
            .get(&r.key())
            .unwrap()
            .unwrap()
            .access_token,
        "same-access"
    );

    let newer = record(
        "shared@example.test",
        "new-access",
        "new-refresh",
        chrono::Utc::now().timestamp_millis() + 7_200_000,
    );
    live(&c, &newer);
    // The active CLI token has no trusted cached identity; sync must verify it
    // through the profile endpoint before adopting it.
    fs::write(&c.claude_global_config, "{}").unwrap();
    let profile_server = tiny_http::Server::http("127.0.0.1:0").unwrap();
    let profile_url = format!("http://{}", profile_server.server_addr());
    let bearer_seen = Arc::new(AtomicUsize::new(0));
    let profile_seen = Arc::new(AtomicUsize::new(0));
    let bearer_flag = bearer_seen.clone();
    let profile_count = profile_seen.clone();
    let profile_worker = thread::spawn(move || {
        let req = profile_server.recv().unwrap();
        profile_count.fetch_add(1, Ordering::SeqCst);
        if req.headers().iter().any(|h| {
            h.field.to_string().eq_ignore_ascii_case("authorization")
                && h.value.as_str().contains("new-access")
        }) {
            bearer_flag.store(1, Ordering::SeqCst);
        }
        req.respond(tiny_http::Response::from_string(
            r#"{"account":{"email":"shared@example.test"},"organization":{"uuid":"org"}}"#,
        ))
        .unwrap();
    });
    c.claude_profile_url = profile_url;
    let _ = App::new(c.clone())
        .unwrap()
        .sync(Some(Provider::Claude))
        .unwrap();
    profile_worker.join().unwrap();
    assert_eq!(profile_seen.load(Ordering::SeqCst), 1);
    assert_eq!(bearer_seen.load(Ordering::SeqCst), 1);
    let cached: serde_json::Value = aiu::storage::read_json(&c.cache_file()).unwrap().unwrap();
    assert!(!cached[r.key()]["needsLogin"].as_bool().unwrap_or(false));
}

#[test]
fn explicit_switch_refreshes_expired_saved_login_and_updates_cli() {
    let d = TempDir::new().unwrap();
    let mut c = config(&d);
    let r = record("switch@example.test", "old-access", "old-refresh", 1);
    seed(&c, &r);
    live(&c, &r);
    let (url, calls, worker) = server(
        200,
        r#"{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}"#,
    );
    c.claude_token_urls = vec![url];
    let warnings = App::new(c.clone())
        .unwrap()
        .switch("switch@example.test", Some(Provider::Claude))
        .unwrap();
    worker.join().unwrap();
    assert!(warnings.is_empty());
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        Store::new(c.clone())
            .get(&r.key())
            .unwrap()
            .unwrap()
            .access_token,
        "new-access"
    );
    assert_eq!(
        aiu::live::read(&c, Provider::Claude)
            .unwrap()
            .unwrap()
            .access_token,
        "new-access"
    );
}
