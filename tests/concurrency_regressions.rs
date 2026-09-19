use aiu::{
    app::App,
    config::{Config, StoreMode},
    model::{Index, IndexEntry, Provider, Record},
    storage::Store,
};
use base64::Engine;
use serde_json::json;
use std::{sync::mpsc, thread, time::Duration};
use tempfile::TempDir;
use tiny_http::{Response, Server};

fn config(temp: &TempDir) -> Config {
    let mut c = Config::for_home(temp.path().join("home"), temp.path().join("state"));
    c.store_mode = StoreMode::File;
    c.claude_use_keychain = false;
    c.claude_usage_url = "http://127.0.0.1:9/usage".into();
    c.claude_profile_url = "http://127.0.0.1:9/profile".into();
    c.claude_token_urls = vec!["http://127.0.0.1:9/token".into()];
    c.codex_usage_url = "http://127.0.0.1:9/codex-usage".into();
    c
}

fn record(provider: Provider, email: &str) -> Record {
    Record {
        provider,
        email: email.into(),
        access_token: "access-secret".into(),
        refresh_token: "refresh-secret".into(),
        expires_at: chrono::Utc::now().timestamp_millis() + 3_600_000,
        ..Default::default()
    }
}

fn codex_record(email: &str, account: &str) -> Record {
    let mut r = record(Provider::Codex, email);
    r.account_id = account.into();
    let payload = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(
        json!({
            "email": email,
            "exp": chrono::Utc::now().timestamp() + 3600,
            "https://api.openai.com/auth": { "chatgpt_account_id": account }
        })
        .to_string(),
    );
    r.id_token = format!("e30.{payload}.sig");
    r
}

fn seed(config: &Config, records: &[Record]) {
    let store = Store::new(config.clone());
    let mut index = Index::default();
    for record in records {
        store.set(record).unwrap();
        index.accounts.push(IndexEntry {
            email: record.email.clone(),
            label: record.label.clone(),
            org: record.org_uuid.clone(),
            org_name: record.org_name.clone(),
            provider: record.provider,
            added_at: "now".into(),
        });
    }
    store.save_index(&index).unwrap();
}

fn request(server: &Server) -> tiny_http::Request {
    server
        .recv_timeout(Duration::from_secs(5))
        .unwrap()
        .expect("expected request")
}

fn respond(request: tiny_http::Request, status: u16, body: &str) {
    request
        .respond(Response::from_string(body).with_status_code(status))
        .unwrap();
}

#[test]
fn slow_status_request_does_not_block_unrelated_codex_switch() {
    let temp = TempDir::new().unwrap();
    let mut cfg = config(&temp);
    let claude = record(Provider::Claude, "slow@example.test");
    let codex = codex_record("target@example.test", "org-target");
    seed(&cfg, &[claude, codex]);
    let server = Server::http("127.0.0.1:0").unwrap();
    let url = format!("http://{}", server.server_addr());
    cfg.claude_usage_url = format!("{url}/usage");
    let (started_tx, started_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let worker = thread::spawn(move || {
        let request = request(&server);
        started_tx.send(()).unwrap();
        release_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        respond(request, 200, r#"{"five_hour":{"utilization":1}}"#);
    });

    let status_app = App::new(cfg.clone()).unwrap();
    let status = thread::spawn(move || status_app.status(Some(Provider::Claude), true));
    started_rx.recv_timeout(Duration::from_secs(2)).unwrap();
    let switch_app = App::new(cfg).unwrap();
    let switch =
        thread::spawn(move || switch_app.switch("target@example.test", Some(Provider::Codex)));
    let result = switch.join().expect("switch thread panicked");
    assert!(result.is_ok(), "unrelated switch failed: {result:?}");
    release_tx.send(()).unwrap();
    assert!(status.join().unwrap().is_ok());
    worker.join().unwrap();
}

#[test]
fn concurrent_same_account_status_claims_one_request_and_keeps_cache() {
    let temp = TempDir::new().unwrap();
    let mut cfg = config(&temp);
    seed(&cfg, &[record(Provider::Claude, "same@example.test")]);
    let server = Server::http("127.0.0.1:0").unwrap();
    let url = format!("http://{}", server.server_addr());
    cfg.claude_usage_url = format!("{url}/usage");
    let (started_tx, started_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let (count_tx, count_rx) = mpsc::channel();
    let worker = thread::spawn(move || {
        let request = request(&server);
        started_tx.send(()).unwrap();
        release_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        respond(request, 429, r#"{}"#);
        count_tx.send(()).unwrap();
        assert!(
            server
                .recv_timeout(Duration::from_millis(500))
                .unwrap()
                .is_none()
        );
    });

    let first = thread::spawn({
        let app = App::new(cfg.clone()).unwrap();
        move || app.status(Some(Provider::Claude), true)
    });
    started_rx.recv_timeout(Duration::from_secs(2)).unwrap();
    let second = thread::spawn({
        let app = App::new(cfg.clone()).unwrap();
        move || app.status(Some(Provider::Claude), true)
    });
    let second_result = second
        .join()
        .expect("second status thread panicked")
        .expect("second status failed");
    assert_eq!(second_result.accounts.len(), 1);
    release_tx.send(()).unwrap();
    first.join().unwrap().unwrap();
    count_rx.recv_timeout(Duration::from_secs(2)).unwrap();
    worker.join().unwrap();

    let third = App::new(cfg)
        .unwrap()
        .status(Some(Provider::Claude), true)
        .unwrap();
    assert_eq!(third.accounts.len(), 1);
    assert!(third.accounts[0].error.contains("throttled") || !third.accounts[0].stale.is_empty());
}

#[test]
fn remove_during_inflight_refresh_cannot_resurrect_deleted_account() {
    let temp = TempDir::new().unwrap();
    let mut cfg = config(&temp);
    let mut expired = record(Provider::Claude, "remove@example.test");
    expired.expires_at = 1;
    seed(&cfg, &[expired]);
    let server = Server::http("127.0.0.1:0").unwrap();
    let url = format!("http://{}", server.server_addr());
    cfg.claude_usage_url = format!("{url}/usage");
    cfg.claude_token_urls = vec![format!("{url}/token")];
    let (token_started_tx, token_started_rx) = mpsc::channel();
    let (release_tx, release_rx) = mpsc::channel();
    let worker = thread::spawn(move || {
        let token = request(&server);
        token_started_tx.send(()).unwrap();
        release_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        respond(
            token,
            200,
            r#"{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":3600}"#,
        );
        if let Some(request) = server.recv_timeout(Duration::from_millis(750)).unwrap() {
            respond(request, 200, r#"{"five_hour":{"utilization":2}}"#);
        }
    });
    let status_app = App::new(cfg.clone()).unwrap();
    let status = thread::spawn(move || status_app.status(Some(Provider::Claude), true));
    token_started_rx
        .recv_timeout(Duration::from_secs(2))
        .unwrap();
    let remove_app = App::new(cfg.clone()).unwrap();
    let remove =
        thread::spawn(move || remove_app.remove("remove@example.test", Some(Provider::Claude)));
    thread::sleep(Duration::from_millis(100));
    release_tx.send(()).unwrap();
    assert!(remove.join().unwrap().is_ok());
    let _ = status.join().unwrap();
    worker.join().unwrap();
    let store = Store::new(cfg);
    assert!(store.get("claude:remove@example.test").unwrap().is_none());
    assert!(store.load_index().unwrap().accounts.is_empty());
}
